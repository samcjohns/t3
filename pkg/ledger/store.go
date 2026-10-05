package ledger

import (
	"context"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
)

// Store durably records ledger state. Apply must persist all of a change set
// atomically, or none of it, before returning.
type Store interface {
	Load(ctx context.Context) (State, error)
	Apply(ctx context.Context, ch Changes) error
}

// State is the full persisted ledger state.
type State struct {
	Accounts   []Account
	Holds      []HoldState
	References []Reference
	LastTick   uint64
}

// HoldState is a persisted hold.
type HoldState struct {
	Order     engine.Order
	Remaining int64
	Cash      int64
	Shares    int64
	CreatedAt time.Time
}

// Reference is an idempotency key already used by a deposit.
type Reference struct {
	AccountID string
	Reference string
}

// EntryKind classifies an Entry.
type EntryKind string

const (
	EntryDeposit      EntryKind = "deposit"
	EntryShareDeposit EntryKind = "share_deposit"
	EntryBuy          EntryKind = "buy"
	EntrySell         EntryKind = "sell"
)

// Entry is one immutable movement of an account's cash or shares. Summing an
// account's entries reproduces its cash and share totals; holds move nothing
// and have no entries.
type Entry struct {
	AccountID string
	Kind      EntryKind
	// Tick and OrderID are set for trades.
	Tick    uint64
	OrderID string
	// Reference is the deposit's idempotency key, if any.
	Reference  string
	CashDelta  int64
	Symbol     string
	ShareDelta int64
	CreatedAt  time.Time
}

// Changes is the effect of one ledger operation.
type Changes struct {
	// Accounts holds the complete new state of every account changed,
	// including all of its holdings. A holding absent from the list no
	// longer exists.
	Accounts    []Account
	PutHolds    []HoldState
	DeleteHolds []string
	Entries     []Entry
	// LastTick is set when the operation applied a tick.
	LastTick *uint64
}
