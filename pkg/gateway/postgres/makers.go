package postgres

import (
	"context"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgmigrate"
	"github.com/samcjohns/t3/pkg/gateway"
)

// MakerStore implements gateway.MakerStore.
type MakerStore struct {
	pool *pgxpool.Pool
}

var _ gateway.MakerStore = (*MakerStore)(nil)

// NewMakerStore migrates the gateway schema and returns a MakerStore using it.
func NewMakerStore(ctx context.Context, pool *pgxpool.Pool) (*MakerStore, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	if err := pgmigrate.Migrate(ctx, pool, "gateway", sub); err != nil {
		return nil, err
	}
	return &MakerStore{pool: pool}, nil
}

func (s *MakerStore) MakerControl(ctx context.Context, userID string) (gateway.MakerControl, error) {
	var c gateway.MakerControl
	err := s.pool.QueryRow(ctx, `SELECT paused, overrides FROM gateway.maker_controls WHERE user_id = $1`, userID).
		Scan(&c.Paused, &c.Overrides)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.MakerControl{}, nil
	}
	return c, err
}

func (s *MakerStore) SetMakerControl(ctx context.Context, userID string, c gateway.MakerControl) error {
	overrides := c.Overrides
	if overrides == nil {
		overrides = map[string]float64{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gateway.maker_controls (user_id, paused, overrides) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET paused = EXCLUDED.paused, overrides = EXCLUDED.overrides`,
		userID, c.Paused, overrides)
	return err
}
