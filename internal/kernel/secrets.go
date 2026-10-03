package kernel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Box encrypts small secrets that must be read back, such as TOTP seeds (a hash would not do).
// It uses AES-256-GCM. The stored form is version byte, nonce, ciphertext; the version byte leaves
// room to rotate the key or change the scheme later.
type Box struct {
	aead cipher.AEAD
}

const boxVersion = 1

// NewBox builds a Box from a base64url-encoded 32-byte key.
func NewBox(key string) (*Box, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(key), "="))
	if err != nil {
		return nil, fmt.Errorf("kernel: secrets key: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("kernel: secrets key is %d bytes, want 32", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. context is authenticated but not stored (for example the row's id), so
// a ciphertext copied to another row will not open.
func (b *Box) Seal(plaintext, context []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{boxVersion}, nonce...)
	return b.aead.Seal(out, nonce, plaintext, context), nil
}

// Open decrypts the result of Seal with the same context.
func (b *Box) Open(sealed, context []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < 1+n+b.aead.Overhead() || sealed[0] != boxVersion {
		return nil, errors.New("kernel: malformed sealed secret")
	}
	pt, err := b.aead.Open(nil, sealed[1:1+n], sealed[1+n:], context)
	if err != nil {
		return nil, errors.New("kernel: cannot open sealed secret")
	}
	return pt, nil
}
