package billflow_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
	"fees-api/internal/money"
)

var (
	usd = money.MustLookup("USD")
	gel = money.MustLookup("GEL")

	// A fee period of a calendar month. Every test below runs it to completion in
	// milliseconds: the test environment skips time rather than waiting for it.
	startTime   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = startTime.AddDate(0, 1, 0)
	periodLen   = periodEnd.Sub(startTime)
	defaultBill = billflow.StartBillInput{
		BillID:      "bill_test",
		Currency:    "USD",
		PeriodStart: startTime,
		PeriodEnd:   periodEnd,
	}
)

// updateResult captures how an update was resolved. The distinction between
// Reject and Complete-with-error matters: a rejected update never enters
// workflow history, which is where business refusals belong.
type updateResult struct {
	accepted    bool
	rejectErr   error
	value       interface{}
	completeErr error
}

func (u *updateResult) Accept()          { u.accepted = true }
func (u *updateResult) Reject(err error) { u.rejectErr = err }
func (u *updateResult) Complete(success interface{}, err error) {
	u.value = success
	u.completeErr = err
}

func (u *updateResult) rejected() bool { return u.rejectErr != nil }

// decode unpacks an update's return value into out.
func (u *updateResult) decode(t *testing.T, out interface{}) {
	t.Helper()
	if u.completeErr != nil {
		t.Fatalf("update completed with error: %v", u.completeErr)
	}
	if ev, ok := u.value.(converter.EncodedValue); ok {
		if err := ev.Get(out); err != nil {
			t.Fatalf("decoding update value: %v", err)
		}
		return
	}
	// The test environment delivers the handler's return value directly rather
	// than as an encoded payload.
	dst := reflect.ValueOf(out)
	if dst.Kind() != reflect.Ptr {
		t.Fatalf("decode target is %T, want a pointer", out)
	}
	src := reflect.ValueOf(u.value)
	if !src.IsValid() || !src.Type().AssignableTo(dst.Elem().Type()) {
		t.Fatalf("update value is %T, want %s", u.value, dst.Elem().Type())
	}
	dst.Elem().Set(src)
}

// env builds a test environment with the invoice activities stubbed out. Both
// are out of this service's scope; what matters to the workflow is that they run
// in the right order and that failure is retried.
type harness struct {
	*testsuite.TestWorkflowEnvironment
	calls    *[]string
	updateNo int
}

// nextUpdateID returns a fresh update id.
//
// Update ids matter more than they look. Temporal deduplicates by update id
// itself, so reusing one causes the server to answer from the first result
// without re-running the validator or handler. In production that is a welcome
// second layer of retry safety - the API sets the update id from the line item
// id, so a retry is absorbed before it reaches the workflow. In these tests it
// would mask the behaviour under test, so each update gets its own id and the
// domain's own deduplication is what gets exercised.
func (h *harness) nextUpdateID(prefix string) string {
	h.updateNo++
	return fmt.Sprintf("%s_%d", prefix, h.updateNo)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(startTime)

	calls := &[]string{}
	record := func(name string) func(context.Context, interface{}) error {
		return func(context.Context, interface{}) error {
			*calls = append(*calls, name)
			return nil
		}
	}

	env.RegisterActivityWithOptions(record(billflow.ActivityPersistInvoice),
		activity.RegisterOptions{Name: billflow.ActivityPersistInvoice})
	env.RegisterActivityWithOptions(record(billflow.ActivityEmitInvoice),
		activity.RegisterOptions{Name: billflow.ActivityEmitInvoice})
	env.RegisterActivityWithOptions(record(billflow.ActivityFinalizeInvoice),
		activity.RegisterOptions{Name: billflow.ActivityFinalizeInvoice})

	t.Cleanup(func() { env.AssertExpectations(t) })
	return &harness{TestWorkflowEnvironment: env, calls: calls}
}

func addItem(id string, minorUnits int64, c money.Currency, desc string) billflow.AddLineItemInput {
	return billflow.AddLineItemInput{
		ItemID:      id,
		Amount:      money.New(minorUnits, c),
		Description: desc,
	}
}

// scheduleAdd sends an AddLineItem update after d of workflow time.
func (h *harness) scheduleAdd(d time.Duration, in billflow.AddLineItemInput) *updateResult {
	res := &updateResult{}
	h.RegisterDelayedCallback(func() {
		h.UpdateWorkflow(billflow.UpdateAddLineItem, h.nextUpdateID("add"), res, in)
	}, d)
	return res
}

