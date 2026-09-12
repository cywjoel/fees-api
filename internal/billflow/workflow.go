// Package billflow contains the Temporal workflow that owns a bill for the
// duration of its fee period.
//
// One workflow execution corresponds to exactly one bill, and the workflow is
// the authoritative writer for that bill while it is open. This is what Temporal
// is here for, and the reasons are specific:
//
//   - Serialised writes. A workflow executes as a single-threaded deterministic
//     coroutine scheduler, so concurrent line-item additions to one bill are
//     ordered by construction. There is no row lock, no optimistic-concurrency
//     retry loop, and no lost update to defend against.
//   - A durable deadline. A fee period may run for a month. The period-end timer
//     survives process restarts, deploys, and crashes.
//   - Reliable hand-off. The invoice emission at the end of the period is an
//     activity with a retry policy, so a transient downstream failure does not
//     lose the close.
//   - A replayable audit trail. The workflow's history is an ordered record of
//     every charge and every state change.
//
// The package deliberately does not import Encore. It depends on the domain in
// internal/bill and on the Temporal SDK, so the lifecycle can be exercised in
// tests without any infrastructure.
package billflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

// TaskQueue is the queue the bill workflow and its activities run on.
const TaskQueue = "fees-api-bills"

// Update, query, and activity names. They are part of the workflow's contract
// with its callers and its worker, so they are named constants rather than
// literals scattered across the codebase.
const (
	// UpdateAddLineItem accrues a charge onto the bill.
	UpdateAddLineItem = "addLineItem"

	// UpdateCloseBill closes the bill early, before its period ends.
	UpdateCloseBill = "closeBill"

	// QueryGetBill reads the bill's live state.
	QueryGetBill = "getBill"

	// ActivityPersistInvoice writes the frozen invoice to durable storage.
	ActivityPersistInvoice = "PersistInvoice"

	// ActivityEmitInvoice hands the invoice to the payee. Its implementation is
	// out of scope for this service; the seam and its retry behaviour are not.
	ActivityEmitInvoice = "EmitInvoice"

	// ActivityFinalizeInvoice records that the hand-off completed.
	ActivityFinalizeInvoice = "FinalizeInvoice"
)

