package bill

import (
	"errors"
	"fmt"
	"time"

	"fees-api/internal/money"
)

var (
	// ErrNotOpen is returned when a line item is offered to a bill whose totals
	// are already frozen. It is the state-integrity rejection: a contradiction
	// between what the caller wants and what the bill now is.
	ErrNotOpen = errors.New("bill: bill is not open")

	// ErrLineItemConflict is returned when a line item identifier already
	// present on the bill is reused with different detail. That is not a retry
	// - it is two different charges claiming one identity - so it is rejected
	// rather than resolved by overwriting.
	ErrLineItemConflict = errors.New("bill: line item id already used with different detail")

	// ErrCustomerMismatch is returned when a line item states a customer that is
	// not the one the bill belongs to.
	//
	// The caller already chose the bill by its identifier, so this does not tell
	// the system who is being charged - it tells the system whether the caller and
	// the bill agree. A fee engine that computes the wrong bill id otherwise
	// charges the wrong customer in silence.
	ErrCustomerMismatch = errors.New("bill: line item customer does not match the bill")

	// ErrInvalidLineItem is returned when a line item is missing required detail.
	ErrInvalidLineItem = errors.New("bill: line item is missing required detail")

	// ErrInvalidPeriod is returned when a fee period does not end after it begins.
	ErrInvalidPeriod = errors.New("bill: period end must be after period start")

	// ErrInvalidBill is returned when a bill is missing required detail.
	ErrInvalidBill = errors.New("bill: bill is missing required detail")
)

