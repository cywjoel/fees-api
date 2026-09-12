package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"encore.dev"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
	"fees-api/internal/money"
)

// temporalCallTimeout bounds how long an API request waits on Temporal before
// answering the caller.
//
// Without it the SDK retries a transient failure against the request's own
// context, which has no deadline, so an outage leaves a mutation hanging rather
// than refusing it - the caller learns nothing and holds a connection open until
// it gives up. A bounded wait turns that into a 503 the caller can act on.
//
// Timing out is safe here precisely because every mutation is idempotent: an
// update that did apply before the deadline is recognised as already applied when
// the caller retries, so a premature 503 cannot double-charge a bill.
const temporalCallTimeout = 10 * time.Second

// temporalContext bounds a Temporal call to temporalCallTimeout, while still
// cancelling if the client disconnects first.
func temporalContext(req *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), temporalCallTimeout)
}

// CreateBillRequest opens a bill for a fee period.
type CreateBillRequest struct {
	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

// AddLineItemRequest accrues one charge. The item's identifier is the path
// segment, not a body field: it identifies the resource being created.
type AddLineItemRequest struct {
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
}

// CreateBill opens a new bill and starts the workflow that owns it.
//
// Creation is idempotent through the Idempotency-Key header, which is mapped
// deterministically onto the workflow id. Uniqueness then comes from Temporal
// itself rather than from a deduplication table: the same key can only ever name
// one workflow execution.
//
// Two policies make that work, and both are load-bearing. Without
// REJECT_DUPLICATE, the default reuse policy would let a repeated request start a
// brand new run under the same id once the first bill had closed - a second bill,
// created silently, with no error anywhere. USE_EXISTING returns the running
// execution instead of failing when the bill is still open.
//
//encore:api public raw method=POST path=/bills
func (s *Service) CreateBill(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()

	var in CreateBillRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeProblem(w, http.StatusBadRequest, billflow.ReasonInvalidBill,
			"request body is not valid JSON: "+err.Error())
		return
	}

	currency, err := money.Lookup(in.Currency)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonUnsupportedCurrency,
			"currency must be one of "+joinCodes(money.Supported()))
		return
	}
	if !in.PeriodEnd.After(in.PeriodStart) {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidPeriod,
			"periodEnd must be strictly after periodStart")
		return
	}

	billID, keyed := billIDFor(req.Header.Get("Idempotency-Key"))

	// A key that has already produced a bill returns that bill rather than a
	// second one. This covers the completed case too, where the workflow is gone
	// and the invoice is read from storage.
	if keyed {
		switch snap, readErr := s.readBill(ctx, billID); {
		case readErr == nil:
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusOK, snap)
			return
		case isWorkflowBusy(readErr):
			// Whether this key already made a bill is unknowable right now.
			// Starting one anyway risks a second bill for the same key, so the
			// honest answer is to ask the caller to retry.
			writeUnavailable(w, "could not determine whether this idempotency key already created a bill: "+readErr.Error())
			return
		}
	}

	_, err = s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       billID,
		TaskQueue:                billflow.TaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, billflow.BillWorkflow, billflow.StartBillInput{
		BillID:      billID,
		Currency:    currency.Code,
		PeriodStart: in.PeriodStart,
		PeriodEnd:   in.PeriodEnd,
	})
	if err != nil {
		// The key names a bill that already exists. Return it rather than a second.
		if isWorkflowAlreadyStarted(err) {
			if snap, readErr := s.readBill(ctx, billID); readErr == nil {
				w.Header().Set("Location", "/bills/"+billID)
				writeJSON(w, http.StatusOK, snap)
				return
			}
		}
		if isWorkflowBusy(err) {
			writeUnavailable(w, "the bill could not be started: "+err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"starting bill workflow: "+err.Error())
		return
	}

	snap, err := s.readBill(ctx, billID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"reading the new bill: "+err.Error())
		return
	}

	w.Header().Set("Location", "/bills/"+billID)
	writeJSON(w, http.StatusCreated, snap)
}

