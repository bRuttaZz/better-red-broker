package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Limits. Generous for one person's 60 days, small enough that nobody can fill the disk quickly.
const (
	maxMintBody  = 1 << 10   // 1 KB
	maxPushBody  = 512 << 10 // 512 KB
	minBlobBytes = 1 + 65 + 12 + 16 + 1
	maxBlobBytes = 4 << 10 // 4 KB per day after decoding
	maxBlobText  = (maxBlobBytes*4 + 2) / 3
	configKey    = "0"
	blobVersion  = 1
)

var dayKey = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

/*
API serves /v1/…. The broker is dumb on purpose:

  - POST /v1/pairs          her phone mints a pair id. Nothing is stored (old pairs of her device are deleted).
  - PUT  /v1/days           her phone writes (with the pair secret). The row is created if missing.
  - GET  /v1/days/index     her phone asks what's there (keys + times). Empty if nothing is stored.
  - DELETE /v1/days         her phone deletes the row (unpair).
  - GET  /v1/days           the partner reads, with a one-time code made from the pair secret.

The pair id travels in the X-Better-Red-Pair header (not the URL, so it stays out of URL logs).
Her requests carry her device key (X-Better-Red-Device): the server hashes it and compares it with
the device id inside the pair id. The partner's carry a one-time code (X-Better-Red-Code).
Every handler validates its input completely before touching the database.
*/
type API struct {
	cfg          Config
	store        *Store
	signer       Signer
	now          func() time.Time
	ips          clientIPs
	passwordHash []byte   // SHA-256 of the API password, nil when the broker is open
	general      *limiter // every request         (nil when -rate-limit=false: everything allowed)
	mint         *limiter // pair ids minted
	badAuth      *limiter // failed authentication
}

func NewAPI(cfg Config, store *Store, signer Signer) *API {
	a := &API{cfg: cfg, store: store, signer: signer, now: time.Now,
		ips: clientIPs{header: cfg.ClientIP, trusted: cfg.Proxies}}
	if cfg.Password != "" {
		h := sha256.Sum256([]byte(cfg.Password))
		a.passwordHash = h[:]
	}
	if cfg.RateLimit {
		a.general = newLimiter(2, 60)     // 2/s sustained, bursts of 60
		a.mint = newLimiter(1.0/360, 5)   // 10 per hour, bursts of 5
		a.badAuth = newLimiter(1.0/6, 10) // 10 per minute
	}
	return a
}

func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/info", a.info) // public: says whether a password is needed
	mux.HandleFunc("POST /v1/pairs", a.locked(a.mintPair))
	mux.HandleFunc("GET /v1/days", a.locked(a.read))
	mux.HandleFunc("GET /v1/days/index", a.locked(a.index))
	mux.HandleFunc("PUT /v1/days", a.locked(a.push))
	mux.HandleFunc("DELETE /v1/days", a.locked(a.remove))
}

