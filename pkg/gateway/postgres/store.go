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
	_, err := s.pool.Exec(ctx, `INSERT INTO gateway.users (id, username, role, salt, hash, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, u.Username, u.Role, u.Salt, u.Hash, u.CreatedAt)
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
	err := s.pool.QueryRow(ctx, `SELECT id, username, role, created_at, salt, hash FROM gateway.users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.Salt, &u.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.UserRecord{}, false, nil
	}
	u.CreatedAt = u.CreatedAt.UTC()
	return u, err == nil, err
}

func (s *UserStore) Traders(ctx context.Context) ([]gateway.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, username, role, created_at FROM gateway.users WHERE role = $1 ORDER BY username`, gateway.RoleTrader)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanUser)
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
		SELECT u.id, u.username, u.role, u.created_at, s.expires_at
		FROM gateway.sessions s JOIN gateway.users u ON u.id = s.user_id
		WHERE s.token_digest = $1`, d[:]).Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.User{}, time.Time{}, false, nil
	}
	u.CreatedAt = u.CreatedAt.UTC()
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

func scanUser(row pgx.CollectableRow) (gateway.User, error) {
	var u gateway.User
	err := row.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt)
	u.CreatedAt = u.CreatedAt.UTC()
	return u, err
}

func (s *UserStore) DeleteUserSessions(ctx context.Context, userID string, except gateway.TokenDigest) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM gateway.sessions WHERE user_id = $1 AND token_digest <> $2`, userID, except[:])
	return err
}

func (s *UserStore) SetPassword(ctx context.Context, userID string, salt, hash []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE gateway.users SET salt = $2, hash = $3 WHERE id = $1`, userID, salt, hash)
	return err
}

func (s *UserStore) SetAPIToken(ctx context.Context, userID string, d gateway.TokenDigest, t gateway.APIToken) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gateway.api_tokens (user_id, token_digest, hint, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET token_digest = EXCLUDED.token_digest, hint = EXCLUDED.hint, created_at = EXCLUDED.created_at`,
		userID, d[:], t.Hint, t.CreatedAt)
	return err
}

func (s *UserStore) DeleteAPIToken(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM gateway.api_tokens WHERE user_id = $1`, userID)
	return err
}

func (s *UserStore) APIToken(ctx context.Context, userID string) (gateway.APIToken, bool, error) {
	var t gateway.APIToken
	err := s.pool.QueryRow(ctx, `SELECT hint, created_at FROM gateway.api_tokens WHERE user_id = $1`, userID).Scan(&t.Hint, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.APIToken{}, false, nil
	}
	t.CreatedAt = t.CreatedAt.UTC()
	return t, err == nil, err
}

func (s *UserStore) UserByAPIToken(ctx context.Context, d gateway.TokenDigest) (gateway.User, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.role, u.created_at
		FROM gateway.api_tokens t JOIN gateway.users u ON u.id = t.user_id
		WHERE t.token_digest = $1`, d[:])
	if err != nil {
		return gateway.User{}, false, err
	}
	u, err := pgx.CollectExactlyOneRow(rows, scanUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.User{}, false, nil
	}
	return u, err == nil, err
}