// scheduleClose sends a CloseBill update after d of workflow time.
func (h *harness) scheduleClose(d time.Duration, updateID string) *updateResult {
	res := &updateResult{}
	h.RegisterDelayedCallback(func() {
		h.UpdateWorkflow(billflow.UpdateCloseBill, updateID, res, billflow.CloseBillInput{})
	}, d)
	return res
}

func (h *harness) result(t *testing.T) bill.Snapshot {
	t.Helper()
	if !h.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := h.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned error: %v", err)
	}
	var snap bill.Snapshot
	if err := h.GetWorkflowResult(&snap); err != nil {
		t.Fatalf("decoding workflow result: %v", err)
	}
	return snap
}

// --- 8.1 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Bill is created successfully".
//
// Also the harness check for every test below: a one-month fee period runs to
// completion in a few milliseconds of real time because the test environment
// skips timers rather than waiting on them.
func TestFeePeriodOfOneMonthRunsInMilliseconds(t *testing.T) {
	h := newHarness(t)

	realStart := time.Now()
	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)
	elapsed := time.Since(realStart)

	snap := h.result(t)
	if snap.State != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED", snap.State)
	}
	if elapsed > time.Second {
		t.Errorf("a %s fee period took %s of real time, want under 1s", periodLen, elapsed)
	}
	t.Logf("fee period of %s executed in %s of real time", periodLen, elapsed)
}

func TestNewBillQueriesAsOpenAndEmpty(t *testing.T) {
	h := newHarness(t)

	h.RegisterDelayedCallback(func() {
		ev, err := h.QueryWorkflow(billflow.QueryGetBill)
		if err != nil {
			t.Errorf("QueryWorkflow returned error: %v", err)
			return
		}
		var snap bill.Snapshot
		if err := ev.Get(&snap); err != nil {
			t.Errorf("decoding query: %v", err)
			return
		}
		if snap.State != bill.StateOpen {
			t.Errorf("state = %s, want OPEN", snap.State)
		}
		if !snap.Total.IsZero() {
			t.Errorf("total = %s, want zero", snap.Total)
		}
		if got := len(snap.LineItems); got != 0 {
			t.Errorf("line items = %d, want 0", got)
		}
		if got := snap.Currency.Code; got != "USD" {
			t.Errorf("currency = %q, want USD", got)
		}
	}, time.Hour)

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)
	h.result(t)
}

