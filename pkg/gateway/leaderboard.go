package gateway

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/samcjohns/t3/pkg/ledger"
)

// Standing is one trader's place on the leaderboard.
type Standing struct {
	// Rank is shared by equal values, so ties are both ranked 1 and the next
	// trader is ranked 3.
	Rank       int    `json:"rank"`
	Username   string `json:"username"`
	TotalValue int64  `json:"total_value"`
	// Gain is TotalValue less the starting cash every trader receives.
	Gain   int64 `json:"gain"`
	userID string
}

// leaderboard caches the ranked standings for one engine tick, since values
// only change when an auction clears. Registrations appear from the next tick.
type leaderboard struct {
	mu        sync.Mutex
	tick      uint64
	built     bool
	standings []Standing
}

// standings returns every trader ranked by account value, highest first.
func (g *Gateway) standings(ctx context.Context) (tick uint64, _ []Standing, err error) {
	lb := &g.board
	lb.mu.Lock()
	defer lb.mu.Unlock()
	tick = g.market.LastTick()
	if lb.built && lb.tick == tick {
		return tick, lb.standings, nil
	}

	traders, err := g.auth.store.Traders(ctx)
	if err != nil {
		return 0, nil, err
	}
	out := make([]Standing, 0, len(traders))
	for _, u := range traders {
		p, err := g.reports.Portfolio(u.ID)
		if errors.Is(err, ledger.ErrUnknownAccount) {
			continue // registration still setting up the account
		}
		if err != nil {
			return 0, nil, err
		}
		out = append(out, Standing{Username: u.Username, TotalValue: p.TotalValue, Gain: p.TotalValue - g.cfg.StartingCash, userID: u.ID})
	}
	slices.SortFunc(out, func(a, b Standing) int {
		return cmp.Or(cmp.Compare(b.TotalValue, a.TotalValue), strings.Compare(a.Username, b.Username))
	})
	for i := range out {
		out[i].Rank = i + 1
		if i > 0 && out[i].TotalValue == out[i-1].TotalValue {
			out[i].Rank = out[i-1].Rank
		}
	}
	lb.tick, lb.built, lb.standings = tick, true, out
	return tick, out, nil
}

func (g *Gateway) getLeaderboard(w http.ResponseWriter, r *http.Request, u *User) error {
	limit, err := limitParam(r)
	if err != nil {
		return err
	}
	tick, all, err := g.standings(r.Context())
	if err != nil {
		return err
	}
	resp := map[string]any{
		"tick":          tick,
		"starting_cash": g.cfg.StartingCash,
		"traders":       len(all),
		"standings":     all[:min(limit, len(all))],
		"you":           nil,
	}
	// A signed-in trader also gets their own standing, even outside the top.
	if u != nil {
		if i := slices.IndexFunc(all, func(s Standing) bool { return s.userID == u.ID }); i >= 0 {
			resp["you"] = all[i]
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}
