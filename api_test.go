package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testEnv struct {
	t     *testing.T
	api   *API
	srv   *httptest.Server
	now   time.Time
	store *Store
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := Config{MaxDays: 60, StaleDays: 10, MaxPairs: 3, Readers: 2, RateLimit: true}
	store, err := OpenStore(filepath.Join(t.TempDir(), "b.db"), cfg.Readers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &testEnv{t: t, store: store, now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	e.api = NewAPI(cfg, store, Signer{key: bytes.Repeat([]byte{7}, 32)})
	e.api.now = func() time.Time { return e.now }
	e.api.general = newLimiter(1000, 1000)
	e.api.mint = newLimiter(1000, 1000)
	e.srv = httptest.NewServer(newHandler(cfg, e.api))
	t.Cleanup(e.srv.Close)
	return e
}

// do sends a request; headers are name/value pairs.
func (e *testEnv) do(method, path string, body any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func key(b byte) string { return b64.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func blob(n int) string {
	raw := make([]byte, n)
	raw[0] = blobVersion
	return b64.EncodeToString(raw)
}

// One pairing: what her phone has (device key) and what the QR code gives the partner (pair id, secret).
type pairing struct {
	e               *testEnv
	id, dev, secret string
}

func (e *testEnv) mint(dev string) *pairing {
	e.t.Helper()
	status, body := e.do("POST", "/v1/pairs", map[string]string{"deviceKey": dev})
	if status != http.StatusCreated {
		e.t.Fatalf("mint: %d %v", status, body)
	}
	return &pairing{e: e, id: body["pairId"].(string), dev: dev, secret: key(42)}
}

func (p *pairing) her() []string { return []string{pairHeader, p.id, deviceHeader, p.dev} }

func (p *pairing) partner() []string {
	s, _ := b64.DecodeString(p.secret)
	return []string{pairHeader, p.id, codeHeader, totp(s, p.e.now.Unix()/codeStep)}
}

func (p *pairing) push(put map[string]Entry, del ...string) (int, map[string]any) {
	if del == nil {
		del = []string{}
	}
	return p.e.do("PUT", "/v1/days", map[string]any{"secret": p.secret, "put": put, "delete": del}, p.her()...)
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	p := e.mint(key(1))

	// Minting stores nothing: the partner has nothing to read yet.
	if n, _ := e.store.Count(context.Background()); n != 0 {
		t.Fatalf("mint must not store anything, %d rows", n)
	}
	if s, _ := e.do("GET", "/v1/days", nil, p.partner()...); s != 404 {
		t.Fatalf("read before any write: %d", s)
	}
	if s, b := e.do("GET", "/v1/days/index", nil, p.her()...); s != 200 || len(b["days"].(map[string]any)) != 0 {
		t.Fatalf("index before any write: %d %v", s, b)
	}

	// Her first write creates the row.
	put := map[string]Entry{
		"0":          {Blob: blob(120), UpdatedAt: e.now.Unix()},
		"2026-11-20": {Blob: blob(100), UpdatedAt: e.now.Unix() - 50},
		"2026-11-21": {Blob: blob(100), UpdatedAt: e.now.Unix() - 40},
	}
	if s, b := p.push(put); s != 200 {
		t.Fatalf("push: %d %v", s, b)
	}
	_, b := e.do("GET", "/v1/days/index", nil, p.her()...)
	days := b["days"].(map[string]any)
	if len(days) != 3 || days["2026-11-20"].(float64) != float64(e.now.Unix()-50) {
		t.Fatalf("index: %v", b)
	}

	// The partner reads with a code from the secret.
	s, b := e.do("GET", "/v1/days", nil, p.partner()...)
	full := b["days"].(map[string]any)
	if s != 200 || len(full) != 3 || full["2026-11-21"].(map[string]any)["blob"] != blob(100) {
		t.Fatalf("read: %d %v", s, b)
	}

	// The partner can't write or index: it doesn't have her device key.
	partnerAsHer := []string{pairHeader, p.id, deviceHeader, key(42)}
	if s, _ := e.do("PUT", "/v1/days", map[string]any{"secret": p.secret}, partnerAsHer...); s != 401 {
		t.Fatalf("write without her device key: %d", s)
	}
	if s, _ := e.do("GET", "/v1/days/index", nil, pairHeader, p.id); s != 401 {
		t.Fatalf("index without device key: %d", s)
	}

	// Delete one day.
	if s, _ := p.push(nil, "2026-11-20"); s != 200 {
		t.Fatal("delete day failed")
	}
	_, b = e.do("GET", "/v1/days/index", nil, p.her()...)
	if len(b["days"].(map[string]any)) != 2 {
		t.Fatalf("after delete: %v", b)
	}

	// A new pairing of the same device: a new id, and the old pair is gone.
	p2 := e.mint(key(1))
	if p2.id == p.id {
		t.Fatal("a new pairing must get a new id")
	}
	if s, _ := e.do("GET", "/v1/days", nil, p.partner()...); s != 404 {
		t.Fatalf("old partner after re-pairing: %d", s)
	}

	// Unpair: delete.
	p2.push(map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}})
	if s, _ := e.do("DELETE", "/v1/days", nil, p2.her()...); s != 204 {
		t.Fatalf("delete: %d", s)
	}
	if s, _ := e.do("GET", "/v1/days", nil, p2.partner()...); s != 404 {
		t.Fatalf("after delete: %d", s)
	}
}

// The broker can lose everything: her next write puts it all back, the same pair id still works.
func TestServerDataLossHeals(t *testing.T) {
	e := newEnv(t)
	p := e.mint(key(1))
	p.push(map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}})
	if _, err := e.store.w.Exec("DELETE FROM pair"); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.do("GET", "/v1/days", nil, p.partner()...); s != 404 {
		t.Fatalf("partner while empty: %d (should wait)", s)
	}
	_, b := e.do("GET", "/v1/days/index", nil, p.her()...)
	if len(b["days"].(map[string]any)) != 0 {
		t.Fatal("her phone should see an empty index and send everything")
	}
	p.push(map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}})
	if s, _ := e.do("GET", "/v1/days", nil, p.partner()...); s != 200 {
		t.Fatalf("partner after refill: %d", s)
	}
}