// --- 8.2 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Open bill is closed on request" and "Close
// response carries the full invoice".
func TestAddThreeItemsThenCloseEarly(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(1*time.Hour, addItem("txn_1", 1000, usd, "card fee"))
	h.scheduleAdd(2*time.Hour, addItem("txn_2", 200, usd, "transfer fee"))
	h.scheduleAdd(3*time.Hour, addItem("txn_3", 55, usd, "fx fee"))
	closed := h.scheduleClose(4*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	var snap bill.Snapshot
	closed.decode(t, &snap)

	if snap.State != bill.StateClosing {
		t.Errorf("close returned state %s, want CLOSING", snap.State)
	}
	if snap.ClosedBy != bill.TriggerAPIRequest {
		t.Errorf("closedBy = %s, want api_request", snap.ClosedBy)
	}
	if got, want := snap.Total.MinorUnits(), int64(1255); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
	if got, want := len(snap.LineItems), 3; got != want {
		t.Fatalf("line items = %d, want %d", got, want)
	}
	for i, want := range []string{"txn_1", "txn_2", "txn_3"} {
		if snap.LineItems[i].ID != want {
			t.Errorf("line item %d = %q, want %q", i, snap.LineItems[i].ID, want)
		}
	}

	final := h.result(t)
	if final.State != bill.StateClosed {
		t.Errorf("final state = %s, want CLOSED", final.State)
	}
	if !final.Total.Equal(snap.Total) {
		t.Errorf("final total %s differs from the total frozen at close %s", final.Total, snap.Total)
	}
}

// Spec: billing/line-item-accrual - "Running total accompanies each addition".
func TestAddLineItemReturnsRunningTotal(t *testing.T) {
	h := newHarness(t)

	first := h.scheduleAdd(1*time.Hour, addItem("txn_1", 1255, usd, "card fee"))
	second := h.scheduleAdd(2*time.Hour, addItem("txn_2", 500, usd, "transfer fee"))
	h.scheduleClose(3*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	var r1, r2 bill.AddResult
	first.decode(t, &r1)
	second.decode(t, &r2)

	if r1.Outcome != bill.OutcomeCreated {
		t.Errorf("first outcome = %s, want created", r1.Outcome)
	}
	if got, want := r1.RunningTotal.MinorUnits(), int64(1255); got != want {
		t.Errorf("running total after first = %d, want %d", got, want)
	}
	if got, want := r2.RunningTotal.MinorUnits(), int64(1755); got != want {
		t.Errorf("running total after second = %d, want %d", got, want)
	}
}

// --- 8.3 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Bill closes automatically at period end".
func TestBillClosesOnPeriodEndWithoutARequest(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(1*time.Hour, addItem("txn_1", 1000, usd, "card fee"))
	h.scheduleAdd(10*24*time.Hour, addItem("txn_2", 255, usd, "transfer fee"))

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	snap := h.result(t)
	if snap.ClosedBy != bill.TriggerPeriodEnd {
		t.Errorf("closedBy = %s, want period_end", snap.ClosedBy)
	}
	if snap.State != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED", snap.State)
	}
	if got, want := snap.Total.MinorUnits(), int64(1255); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
	if snap.ClosedAt == nil || !snap.ClosedAt.Equal(periodEnd) {
		t.Errorf("closedAt = %v, want the period end %s", snap.ClosedAt, periodEnd)
	}
}

// --- 8.4 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Concurrent close triggers resolve
// deterministically".
//
// This does not assert that the race is handled; it demonstrates the outcome
// flipping on which trigger arrives first, one nanosecond either side of the
// deadline. Both triggers mutate the same variable on the same workflow thread,
// so whichever reaches it first wins and the other is a no-op.
func TestCloseRaceAgainstPeriodEndResolvesDeterministically(t *testing.T) {
	tests := []struct {
		name        string
		closeAt     time.Duration
		wantTrigger bill.Trigger
	}{
		{
			name:        "request one nanosecond before the deadline wins",
			closeAt:     periodLen - time.Nanosecond,
			wantTrigger: bill.TriggerAPIRequest,
		},
		{
			name:        "deadline wins when the request is one nanosecond late",
			closeAt:     periodLen + time.Nanosecond,
			wantTrigger: bill.TriggerPeriodEnd,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.scheduleAdd(time.Hour, addItem("txn_1", 500, usd, "card fee"))
			h.scheduleClose(tc.closeAt, "close_1")

			h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

			snap := h.result(t)
			if snap.ClosedBy != tc.wantTrigger {
				t.Errorf("closedBy = %s, want %s", snap.ClosedBy, tc.wantTrigger)
			}
			// Whoever won, the bill closed exactly once and its total is intact.
			if got, want := snap.Total.MinorUnits(), int64(500); got != want {
				t.Errorf("total = %d minor units, want %d", got, want)
			}
			if snap.State != bill.StateClosed {
				t.Errorf("state = %s, want CLOSED", snap.State)
			}
		})
	}
}

// Spec: billing/bill-lifecycle - "Repeated close request" and "Close request
// against a bill already closed by the deadline".
func TestRepeatedCloseIsNotAnErrorAndReportsTheOriginalTrigger(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(time.Hour, addItem("txn_1", 500, usd, "card fee"))

	// Both requests are issued in the same callback. A close ends the wait and
	// the main loop proceeds straight to the activities, so a request scheduled
	// for a later instant would arrive after the workflow had already completed.
	first, second := &updateResult{}, &updateResult{}
	h.RegisterDelayedCallback(func() {
		h.UpdateWorkflow(billflow.UpdateCloseBill, "close_1", first, billflow.CloseBillInput{})
		h.UpdateWorkflow(billflow.UpdateCloseBill, "close_2", second, billflow.CloseBillInput{})
	}, 2*time.Hour)

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	var s1, s2 bill.Snapshot
	first.decode(t, &s1)
	second.decode(t, &s2)

	if second.rejected() {
		t.Fatalf("second close was rejected: %v", second.rejectErr)
	}
	if s2.ClosedBy != bill.TriggerAPIRequest {
		t.Errorf("closedBy = %s, want api_request", s2.ClosedBy)
	}
	if !s2.Total.Equal(s1.Total) {
		t.Errorf("second close total %s differs from the first %s", s2.Total, s1.Total)
	}
	if s1.ClosedAt == nil || s2.ClosedAt == nil || !s2.ClosedAt.Equal(*s1.ClosedAt) {
		t.Errorf("closedAt changed between closes: %v then %v", s1.ClosedAt, s2.ClosedAt)
	}
}

