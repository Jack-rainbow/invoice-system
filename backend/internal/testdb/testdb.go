// Package testdb connects integration tests only to the temporary PostgreSQL
// cluster created and destroyed by scripts/test-db.sh --integration.
package testdb

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open refuses a missing marker, remote socket, mismatched database, nonce or
// data directory before returning a connection to a mutating test. This is an
// accidental-target guard, not a security boundary against the test's author.
func Open(t *testing.T) *pgxpool.Pool {
	t.Helper()
	root, err := projectRoot()
	if err != nil {
		t.Fatal("cannot locate private test project")
	}
	marker := filepath.Clean(os.Getenv("INVOICE_TEST_GUARD_FILE"))
	ownedPrefix := filepath.Join(root, ".runtime", "db-test.")
	if !strings.HasPrefix(marker, ownedPrefix) || filepath.Base(marker) != "guard" {
		t.Fatal("use scripts/test-db.sh --integration; private test marker missing")
	}
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4096 {
		t.Fatal("invalid private test marker")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("cannot read private test marker")
	}
	parts := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(parts) != 3 || parts[0] == "" || !strings.HasPrefix(parts[1], "invoice_test_") ||
		parts[2] != filepath.Join(filepath.Dir(marker), "data") {
		t.Fatal("private marker does not identify this test cluster")
	}
	config, err := pgxpool.ParseConfig(os.Getenv("INVOICE_TEST_DSN"))
	if err != nil || config.ConnConfig.Database != parts[1] ||
		!filepath.IsAbs(config.ConnConfig.Host) || !strings.HasPrefix(filepath.Base(config.ConnConfig.Host), "invoice-db.") ||
		(filepath.Dir(config.ConnConfig.Host) != "/tmp" && filepath.Dir(config.ConnConfig.Host) != "/private/tmp") {
		t.Fatal("only the runner-owned Unix-socket database is permitted")
	}
	config.MaxConns = 24
	config.MinConns = 0
	config.ConnConfig.RuntimeParams["search_path"] = "public,pg_catalog"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10s"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "5s"
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var nonce, database, dataDir string
		err := conn.QueryRow(ctx, `SELECT nonce::text, current_database(), current_setting('data_directory') FROM public.invoice_test_guard`).Scan(&nonce, &database, &dataDir)
		if err != nil || nonce != parts[0] || database != parts[1] || dataDir != parts[2] {
			return pgx.ErrNoRows // Do not log a possibly supplied DSN or raw error.
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("private test database connection failed")
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("private test database ownership check failed")
	}
	return pool
}

func UUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatal("test UUID entropy unavailable")
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return pgtype.UUID{Bytes: bytes, Valid: true}
}

func projectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "backend", "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "api", "openapi.json")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
