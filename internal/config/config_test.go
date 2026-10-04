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
	} {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "staging" || c.DatabaseURL != "db" || c.MigrateDatabaseURL != "mig" || c.PlatformDatabaseURL != "plat" ||
		c.PublicURL != "https://app.test" || c.TenantJWTKeys != "t" || c.DeviceJWTKeys != "d" || c.OperatorJWTKeys != "o" ||
		c.SecretsKey != "s" || !c.TrustProxy || c.HTTPAddr != ":9" || c.ShutdownTimeout.Seconds() != 3 {
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
