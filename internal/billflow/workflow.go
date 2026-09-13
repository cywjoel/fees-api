// Package billflow contains the Temporal workflow that owns a bill for the
// duration of its fee period.
//
// One workflow execution is one bill, and the workflow is the authoritative
// writer for that bill while it is open. Temporal is here for four specific
// things: serialised writes (a workflow is a single-threaded deterministic
// coroutine scheduler, so concurrent additions to one bill are ordered by
// construction - no row lock, no lost update), a durable month-long deadline
// that survives restarts and deploys, a retrying invoice hand-off so a transient
// downstream failure does not lose the close, and a replayable audit trail.
//
// The package deliberately does not import Encore, so the lifecycle can be
// exercised in tests without any infrastructure.
package billflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"fees-api/internal/bill"
	"fees-api/internal/money"
)

const TaskQueue = "fees-api-bills"

// WorkflowTypeName is the name the workflow is registered under and the name a
// start request must ask for. It is a constant because bill creation starts the
// workflow through the raw service API rather than the SDK helper, so the two
// cannot drift - and it matches the type recorded in the committed replay
// fixtures, which must keep replaying.
const WorkflowTypeName = "BillWorkflow"

// Update, query, and activity names: part of the workflow's contract with its
// callers and its worker.
const (
	UpdateAddLineItem = "addLineItem"
	UpdateCloseBill   = "closeBill"
	QueryGetBill      = "getBill"

	ActivityPersistInvoice = "PersistInvoice"

	// ActivityEmitInvoice hands the invoice to the payee. Its implementation is
	// out of scope for this service; the seam and its retry behaviour are not.
	ActivityEmitInvoice = "EmitInvoice"

	ActivityFinalizeInvoice = "FinalizeInvoice"
)

