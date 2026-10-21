package ledger

import (
	"context"
	"math/big"
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
	Pool       PoolState
	// Marks are the last clearing prices of symbols that have traded.
	Marks        map[string]int64
	Bankruptcies []Bankruptcy
}

// PoolState is the persisted lending pool.
type PoolState struct {
	// Cash is the pool's cash that is not lent out, including Reserved.
	Cash int64
	// Reserved is cash set aside for open leveraged buy orders.
	Reserved int64
	// Shares is the total of every lender's pool shares.
	Shares int64
	// Index and Scaled hold, per leverage ratio, the borrow index and the
	// total scaled debt of every account. Nil maps mean none.
	Index  map[int64]*big.Int
	Scaled map[int64]*big.Int
}

// HoldState is a persisted hold.
type HoldState struct {
	Order     engine.Order
	Remaining int64
	Cash      int64
	Shares    int64
	// Leverage is the order's leverage ratio, or 0 if it borrows nothing,
	// and Loan the pool cash still set aside for it.
	Leverage  int64
	Loan      int64
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
	EntryWithdrawal   EntryKind = "withdrawal"
	EntryShareDeposit EntryKind = "share_deposit"
	EntryBuy          EntryKind = "buy"
	EntrySell         EntryKind = "sell"
	// EntryBorrow is cash lent to the account by the pool to fund a buy, and
	// EntryRepay cash it paid back.
	EntryBorrow EntryKind = "borrow"
	EntryRepay  EntryKind = "repay"
	// EntryLend is cash the account put into the pool, and EntryRedeem cash
	// it took back out.
	EntryLend   EntryKind = "lend"
	EntryRedeem EntryKind = "redeem"
)

// Entry is one immutable movement of an account's cash or shares. Summing an
// account's entries reproduces its cash and share totals; holds move nothing
// and have no entries.
type Entry struct {
	AccountID string
	Kind      EntryKind
	// Tick and OrderID are set for trades, and the borrowing and automatic
	// repayment they cause.
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
	// Pool is set when the operation changed the lending pool.
	Pool *PoolState
	// Marks holds the symbols whose last clearing price changed.
	Marks        map[string]int64
	Bankruptcies []Bankruptcy
}