func TestRejections(t *testing.T) {
	e := newEnv(t)
	p := e.mint(key(4))
	put := func(k string, en Entry) int { s, _ := p.push(map[string]Entry{k: en}); return s }
	ok := Entry{Blob: blob(100), UpdatedAt: e.now.Unix()}
	cases := map[string]int{
		"too old":        put("2026-09-01", ok),
		"future":         put("2026-12-30", ok),
		"bad date":       put("2026-02-30", ok),
		"bad key":        put("hello", ok),
		"tiny blob":      put("2026-11-20", Entry{Blob: blob(10), UpdatedAt: e.now.Unix()}),
		"huge blob":      put("2026-11-20", Entry{Blob: blob(maxBlobBytes + 1), UpdatedAt: e.now.Unix()}),
		"bad version":    put("2026-11-20", Entry{Blob: b64.EncodeToString(make([]byte, 100)), UpdatedAt: e.now.Unix()}),
		"not base64":     put("2026-11-20", Entry{Blob: "!!!" + blob(100), UpdatedAt: e.now.Unix()}),
		"bad updatedAt":  put("2026-11-20", Entry{Blob: blob(100), UpdatedAt: 5}),
		"future updated": put("2026-11-20", Entry{Blob: blob(100), UpdatedAt: e.now.Unix() + 3*86400}),
	}
	for name, status := range cases {
		if status != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, status)
		}
	}
	if s := put("2026-11-20", ok); s != 200 {
		t.Fatalf("valid push: %d", s)
	}
	// Secret missing or malformed.
	for _, sec := range []string{"", "short", key(1) + "A"} {
		if s, _ := e.do("PUT", "/v1/days", map[string]any{"secret": sec, "put": map[string]Entry{}}, p.her()...); s != 400 {
			t.Errorf("secret %q: %d", sec, s)
		}
	}
	// Body too large.
	big := map[string]any{"secret": p.secret, "put": map[string]Entry{"0": {Blob: strings.Repeat("A", maxPushBody), UpdatedAt: e.now.Unix()}}}
	if s, _ := e.do("PUT", "/v1/days", big, p.her()...); s != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d", s)
	}
	// Unknown fields.
	if s, _ := e.do("PUT", "/v1/days", map[string]any{"secret": p.secret, "x": 1}, p.her()...); s != 400 {
		t.Errorf("unknown field: %d", s)
	}
	// Made-up, tampered, missing or re-targeted pair ids are refused before any database work.
	for _, id := range []string{"", "nonsense", p.id[:len(p.id)-2] + "AA", deviceIDFromKey([]byte("x")) + p.id[idTextLen:]} {
		if s, _ := e.do("GET", "/v1/days", nil, pairHeader, id, codeHeader, "12345678"); s != 403 {
			t.Errorf("pair id %q: %d", id, s)
		}
		e.api.badAuth = newLimiter(1000, 1000)
	}
	// Bad mint bodies.
	for _, b := range []any{map[string]string{"deviceKey": "short"}, map[string]string{}, "nope"} {
		if s, _ := e.do("POST", "/v1/pairs", b); s != 400 {
			t.Errorf("mint %v: %d", b, s)
		}
	}
	// Server full (MaxPairs = 3): only new rows are refused, existing ones keep working.
	for i := byte(10); i < 12; i++ {
		if s, _ := e.mint(key(i)).push(map[string]Entry{"0": ok}); s != 200 {
			t.Fatalf("push %d: %d", i, s)
		}
	}
	if s, _ := e.mint(key(12)).push(map[string]Entry{"0": ok}); s != 503 {
		t.Errorf("full: %d", s)
	}
	if s, _ := p.push(map[string]Entry{"0": ok}); s != 200 {
		t.Errorf("existing pair when full: %d", s)
	}
}

