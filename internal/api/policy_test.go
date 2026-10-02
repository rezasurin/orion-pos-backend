package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func specWith(t *testing.T, opYAML string) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
security: [{userAuth: []}]
paths:
  /x:
    get:
      operationId: op
` + opYAML + `
      responses: {"200": {description: ok}}
components:
  securitySchemes:
    userAuth: {type: http, scheme: bearer}
    deviceAuth: {type: http, scheme: bearer}
`))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPoliciesFromSpec(t *testing.T) {
	for name, tt := range map[string]struct {
		op      string
		want    policy
		wantErr string
	}{
		"default is user auth": {op: "", want: policy{audience: identity.AudienceTenant}},
		"public":               {op: "      security: []", want: policy{public: true}},
		"device":               {op: "      security: [{deviceAuth: []}]", want: policy{audience: identity.AudienceDevice}},
		"permission":           {op: "      x-permission: staff.manage", want: policy{audience: identity.AudienceTenant, permission: "staff.manage"}},
		"permission on public": {op: "      security: []\n      x-permission: staff.manage", wantErr: "public"},
		"empty permission":     {op: "      x-permission: ''", wantErr: "non-empty"},
		"numeric permission":   {op: "      x-permission: 5", wantErr: "non-empty"},
		"two schemes at once":  {op: "      security: [{userAuth: [], deviceAuth: []}]", wantErr: "exactly one"},
		"alternative schemes":  {op: "      security: [{userAuth: []}, {deviceAuth: []}]", wantErr: "exactly one"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := policiesFromSpec(specWith(t, tt.op))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got["op"] != tt.want {
				t.Errorf("policy = %+v, want %+v", got["op"], tt.want)
			}
		})
	}
}

// Every operation in the contract has a policy (keyed by the generated, capitalised operation id), the auth endpoints are public, and the rest need
// a token. A new operation that forgets `security: []` is protected by default.
func TestRealSpecPolicies(t *testing.T) {
	doc, err := openapi.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	policies, err := policiesFromSpec(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"Login", "RefreshSession", "Logout", "VerifyEmail", "ResendVerification"} {
		if !policies[id].public {
			t.Errorf("%s should be public", id)
		}
	}
	for _, id := range []string{"GetMe", "ListOutlets", "GetOutlet"} {
		if p := policies[id]; p.public || p.audience != identity.AudienceTenant {
			t.Errorf("%s policy = %+v, want user auth", id, p)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	s := &Server{Deps: Deps{Logger: discardLogger()}}
	for name, tt := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"validation":     {errors.Join(kernel.ErrValidation), http.StatusBadRequest, "validation_failed"},
		"not found":      {kernel.ErrNotFound, http.StatusNotFound, "not_found"},
		"conflict":       {kernel.ErrConflict, http.StatusConflict, "conflict"},
		"credentials":    {identity.ErrInvalidCredentials, http.StatusUnauthorized, "invalid_credentials"},
		"token":          {identity.ErrInvalidToken, http.StatusUnauthorized, "invalid_token"},
		"reuse":          {identity.ErrTokenReuse, http.StatusUnauthorized, "token_reused"},
		"unverified":     {identity.ErrEmailNotVerified, http.StatusForbidden, "email_not_verified"},
		"suspended":      {tenancy.ErrSuspended, http.StatusForbidden, "tenant_suspended"},
		"revoked device": {identity.ErrDeviceRevoked, http.StatusForbidden, "device_revoked"},
		"forbidden":      {&identity.ForbiddenError{Permission: "x.y"}, http.StatusForbidden, "forbidden"},
		"unexpected":     {errors.New("pq: password authentication failed for user orion_secret"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.responseError(rec, httptest.NewRequest("GET", "/", nil), tt.err)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), `"code":"`+tt.code+`"`) {
				t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tt.status, tt.code)
			}
			if tt.status == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "orion_secret") {
				t.Error("an internal error message leaked into the response")
			}
		})
	}
}
