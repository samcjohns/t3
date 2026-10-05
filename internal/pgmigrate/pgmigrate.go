// Package pgmigrate applies a service's versioned SQL migrations to its own
// PostgreSQL schema.
package pgmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrate applies every migration in files not yet recorded in
// <schema>.schema_migrations, in version order, each in its own transaction.
// Files are named NNN_description.sql. Concurrent callers are serialised with
// an advisory lock. The schema is created if it does not exist, so a role
// that owns a pre-created schema needs no database-level privileges.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string, files fs.FS) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	type migration struct {
		version int
		name    string
	}
	var ms []migration
	for _, n := range names {
		v, err := strconv.Atoi(strings.SplitN(path.Base(n), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %q: name must start with a version number", n)
		}
		ms = append(ms, migration{v, n})
	}
	slices.SortFunc(ms, func(a, b migration) int { return a.version - b.version })

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lock := "t3:migrate:" + schema
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", lock); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", lock)

	ident := pgx.Identifier{schema}.Sanitize()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+ident+`.schema_migrations (
		version integer PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}

	for _, m := range ms {
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+ident+".schema_migrations WHERE version = $1)", m.version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := fs.ReadFile(files, m.name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO "+ident+".schema_migrations (version) VALUES ($1)", m.version)
			return err
		})
		if err != nil {
			return fmt.Errorf("applying %s migration %s: %w", schema, m.name, err)
		}
	}
	return nil
}
