package identity

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestParseKeyring(t *testing.T) {
	key := func(n int) string { return base64.RawURLEncoding.EncodeToString(make([]byte, n)) }
	for name, tt := range map[string]struct {
		spec    string
		wantErr bool
		active  string
	}{
		"one key":          {spec: "k1:" + key(32), active: "k1"},
		"first one signs":  {spec: "new:" + key(32) + ", old:" + key(48), active: "new"},
		"empty":            {spec: "", wantErr: true},
		"too short":        {spec: "k1:" + key(16), wantErr: true},
		"no id":            {spec: ":" + key(32), wantErr: true},
		"no separator":     {spec: key(32), wantErr: true},
		"duplicate id":     {spec: "k:" + key(32) + ",k:" + key(32), wantErr: true},
		"not base64":       {spec: "k:***", wantErr: true},
		"trailing padding": {spec: "k:" + key(32) + "==", active: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			k, err := ParseKeyring(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && k.activeID != tt.active {
				t.Errorf("active = %q, want %q", k.activeID, tt.active)
			}
		})
	}
}

func claimsAt(now time.Time, aud Audience, ttl time.Duration) accessClaims {
	return accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: uuid.NewString(), Audience: jwt.ClaimStrings{string(aud)},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		TenantID: uuid.NewString(),
	}
}

func TestKeyringVerify(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	keys := NewEphemeralKeyring()
	token, err := keys.sign(claimsAt(now, AudienceTenant, 15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	var c accessClaims
	if err := keys.verify(token, AudienceTenant, clock, &c); err != nil {
		t.Fatalf("valid token: %v", err)
	}

	later := func() time.Time { return now.Add(16 * time.Minute) }
	for name, tc := range map[string]struct {
		keys  *Keyring
		token string
		aud   Audience
		clock func() time.Time
	}{
		"expired":         {keys, token, AudienceTenant, later},
		"wrong audience":  {keys, token, AudienceDevice, clock},
		"other key":       {NewEphemeralKeyring(), token, AudienceTenant, clock},
		"tampered":        {keys, token[:len(token)-3] + "AAA", AudienceTenant, clock},
		"garbage":         {keys, "not.a.jwt", AudienceTenant, clock},
		"empty":           {keys, "", AudienceTenant, clock},
		"alg none":        {keys, unsignedToken(t, now), AudienceTenant, clock},
		"not yet issued":  {keys, token, AudienceTenant, func() time.Time { return now.Add(-time.Hour) }},
		"no expiry claim": {keys, noExpiryToken(t, keys, now), AudienceTenant, clock},
	} {
		var c accessClaims
		if err := tc.keys.verify(tc.token, tc.aud, tc.clock, &c); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}

func unsignedToken(t *testing.T, now time.Time) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodNone, claimsAt(now, AudienceTenant, time.Hour)).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func noExpiryToken(t *testing.T, k *Keyring, now time.Time) string {
	t.Helper()
	c := claimsAt(now, AudienceTenant, time.Hour)
	c.ExpiresAt = nil
	s, err := k.sign(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestKeyRotation(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	oldKeys := NewEphemeralKeyring()
	token, _ := oldKeys.sign(claimsAt(now, AudienceTenant, time.Hour))

	// A ring that lists the new key first and keeps the old one still verifies old tokens.
	rotated := &Keyring{activeID: "new", keys: map[string][]byte{"new": make([]byte, 32), "ephemeral": oldKeys.keys["ephemeral"]}}
	var c accessClaims
	if err := rotated.verify(token, AudienceTenant, clock, &c); err != nil {
		t.Errorf("token signed by the retired key: %v", err)
	}
	fresh, _ := rotated.sign(claimsAt(now, AudienceTenant, time.Hour))
	if err := oldKeys.verify(fresh, AudienceTenant, clock, &c); err == nil {
		t.Error("a ring without the new key accepted a token signed with it")
	}
}

func TestOpaqueTokens(t *testing.T) {
	tenant, device := uuid.New(), uuid.New()
	token, hash := mintOpaque(prefixDeviceSecret, tenant, device)

	ids, got, err := parseOpaque(prefixDeviceSecret, 2, token)
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != tenant || ids[1] != device || string(got) != string(hash) || len(hash) != 32 {
		t.Errorf("round trip mismatch: ids=%v", ids)
	}

	other, _ := mintOpaque(prefixDeviceSecret, tenant, device)
	if other == token {
		t.Error("two tokens are identical")
	}

	for name, bad := range map[string]string{
		"wrong prefix":   strings.Replace(token, prefixDeviceSecret, prefixRefresh, 1),
		"missing part":   prefixDeviceSecret + "." + tenant.String() + "." + "x",
		"bad uuid":       prefixDeviceSecret + ".nope." + device.String() + "." + token[len(token)-43:],
		"nil uuid":       prefixDeviceSecret + "." + uuid.Nil.String() + "." + device.String() + "." + token[len(token)-43:],
		"short secret":   prefixDeviceSecret + "." + tenant.String() + "." + device.String() + ".AAAA",
		"empty":          "",
		"extra part":     token + ".x",
		"not base64 end": prefixDeviceSecret + "." + tenant.String() + "." + device.String() + ".***",
	} {
		if _, _, err := parseOpaque(prefixDeviceSecret, 2, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}
