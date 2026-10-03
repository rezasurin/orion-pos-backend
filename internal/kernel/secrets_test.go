package kernel

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func testKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestBoxRoundTrip(t *testing.T) {
	b, err := NewBox(testKey())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal([]byte("JBSWY3DPEHPK3PXP"), []byte("row-1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("JBSWY3")) {
		t.Error("the plaintext appears in the ciphertext")
	}
	if got, err := b.Open(sealed, []byte("row-1")); err != nil || string(got) != "JBSWY3DPEHPK3PXP" {
		t.Errorf("Open = %q, %v", got, err)
	}
	again, _ := b.Seal([]byte("JBSWY3DPEHPK3PXP"), []byte("row-1"))
	if bytes.Equal(sealed, again) {
		t.Error("two seals are identical: the nonce is not random")
	}
	// Bound to its row, to its key, and tamper-evident.
	if _, err := b.Open(sealed, []byte("row-2")); err == nil {
		t.Error("opened with another context")
	}
	other, _ := NewBox(testKey())
	if _, err := other.Open(sealed, []byte("row-1")); err == nil {
		t.Error("opened with another key")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed, []byte("row-1")); err == nil {
		t.Error("opened a tampered ciphertext")
	}
	if _, err := b.Open([]byte{1, 2}, nil); err == nil {
		t.Error("opened a truncated ciphertext")
	}
}

func TestNewBoxRejectsBadKeys(t *testing.T) {
	for name, k := range map[string]string{"empty": "", "short": "AAAA", "not base64": "***", "long": base64.RawURLEncoding.EncodeToString(make([]byte, 33))} {
		if _, err := NewBox(k); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
