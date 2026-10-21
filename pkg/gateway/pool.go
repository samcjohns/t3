package gateway

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/samcjohns/t3/pkg/ledger"
)

// poolStake is the signed-in trader's side of the lending pool.
type poolStake struct {
	Status ledger.Status `json:"status"`
	// CashAvailable is cash not held for open orders, which can be lent or
	// used to repay.
	CashAvailable int64 `json:"cash_available"`
	PoolShares    int64 `json:"pool_shares"`
	PoolValue     int64 `json:"pool_value"`
	Owed          int64 `json:"owed"`
}

func (g *Gateway) getPool(w http.ResponseWriter, _ *http.Request, u *User) error {
	resp := map[string]any{"pool": g.ledger.Pool(), "you": nil}
	if u != nil {
		if a, err := g.ledger.Account(u.ID); err == nil {
			resp["you"] = poolStake{a.Status, a.Cash - a.CashHeld, a.PoolShares, a.PoolValue, a.Owed}
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

type amountRequest struct {
	Amount int64 `json:"amount"`
}

func (g *Gateway) lend(w http.ResponseWriter, r *http.Request, u *User) error {
	var req amountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if err := g.ledger.Lend(r.Context(), u.ID, req.Amount); err != nil {
		return err
	}
	g.log.Info("pool lend", "user_id", u.ID, "amount", req.Amount)
	return g.getPool(w, r, u)
}

func (g *Gateway) withdrawFromPool(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Amount int64 `json:"amount"`
		// All withdraws the whole stake, whatever it is worth by then.
		All bool `json:"all"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	switch {
	case req.All && req.Amount != 0:
		return badRequest("give an amount or all, not both")
	case !req.All && req.Amount <= 0:
		return badRequest("amount must be positive")
	}
	paid, err := g.ledger.Redeem(r.Context(), u.ID, req.Amount)
	if err != nil {
		return err
	}
	g.log.Info("pool withdrawal", "user_id", u.ID, "amount", paid)
	return g.getPool(w, r, u)
}

func (g *Gateway) repay(w http.ResponseWriter, r *http.Request, u *User) error {
	var req amountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	paid, err := g.ledger.Repay(r.Context(), u.ID, req.Amount)
	if err != nil {
		return err
	}
	g.log.Info("pool repayment", "user_id", u.ID, "amount", paid)
	return g.getPool(w, r, u)
}

// BankruptTrader is a trader whose account was closed for debt it could not
// repay.
type BankruptTrader struct {
	Username string    `json:"username"`
	Tick     uint64    `json:"tick"`
	Time     time.Time `json:"time"`
	// WrittenOff is the debt left unpaid, which the pool's lenders lost.
	WrittenOff int64 `json:"written_off"`
}

// getBankruptcies lists bankrupt traders, most debt left unpaid first.
func (g *Gateway) getBankruptcies(w http.ResponseWriter, r *http.Request, _ *User) error {
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	all := g.ledger.Bankruptcies()
	names := map[string]string{}
	if len(all) > 0 {
		traders, err := g.auth.store.Traders(r.Context())
		if err != nil {
			return err
		}
		for _, u := range traders {
			names[u.ID] = u.Username
		}
	}
	out := make([]BankruptTrader, 0, len(all))
	for _, b := range all {
		if name, ok := names[b.AccountID]; ok {
			out = append(out, BankruptTrader{name, b.Tick, b.Time, b.WrittenOff})
		}
	}
	slices.SortStableFunc(out, func(a, b BankruptTrader) int { return cmp.Compare(b.WrittenOff, a.WrittenOff) })
	writeJSON(w, http.StatusOK, map[string]any{"bankruptcies": out[:min(limit, len(out))], "total": len(out)})
	return nil
}
