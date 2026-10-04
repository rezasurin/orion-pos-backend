// Package testdb gives tests a real, migrated PostgreSQL database.
//
// One Postgres server is shared per test binary: a container started with testcontainers, or the
// server at ORION_TEST_DATABASE_URL (a superuser URL) when that is set. Migrations run once into
// a template database, and each test gets a fresh copy of it, so tests are isolated and fast.
//
// Each package that uses it runs its tests through Main:
//
//	func TestMain(m *testing.M) { testdb.Main(m) }
package testdb

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/rezasurin/orion-pos-backend/internal/database"
)

const (
	templateDB = "orion_template"
	// Login roles used by tests, members of the group roles created by the first migration.
	appUser      = "orion_test_app"
	platformUser = "orion_test_platform"
	testPassword = "orion-test"
)

var (
	adminURL  string // superuser URL on the maintenance database
	setupOnce sync.Once
	setupErr  error
	dbCounter atomic.Int64
	terminate func()
)

// Main runs the package's tests and stops the shared container afterwards.
func Main(m *testing.M) {
	code := m.Run()
	if terminate != nil {
		terminate()
	}
	os.Exit(code)
}

// DB is one test's database.
type DB struct {
	Name string
	// Owner connects as the schema owner (a superuser in tests). Use it only to arrange data
	// that the app roles may not write.
	Owner *pgxpool.Pool
	// App connects as a member of orion_app, like the running service. Row-level security
	// applies to it.
	App *pgxpool.Pool
	// Platform connects as a member of orion_platform.
	Platform *pgxpool.Pool
	// AppURL is the connection URL behind App.
	AppURL string
	// Queries counts the statements sent over App, so a test can assert that a request makes the
	// same number of queries for 1 row as for 50 (no N+1).
	Queries *QueryCounter
}

// QueryCounter counts the statements a pool sends. It implements pgx.QueryTracer.
type QueryCounter struct{ n atomic.Int64 }

func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count is the number of statements counted so far. BEGIN, COMMIT and the statement that sets the
// tenant count like any other, which is fine: tests compare counts, they do not pin them.
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// During runs fn and returns how many statements it sent.
func (c *QueryCounter) During(fn func()) int64 {
	before := c.Count()
	fn()
	return c.Count() - before
}

// New returns a fresh database with every migration applied. It is dropped when the test ends.
func New(t testing.TB) *DB {
	t.Helper()
	setupOnce.Do(func() { setupErr = setup() })
	if setupErr != nil {
		t.Fatalf("testdb: %v", setupErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("orion_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	admin := mustConnect(t, adminURL)
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}

	d := &DB{Name: name, AppURL: withUserAndDB(adminURL, appUser, testPassword, name), Queries: &QueryCounter{}}
	d.Owner = mustConnect(t, withDB(adminURL, name))
	cfg, err := pgxpool.ParseConfig(d.AppURL)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	cfg.ConnConfig.Tracer = d.Queries
	if d.App, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	d.Platform = mustConnect(t, withUserAndDB(adminURL, platformUser, testPassword, name))

	t.Cleanup(func() {
		d.Owner.Close()
		d.App.Close()
		d.Platform.Close()
		admin := mustConnect(t, adminURL)
		defer admin.Close()
		_, _ = admin.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})
	return d
}

func setup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminURL = os.Getenv("ORION_TEST_DATABASE_URL")
	if adminURL == "" {
		image := os.Getenv("ORION_TEST_PG_IMAGE")
		if image == "" {
			image = "postgres:17-alpine"
		}
		c, err := postgres.Run(ctx, image,
			postgres.WithDatabase("postgres"),
			postgres.WithUsername("postgres"),
			postgres.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(time.Minute)),
		)
		if err != nil {
			return fmt.Errorf("start postgres container: %w", err)
		}
		terminate = func() { _ = testcontainers.TerminateContainer(c) }
		adminURL, err = c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			return err
		}
	}

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", templateDB)); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := database.Migrate(ctx, withDB(adminURL, templateDB), logger); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}

	// The migration created the group roles; give the test login roles membership.
	for user, group := range map[string]string{appUser: "orion_app", platformUser: "orion_platform"} {
		_, err := admin.Exec(ctx, fmt.Sprintf(`
			DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN
					CREATE ROLE %[1]s LOGIN PASSWORD '%[3]s';
				END IF;
			END $$;
			GRANT %[2]s TO %[1]s;`, user, group, testPassword))
		if err != nil {
			return fmt.Errorf("create role %s: %w", user, err)
		}
	}
	return nil
}

func mustConnect(t testing.TB, u string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), u)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	return pool
}

func withDB(raw, db string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + db
	return u.String()
}

func withUserAndDB(raw, user, password, db string) string {
	u, err := url.Parse(withDB(raw, db))
	if err != nil {
		panic(err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// Exec runs SQL as the owner and fails the test on error.
func (d *DB) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("testdb: exec %q: %v", strings.TrimSpace(sql), err)
	}
}
