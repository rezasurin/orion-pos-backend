package identity

import (
	"context"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(2)
	ctx := context.Background()

	hash, err := h.Hash(ctx, "correct horse battery", PINParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q, want PHC argon2id with the PIN parameters", hash)
	}
	if ok, err := h.Verify(ctx, "correct horse battery", hash); err != nil || !ok {
		t.Errorf("Verify(right) = %v, %v", ok, err)
	}
	if ok, err := h.Verify(ctx, "wrong", hash); err != nil || ok {
		t.Errorf("Verify(wrong) = %v, %v", ok, err)
	}

	again, _ := h.Hash(ctx, "correct horse battery", PINParams)
	if again == hash {
		t.Error("two hashes of one secret are identical: the salt is not random")
	}
}

func TestVerifyRejectsMalformedAndHostileHashes(t *testing.T) {
	h := NewHasher(1)
	for name, enc := range map[string]string{
		"empty":         "",
		"wrong algo":    "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"wrong version": "$argon2id$v=16$m=65536,t=3,p=2$c2FsdA$a2V5",
		"no params":     "$argon2id$v=19$x$c2FsdA$a2V5",
		"huge memory":   "$argon2id$v=19$m=4194304,t=3,p=2$c2FsdA$a2V5",
		"huge time":     "$argon2id$v=19$m=65536,t=900,p=2$c2FsdA$a2V5",
		"zero threads":  "$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$a2V5",
		"bad salt":      "$argon2id$v=19$m=65536,t=3,p=2$!!!$a2V5",
		"empty key":     "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
	} {
		if _, err := h.Verify(context.Background(), "x", enc); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestHasherHonoursContextWhileWaiting(t *testing.T) {
	h := NewHasher(1)
	h.sem <- struct{}{} // the only slot is taken
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Hash(ctx, "x", PINParams); err == nil {
		t.Error("want context error")
	}
}