// StartBillInput opens a bill for a fee period.
//
// The period bounds are supplied by the caller rather than derived from a
// billing calendar here. That keeps calendar policy out of this service, and it
// makes the timer path demonstrable: a reviewer can set PeriodEnd seconds away
// and watch the bill close, which a hard-coded month would make impossible to
// exercise by hand.
type StartBillInput struct {
	BillID      string    `json:"billId"`
	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

// AddLineItemInput accrues one charge.
type AddLineItemInput struct {
	ItemID      string      `json:"itemId"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
}

// CloseBillInput requests an early close. It carries no fields today; it exists
// so the update has a stable shape to grow into.
type CloseBillInput struct{}

// FinalizeInvoiceInput records a bill's terminal state in durable storage.
type FinalizeInvoiceInput struct {
	BillID string     `json:"billId"`
	State  bill.State `json:"state"`
}

// persistOptions govern the durable write. The invoice must be recorded, so this
// retries patiently.
func persistOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    20,
		},
	}
}

// emitOptions govern the invoice hand-off. Attempts are bounded: a hand-off that
// keeps failing leaves the bill visibly in CLOSING rather than retrying forever
// in silence, which is the state an operator should be alerted on.
func emitOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    10,
		},
	}
}

// BillWorkflow owns one bill from the start of its fee period to the moment its
// invoice has been handed off.
//
// The shape of the body matters as much as its behaviour. Update handlers are
// yield-free: they validate, mutate in memory, and return. All waiting - the
// period timer, the activity calls - happens in the main loop below. Because
// handlers and the main loop run on the same thread and interleave only at yield
// points, a handler containing no yield point is atomic with respect to the
// timer. That is what makes the close race resolvable without a lock: whichever
// trigger reaches the state first wins, and the loser is a no-op reading the
// same variable on the same thread.
//
// Were the close handler to call an activity itself, it would yield mid-
// transition and the timer could fire inside that window, producing a bill that
// is half-closed. It does not; the main loop runs the activities afterwards.
func BillWorkflow(ctx workflow.Context, in StartBillInput) (bill.Snapshot, error) {
	logger := workflow.GetLogger(ctx)

	currency, err := money.Lookup(in.Currency)
	if err != nil {
		return bill.Snapshot{}, temporal.NewNonRetryableApplicationError(
			err.Error(), ClassifyRejection(err), err)
	}

	b, err := bill.New(in.BillID, currency, in.PeriodStart, in.PeriodEnd, workflow.Now(ctx))
	if err != nil {
		return bill.Snapshot{}, temporal.NewNonRetryableApplicationError(
			err.Error(), ClassifyRejection(err), err)
	}

	// closeRequested lets an early close wake the main loop. The settable is
	// resolved by the close handler, which is why the handler can stay yield-free
	// while still ending the wait.
	closeRequested, requestClose := workflow.NewFuture(ctx)
	closeSignalled := false

	// beginClose is the single place the bill's state changes to CLOSING. Both
	// triggers route through it, and it is a no-op once the bill has left OPEN,
	// so whichever arrives second changes nothing.
	beginClose := func(trigger bill.Trigger) bill.Snapshot {
		snap, closeErr := b.Close(trigger, workflow.Now(ctx))
		if closeErr != nil {
			// Close only errors on an impossible transition, which the state
			// machine table rules out from OPEN.
			logger.Error("closing bill failed", "billID", b.ID(), "error", closeErr)
			return b.Snapshot()
		}
		if !closeSignalled {
			closeSignalled = true
			requestClose.Set(nil, nil)
		}
		return snap
	}

	// Handlers are registered before the first yield so that updates delivered
	// while the workflow was starting are not lost.

	if err := workflow.SetQueryHandler(ctx, QueryGetBill, func() (bill.Snapshot, error) {
		return b.Snapshot(), nil
	}); err != nil {
		return bill.Snapshot{}, err
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddLineItem,
		// Handler: yield-free. Validation has already passed, so this only
		// mutates and returns.
		func(ctx workflow.Context, req AddLineItemInput) (bill.AddResult, error) {
			return b.AddLineItem(bill.LineItem{
				ID:          req.ItemID,
				Amount:      req.Amount,
				Description: req.Description,
				AccruedAt:   workflow.Now(ctx),
			})
		},
		workflow.UpdateHandlerOptions{
			// Validator: rejects synchronously, so the caller gets a real refusal
			// and the rejection never enters workflow history.
			Validator: func(ctx workflow.Context, req AddLineItemInput) error {
				return rejection(b.ValidateLineItem(bill.LineItem{
					ID:          req.ItemID,
					Amount:      req.Amount,
					Description: req.Description,
					AccruedAt:   workflow.Now(ctx),
				}))
			},
		})
	if err != nil {
		return bill.Snapshot{}, err
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateCloseBill,
		// Handler: yield-free. It freezes the totals and returns the snapshot the
		// caller sees, while the invoice hand-off happens in the main loop.
		//
		// There is no validator, deliberately. Closing a bill that is already
		// closing or closed is not a contradiction - the caller wanted the bill
		// closed and it is - so it returns the frozen invoice with the trigger
		// that actually caused the close, rather than an error.
		func(ctx workflow.Context, _ CloseBillInput) (bill.Snapshot, error) {
			return beginClose(bill.TriggerAPIRequest), nil
		},
		workflow.UpdateHandlerOptions{})
	if err != nil {
		return bill.Snapshot{}, err
	}

	// The two close triggers, raced in a selector. The timer is a durable
	// Temporal timer built from workflow.Now - never time.Now, which would be
	// non-deterministic on replay and would break every in-flight bill the moment
	// a worker restarted.
	periodTimer := workflow.NewTimer(ctx, b.PeriodEnd().Sub(workflow.Now(ctx)))

	selector := workflow.NewSelector(ctx)
	selector.AddFuture(periodTimer, func(workflow.Future) {
		// Guarded, because an early close may already have won this race.
		if b.State() == bill.StateOpen {
			beginClose(bill.TriggerPeriodEnd)
		}
	})
	selector.AddFuture(closeRequested, func(workflow.Future) {
		// The close handler already froze the totals; this branch exists only to
		// end the wait.
	})
	selector.Select(ctx)

	// Belt and braces: the selector woke us, but the bill is only genuinely
	// closed once its state says so.
	if err := workflow.Await(ctx, func() bool { return b.State() != bill.StateOpen }); err != nil {
		return bill.Snapshot{}, err
	}

	frozen := b.Snapshot()
	logger.Info("bill closed",
		"billID", b.ID(), "closedBy", frozen.ClosedBy,
		"total", frozen.Total.String(), "lineItems", len(frozen.LineItems))

	// Persist before emitting. The ordering is deliberate: if the hand-off keeps
	// failing, the durable record of what was charged still exists. The reverse
	// ordering would allow an invoice to reach the payee with no record of it
	// here, which is the worse of the two failures.
	pctx := workflow.WithActivityOptions(ctx, persistOptions())
	if err := workflow.ExecuteActivity(pctx, ActivityPersistInvoice, frozen).Get(pctx, nil); err != nil {
		return bill.Snapshot{}, err
	}

	ectx := workflow.WithActivityOptions(ctx, emitOptions())
	if err := workflow.ExecuteActivity(ectx, ActivityEmitInvoice, frozen).Get(ectx, nil); err != nil {
		// The bill stays visibly in CLOSING with its frozen total. That is the
		// honest state: the totals are final, the hand-off is not done.
		return bill.Snapshot{}, err
	}

	if err := b.MarkInvoiced(); err != nil {
		return bill.Snapshot{}, err
	}

	// Record the terminal state durably. Until this lands, storage says CLOSING,
	// which is true: CLOSED means the hand-off completed.
	final := b.Snapshot()
	if err := workflow.ExecuteActivity(pctx, ActivityFinalizeInvoice, FinalizeInvoiceInput{
		BillID: b.ID(),
		State:  final.State,
	}).Get(pctx, nil); err != nil {
		return bill.Snapshot{}, err
	}

	return final, nil
}