func TestWrongCodesAreRateLimited(t *testing.T) {
	e := newEnv(t)
	p := e.mint(key(1))
	p.push(map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}})
	got429 := false
	for i := 0; i < 15; i++ {
		s, _ := e.do("GET", "/v1/days", nil, pairHeader, p.id, codeHeader, "00000000")
		if s == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("guessing codes should be rate limited")
	}
}

func TestCleanup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.mint(key(1))
	p.push(map[string]Entry{
		"0":          {Blob: blob(100), UpdatedAt: e.now.Unix()},
		"2026-09-30": {Blob: blob(100), UpdatedAt: e.now.Unix()},
		"2026-11-20": {Blob: blob(100), UpdatedAt: e.now.Unix()},
	})
	// An abandoned pair.
	q := e.mint(key(9))
	q.push(map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}})
	if _, err := e.store.w.Exec("UPDATE pair SET updated_at = ? WHERE pair_id = ?", e.now.AddDate(0, 0, -11).Unix(), q.id); err != nil {
		t.Fatal(err)
	}

	e.now = e.now.AddDate(0, 0, 7) // a week later: 2026-09-30 falls out of the 60(+1)-day window
	oldest, _ := e.api.window()
	deleted, pruned, err := e.store.Cleanup(ctx, e.now.AddDate(0, 0, -10), oldest)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || pruned != 1 {
		t.Fatalf("deleted %d, pruned %d", deleted, pruned)
	}
	got, err := e.store.Get(ctx, p.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Data["2026-09-30"]; ok || len(got.Data) != 2 {
		t.Fatalf("after cleanup: %v", got.Data)
	}
}

func TestSite(t *testing.T) {
	e := newEnv(t)
	res, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(raw)
	for _, want := range []string{"Better Red", "Download for Android", version, "Source code"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing CSP header")
	}
}
