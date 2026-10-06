package config

import (
	"strings"
	"testing"
)

func TestLoadReadsEveryVariable(t *testing.T) {
	for k, v := range map[string]string{
		"ORION_ENV": "staging", "ORION_DATABASE_URL": "db", "ORION_MIGRATE_DATABASE_URL": "mig",
		"ORION_PLATFORM_DATABASE_URL": "plat", "ORION_PUBLIC_URL": "https://app.test",
		"ORION_JWT_TENANT_KEYS": "t", "ORION_JWT_DEVICE_KEYS": "d", "ORION_JWT_OPERATOR_KEYS": "o",
		"ORION_SECRETS_KEY": "s", "ORION_TRUST_PROXY": "true", "ORION_HTTP_ADDR": ":9", "ORION_SHUTDOWN_TIMEOUT": "3s",
		"ORION_ALERT_UNSYNCED_AFTER": "45m", "ORION_ALERT_SILENT_AFTER": "2h", "ORION_ALERT_OPERATOR_EMAIL": "ops@orion.test",
		"ORION_EMAIL_PROVIDER": "resend", "ORION_EMAIL_API_KEY": "re_k", "ORION_EMAIL_FROM": "Orion <no-reply@mail.orion.test>",
	} {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "staging" || c.DatabaseURL != "db" || c.MigrateDatabaseURL != "mig" || c.PlatformDatabaseURL != "plat" ||
		c.PublicURL != "https://app.test" || c.TenantJWTKeys != "t" || c.DeviceJWTKeys != "d" || c.OperatorJWTKeys != "o" ||
		c.SecretsKey != "s" || !c.TrustProxy || c.HTTPAddr != ":9" || c.ShutdownTimeout.Seconds() != 3 ||
		c.AlertUnsyncedAfter.Minutes() != 45 || c.AlertSilentAfter.Hours() != 2 || c.AlertOperatorEmail != "ops@orion.test" ||
		c.EmailProvider != "resend" || c.EmailAPIKey != "re_k" || c.EmailFrom != "Orion <no-reply@mail.orion.test>" {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"environment": {"ORION_ENV": "prod"},
		"log format":  {"ORION_LOG_FORMAT": "xml"},
		"timeout":     {"ORION_SHUTDOWN_TIMEOUT": "soon"},
		"bool":        {"ORION_TRUST_PROXY": "maybe"},
		"provider":    {"ORION_EMAIL_PROVIDER": "pigeon"},
		"resend key":  {"ORION_EMAIL_PROVIDER": "resend", "ORION_EMAIL_FROM": "a@b.c"},
		"resend from": {"ORION_EMAIL_PROVIDER": "resend", "ORION_EMAIL_API_KEY": "re_k"},
		"alert time":  {"ORION_ALERT_UNSYNCED_AFTER": "never"},
		"alert zero":  {"ORION_ALERT_SILENT_AFTER": "0s"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ORION_") {
			t.Errorf("%s: err = %v, want a message naming the variable", name, err)
		}
		for k := range env {
			t.Setenv(k, "")
		}
	}
}

func TestCORSOrigins(t *testing.T) {
	for name, tc := range map[string]struct {
		env     map[string]string
		want    []string
		wantErr string
	}{
		"local defaults to the front end": {env: map[string]string{"ORION_ENV": "local", "ORION_PUBLIC_URL": "http://localhost:5173"}, want: []string{"http://localhost:5173"}},
		"elsewhere defaults to none":      {env: map[string]string{"ORION_ENV": "staging", "ORION_PUBLIC_URL": "https://app.orion.test"}, want: nil},
		"a list is normalised": {
			env:  map[string]string{"ORION_CORS_ALLOWED_ORIGINS": " https://App.Orion.test/ , http://localhost:5173 "},
			want: []string{"https://app.orion.test", "http://localhost:5173"},
		},
		"setting it replaces the local default": {env: map[string]string{"ORION_ENV": "local", "ORION_CORS_ALLOWED_ORIGINS": "https://a.test"}, want: []string{"https://a.test"}},
		"a wildcard":                            {env: map[string]string{"ORION_CORS_ALLOWED_ORIGINS": "*"}, wantErr: "wildcard"},
		"a subdomain wildcard":                  {env: map[string]string{"ORION_CORS_ALLOWED_ORIGINS": "https://*.orion.test"}, wantErr: "wildcard"},
		"a path":                                {env: map[string]string{"ORION_CORS_ALLOWED_ORIGINS": "https://app.orion.test/app"}, wantErr: "no path"},
		"no scheme":                             {env: map[string]string{"ORION_CORS_ALLOWED_ORIGINS": "app.orion.test"}, wantErr: "scheme"},
		"credentials":                           {env: map[string]string{"ORION_CORS_ALLOWED_ORIGINS": "https://u:p@app.orion.test"}, wantErr: "origin like"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"ORION_ENV", "ORION_PUBLIC_URL", "ORION_CORS_ALLOWED_ORIGINS"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), "ORION_CORS_ALLOWED_ORIGINS") || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming the variable and %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(c.CORSAllowedOrigins, ",") != strings.Join(tc.want, ",") {
				t.Errorf("origins = %v, want %v", c.CORSAllowedOrigins, tc.want)
			}
		})
	}
}