// AddLineItem accrues a charge onto an open bill.
//
// PUT rather than POST, because the caller supplies the item's identifier and the
// operation is idempotent - which is what PUT means in RFC 9110. The identifier
// is not a retry token bolted onto the request: a fee is caused by something that
// already has an identity, so keying on it expresses a real invariant, that the
// fee for a given source event appears at most once on this bill.
//
// The item id is deliberately *not* used as the Temporal update id, despite the
// temptation: Temporal deduplicates by update id before the validator runs, so a
// second request reusing an item id with a different amount would be answered
// from the first result and the conflict would never be detected. The bill would
// be correct - the stored item is never overwritten - but the caller would be
// told its charge was accepted when a contradictory one was silently ignored.
//
// Deduplication therefore belongs to the domain, which distinguishes an identical
// retry from a conflicting reuse. Updates are delivered at least once and the
// handler is idempotent, so nothing is lost by letting each request through.
//
//encore:api public raw method=PUT path=/bills/:billId/line-items/:itemId
func (s *Service) AddLineItem(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()
	billID := pathParam("billId")
	itemID := pathParam("itemId")

	var in AddLineItemRequest
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidLineItem,
			"request body is not a valid line item: "+err.Error())
		return
	}

	handle, err := s.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   billID,
		UpdateName:   billflow.UpdateAddLineItem,
		WaitForStage: client.WorkflowUpdateStageCompleted,
		Args: []any{billflow.AddLineItemInput{
			ItemID:      itemID,
			Amount:      in.Amount,
			Description: in.Description,
		}},
	})
	if err != nil {
		s.writeAddFailure(w, ctx, billID, itemID, in, err)
		return
	}

	var result bill.AddResult
	if err := handle.Get(ctx, &result); err != nil {
		s.writeAddFailure(w, ctx, billID, itemID, in, err)
		return
	}

	location := "/bills/" + billID + "/line-items/" + itemID
	w.Header().Set("Location", location)

	// 201 when the charge was newly accrued, 200 when this was a retry of one the
	// bill already carries.
	status := http.StatusOK
	if result.Outcome == bill.OutcomeCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// CloseBill freezes a bill's totals and starts the invoice hand-off.
//
// The response is 202, not 200: the totals are final, but the bill is not. It
// carries the frozen total and every line item charged, so the caller gets the
// invoice in the same round trip that requests it.
//
// Closing a bill that is already closing or closed is not an error. The caller
// wanted the bill closed and it is; the response reports closedBy, so a request
// that lost the race against the period-end deadline can tell that the deadline
// closed it rather than being handed a failure for a state it asked for.
//
//encore:api public raw method=POST path=/bills/:billId/close
func (s *Service) CloseBill(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := temporalContext(req)
	defer cancel()
	billID := pathParam("billId")

	handle, err := s.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   billID,
		UpdateName:   billflow.UpdateCloseBill,
		WaitForStage: client.WorkflowUpdateStageCompleted,
		Args:         []any{billflow.CloseBillInput{}},
	})
	if err == nil {
		var snap bill.Snapshot
		if err := handle.Get(ctx, &snap); err != nil {
			s.writeUpdateFailure(w, billID, err)
			return
		}
		w.Header().Set("Location", "/bills/"+billID)
		writeJSON(w, http.StatusAccepted, snap)
		return
	}

	// The workflow has already finished, so the bill is closed and its invoice
	// lives in storage. That still satisfies the request.
	if isWorkflowBusy(err) {
		writeUnavailable(w, "the bill could not be reached to close it: "+err.Error())
		return
	}
	if isWorkflowAbsent(err) {
		snap, readErr := loadInvoice(ctx, billID)
		switch {
		case readErr == nil:
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusAccepted, snap)
		case errors.Is(readErr, ErrInvoiceNotFound):
			// Neither source has the bill, so it really is absent.
			writeNotFound(w, billID)
		default:
			// Storage failed. Falling through here reported a database outage as a
			// missing bill, which reads as "this never existed" and pages nobody.
			writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
				"reading the invoice for bill "+billID+": "+readErr.Error())
		}
		return
	}
	s.writeUpdateFailure(w, billID, err)
}

