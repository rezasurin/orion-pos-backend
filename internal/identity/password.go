package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ArgonParams are the argon2id cost parameters. They are written into every hash (PHC string
// format), so they can change later and old hashes still verify.
type ArgonParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

var (
	// PasswordParams protect account passwords. Verifying one takes tens of milliseconds and
	// 64 MiB, so Hasher limits how many run at once.
	PasswordParams = ArgonParams{Time: 3, Memory: 64 * 1024, Threads: 2}
	// PINParams protect staff PINs. The hashes are sent to tablets and checked there, often in
	// WebAssembly, so they are cheaper. A 4 to 6 digit PIN has too few possibilities for any cost
	// to stop an attacker who holds the hash; see BACKEND_PLAN.md section 4.3.
	PINParams = ArgonParams{Time: 2, Memory: 19 * 1024, Threads: 1}
)

const (
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Hasher computes and checks argon2id hashes, at most `limit` at a time so a burst of logins
// cannot exhaust memory.
type Hasher struct {
	sem chan struct{}
}

// NewHasher returns a Hasher that runs at most limit hashes concurrently.
func NewHasher(limit int) *Hasher {
	return &Hasher{sem: make(chan struct{}, max(limit, 1))}
}

func (h *Hasher) acquire(ctx context.Context) (release func(), err error) {
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Hash returns the PHC-format argon2id hash of secret.
func (h *Hasher) Hash(ctx context.Context, secret string, p ArgonParams) (string, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether secret matches the PHC-format hash. A malformed hash is an error, not a
// mismatch.
func (h *Hasher) Verify(ctx context.Context, secret, encoded string) (bool, error) {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	got := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, uint32(len(want))) //nolint:gosec // parsePHC bounds the length
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func parsePHC(encoded string) (p ArgonParams, salt, key []byte, err error) {
	// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("identity: unsupported password hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errors.New("identity: unsupported argon2 version")
	}
	var threads uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &threads); err != nil {
		return p, nil, nil, errors.New("identity: bad argon2 parameters")
	}
	p.Threads = uint8(threads) //nolint:gosec // bounded below
	// Refuse parameters that would let a stored hash make verification a denial of service.
	if p.Time == 0 || p.Time > 10 || p.Memory < 8 || p.Memory > 256*1024 || threads == 0 || threads > 16 {
		return p, nil, nil, errors.New("identity: argon2 parameters out of range")
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, fmt.Errorf("identity: bad salt: %w", err)
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(key) == 0 || len(key) > 128 {
		return p, nil, nil, errors.New("identity: bad hash")
	}
	return p, salt, key, nil
}