// locked checks the optional API password (BR_PASSWORD) before anything else. Without one, every
// request passes. Wrong or missing passwords count as failed authentication (rate-limited).
func (a *API) locked(next http.HandlerFunc) http.HandlerFunc {
	if a.passwordHash == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := a.ips.of(r)
		if !a.badAuth.peek(ip, a.now()) {
			a.fail(w, http.StatusTooManyRequests, "too many failed attempts, try later")
			return
		}
		// Compare hashes, so the time taken says nothing about the password or its length.
		got := sha256.Sum256([]byte(r.Header.Get(passwordHeader)))
		if subtle.ConstantTimeCompare(got[:], a.passwordHash) != 1 {
			w.Header().Set("WWW-Authenticate", passwordChallenge)
			a.denied(w, ip, http.StatusUnauthorized, "this server needs a password")
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------- helpers

type apiError struct {
	Error      string `json:"error"`
	ServerTime int64  `json:"serverTime,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg, ServerTime: a.now().Unix()})
}

// decode reads a JSON body of at most max bytes, rejecting unknown fields and trailing data.
func decode(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("content type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON body")
	}
	return nil
}

func decodeKey(s string) ([]byte, bool) {
	b, err := b64.DecodeString(s)
	return b, err == nil && len(b) == keyLen && b64.EncodeToString(b) == s
}

// window is the range of day keys a pair may hold right now: [oldest, newest].
// One extra day on each side covers time zones (the phone uses local dates, the server UTC).
func (a *API) window() (oldest, newest string) {
	today := a.now().UTC()
	return today.AddDate(0, 0, -(a.cfg.MaxDays + 1)).Format(time.DateOnly), today.AddDate(0, 0, 1).Format(time.DateOnly)
}

func validDayKey(k string) bool {
	if !dayKey.MatchString(k) {
		return false
	}
	_, err := time.Parse(time.DateOnly, k)
	return err == nil
}

func (a *API) denied(w http.ResponseWriter, ip string, status int, msg string) {
	a.badAuth.allow(ip, a.now())
	a.fail(w, status, msg)
}

// pairID reads and checks the pair id header (signature only: no database work for made-up ids).
func (a *API) pairID(w http.ResponseWriter, r *http.Request, ip string) (string, bool) {
	if !a.badAuth.peek(ip, a.now()) {
		a.fail(w, http.StatusTooManyRequests, "too many failed attempts, try later")
		return "", false
	}
	id := r.Header.Get(pairHeader)
	if !a.signer.Valid(id) {
		a.denied(w, ip, http.StatusForbidden, "invalid pair id")
		return "", false
	}
	return id, true
}

// herPair authenticates her phone: a valid pair id, and the device key whose hash is the
// device id inside it. Nothing is looked up: the check is pure computation.
func (a *API) herPair(w http.ResponseWriter, r *http.Request) (string, bool) {
	ip := a.ips.of(r)
	id, ok := a.pairID(w, r, ip)
	if !ok {
		return "", false
	}
	key, ok := decodeKey(r.Header.Get(deviceHeader))
	if !ok || subtle.ConstantTimeCompare([]byte(deviceIDFromKey(key)), []byte(deviceOf(id))) != 1 {
		a.denied(w, ip, http.StatusUnauthorized, "not this pair's device")
		return "", false
	}
	return id, true
}

func (a *API) internal(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	log.Printf("error: %v", err)
	a.fail(w, http.StatusInternalServerError, "server error")
}

func (a *API) inWindow(data map[string]Entry) map[string]Entry {
	oldest, _ := a.window()
	out := make(map[string]Entry, len(data))
	for k, e := range data {
		if k == configKey || k >= oldest { // never hand out more than the window, even before clean-up runs
			out[k] = e
		}
	}
	return out
}

// ---------------------------------------------------------------- handlers

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "better-red-broker", "version": version,
		"maxDays": a.cfg.MaxDays, "staleDays": a.cfg.StaleDays, "serverTime": a.now().Unix(),
		"password": a.passwordHash != nil,
	})
}

type mintReq struct {
	DeviceKey string `json:"deviceKey"` // 32 random bytes only her phone has; its hash is the device id
}

// mintPair: POST /v1/pairs. Signs a new pair id for her device. Stores nothing; deletes every
// earlier pair of the device ("<device-id>.*"), so the previous partner loses access.
func (a *API) mintPair(w http.ResponseWriter, r *http.Request) {
	if !a.mint.allow(a.ips.of(r), a.now()) {
		a.fail(w, http.StatusTooManyRequests, "too many pairings from this network, try later")
		return
	}
	var req mintReq
	if err := decode(w, r, maxMintBody, &req); err != nil {
		a.fail(w, http.StatusBadRequest, "bad request")
		return
	}
	deviceKey, ok := decodeKey(req.DeviceKey)
	if !ok {
		a.fail(w, http.StatusBadRequest, "deviceKey must be 32 bytes, base64url")
		return
	}
	deviceID := deviceIDFromKey(deviceKey)
	pairID, err := a.signer.PairID(deviceID)
	if err == nil {
		err = a.store.DeleteDevice(r.Context(), deviceID)
	}
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pairId": pairID, "maxDays": a.cfg.MaxDays})
}

// read: GET /v1/days. The partner, with a one-time code made from the pair secret.
// 404 = nothing stored (yet, or any more): the partner waits for her phone.
func (a *API) read(w http.ResponseWriter, r *http.Request) {
	ip := a.ips.of(r)
	id, ok := a.pairID(w, r, ip)
	if !ok {
		return
	}
	p, err := a.store.Get(r.Context(), id)
	if errors.Is(err, errNotFound) {
		a.fail(w, http.StatusNotFound, "nothing stored for this pair")
		return
	} else if err != nil {
		a.internal(w, err)
		return
	}
	if !verifyCode(p.Secret, r.Header.Get(codeHeader), a.now()) {
		a.denied(w, ip, http.StatusUnauthorized, "wrong or expired code (check the phone's clock)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": a.inWindow(p.Data), "maxDays": a.cfg.MaxDays, "updatedAt": p.UpdatedAt})
}

// index: GET /v1/days/index. Her phone: which keys are stored and when they changed, no blobs.
// Nothing stored → empty, and her phone sends everything again.
func (a *API) index(w http.ResponseWriter, r *http.Request) {
	id, ok := a.herPair(w, r)
	if !ok {
		return
	}
	days := map[string]int64{}
	p, err := a.store.Get(r.Context(), id)
	if err != nil && !errors.Is(err, errNotFound) {
		a.internal(w, err)
		return
	}
	if p != nil {
		for k, e := range a.inWindow(p.Data) {
			days[k] = e.UpdatedAt
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "maxDays": a.cfg.MaxDays})
}

type pushReq struct {
	Secret string           `json:"secret"` // the pair secret (also in the QR code); stored as sent
	Put    map[string]Entry `json:"put"`    // add or replace these keys
	Delete []string         `json:"delete"` // remove these keys
}

// push: PUT /v1/days. Her phone writes; the row is created if it doesn't exist.
func (a *API) push(w http.ResponseWriter, r *http.Request) {
	id, ok := a.herPair(w, r)
	if !ok {
		return
	}
	var req pushReq
	if err := decode(w, r, maxPushBody, &req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			a.fail(w, http.StatusBadRequest, "bad request")
		}
		return
	}
	secret, ok := decodeKey(req.Secret)
	if !ok {
		a.fail(w, http.StatusBadRequest, "secret must be 32 bytes, base64url")
		return
	}
	if msg := a.validatePush(&req); msg != "" {
		a.fail(w, http.StatusBadRequest, msg)
		return
	}
	oldest, _ := a.window()
	err := a.store.Upsert(r.Context(), id, secret, a.now(), a.cfg.MaxPairs, func(data map[string]Entry) {
		for _, k := range req.Delete {
			delete(data, k)
		}
		for k, e := range req.Put {
			data[k] = e
		}
		for k := range data { // drop anything that fell out of the window
			if k != configKey && k < oldest {
				delete(data, k)
			}
		}
	})
	switch {
	case errors.Is(err, errFull):
		a.fail(w, http.StatusServiceUnavailable, "this server is full")
	case err != nil:
		a.internal(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updatedAt": a.now().Unix()})
	}
}

// validatePush returns a reason when the request is not acceptable, or "".
func (a *API) validatePush(req *pushReq) string {
	maxKeys := a.cfg.MaxDays + 4 // the window (+2 days slack) and the config
	if len(req.Put) > maxKeys || len(req.Delete) > maxKeys {
		return "too many keys"
	}
	oldest, newest := a.window()
	nowUnix := a.now().Unix()
	for k, e := range req.Put {
		if k != configKey && (!validDayKey(k) || k < oldest || k > newest) {
			return "key outside the allowed days: " + k
		}
		if e.UpdatedAt < 1_577_836_800 || e.UpdatedAt > nowUnix+86_400 { // 2020-01-01 … tomorrow
			return "bad updatedAt for " + k
		}
		if len(e.Blob) == 0 || len(e.Blob) > maxBlobText {
			return "bad blob size for " + k
		}
		raw, err := b64.DecodeString(e.Blob)
		if err != nil || len(raw) < minBlobBytes || len(raw) > maxBlobBytes || raw[0] != blobVersion {
			return "bad blob for " + k
		}
	}
	for _, k := range req.Delete {
		if k != configKey && !validDayKey(k) {
			return "bad delete key"
		}
	}
	return ""
}

// remove: DELETE /v1/days. Her phone unpairs: the row is deleted (fine if it wasn't there).
func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := a.herPair(w, r)
	if !ok {
		return
	}
	if err := a.store.Delete(r.Context(), id); err != nil {
		a.internal(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
