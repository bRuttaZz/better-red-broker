package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPassword(t *testing.T) {
	const pw = "s3cret@Pass|word"
	e := newEnvWith(t, pw)
	dev := key(1)

	// Without the password: refused with the password challenge, before anything else.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/pairs", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") != passwordChallenge {
		t.Fatalf("no password: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	if s, _ := e.do("POST", "/v1/pairs", map[string]string{"deviceKey": dev}, passwordHeader, "wrong"); s != 401 {
		t.Errorf("wrong password: %d", s)
	}

	// With it, the whole flow works as usual.
	s, body := e.do("POST", "/v1/pairs", map[string]string{"deviceKey": dev}, passwordHeader, pw)
	if s != 201 {
		t.Fatalf("mint: %d %v", s, body)
	}
	p := &pairing{e: e, id: body["pairId"].(string), dev: dev, secret: key(42)}
	put := map[string]any{"secret": p.secret, "put": map[string]Entry{"0": {Blob: blob(100), UpdatedAt: e.now.Unix()}}, "delete": []string{}}
	if s, _ := e.do("PUT", "/v1/days", put, append(p.her(), passwordHeader, pw)...); s != 200 {
		t.Errorf("write: %d", s)
	}
	if s, _ := e.do("PUT", "/v1/days", put, p.her()...); s != 401 {
		t.Errorf("write without password: %d", s)
	}
	if s, _ := e.do("GET", "/v1/days", nil, append(p.partner(), passwordHeader, pw)...); s != 200 {
		t.Errorf("partner read: %d", s)
	}
	if s, _ := e.do("GET", "/v1/days", nil, p.partner()...); s != 401 {
		t.Errorf("partner read without password: %d", s)
	}

	// Public: info (says a password is needed), the website, health.
	if s, info := e.do("GET", "/v1/info", nil); s != 200 || info["password"] != true {
		t.Errorf("info: %d %v", s, info)
	}
	for _, path := range []string{"/", "/healthz"} {
		if s, _ := e.do("GET", path, nil); s != 200 {
			t.Errorf("%s: %d", path, s)
		}
	}

	// Guessing the password is rate-limited like any failed authentication.
	got429 := false
	for i := 0; i < 15 && !got429; i++ {
		s, _ := e.do("GET", "/v1/days/index", nil, passwordHeader, "guess")
		got429 = s == 429
	}
	if !got429 {
		t.Error("password guessing should be rate limited")
	}
}

func TestOpenBrokerIgnoresPasswordHeader(t *testing.T) {
	e := newEnv(t)
	if s, info := e.do("GET", "/v1/info", nil); s != 200 || info["password"] != false {
		t.Errorf("info: %d %v", s, info)
	}
	if s, _ := e.do("POST", "/v1/pairs", map[string]string{"deviceKey": key(1)}, passwordHeader, "anything"); s != 201 {
		t.Errorf("an open broker must ignore a password: %d", s)
	}
}

func TestLoadPassword(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(file, []byte("from-file\n"), 0o600)
	cases := []struct {
		env, file, want string
		ok              bool
	}{
		{"", "", "", true},
		{"abc", "", "abc", true},
		{"", file, "from-file", true},
		{"abc", file, "", false},
		{"has space", "", "", false},
		{"ünïcode", "", "", false},
		{string(make([]byte, 129)), "", "", false},
	}
	for _, c := range cases {
		got, err := loadPassword(c.env, c.file)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("loadPassword(%q, %q) = %q, %v", c.env, c.file, got, err)
		}
	}
}