// LineItem is a single fee accrued onto a bill.
//
// The identifier is supplied by the caller, not generated here. A fee is caused
// by something that already has an identity - a transaction, a transfer, a card
// authorisation - so keying the item by that identity makes deduplication a
// business invariant rather than a transport trick: the fee for a given source
// event appears at most once on a bill.
type LineItem struct {
	ID          string      `json:"id"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
	AccruedAt   time.Time   `json:"accruedAt"`
}

// sameChargeAs reports whether other states the same charge as l.
//
// AccruedAt is deliberately excluded. A retry of an addition carries a later
// timestamp than the original, and that difference must not be mistaken for a
// conflicting charge; the stored item keeps the time it was first accrued.
func (l LineItem) sameChargeAs(other LineItem) bool {
	return l.Amount.Equal(other.Amount) && l.Description == other.Description
}

// Outcome reports whether an addition created a new line item or matched one
// already accrued, so the API layer can answer 201 or 200 accordingly.
type Outcome string

const (
	// OutcomeCreated means the line item was newly accrued.
	OutcomeCreated Outcome = "created"

	// OutcomeAlreadyAccrued means an identical line item was already present, so
	// the addition was a retry and changed nothing.
	OutcomeAlreadyAccrued Outcome = "already_accrued"
)

// AddResult is the outcome of accruing a line item.
type AddResult struct {
	Outcome      Outcome     `json:"outcome"`
	LineItem     LineItem    `json:"lineItem"`
	RunningTotal money.Money `json:"runningTotal"`
}

// Snapshot is a bill's externally visible state at a point in time. For a closed
// bill it is the invoice: the total charged and every line item comprising it.
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
// It is not safe for concurrent use, and deliberately so. Serialisation is the
// workflow's job - a bill is mutated only from a single workflow goroutine - so
// carrying a mutex here would suggest a concurrency model the design does not
// have.
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
	// which would make the reported line item order vary between runs; inside a
	// workflow that non-determinism would break history replay outright.
	order []string

	closedAt time.Time
	closedBy Trigger
}

// New opens a bill for a fee period, belonging to a customer.
//
// It deliberately does NOT reject an empty customer, and that omission is
// load-bearing rather than an oversight.
//
// A bill's workflow may run for a month, and Temporal reconstructs a running one
// by replaying its recorded history through whatever code is deployed now. A
// history recorded before the customer existed decodes with an empty one -
// legally and silently. Were this constructor to reject that, the workflow would
// return before issuing any command, while the history it is being replayed
// against records a timer and two activities. Replay would diverge, and every
// bill open across the deploy would be stranded.
//
// The requirement that a customer be supplied therefore lives at the API
// boundary, where it runs once per request and never on replay. A later
// "tightening" here would break bills that are already running; the test for
// this defends the omission.
func New(id string, customerID string, currency money.Currency, periodStart, periodEnd, createdAt time.Time) (*Bill, error) {
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

// ID returns the bill's identifier.
func (b *Bill) ID() string { return b.id }

// CustomerID returns the customer the bill belongs to. It is empty only for a
// bill created before bills had owners; see New.
func (b *Bill) CustomerID() string { return b.customerID }

// State returns the bill's current lifecycle state.
func (b *Bill) State() State { return b.state }

// Currency returns the currency the bill is denominated in.
func (b *Bill) Currency() money.Currency { return b.currency }

// PeriodEnd returns the instant the fee period closes.
func (b *Bill) PeriodEnd() time.Time { return b.periodEnd }

// Total returns the running total of an open bill, or the frozen total of one
// that has closed.
func (b *Bill) Total() money.Money { return b.total }

// ValidateLineItem reports the error that AddLineItem would return for item,
// or nil if the addition would be accepted, without mutating the bill.
//
// It exists so that a Temporal update validator can reject an addition before it
// enters workflow history. A rejected update is never recorded; an update that
// fails inside its handler is. Validating first is therefore both the correct
// semantics - the caller gets a synchronous refusal - and the cheaper one.
func (b *Bill) ValidateLineItem(item LineItem, assertCustomerID string) error {
	_, err := b.checkLineItem(item, assertCustomerID)
	return err
}

// validateLineItem reports whether an item carries the detail every charge must
// have, independent of any bill. Shared by the live path and the storage path so
// a malformed item is refused the same way whichever answers it.
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

// checkLineItem applies every acceptance rule without mutating the bill. It
// returns the already-accrued item when the addition is an idempotent retry.
// assertCustomerID is what the caller believes this bill's customer to be, or
// empty when it did not say. It is compared, never stored: the caller already
// chose the bill by its identifier, so this establishes whether the caller and
// the bill agree, not who is being charged.
//
// It is checked first, before deduplication and before the state check, and that
// position is deliberate rather than incidental.
//
// The customer assertion is not the same kind of check as the currency, though
// the two look alike. The currency asks whether this charge is compatible with
// this bill, which only matters once the bill can accept charges at all - so it
// belongs after the state check. The customer asks whether this is the right bill
// in the first place. If it is not, nothing that follows is meaningful: whether
// the bill is open, and whether it already carries an item with this id, are
// facts about a bill the caller did not mean to address.
//
// Checking it after deduplication made the checksum silent exactly where it was
// most needed. A fee engine that computes the wrong bill id and re-sends was told
// "already accrued" for a charge sitting on someone else's invoice, because an
// item with that id happened to exist there.
//
// An empty assertion is not a mismatch. A workflow history recorded before the
// field existed decodes it as empty, so treating absence as disagreement would
// introduce a branch on that absence and break replay.
func (b *Bill) checkLineItem(item LineItem, assertCustomerID string) (*LineItem, error) {
	if err := validateLineItem(item); err != nil {
		return nil, err
	}

	if assertCustomerID != "" && assertCustomerID != b.customerID {
		// The bill's own customer is deliberately not named.
		//
		// A check that reveals the right answer when the caller guesses wrong is a
		// lookup with extra steps. Today that leaks nothing, because retrieving the
		// bill reports its customer anyway - but the design expects authentication
		// upstream, and the moment retrieval starts refusing strangers this message
		// would keep answering the question retrieval has stopped answering. Whoever
		// builds that layer will check the read endpoints; nobody thinks of an error
		// message as somewhere data escapes from.
		//
		// The caller learns what it needs: it addressed the wrong bill, and should
		// fix the id it computed.
		return nil, fmt.Errorf("%w: line item states a customer that is not the one bill %q belongs to",
			ErrCustomerMismatch, b.id)
	}

	// Deduplication precedes the state check. An identical retry of an item that
	// was already accrued while the bill was open describes a charge the bill
	// already carries; answering "conflict" because the bill has since closed
	// would report a failure for something that in fact succeeded, and invite the
	// caller to compensate for money that is genuinely on the invoice.
	if existing, ok := b.items[item.ID]; ok {
		if !existing.sameChargeAs(item) {
			return nil, fmt.Errorf("%w: %q", ErrLineItemConflict, item.ID)
		}
		return &existing, nil
	}

	if !AcceptsLineItems(b.state) {
		return nil, fmt.Errorf("%w: %q is in %s", ErrNotOpen, b.id, b.state)
	}
	if item.Amount.Currency().Code != b.currency.Code {
		return nil, fmt.Errorf("%w: bill %q is denominated in %s, line item is in %s",
			money.ErrCurrencyMismatch, b.id, b.currency.Code, item.Amount.Currency().Code)
	}
	return nil, nil
}

// CheckAgainstSnapshot applies the acceptance rules to a line item offered to a
// bill that is no longer live, using the frozen invoice as the only evidence.
//
// It exists because a bill whose workflow has finished still has to answer for
// charges. The invoice is read back from storage as a Snapshot rather than a
// Bill, so AddLineItem cannot be used, and the obvious alternative - comparing
// the item against the snapshot at the call site - would state the deduplication
// rule a second time, in a second place, where the two would drift.
//
// It returns the already-accrued item when the addition is an identical retry,
// ErrLineItemConflict when the identifier was reused with different detail, and
// ErrNotOpen when the charge is genuinely new and has arrived too late.
func CheckAgainstSnapshot(snap Snapshot, item LineItem, assertCustomerID string) (*LineItem, error) {
	if err := validateLineItem(item); err != nil {
		return nil, err
	}

	// First, as on the live path: addressing the wrong bill makes everything
	// after it a fact about a bill the caller did not mean to reach.
	if assertCustomerID != "" && assertCustomerID != snap.CustomerID {
		// Named neither here nor on the live path; see checkLineItem.
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
		// A live bill's charges are the workflow's to answer for, not storage's.
		// Reaching here would mean reading a stale invoice for an open bill.
		return nil, fmt.Errorf("%w: %q is in %s and should be answered by its workflow",
			ErrNotOpen, snap.ID, snap.State)
	}
	return nil, fmt.Errorf("%w: %q is in %s", ErrNotOpen, snap.ID, snap.State)
}

// AddLineItem accrues a charge onto an open bill.
//
// The addition is idempotent by identifier: re-offering an identical item
// returns the one already accrued and leaves the total untouched, so a client
// that retries after a timeout cannot double-charge. Re-offering the identifier
// with different detail is a conflict, not a retry.
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
//
// Closing a bill that has already left OPEN is not an error. The caller asked
// for the bill to be closed and the bill is closed; the returned snapshot
// reports the trigger that actually caused it, so a request that lost a race
// against the period-end deadline learns what happened without being handed a
// failure for a state it wanted.
func (b *Bill) Close(trigger Trigger, at time.Time) (Snapshot, error) {
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

// LineItems returns the accrued line items in the order they were accrued.
//
// The slice is freshly allocated on every call, so a caller holding a snapshot
// of a closed bill cannot have it altered underneath them.
func (b *Bill) LineItems() []LineItem {
	items := make([]LineItem, 0, len(b.order))
	for _, id := range b.order {
		items = append(items, b.items[id])
	}
	return items
}

// Snapshot returns the bill's externally visible state, copying the line items
// so that the result is independent of any later mutation of the bill.
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