// GetBill returns a bill in any state.
//
// While the bill is open its live state comes from the workflow, which is the
// authoritative writer and answers with strong consistency. Once the workflow has
// finished, the invoice is read from storage - which is the point of persisting
// it, since workflow history is retained for a limited window and a financial
// artifact must outlive the process that produced it.
//
//encore:api public raw method=GET path=/bills/:billId
func (s *Service) GetBill(w http.ResponseWriter, req *http.Request) {
	billID := pathParam("billId")

	ctx, cancel := temporalContext(req)
	defer cancel()

	snap, err := s.readBill(ctx, billID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvoiceNotFound):
			// Both sources were asked and neither had the bill. This is the only
			// circumstance in which "not found" is true.
			writeNotFound(w, billID)
		case isWorkflowBusy(err):
			writeUnavailable(w, "the bill's state could not be read: "+err.Error())
		default:
			writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// readBill returns a bill from whichever source can answer for it: the running
// workflow first, then durable storage.
func (s *Service) readBill(ctx context.Context, billID string) (bill.Snapshot, error) {
	ev, err := s.temporal.QueryWorkflow(ctx, billID, "", billflow.QueryGetBill)
	switch {
	case err == nil:
		var snap bill.Snapshot
		if decodeErr := ev.Get(&snap); decodeErr != nil {
			// Previously discarded, which turned a malformed answer into a missing
			// bill. A workflow that answers unintelligibly is a fault, not an absence.
			return bill.Snapshot{}, fmt.Errorf("billing: decoding bill %q from its workflow: %w", billID, decodeErr)
		}
		return snap, nil

	case isWorkflowAbsent(err):
		// The only condition under which storage is the right place to look: there
		// is no live execution to ask, so the persisted invoice is all there is.
		return loadInvoice(ctx, billID)

	default:
		// Everything else - unreachable, busy, overloaded, or an outright fault -
		// propagates. Falling back here is what made a Temporal outage look like
		// every open bill having been deleted.
		return bill.Snapshot{}, fmt.Errorf("billing: querying bill %q: %w", billID, err)
	}
}

// writeUpdateFailure reports an update that did not apply.
func (s *Service) writeUpdateFailure(w http.ResponseWriter, billID string, err error) {
	switch {
	case isWorkflowBusy(err):
		writeUnavailable(w, "the bill could not be reached: "+err.Error())
	case isWorkflowAbsent(err):
		writeNotFound(w, billID)
	default:
		writeRejection(w, err)
	}
}

// writeAddFailure answers a charge the workflow could not take.
//
// A bill's workflow finishes within seconds of the bill closing, and stays
// finished for the rest of the bill's life - so "no live execution" is the
// ordinary condition of a closed bill, not an exotic one. Treating it as a
// missing bill answered 404 for a charge on a bill that GET returns 200 for, and
// a caller told its charge was refused as unknown may compensate for money that
// is genuinely on the invoice.
//
// So the persisted invoice is consulted, exactly as CloseBill already does, and
// the answer comes from the same domain rule the live path applies.
func (s *Service) writeAddFailure(w http.ResponseWriter, ctx context.Context, billID, itemID string, in AddLineItemRequest, err error) {
	if !isWorkflowAbsent(err) {
		s.writeUpdateFailure(w, billID, err)
		return
	}

	snap, loadErr := loadInvoice(ctx, billID)
	if loadErr != nil {
		if errors.Is(loadErr, ErrInvoiceNotFound) {
			// Neither the workflow nor storage has this bill. Now 404 is true.
			writeNotFound(w, billID)
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"reading the invoice for bill "+billID+": "+loadErr.Error())
		return
	}

	existing, checkErr := bill.CheckAgainstSnapshot(snap, bill.LineItem{
		ID:          itemID,
		Amount:      in.Amount,
		Description: in.Description,
	})
	if checkErr != nil {
		writeProblem(w, statusForDomainError(checkErr), billflow.ClassifyRejection(checkErr), checkErr.Error())
		return
	}

	// The charge is already on the invoice, so the retry succeeded the first time.
	w.Header().Set("Location", "/bills/"+billID+"/line-items/"+itemID)
	writeJSON(w, http.StatusOK, bill.AddResult{
		Outcome:      bill.OutcomeAlreadyAccrued,
		LineItem:     *existing,
		RunningTotal: snap.Total,
	})
}

// statusForDomainError maps a domain error to its status using the same table
// the workflow's rejections go through, so a charge refused from storage and the
// same charge refused by the workflow answer identically.
func statusForDomainError(err error) int {
	if status, ok := reasonStatus[billflow.ClassifyRejection(err)]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// pathParam reads a path parameter from the in-flight Encore request.
func pathParam(name string) string {
	if req := encore.CurrentRequest(); req != nil {
		return req.PathParams.Get(name)
	}
	return ""
}

// billIDFor derives a bill id from an idempotency key, or generates one when no
// key was supplied. The second return reports whether the id came from a key and
// is therefore reproducible by a retry.
//
// The key is hashed rather than used directly so that an arbitrary client string
// cannot become a workflow id with whatever characters and length it happens to
// carry.
func billIDFor(idempotencyKey string) (string, bool) {
	if idempotencyKey == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return "bill_" + hex.EncodeToString(b[:]), false
	}
	sum := sha256.Sum256([]byte(idempotencyKey))
	return "bill_" + hex.EncodeToString(sum[:])[:32], true
}

// joinCodes renders supported currency codes for an error message.
func joinCodes(codes []string) string {
	out := ""
	for i, c := range codes {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