// --- 8.5 -------------------------------------------------------------------

// Spec: billing/line-item-accrual - "Addition to a closed bill is rejected".
//
// The assertion is specifically that the update was *rejected*, not that it
// completed with an error. A rejected update never enters workflow history; a
// failed one does. Rejecting in the validator is both the correct semantics and
// the cheaper path.
func TestAddAfterCloseIsRejectedByTheValidator(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(time.Hour, addItem("txn_1", 500, usd, "card fee"))

	late := &updateResult{}
	h.RegisterDelayedCallback(func() {
		h.UpdateWorkflow(billflow.UpdateCloseBill, "close_1", &updateResult{}, billflow.CloseBillInput{})
		// Offered immediately after the close, while the workflow still runs.
		h.UpdateWorkflow(billflow.UpdateAddLineItem, "add_late", late, addItem("txn_2", 700, usd, "late fee"))
	}, 2*time.Hour)

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !late.rejected() {
		t.Fatalf("late addition was not rejected (accepted=%v, completeErr=%v)", late.accepted, late.completeErr)
	}
	if late.accepted {
		t.Error("late addition was accepted before being rejected; the validator should refuse it outright")
	}

	snap := h.result(t)
	if got, want := snap.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d: a rejected addition must not change a frozen total", got, want)
	}
	if got, want := len(snap.LineItems), 1; got != want {
		t.Errorf("line items = %d, want %d", got, want)
	}
}

// Spec: billing/line-item-accrual - "Addition racing a close does not join a
// frozen total".
func TestAdditionRacingACloseDoesNotJoinTheFrozenTotal(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(time.Hour, addItem("txn_1", 500, usd, "card fee"))

	closed, racing := &updateResult{}, &updateResult{}
	h.RegisterDelayedCallback(func() {
		h.UpdateWorkflow(billflow.UpdateCloseBill, "close_1", closed, billflow.CloseBillInput{})
		h.UpdateWorkflow(billflow.UpdateAddLineItem, "add_racing", racing, addItem("txn_2", 700, usd, "just too late"))
	}, 2*time.Hour)

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !racing.rejected() {
		t.Errorf("addition arriving just after the close was not rejected")
	}

	var atClose bill.Snapshot
	closed.decode(t, &atClose)
	final := h.result(t)

	if !final.Total.Equal(atClose.Total) {
		t.Errorf("total reported at closure %s differs from the final total %s", atClose.Total, final.Total)
	}
	if got, want := final.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
}

// --- 8.6 -------------------------------------------------------------------

// Spec: billing/line-item-accrual - "Retried addition does not duplicate a
// charge".
func TestDuplicateItemIDDoesNotDoubleCharge(t *testing.T) {
	h := newHarness(t)

	in := addItem("txn_1", 500, usd, "card fee")
	first := h.scheduleAdd(1*time.Hour, in)
	retry := h.scheduleAdd(2*time.Hour, in)
	h.scheduleClose(3*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	var r1, r2 bill.AddResult
	first.decode(t, &r1)
	retry.decode(t, &r2)

	if r1.Outcome != bill.OutcomeCreated {
		t.Errorf("first outcome = %s, want created", r1.Outcome)
	}
	if r2.Outcome != bill.OutcomeAlreadyAccrued {
		t.Errorf("retry outcome = %s, want already_accrued", r2.Outcome)
	}
	if r2.LineItem.ID != in.ItemID {
		t.Errorf("retry returned item %q, want %q", r2.LineItem.ID, in.ItemID)
	}

	snap := h.result(t)
	if got, want := len(snap.LineItems), 1; got != want {
		t.Errorf("line items = %d, want %d", got, want)
	}
	if got, want := snap.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d: a retry must not double-charge", got, want)
	}
}

