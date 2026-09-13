package bill

import (
	"errors"
	"fmt"
	"time"

	"fees-api/internal/money"
)

var (
	ErrNotOpen = errors.New("bill: bill is not open")

	// ErrLineItemConflict means one identifier claimed two different charges.
	// That is not a retry, so it is rejected rather than resolved by overwriting.
	ErrLineItemConflict = errors.New("bill: line item id already used with different detail")

	// ErrCustomerMismatch means a line item states a customer that is not the
	// one the bill belongs to. The caller already chose the bill by its
	// identifier, so this reports whether the caller and the bill agree, not who
	// is being charged.
	ErrCustomerMismatch = errors.New("bill: line item customer does not match the bill")

	ErrInvalidLineItem = errors.New("bill: line item is missing required detail")
	ErrInvalidPeriod   = errors.New("bill: period end must be after period start")
	ErrInvalidBill     = errors.New("bill: bill is missing required detail")
)

// LineItem is a single fee accrued onto a bill.
//
// The identifier is supplied by the caller, not generated here: a fee is caused
// by something that already has an identity, so keying the item by that identity
// makes deduplication a business invariant rather than a transport trick.
type LineItem struct {
	ID          string      `json:"id"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
	AccruedAt   time.Time   `json:"accruedAt"`
}

// sameChargeAs deliberately excludes AccruedAt: a retry carries a later
// timestamp than the original, and that difference must not be mistaken for a
// conflicting charge.
func (l LineItem) sameChargeAs(other LineItem) bool {
	return l.Amount.Equal(other.Amount) && l.Description == other.Description
}

// Outcome reports whether an addition created a line item or matched one already
// accrued, so the API layer can answer 201 or 200 accordingly.
type Outcome string

const (
	OutcomeCreated        Outcome = "created"
	OutcomeAlreadyAccrued Outcome = "already_accrued"
)

type AddResult struct {
	Outcome      Outcome     `json:"outcome"`
	LineItem     LineItem    `json:"lineItem"`
	RunningTotal money.Money `json:"runningTotal"`
}

// Snapshot is a bill's externally visible state at a point in time. For a closed
// bill it is the invoice.
type Snapshot struct {
	ID          string         `json:"id"`
	CustomerID  string         `json:"customerId"`
	State       State          `json:"state"`
	Currency    money.Currency `json:"currency"`
	PeriodStart time.Time      `json:"periodStart"`
	PeriodEnd   time.Time      `json:"periodEnd"`
	CreatedAt   time.Time      `json:"createdAt"`
	Total       money.Money    `json:"total"`
	LineItems   []LineItem     `json:"lineItems"`
	ClosedAt    *time.Time     `json:"closedAt,omitempty"`
	ClosedBy    Trigger        `json:"closedBy,omitempty"`
}

// Bill is the aggregate: a fee period, the charges accrued against it, and the
// lifecycle position that decides what may still happen to it.
//
// Not safe for concurrent use, deliberately: a bill is mutated only from a
// single workflow goroutine, and a mutex here would suggest a concurrency model
// the design does not have.
type Bill struct {
	id          string
	customerID  string
	currency    money.Currency
	periodStart time.Time
	periodEnd   time.Time
	createdAt   time.Time

	state State
	total money.Money

	items map[string]LineItem
	// order preserves insertion order for listing. Go randomises map iteration,
	// which inside a workflow would break history replay outright.
	order []string

	closedAt time.Time
	closedBy Trigger
}

// New opens a bill for a fee period, belonging to a customer.
//
// It deliberately does not reject an empty customer; the requirement lives at the
// API boundary instead. See the note on the missing check below.
func New(id string, customerID string, currency money.Currency, periodStart, periodEnd, createdAt time.Time) (*Bill, error) {
	// No customer check here, and its absence is load-bearing. Temporal replays a
	// running workflow through the code deployed now, and a history recorded before
	// the customer existed decodes with an empty one. Rejecting it would return
	// before issuing any command, against a history recording a timer and two
	// activities: replay diverges and every bill open across the deploy is
	// stranded. A later "tightening" here breaks running bills; a test defends it.
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidBill)
	}
	if currency.IsZero() {
		return nil, fmt.Errorf("%w: currency is required", ErrInvalidBill)
	}
	if !periodEnd.After(periodStart) {
		return nil, fmt.Errorf("%w: %s is not after %s",
			ErrInvalidPeriod, periodEnd.Format(time.RFC3339), periodStart.Format(time.RFC3339))
	}
	return &Bill{
		id:          id,
		customerID:  customerID,
		currency:    currency,
		periodStart: periodStart,
		periodEnd:   periodEnd,
		createdAt:   createdAt,
		state:       StateOpen,
		total:       money.Zero(currency),
		items:       make(map[string]LineItem),
	}, nil
}

func (b *Bill) ID() string               { return b.id }
func (b *Bill) State() State             { return b.state }
func (b *Bill) Currency() money.Currency { return b.currency }
func (b *Bill) PeriodEnd() time.Time     { return b.periodEnd }
func (b *Bill) Total() money.Money       { return b.total }

// CustomerID is empty only for a bill created before bills had owners; see New.
func (b *Bill) CustomerID() string { return b.customerID }

// ValidateLineItem reports the error AddLineItem would return, without mutating
// the bill.
//
// It exists so a Temporal update validator can reject an addition before it
// enters workflow history: a rejected update is never recorded, one that fails
// inside its handler is.
func (b *Bill) ValidateLineItem(item LineItem, assertCustomerID string) error {
	_, err := b.checkLineItem(item, assertCustomerID)
	return err
}

// validateLineItem checks the detail every charge must carry, independent of any
// bill. Shared by the live and storage paths so a malformed item is refused the
// same way whichever answers it.
func validateLineItem(item LineItem) error {
	if item.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidLineItem)
	}
	if item.Amount.Currency().IsZero() {
		return fmt.Errorf("%w: amount and currency are required", ErrInvalidLineItem)
	}
	if item.Description == "" {
		return fmt.Errorf("%w: description is required", ErrInvalidLineItem)
	}
	return nil
}

// checkLineItem applies every acceptance rule without mutating the bill,
// returning the already-accrued item when the addition is an idempotent retry.
//
// assertCustomerID is what the caller believes the bill's customer to be, or
// empty when it did not say. It is compared, never stored. The order of the
// checks below is load-bearing; each says why in place.
func (b *Bill) checkLineItem(item LineItem, assertCustomerID string) (*LineItem, error) {
	if err := validateLineItem(item); err != nil {
		return nil, err
	}

	// First, because this asks whether it is the right bill at all. If it is not,
	// whether the bill is open and whether it already carries this item id are
	// facts about a bill the caller did not mean to address. Checking it after
	// deduplication made the checksum silent exactly where it was needed: a fee
	// engine computing the wrong bill id was told "already accrued" for a charge
	// on someone else's invoice. An empty assertion is not a mismatch - an old
	// history decodes it as empty, and branching on that would break replay.
	if assertCustomerID != "" && assertCustomerID != b.customerID {
		// The bill's own customer is deliberately not named: a check that reveals
		// the right answer when the caller guesses wrong is a lookup with extra
		// steps. It leaks nothing today, but the design expects authentication
		// upstream, and this message would outlive retrieval's own refusal.
		return nil, fmt.Errorf("%w: line item states a customer that is not the one bill %q belongs to",
			ErrCustomerMismatch, b.id)
	}

	// Deduplication precedes the state check: an identical retry of an item
	// accrued while the bill was open describes a charge the bill already
	// carries, and answering "conflict" because the bill has since closed would
	// invite the caller to compensate for money genuinely on the invoice.
	if existing, ok := b.items[item.ID]; ok {
		if !existing.sameChargeAs(item) {
			return nil, fmt.Errorf("%w: %q", ErrLineItemConflict, item.ID)
		}
		return &existing, nil
	}

	if !AcceptsLineItems(b.state) {
		return nil, fmt.Errorf("%w: %q is in %s", ErrNotOpen, b.id, b.state)
	}
	// After the state check, unlike the customer: this asks whether the charge is
	// compatible with the bill, which only matters once the bill can accept one.
	if item.Amount.Currency().Code != b.currency.Code {
		return nil, fmt.Errorf("%w: bill %q is denominated in %s, line item is in %s",
			money.ErrCurrencyMismatch, b.id, b.currency.Code, item.Amount.Currency().Code)
	}
	return nil, nil
}

// CheckAgainstSnapshot applies the acceptance rules to a line item offered to a
// bill whose workflow has finished, using the frozen invoice as the only
// evidence.
//
// It exists so the deduplication rule is not stated a second time at the call
// site, where the two would drift. Its ordering mirrors checkLineItem.
func CheckAgainstSnapshot(snap Snapshot, item LineItem, assertCustomerID string) (*LineItem, error) {
	if err := validateLineItem(item); err != nil {
		return nil, err
	}

	if assertCustomerID != "" && assertCustomerID != snap.CustomerID {
		return nil, fmt.Errorf("%w: line item states a customer that is not the one bill %q belongs to",
			ErrCustomerMismatch, snap.ID)
	}
	for _, existing := range snap.LineItems {
		if existing.ID != item.ID {
			continue
		}
		if !existing.sameChargeAs(item) {
			return nil, fmt.Errorf("%w: %q", ErrLineItemConflict, item.ID)
		}
		return &existing, nil
	}
	if AcceptsLineItems(snap.State) {
		// An open bill's charges are the workflow's to answer for. Reaching here
		// means a stale invoice was read for a bill that is still live.
		return nil, fmt.Errorf("%w: %q is in %s and should be answered by its workflow",
			ErrNotOpen, snap.ID, snap.State)
	}
	return nil, fmt.Errorf("%w: %q is in %s", ErrNotOpen, snap.ID, snap.State)
}

// AddLineItem accrues a charge onto an open bill.
//
// Idempotent by identifier: re-offering an identical item returns the one already
// accrued and leaves the total untouched, so a retry cannot double-charge.
func (b *Bill) AddLineItem(item LineItem, assertCustomerID string) (AddResult, error) {
	existing, err := b.checkLineItem(item, assertCustomerID)
	if err != nil {
		return AddResult{}, err
	}
	if existing != nil {
		return AddResult{
			Outcome:      OutcomeAlreadyAccrued,
			LineItem:     *existing,
			RunningTotal: b.total,
		}, nil
	}

	total, err := b.total.Add(item.Amount)
	if err != nil {
		return AddResult{}, err
	}

	b.items[item.ID] = item
	b.order = append(b.order, item.ID)
	b.total = total

	return AddResult{
		Outcome:      OutcomeCreated,
		LineItem:     item,
		RunningTotal: b.total,
	}, nil
}

// Close freezes the bill's total and line items and records what closed it.
// Closing a bill that has already left OPEN is not an error.
func (b *Bill) Close(trigger Trigger, at time.Time) (Snapshot, error) {
	// The caller asked for the bill to be closed and it is. The snapshot reports
	// the trigger that actually caused it, so a request that lost a race against
	// the period-end deadline learns what happened rather than getting a failure.
	if b.state != StateOpen {
		return b.Snapshot(), nil
	}
	next, err := Transition(b.state, EventClose)
	if err != nil {
		return Snapshot{}, err
	}
	b.state = next
	b.closedAt = at
	b.closedBy = trigger
	return b.Snapshot(), nil
}

// MarkInvoiced records that the invoice hand-off succeeded, completing the bill.
func (b *Bill) MarkInvoiced() error {
	if b.state == StateClosed {
		return nil
	}
	next, err := Transition(b.state, EventInvoiced)
	if err != nil {
		return err
	}
	b.state = next
	return nil
}

// LineItems returns the accrued line items in accrual order, freshly allocated so
// a caller holding a snapshot cannot have it altered underneath.
func (b *Bill) LineItems() []LineItem {
	items := make([]LineItem, 0, len(b.order))
	for _, id := range b.order {
		items = append(items, b.items[id])
	}
	return items
}

// Snapshot returns the bill's externally visible state, copying the line items
// so the result is independent of any later mutation.
func (b *Bill) Snapshot() Snapshot {
	s := Snapshot{
		ID:          b.id,
		CustomerID:  b.customerID,
		State:       b.state,
		Currency:    b.currency,
		PeriodStart: b.periodStart,
		PeriodEnd:   b.periodEnd,
		CreatedAt:   b.createdAt,
		Total:       b.total,
		LineItems:   b.LineItems(),
	}
	if b.state != StateOpen {
		closedAt := b.closedAt
		s.ClosedAt = &closedAt
		s.ClosedBy = b.closedBy
	}
	return s
}
