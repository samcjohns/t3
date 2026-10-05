// Package pgtest gives tests a fresh, isolated PostgreSQL database.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVar names the connection URL of a server the tests may create and drop
// databases on. Tests that need PostgreSQL are skipped when it is unset.
const EnvVar = "T3_TEST_DATABASE_URL"

// New creates an empty database for the test, dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, _ := NewURL(t)
	return pool
}

// NewURL is New, also returning the database's connection URL so a test can
// reconnect, e.g. to simulate a restart.
func NewURL(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skip(EnvVar + " not set; skipping PostgreSQL test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	b := make([]byte, 6)
	rand.Read(b)
	name := "t3test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL: %v", EnvVar, err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	return pool, u.String()
}