// Spec: billing/line-item-accrual - "Same identifier with a different amount is
// rejected".
func TestSameItemIDWithDifferentAmountIsRejected(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(1*time.Hour, addItem("txn_1", 500, usd, "card fee"))
	conflict := h.scheduleAdd(2*time.Hour, addItem("txn_1", 999, usd, "card fee"))
	h.scheduleClose(3*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !conflict.rejected() {
		t.Fatalf("conflicting addition was not rejected")
	}

	snap := h.result(t)
	if got, want := snap.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
	if got, want := snap.LineItems[0].Amount.MinorUnits(), int64(500); got != want {
		t.Errorf("stored amount = %d, want %d: the stored item must not be overwritten", got, want)
	}
}

// --- 8.7 -------------------------------------------------------------------

// Spec: billing/money-representation - "Line item in a different currency is
// rejected".
func TestGELItemOnUSDBillIsRejected(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(1*time.Hour, addItem("txn_1", 500, usd, "card fee"))
	wrong := h.scheduleAdd(2*time.Hour, addItem("txn_2", 700, gel, "lari fee"))
	h.scheduleClose(3*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !wrong.rejected() {
		t.Fatalf("GEL item on a USD bill was not rejected")
	}
	if !errors.Is(wrong.rejectErr, money.ErrCurrencyMismatch) &&
		!strings.Contains(wrong.rejectErr.Error(), "currency mismatch") {
		t.Errorf("rejection reason = %v, want a currency mismatch", wrong.rejectErr)
	}

	snap := h.result(t)
	if got, want := snap.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d", got, want)
	}
}

// --- 8.8 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Bill remains in CLOSING while the invoice
// hand-off is retried".
//
// This is what the CLOSING state is for, and the scenario that is genuinely
// painful to build without a durable execution engine: the totals are frozen,
// the hand-off is failing, and the bill must neither lose the close nor claim to
// be finished.
func TestInvoiceHandOffIsRetriedUntilItSucceeds(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(startTime)

	env.RegisterActivityWithOptions(
		func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityPersistInvoice})
	env.RegisterActivityWithOptions(
		func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityEmitInvoice})
	env.RegisterActivityWithOptions(
		func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityFinalizeInvoice})

	env.OnActivity(billflow.ActivityPersistInvoice, mock.Anything, mock.Anything).
		Return(nil).Once()
	env.OnActivity(billflow.ActivityEmitInvoice, mock.Anything, mock.Anything).
		Return(errors.New("payee gateway unavailable")).Twice()
	env.OnActivity(billflow.ActivityEmitInvoice, mock.Anything, mock.Anything).
		Return(nil).Once()
	env.OnActivity(billflow.ActivityFinalizeInvoice, mock.Anything, mock.Anything).
		Return(nil).Once()

	addRes := &updateResult{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(billflow.UpdateAddLineItem, "txn_1", addRes, addItem("txn_1", 500, usd, "card fee"))
	}, time.Hour)
	closeRes := &updateResult{}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(billflow.UpdateCloseBill, "close_1", closeRes, billflow.CloseBillInput{})
	}, 2*time.Hour)

	env.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete despite the hand-off eventually succeeding")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned error: %v", err)
	}

	var snap bill.Snapshot
	if err := env.GetWorkflowResult(&snap); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if snap.State != bill.StateClosed {
		t.Errorf("state = %s, want CLOSED after the retries succeeded", snap.State)
	}
	if got, want := snap.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total = %d minor units, want %d: retries must not alter the frozen total", got, want)
	}

	// While the hand-off was failing the bill was CLOSING, with its total frozen.
	var atClose bill.Snapshot
	closeRes.decode(t, &atClose)
	if atClose.State != bill.StateClosing {
		t.Errorf("state at close = %s, want CLOSING", atClose.State)
	}

	env.AssertExpectations(t)
}

// --- 4.7 -------------------------------------------------------------------

// The durable record is written before the invoice is handed off. If the
// hand-off keeps failing there is still a record of what was charged; the
// reverse ordering would let an invoice reach the payee with nothing recorded
// here, which is the worse of the two failures.
func TestInvoiceIsPersistedBeforeItIsEmitted(t *testing.T) {
	h := newHarness(t)

	h.scheduleAdd(time.Hour, addItem("txn_1", 500, usd, "card fee"))
	h.scheduleClose(2*time.Hour, "close_1")

	h.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)
	h.result(t)

	want := []string{
		billflow.ActivityPersistInvoice,
		billflow.ActivityEmitInvoice,
		billflow.ActivityFinalizeInvoice,
	}
	got := *h.calls
	if len(got) != len(want) {
		t.Fatalf("activity calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("activity call %d = %q, want %q (order: %v)", i, got[i], want[i], got)
		}
	}
}

