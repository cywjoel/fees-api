package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"encore.dev"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"fees-api/internal/bill"
	"fees-api/internal/billflow"
	"fees-api/internal/money"
)

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
	ctx := req.Context()

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
		if snap, err := s.readBill(ctx, billID); err == nil {
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusOK, snap)
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
		// The key names a bill that already exists and has finished. Return it.
		if isWorkflowGone(err) {
			if snap, readErr := s.readBill(ctx, billID); readErr == nil {
				w.Header().Set("Location", "/bills/"+billID)
				writeJSON(w, http.StatusOK, snap)
				return
			}
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
	ctx := req.Context()
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
		s.writeUpdateFailure(w, billID, err)
		return
	}

	var result bill.AddResult
	if err := handle.Get(ctx, &result); err != nil {
		s.writeUpdateFailure(w, billID, err)
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
	ctx := req.Context()
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
	if isWorkflowGone(err) {
		snap, readErr := loadInvoice(ctx, billID)
		if readErr == nil {
			w.Header().Set("Location", "/bills/"+billID)
			writeJSON(w, http.StatusAccepted, snap)
			return
		}
		if errors.Is(readErr, ErrInvoiceNotFound) {
			writeNotFound(w, billID)
			return
		}
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
	snap, err := s.readBill(req.Context(), pathParam("billId"))
	if err != nil {
		if errors.Is(err, ErrInvoiceNotFound) {
			writeNotFound(w, pathParam("billId"))
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// readBill returns a bill from whichever source can answer for it: the running
// workflow first, then durable storage.
func (s *Service) readBill(ctx context.Context, billID string) (bill.Snapshot, error) {
	ev, err := s.temporal.QueryWorkflow(ctx, billID, "", billflow.QueryGetBill)
	if err == nil {
		var snap bill.Snapshot
		if decodeErr := ev.Get(&snap); decodeErr == nil {
			return snap, nil
		}
	}
	return loadInvoice(ctx, billID)
}

// writeUpdateFailure reports an update that did not apply. A workflow that no
// longer exists is a missing bill, not an internal fault.
func (s *Service) writeUpdateFailure(w http.ResponseWriter, billID string, err error) {
	if isWorkflowGone(err) {
		writeNotFound(w, billID)
		return
	}
	writeRejection(w, err)
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
