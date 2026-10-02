package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Audience separates the token families. Middleware rejects a token on routes for another
// audience (BACKEND_PLAN.md section 4.3).
type Audience string

const (
	AudienceTenant Audience = "tenant" // owner and manager users
	AudienceDevice Audience = "device" // paired POS devices
)

const (
	issuer         = "orion"
	minKeyBytes    = 32
	clockLeeway    = 5 * time.Second
	opaqueSecretSz = 32
)

// Keyring holds the HMAC keys for one audience. The first key signs; every key verifies, so a key
// can be rotated by adding the new one in front and removing the old one after the longest token
// lifetime. The key id travels in the `kid` header.
type Keyring struct {
	activeID string
	keys     map[string][]byte
}

// ParseKeyring reads "kid:base64url-secret[,kid:base64url-secret...]". Each secret must decode to
// at least 32 bytes.
func ParseKeyring(spec string) (*Keyring, error) {
	k := &Keyring{keys: map[string][]byte{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, secret, ok := strings.Cut(part, ":")
		if !ok || id == "" {
			return nil, errors.New("identity: key must look like kid:base64url-secret")
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
		if err != nil {
			return nil, fmt.Errorf("identity: key %q: %w", id, err)
		}
		if len(b) < minKeyBytes {
			return nil, fmt.Errorf("identity: key %q is %d bytes, want at least %d", id, len(b), minKeyBytes)
		}
		if _, dup := k.keys[id]; dup {
			return nil, fmt.Errorf("identity: duplicate key id %q", id)
		}
		if k.activeID == "" {
			k.activeID = id
		}
		k.keys[id] = b
	}
	if k.activeID == "" {
		return nil, errors.New("identity: no keys")
	}
	return k, nil
}

// NewEphemeralKeyring returns a random single-key ring. Tokens do not survive a restart, so it is
// only for local development and tests.
func NewEphemeralKeyring() *Keyring {
	b := make([]byte, minKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &Keyring{activeID: "ephemeral", keys: map[string][]byte{"ephemeral": b}}
}

func (k *Keyring) sign(claims jwt.Claims) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	t.Header["kid"] = k.activeID
	return t.SignedString(k.keys[k.activeID])
}

func (k *Keyring) verify(token string, aud Audience, now func() time.Time, claims jwt.Claims) error {
	_, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			key, ok := k.keys[kid]
			if !ok {
				return nil, errors.New("unknown key id")
			}
			return key, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(string(aud)),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(now),
		jwt.WithLeeway(clockLeeway),
	)
	if err != nil {
		return ErrInvalidToken
	}
	return nil
}

// accessClaims are the claims of an access token. Device tokens add the device and its outlet.
type accessClaims struct {
	jwt.RegisteredClaims
	TenantID string `json:"tid"`
	DeviceID string `json:"did,omitempty"`
	OutletID string `json:"oid,omitempty"`
}

// Opaque tokens (refresh tokens, email verification links, device secrets) are random secrets
// with the ids needed to find their row in front, so a lookup can run inside the right tenant's
// row-level security: "<prefix>.<id>[.<id>].<secret>". Only the SHA-256 of the secret is stored;
// the secrets have 256 bits of entropy, so a fast hash is enough.
const (
	prefixRefresh      = "rt1"
	prefixVerification = "vt1"
	prefixDeviceSecret = "dk1"
)

var b64 = base64.RawURLEncoding

func mintOpaque(prefix string, ids ...uuid.UUID) (token string, hash []byte) {
	secret := make([]byte, opaqueSecretSz)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	parts := []string{prefix}
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	parts = append(parts, b64.EncodeToString(secret))
	sum := sha256.Sum256(secret)
	return strings.Join(parts, "."), sum[:]
}

func parseOpaque(prefix string, idCount int, token string) (ids []uuid.UUID, hash []byte, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != idCount+2 || parts[0] != prefix {
		return nil, nil, ErrInvalidToken
	}
	for _, p := range parts[1 : 1+idCount] {
		id, err := uuid.Parse(p)
		if err != nil || id == uuid.Nil {
			return nil, nil, ErrInvalidToken
		}
		ids = append(ids, id)
	}
	secret, err := b64.DecodeString(parts[len(parts)-1])
	if err != nil || len(secret) != opaqueSecretSz {
		return nil, nil, ErrInvalidToken
	}
	sum := sha256.Sum256(secret)
	return ids, sum[:], nil
}