// --- input validation ------------------------------------------------------

func TestWorkflowRejectsUnsupportedCurrency(t *testing.T) {
	h := newHarness(t)

	in := defaultBill
	in.Currency = "EUR"
	h.ExecuteWorkflow(billflow.BillWorkflow, in)

	if !h.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if h.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded with an unsupported currency, want an error")
	}
}

func TestWorkflowRejectsInvalidPeriod(t *testing.T) {
	h := newHarness(t)

	in := defaultBill
	in.PeriodEnd = in.PeriodStart
	h.ExecuteWorkflow(billflow.BillWorkflow, in)

	if h.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded with a period that does not end after it begins, want an error")
	}
}

// --- 4.6 -------------------------------------------------------------------

// Spec: billing/bill-lifecycle - "Retrieve a bill by identifier".
//
// The query must answer in every state, including the window while the invoice
// hand-off is being retried. That window is the whole reason CLOSING is a state
// rather than an instant: the totals are final but the bill is not finished, and
// a caller reading the bill then should be told exactly that.
func TestQueryAnswersInOpenClosingAndClosed(t *testing.T) {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(startTime)

	env.RegisterActivityWithOptions(func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityPersistInvoice})
	env.RegisterActivityWithOptions(func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityEmitInvoice})
	env.RegisterActivityWithOptions(func(context.Context, interface{}) error { return nil },
		activity.RegisterOptions{Name: billflow.ActivityFinalizeInvoice})

	env.OnActivity(billflow.ActivityPersistInvoice, mock.Anything, mock.Anything).Return(nil)
	// Fail the hand-off for long enough that CLOSING is observable.
	env.OnActivity(billflow.ActivityEmitInvoice, mock.Anything, mock.Anything).
		Return(errors.New("payee gateway unavailable")).Times(3)
	env.OnActivity(billflow.ActivityEmitInvoice, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(billflow.ActivityFinalizeInvoice, mock.Anything, mock.Anything).Return(nil)

	query := func(t *testing.T) bill.Snapshot {
		t.Helper()
		ev, err := env.QueryWorkflow(billflow.QueryGetBill)
		if err != nil {
			t.Fatalf("QueryWorkflow returned error: %v", err)
		}
		var snap bill.Snapshot
		if err := ev.Get(&snap); err != nil {
			t.Fatalf("decoding query: %v", err)
		}
		return snap
	}

	var whileOpen, whileClosing bill.Snapshot

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(billflow.UpdateAddLineItem, "add_1", &updateResult{},
			addItem("txn_1", 500, usd, "card fee"))
	}, time.Hour)

	env.RegisterDelayedCallback(func() { whileOpen = query(t) }, 90*time.Minute)

	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow(billflow.UpdateCloseBill, "close_1", &updateResult{}, billflow.CloseBillInput{})
	}, 2*time.Hour)

	// Lands between hand-off attempts, while the bill sits in CLOSING.
	env.RegisterDelayedCallback(func() { whileClosing = query(t) }, 2*time.Hour+1500*time.Millisecond)

	env.ExecuteWorkflow(billflow.BillWorkflow, defaultBill)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned error: %v", err)
	}

	if whileOpen.State != bill.StateOpen {
		t.Errorf("state while open = %s, want OPEN", whileOpen.State)
	}
	if got, want := whileOpen.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("running total while open = %d, want %d", got, want)
	}
	if whileOpen.ClosedAt != nil {
		t.Errorf("closedAt while open = %v, want nil", whileOpen.ClosedAt)
	}

	if whileClosing.State != bill.StateClosing {
		t.Errorf("state during the hand-off = %s, want CLOSING", whileClosing.State)
	}
	if got, want := whileClosing.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total while closing = %d, want the frozen %d", got, want)
	}
	if whileClosing.ClosedBy != bill.TriggerAPIRequest {
		t.Errorf("closedBy while closing = %s, want api_request", whileClosing.ClosedBy)
	}

	whenClosed := query(t)
	if whenClosed.State != bill.StateClosed {
		t.Errorf("state after completion = %s, want CLOSED", whenClosed.State)
	}
	if got, want := whenClosed.Total.MinorUnits(), int64(500); got != want {
		t.Errorf("total when closed = %d, want %d", got, want)
	}
	if got, want := len(whenClosed.LineItems), 1; got != want {
		t.Errorf("line items when closed = %d, want %d", got, want)
	}
}
