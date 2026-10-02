package database_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// Tables that legitimately have no tenant_id but are still isolated by row-level security.
// Adding a table here needs a reason.
var globalTables = []string{
	"tenant",       // isolated on its own id instead
	"user_account", // a person exists before any tenant; visible only to the tenants they belong to
}

// Tables outside row-level security altogether. Adding a table here needs a reason.
func isUnscoped(name string) bool {
	switch {
	case name == "goose_db_version": // migration bookkeeping
		return true
	case name == "plan": // plans are global (ADR 0007)
		return true
	case strings.HasPrefix(name, "river_"): // the job queue; its arguments carry ids only
		return true
	}
	return false
}

// Every table is either global (listed above) or has tenant_id with row-level security enabled
// and policies for both app roles. This catches a new table that forgets them.
func TestEveryTenantTableHasRowLevelSecurity(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()

	rows, err := d.Owner.Query(ctx, `
		SELECT c.relname,
		       EXISTS (SELECT 1 FROM pg_attribute a
		               WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped),
		       c.relrowsecurity,
		       EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid
		               AND 'orion_app'::regrole = ANY (p.polroles)),
		       EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid
		               AND 'orion_platform'::regrole = ANY (p.polroles))
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		Name                                    string
		HasTenantID, RLS, AppPolicy, PlatPolicy bool
	}
	tables, err := pgx.CollectRows(rows, pgx.RowToStructByPos[table])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) == 0 {
		t.Fatal("no tables found")
	}

	for _, tb := range tables {
		if isUnscoped(tb.Name) {
			continue
		}
		if !tb.HasTenantID && !slices.Contains(globalTables, tb.Name) {
			t.Errorf("%s: no tenant_id column and not listed as a global table", tb.Name)
		}
		if !tb.RLS {
			t.Errorf("%s: row-level security is not enabled", tb.Name)
		}
		if !tb.AppPolicy {
			t.Errorf("%s: no policy for orion_app", tb.Name)
		}
		if !tb.PlatPolicy {
			t.Errorf("%s: no policy for orion_platform", tb.Name)
		}
	}
}

func TestMigrationsRoundTrip(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	ownerURL := d.Owner.Config().ConnString()
	logger := discardLogger()

	expected, err := database.ExpectedVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := database.AppliedVersion(ctx, d.App); err != nil || got != expected {
		t.Fatalf("applied version = %d (%v), want %d", got, err, expected)
	}

	// Every migration can be rolled back and applied again.
	for range expected {
		if err := database.MigrateDown(ctx, ownerURL, logger); err != nil {
			t.Fatalf("down: %v", err)
		}
	}
	if err := database.Migrate(ctx, ownerURL, logger); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got, err := database.AppliedVersion(ctx, d.Owner); err != nil || got != expected {
		t.Fatalf("version after round trip = %d (%v), want %d", got, err, expected)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
