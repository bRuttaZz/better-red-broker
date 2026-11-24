package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// All binary values on the wire are base64url without padding.
var b64 = base64.RawURLEncoding

const (
	keyLen       = 32 // device keys and pair secrets: 32 random bytes
	idLen        = 16 // device id: 16 bytes (22 base64 characters)
	idTextLen    = 22
	saltLen      = 8                           // random salt in every pair id
	macLen       = 16                          // signature length
	signTextLen  = 32                          // base64 of salt (8) + signature (16)
	pairIDLen    = idTextLen + 1 + signTextLen // "<device-id>.<salted signature>"
	pairHeader   = "X-Better-Red-Pair"         // every /v1/days request: the pair id
	codeHeader   = "X-Better-Red-Code"         // the partner: one-time code from the pair secret
	deviceHeader = "X-Better-Red-Device"       // her phone: the device key (proves the device id is hers)
	codeDigits   = 8
	codeStep     = 30 // seconds
	codeWindow   = 2  // accept codes up to 2 steps (60 s) early or late (phone clocks drift)
)

// deviceIDFromKey turns the phone's secret device key into its public device id.
// Only her phone knows the key, so only it can mint pair ids for its device id and write data.
// The server keeps neither: it recomputes the id from the key on each request.
func deviceIDFromKey(deviceKey []byte) string {
	h := sha256.Sum256(deviceKey)
	return b64.EncodeToString(h[:idLen])
}

// Signer mints and checks pair ids: "<device-id>.<salted signature>". The second part is
// base64url(salt ‖ HMAC-SHA256(server key, device id ‖ salt)[:16]) with a fresh random salt, so
// every pairing gets a new id. Minting stores nothing: any pair id can be checked from the server
// key alone, and nobody without the key can make one.
type Signer struct{ key []byte }

func (s Signer) mac(deviceID string, salt []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("better-red/pair/v1\x00" + deviceID + "\x00"))
	m.Write(salt)
	return m.Sum(nil)[:macLen]
}

// PairID mints a new pair id for a device.
func (s Signer) PairID(deviceID string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return deviceID + "." + b64.EncodeToString(append(salt, s.mac(deviceID, salt)...)), nil
}

// Valid reports whether pairID is well formed and was signed by this server.
func (s Signer) Valid(pairID string) bool {
	if len(pairID) != pairIDLen || pairID[idTextLen] != '.' {
		return false
	}
	deviceID, sig := pairID[:idTextLen], pairID[idTextLen+1:]
	id, err1 := b64.DecodeString(deviceID)
	raw, err2 := b64.DecodeString(sig)
	if err1 != nil || err2 != nil || len(id) != idLen || len(raw) != saltLen+macLen {
		return false
	}
	// Re-encoding must give the same text (rejects non-canonical base64).
	if b64.EncodeToString(id) != deviceID || b64.EncodeToString(raw) != sig {
		return false
	}
	return hmac.Equal(raw[saltLen:], s.mac(deviceID, raw[:saltLen]))
}

// deviceOf is the device id part of a (valid) pair id.
func deviceOf(pairID string) string { return pairID[:idTextLen] }

// pairPrefixRange gives the [from, to) key range of every pair id of a device, for deletes.
func pairPrefixRange(deviceID string) (string, string) {
	return deviceID + ".", deviceID + "/" // '/' is the character right after '.'
}

// totp is RFC 6238 with HMAC-SHA256, 30 second steps and 8 digits.
func totp(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha256.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", codeDigits, bin%100_000_000)
}

// verifyCode checks a one-time code against every step in the window, in constant time.
func verifyCode(key []byte, code string, now time.Time) bool {
	if len(code) != codeDigits {
		return false
	}
	step := now.Unix() / codeStep
	ok := 0
	for d := int64(-codeWindow); d <= codeWindow; d++ {
		ok |= subtle.ConstantTimeCompare([]byte(totp(key, step+d)), []byte(code))
	}
	return ok == 1
}

// loadOrCreateKey reads the server signing key (hex), or creates a new random one (mode 0600).
// Losing this key makes every existing pair id invalid: partners would have to pair again.
func loadOrCreateKey(path string) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv("BR_SERVER_KEY")); v != "" {
		return parseKey(v)
	}
	if raw, err := os.ReadFile(path); err == nil {
		return parseKey(strings.TrimSpace(string(raw)))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, err
	}
	return key, f.Sync()
}

func parseKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(s)
	if err != nil || len(key) < keyLen {
		return nil, errors.New("server key must be at least 32 bytes, hex encoded")
	}
	return key, nil
}
