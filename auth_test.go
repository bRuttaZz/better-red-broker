package main

import (
	"regexp"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-256 rows (8 digits). The Android app's tests use the same values.
func TestTOTPVectors(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	for _, c := range []struct {
		t    int64
		want string
	}{{59, "46119246"}, {1111111109, "68084774"}, {1111111111, "67062674"}, {1234567890, "91819424"}, {2000000000, "90698825"}} {
		if got := totp(key, c.t/codeStep); got != c.want {
			t.Errorf("totp(%d) = %s, want %s", c.t, got, c.want)
		}
	}
}

func TestVerifyCodeWindow(t *testing.T) {
	key := []byte("k")
	now := time.Unix(1_800_000_000, 0)
	for d := int64(-2); d <= 2; d++ {
		if !verifyCode(key, totp(key, now.Unix()/codeStep+d), now) {
			t.Errorf("step %+d should be accepted", d)
		}
	}
	for _, d := range []int64{-3, 3} {
		if verifyCode(key, totp(key, now.Unix()/codeStep+d), now) {
			t.Errorf("step %+d should be rejected", d)
		}
	}
	if verifyCode(key, "", now) || verifyCode(key, "1234567", now) {
		t.Error("malformed codes must be rejected")
	}
}

// A fixed value, also checked by the Android app (ShareCryptoTest), so both hash device keys alike.
func TestDeviceIDVector(t *testing.T) {
	if got := deviceIDFromKey(make([]byte, 32)); got != deviceIDVector {
		t.Errorf("device id = %s", got)
	}
}

func TestPairIDs(t *testing.T) {
	s := Signer{key: []byte("0123456789abcdef0123456789abcdef")}
	other := Signer{key: []byte("ffffffffffffffffffffffffffffffff")}
	dev := deviceIDFromKey([]byte("device"))
	id, _ := s.PairID(dev)
	id2, _ := s.PairID(dev)
	if len(id) != pairIDLen || !s.Valid(id) || !s.Valid(id2) {
		t.Fatalf("own id %q should be valid", id)
	}
	if id == id2 || deviceOf(id) != dev || deviceOf(id2) != dev {
		t.Error("each pairing of a device must get a new (salted) id with the same device part")
	}
	swapped := deviceIDFromKey([]byte("other")) + id[idTextLen:]
	if s.Valid(swapped) {
		t.Error("a signature must not work for another device")
	}
	if other.Valid(id) {
		t.Error("id signed with another key must be invalid")
	}
	bad := []byte(id)
	bad[len(bad)-1] ^= 1
	for _, x := range []string{"", "x", string(bad), id + "A", id[:pairIDLen-1] + "!", "../../etc/passwd"} {
		if s.Valid(x) {
			t.Errorf("%q should be invalid", x)
		}
	}
}

func TestVersionFile(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`).MatchString(version) {
		t.Errorf("VERSION %q should look like 1.2.3 or 1.2.3-rc1", version)
	}
}
