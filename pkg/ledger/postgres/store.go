// Package postgres is the PostgreSQL adapter for the ledger's Store. It owns
// the ledger schema.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/pgmigrate"
	"github.com/samcjohns/t3/pkg/ledger"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store implements ledger.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ ledger.Store = (*Store)(nil)

// NewStore migrates the ledger schema and returns a Store using it.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	if err := pgmigrate.Migrate(ctx, pool, "ledger", sub); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Load(ctx context.Context) (ledger.State, error) {
	var st ledger.State
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT last_tick FROM ledger.state`).Scan(&st.LastTick); err != nil {
			return err
		}

		byID := map[string]int{}
		rows, err := tx.Query(ctx, `SELECT id, cash, cash_held FROM ledger.accounts ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			a := ledger.Account{Holdings: []ledger.Holding{}}
			if err := rows.Scan(&a.ID, &a.Cash, &a.CashHeld); err != nil {
				return err
			}
			byID[a.ID] = len(st.Accounts)
			st.Accounts = append(st.Accounts, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `SELECT account_id, symbol, quantity, held FROM ledger.holdings ORDER BY account_id, symbol`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var h ledger.Holding
			if err := rows.Scan(&id, &h.Symbol, &h.Quantity, &h.Held); err != nil {
				return err
			}
			a := &st.Accounts[byID[id]]
			a.Holdings = append(a.Holdings, h)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `SELECT "order", remaining, cash, shares, created_at FROM ledger.holds`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw []byte
			var h ledger.HoldState
			if err := rows.Scan(&raw, &h.Remaining, &h.Cash, &h.Shares, &h.CreatedAt); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &h.Order); err != nil {
				return err
			}
			st.Holds = append(st.Holds, h)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `SELECT account_id, reference FROM ledger.entries WHERE reference IS NOT NULL`)
		if err != nil {
			return err
		}
		st.References, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ledger.Reference])
		return err
	})
	return st, err
}

func (s *Store) Apply(ctx context.Context, ch ledger.Changes) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, a := range ch.Accounts {
			b.Queue(`INSERT INTO ledger.accounts (id, cash, cash_held) VALUES ($1, $2, $3)
				ON CONFLICT (id) DO UPDATE SET cash = excluded.cash, cash_held = excluded.cash_held`,
				a.ID, a.Cash, a.CashHeld)
			symbols := make([]string, len(a.Holdings))
			for i, h := range a.Holdings {
				symbols[i] = h.Symbol
				b.Queue(`INSERT INTO ledger.holdings (account_id, symbol, quantity, held) VALUES ($1, $2, $3, $4)
					ON CONFLICT (account_id, symbol) DO UPDATE SET quantity = excluded.quantity, held = excluded.held`,
					a.ID, h.Symbol, h.Quantity, h.Held)
			}
			b.Queue(`DELETE FROM ledger.holdings WHERE account_id = $1 AND NOT (symbol = ANY($2))`, a.ID, symbols)
		}
		for _, h := range ch.PutHolds {
			order, err := json.Marshal(h.Order)
			if err != nil {
				return err
			}
			b.Queue(`INSERT INTO ledger.holds (order_id, account_id, "order", remaining, cash, shares, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (order_id) DO UPDATE SET remaining = excluded.remaining, cash = excluded.cash, shares = excluded.shares`,
				h.Order.ID, h.Order.AccountID, order, h.Remaining, h.Cash, h.Shares, h.CreatedAt)
		}
		if len(ch.DeleteHolds) > 0 {
			b.Queue(`DELETE FROM ledger.holds WHERE order_id = ANY($1)`, ch.DeleteHolds)
		}
		for _, e := range ch.Entries {
			b.Queue(`INSERT INTO ledger.entries (account_id, kind, tick, order_id, reference, cash_delta, symbol, share_delta, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				e.AccountID, e.Kind, nullIf(e.Tick, 0), nullIf(e.OrderID, ""), nullIf(e.Reference, ""),
				e.CashDelta, nullIf(e.Symbol, ""), e.ShareDelta, e.CreatedAt)
		}
		if b.Len() > 0 {
			if err := tx.SendBatch(ctx, b).Close(); err != nil {
				return err
			}
		}
		if ch.LastTick != nil {
			// Guarded so two writers can never both apply the same tick.
			tag, err := tx.Exec(ctx, `UPDATE ledger.state SET last_tick = $1 WHERE last_tick = $1 - 1`, *ch.LastTick)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%w: tick %d does not follow the stored last tick", ledger.ErrTickOutOfOrder, *ch.LastTick)
			}
		}
		return nil
	})
}

// Audit verifies that the append-only entries reproduce every account's cash
// and share balances. It returns an error describing the first mismatch.
func (s *Store) Audit(ctx context.Context) error {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT a.id FROM ledger.accounts a
		LEFT JOIN (SELECT account_id, sum(cash_delta) AS cash FROM ledger.entries GROUP BY account_id) e
		  ON e.account_id = a.id
		WHERE a.cash <> coalesce(e.cash, 0)
		LIMIT 1`).Scan(&id)
	if err == nil {
		return fmt.Errorf("account %q: cash does not match entries", id)
	} else if err != pgx.ErrNoRows {
		return err
	}
	var sym string
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(h.account_id, e.account_id), coalesce(h.symbol, e.symbol)
		FROM ledger.holdings h
		FULL JOIN (SELECT account_id, symbol, sum(share_delta) AS shares FROM ledger.entries
		           WHERE symbol IS NOT NULL GROUP BY account_id, symbol) e
		  ON e.account_id = h.account_id AND e.symbol = h.symbol
		WHERE coalesce(h.quantity, 0) <> coalesce(e.shares, 0)
		LIMIT 1`).Scan(&id, &sym)
	if err == nil {
		return fmt.Errorf("account %q: %s shares do not match entries", id, sym)
	} else if err != pgx.ErrNoRows {
		return err
	}
	return nil
}

// nullIf maps a zero value to SQL NULL.
func nullIf[T comparable](v, zero T) any {
	if v == zero {
		return nil
	}
	return v
}
