// Package postgres is the PostgreSQL adapter for the gateway's UserStore. It
// owns the gateway schema.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgmigrate"
	"github.com/samcjohns/t3/pkg/gateway"
)

//go:embed migrations/*.sql
var migrations embed.FS

// UserStore implements gateway.UserStore.
type UserStore struct {
	pool *pgxpool.Pool
}

var _ gateway.UserStore = (*UserStore)(nil)

// NewUserStore migrates the gateway schema and returns a UserStore using it.
func NewUserStore(ctx context.Context, pool *pgxpool.Pool) (*UserStore, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	if err := pgmigrate.Migrate(ctx, pool, "gateway", sub); err != nil {
		return nil, err
	}
	return &UserStore{pool: pool}, nil
}

func (s *UserStore) CreateUser(ctx context.Context, u gateway.UserRecord) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO gateway.users (id, username, role, salt, hash) VALUES ($1, $2, $3, $4, $5)`,
		u.ID, u.Username, u.Role, u.Salt, u.Hash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_username_key" {
		return fmt.Errorf("%w: %q", gateway.ErrUsernameTaken, u.Username)
	}
	return err
}

func (s *UserStore) DeleteUser(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM gateway.users WHERE id = $1`, id)
	return err
}

func (s *UserStore) UserByName(ctx context.Context, username string) (gateway.UserRecord, bool, error) {
	var u gateway.UserRecord
	err := s.pool.QueryRow(ctx, `SELECT id, username, role, salt, hash FROM gateway.users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.Role, &u.Salt, &u.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.UserRecord{}, false, nil
	}
	return u, err == nil, err
}

func (s *UserStore) Traders(ctx context.Context) ([]gateway.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, username, role FROM gateway.users WHERE role = $1 ORDER BY username`, gateway.RoleTrader)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (gateway.User, error) {
		var u gateway.User
		return u, row.Scan(&u.ID, &u.Username, &u.Role)
	})
}

func (s *UserStore) CreateSession(ctx context.Context, d gateway.TokenDigest, userID string, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO gateway.sessions (token_digest, user_id, expires_at) VALUES ($1, $2, $3)`,
		d[:], userID, expires)
	return err
}

func (s *UserStore) Session(ctx context.Context, d gateway.TokenDigest) (gateway.User, time.Time, bool, error) {
	var u gateway.User
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.username, u.role, s.expires_at
		FROM gateway.sessions s JOIN gateway.users u ON u.id = s.user_id
		WHERE s.token_digest = $1`, d[:]).Scan(&u.ID, &u.Username, &u.Role, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.User{}, time.Time{}, false, nil
	}
	return u, expires, err == nil, err
}

func (s *UserStore) DeleteSession(ctx context.Context, d gateway.TokenDigest) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM gateway.sessions WHERE token_digest = $1`, d[:])
	return err
}

func (s *UserStore) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM gateway.sessions WHERE expires_at <= $1`, now)
	return err
}