// StartBillInput opens a bill for a fee period.
//
// The period bounds come from the caller rather than a billing calendar here,
// which keeps calendar policy out of this service and makes the timer path
// demonstrable: a reviewer can set PeriodEnd seconds away and watch the bill
// close.
type StartBillInput struct {
	BillID string `json:"billId"`

	// A history recorded before this field existed decodes it as empty, and the
	// workflow must not branch on that: see bill.New. The requirement is enforced
	// at the API boundary instead.
	CustomerID string `json:"customerId"`

	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

type AddLineItemInput struct {
	ItemID string `json:"itemId"`

	// What the caller believes the bill's customer to be, or empty when it did
	// not say. Compared in the validator, never stored. An empty assertion is
	// deliberately not a mismatch: an old history decodes it as empty, and
	// treating absence as disagreement would branch on it and break replay.
	CustomerID string `json:"customerId,omitempty"`

	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
}

// CloseBillInput carries no fields today; it exists so the update has a stable
// shape to grow into.
type CloseBillInput struct{}

type FinalizeInvoiceInput struct {
	BillID string     `json:"billId"`
	State  bill.State `json:"state"`
}

// persistOptions govern the durable write: the invoice must be recorded, so this
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

// emitOptions bound the hand-off attempts: one that keeps failing leaves the bill
// visibly in CLOSING rather than retrying forever in silence.
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
// Its update handlers must stay yield-free: they validate, mutate in memory, and
// return, while all waiting happens in the main loop. Handlers and the main loop
// share a thread and interleave only at yield points, so a handler with no yield
// point is atomic with respect to the timer - which is what resolves the close
// race without a lock. A handler that awaited an activity would yield
// mid-transition and let the timer fire inside that window.
func BillWorkflow(ctx workflow.Context, in StartBillInput) (bill.Snapshot, error) {
	logger := workflow.GetLogger(ctx)

	currency, err := money.Lookup(in.Currency)
	if err != nil {
		return bill.Snapshot{}, temporal.NewNonRetryableApplicationError(
			err.Error(), ClassifyRejection(err), err)
	}

	b, err := bill.New(in.BillID, in.CustomerID, currency, in.PeriodStart, in.PeriodEnd, workflow.Now(ctx))
	if err != nil {
		return bill.Snapshot{}, temporal.NewNonRetryableApplicationError(
			err.Error(), ClassifyRejection(err), err)
	}

	// closeRequested lets an early close wake the main loop, which is how the
	// close handler ends the wait while staying yield-free.
	closeRequested, requestClose := workflow.NewFuture(ctx)
	closeSignalled := false

	// beginClose is the single place the state changes to CLOSING. Both triggers
	// route through it, and it is a no-op once the bill has left OPEN, so
	// whichever arrives second changes nothing.
	beginClose := func(trigger bill.Trigger) (bill.Snapshot, error) {
		snap, closeErr := b.Close(trigger, workflow.Now(ctx))
		if closeErr != nil {
			// Unreachable today, but returned rather than logged: swallowing it
			// answered 202 with a snapshot still reading OPEN, telling a caller its
			// bill was closing when nothing had happened.
			logger.Error("closing bill failed", "billID", b.ID(), "error", closeErr)
			return bill.Snapshot{}, closeErr
		}
		if !closeSignalled {
			closeSignalled = true
			requestClose.Set(nil, nil)
		}
		return snap, nil
	}

	// Handlers are registered before the first yield so that updates delivered
	// while the workflow was starting are not lost.

	if err := workflow.SetQueryHandler(ctx, QueryGetBill, func() (bill.Snapshot, error) {
		return b.Snapshot(), nil
	}); err != nil {
		return bill.Snapshot{}, err
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddLineItem,
		func(ctx workflow.Context, req AddLineItemInput) (bill.AddResult, error) {
			return b.AddLineItem(bill.LineItem{
				ID:          req.ItemID,
				Amount:      req.Amount,
				Description: req.Description,
				AccruedAt:   workflow.Now(ctx),
			}, req.CustomerID)
		},
		workflow.UpdateHandlerOptions{
			// The validator rejects synchronously, so the caller gets a real refusal
			// and the rejection never enters workflow history.
			Validator: func(ctx workflow.Context, req AddLineItemInput) error {
				return rejection(b.ValidateLineItem(bill.LineItem{
					ID:          req.ItemID,
					Amount:      req.Amount,
					Description: req.Description,
					AccruedAt:   workflow.Now(ctx),
				}, req.CustomerID))
			},
		})
	if err != nil {
		return bill.Snapshot{}, err
	}

	err = workflow.SetUpdateHandlerWithOptions(ctx, UpdateCloseBill,
		// No validator, deliberately: closing a bill that is already closing or
		// closed is not a contradiction, so this returns the frozen invoice with
		// the trigger that actually caused the close rather than an error.
		func(ctx workflow.Context, _ CloseBillInput) (bill.Snapshot, error) {
			return beginClose(bill.TriggerAPIRequest)
		},
		workflow.UpdateHandlerOptions{})
	if err != nil {
		return bill.Snapshot{}, err
	}

	// workflow.Now, never time.Now: the latter is non-deterministic on replay and
	// would break every in-flight bill the moment a worker restarted.
	untilPeriodEnd := b.PeriodEnd().Sub(workflow.Now(ctx))
	// Clamped: a period already past yields a negative duration, and the workflow
	// must not depend on the API having refused it.
	if untilPeriodEnd < 0 {
		untilPeriodEnd = 0
	}
	periodTimer := workflow.NewTimer(ctx, untilPeriodEnd)

	// A failure to close at period end cannot be swallowed in the callback. The
	// timer has already fired and there is no second one, so the Await below would
	// block forever and the bill would sit OPEN past its period, still taking
	// charges, never invoiced. Carried out and failed on instead.
	var periodEndCloseErr error

	selector := workflow.NewSelector(ctx)
	selector.AddFuture(periodTimer, func(workflow.Future) {
		// Guarded, because an early close may already have won this race.
		if b.State() == bill.StateOpen {
			if _, closeErr := beginClose(bill.TriggerPeriodEnd); closeErr != nil {
				logger.Error("period-end close failed", "billID", b.ID(), "error", closeErr)
				periodEndCloseErr = closeErr
			}
		}
	})
	selector.AddFuture(closeRequested, func(workflow.Future) {
		// The close handler already froze the totals; this only ends the wait.
	})
	selector.Select(ctx)

	if periodEndCloseErr != nil {
		return bill.Snapshot{}, temporal.NewNonRetryableApplicationError(
			periodEndCloseErr.Error(), ClassifyRejection(periodEndCloseErr), periodEndCloseErr)
	}

	// The selector woke us, but the bill is only genuinely closed once its state
	// says so.
	if err := workflow.Await(ctx, func() bool { return b.State() != bill.StateOpen }); err != nil {
		return bill.Snapshot{}, err
	}

	frozen := b.Snapshot()
	logger.Info("bill closed",
		"billID", b.ID(), "closedBy", frozen.ClosedBy,
		"total", frozen.Total.String(), "lineItems", len(frozen.LineItems))

	// Persist before emitting: if the hand-off keeps failing, the durable record
	// of what was charged still exists. The reverse ordering would let an invoice
	// reach the payee with no record of it here, the worse of the two failures.
	pctx := workflow.WithActivityOptions(ctx, persistOptions())
	if err := workflow.ExecuteActivity(pctx, ActivityPersistInvoice, frozen).Get(pctx, nil); err != nil {
		return bill.Snapshot{}, err
	}

	ectx := workflow.WithActivityOptions(ctx, emitOptions())
	if err := workflow.ExecuteActivity(ectx, ActivityEmitInvoice, frozen).Get(ectx, nil); err != nil {
		// The bill stays visibly in CLOSING with its frozen total, which is the
		// honest state: the totals are final, the hand-off is not done.
		return bill.Snapshot{}, err
	}

	if err := b.MarkInvoiced(); err != nil {
		return bill.Snapshot{}, err
	}

	// Until this lands, storage says CLOSING, which is true: CLOSED means the
	// hand-off completed.
	final := b.Snapshot()
	if err := workflow.ExecuteActivity(pctx, ActivityFinalizeInvoice, FinalizeInvoiceInput{
		BillID: b.ID(),
		State:  final.State,
	}).Get(pctx, nil); err != nil {
		return bill.Snapshot{}, err
	}

	return final, nil
}
