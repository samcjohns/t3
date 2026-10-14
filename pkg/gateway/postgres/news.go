package postgres

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgmigrate"
	"github.com/samcjohns/t3/pkg/gateway"
)

// NewsStore implements gateway.NewsStore.
type NewsStore struct {
	pool *pgxpool.Pool
}

var _ gateway.NewsStore = (*NewsStore)(nil)

// NewNewsStore migrates the gateway schema and returns a NewsStore using it.
func NewNewsStore(ctx context.Context, pool *pgxpool.Pool) (*NewsStore, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	if err := pgmigrate.Migrate(ctx, pool, "gateway", sub); err != nil {
		return nil, err
	}
	return &NewsStore{pool: pool}, nil
}

func (s *NewsStore) AddArticle(ctx context.Context, a gateway.Article) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gateway.news (id, tick, created_at, symbol, headline, body, price, target)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.ID, int64(a.Tick), a.Time, a.Symbol, a.Headline, a.Body, a.Price, a.Target)
	return err
}

func (s *NewsStore) Articles(ctx context.Context, symbol string, limit int) ([]gateway.Article, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tick, created_at, symbol, headline, body, price, target FROM gateway.news
		WHERE $1 = '' OR symbol = $1 ORDER BY seq DESC LIMIT $2`, symbol, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (gateway.Article, error) {
		var a gateway.Article
		var tick int64
		err := row.Scan(&a.ID, &tick, &a.Time, &a.Symbol, &a.Headline, &a.Body, &a.Price, &a.Target)
		a.Tick, a.Time = uint64(tick), a.Time.UTC()
		return a, err
	})
	if out == nil {
		out = []gateway.Article{}
	}
	return out, err
}
