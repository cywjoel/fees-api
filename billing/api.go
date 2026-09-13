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
	"strconv"
	"time"

	"encore.dev"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"
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
	CustomerID  string    `json:"customerId"`
	Currency    string    `json:"currency"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

// AddLineItemRequest accrues one charge. The item's identifier is the path
// segment, not a body field: it identifies the resource being created.
type AddLineItemRequest struct {
	// CustomerID is optional. When present it must be the bill's customer.
	//
	// A checksum rather than an identity claim: the caller already chose the bill
	// by its id, so this states what the caller believes and lets the system
	// disagree. Optional because requiring it would force every caller to carry
	// the customer alongside the bill id for a guarantee only some of them need.
	CustomerID  string      `json:"customerId,omitempty"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description"`
}

// matchesRequest reports whether an existing bill is the one this request asked
// for. It is the check that makes an idempotency key safe to reuse by accident:
// without it, a key reused with different parameters is answered with a bill in
// the wrong currency, for the wrong period, and the caller is never told.
//
// Nothing needs to be stored to do this. The request is exactly a currency and a
// fee period, and a bill carries all three, so the bill is its own record of what
// was asked for - which keeps uniqueness coming from Temporal rather than from a
// deduplication table.
//
// Instants are compared with Equal, never ==. The period comes back from the
// workflow as UTC while a caller may have written the same moment with an offset;
// == compares wall clock, location and monotonic reading, so it would refuse a
// correct retry.
//
// Both sides are taken to storage precision first. A bill read back from Postgres
// has lost its sub-microsecond digits, and comparing that against an untruncated
// request refused a correct retry as key reuse - the same false 409 the Equal
// rule above exists to prevent, arriving by a different route. Requests are
// truncated on the way in too, so this is belt and braces for bills created
// before that was so.
func matchesRequest(snap bill.Snapshot, currency money.Currency, in CreateBillRequest) bool {
	return snap.Currency.Code == currency.Code &&
		atStoragePrecision(snap.PeriodStart).Equal(atStoragePrecision(in.PeriodStart)) &&
		atStoragePrecision(snap.PeriodEnd).Equal(atStoragePrecision(in.PeriodEnd))
}

// writeKeyReuse refuses a key that already named a different bill.
func writeKeyReuse(w http.ResponseWriter, billID string, snap bill.Snapshot) {
	writeProblem(w, http.StatusConflict, reasonKeyReuse,
		"idempotency key already created bill "+billID+" in "+snap.Currency.Code+
			" for "+snap.PeriodStart.UTC().Format(time.RFC3339)+" to "+
			snap.PeriodEnd.UTC().Format(time.RFC3339)+
			"; reusing it for different parameters would return a bill that was not asked for")
}

// validateLineItemRequest reports what is wrong with a charge before it is sent
// anywhere, or nil if it is well formed.
//
// It exists because an absent amount is not caught by decoding. A body with no
// amount leaves the zero Money, whose currency is empty; that marshals happily
// onto the update payload and then fails to deserialise on the worker, so the
// validator never runs and the caller is told the service broke rather than that
// its request was malformed.
//
// Pure, so the rule is testable without a running workflow or a live request.
func validateLineItemRequest(itemID string, in AddLineItemRequest) error {
	if itemID == "" {
		return fmt.Errorf("%w: item id is required", bill.ErrInvalidLineItem)
	}
	if in.Amount.Currency().IsZero() {
		return fmt.Errorf("%w: amount and its currency are required", bill.ErrInvalidLineItem)
	}
	if in.Description == "" {
		return fmt.Errorf("%w: description is required", bill.ErrInvalidLineItem)
	}
	return nil
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

	if in.CustomerID == "" {
		// Required here and not in the domain: bill.New must tolerate an empty
		// customer so that histories recorded before this field existed still
		// replay. This is the one place the requirement can be enforced without
		// stranding a bill that is already running.
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidBill,
			"customerId is required; a bill exists to be invoiced to someone")
		return
	}

	currency, err := money.Lookup(in.Currency)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonUnsupportedCurrency,
			"currency must be one of "+joinCodes(money.Supported()))
		return
	}
	// Before validation, not after: two instants less than a microsecond apart
	// would otherwise satisfy "strictly after" and then collapse into one another
	// on the way to storage, leaving a bill whose period does not end after it
	// begins - which the invoice table's own CHECK constraint forbids.
	in.PeriodStart = atStoragePrecision(in.PeriodStart)
	in.PeriodEnd = atStoragePrecision(in.PeriodEnd)

	if !in.PeriodEnd.After(in.PeriodStart) {
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidPeriod,
			"periodEnd must be strictly after periodStart")
		return
	}
	if !in.PeriodEnd.After(time.Now()) {
		// A bill accrues charges over a period that is still running. One whose
		// period has ended builds a timer with a negative duration, fires it at
		// once, and is closing before the caller has read the response saying it
		// was created - a 201 for a bill that can never take a charge.
		writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonInvalidPeriod,
			"periodEnd is in the past; a bill cannot accrue charges over a period that has already ended")
		return
	}

	billID, keyed := billIDForCustomer(in.CustomerID, req.Header.Get("Idempotency-Key"))

	// A key that has already produced a bill returns that bill rather than a
	// second one. This covers the completed case too, where the workflow is gone
	// and the invoice is read from storage.
	if keyed {
		switch snap, readErr := s.readBill(ctx, billID); {
		case readErr == nil:
			if !matchesRequest(snap, currency, in) {
				writeKeyReuse(w, billID, snap)
				return
			}
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

	started, err := s.startBillWorkflow(ctx, billID, billflow.StartBillInput{
		BillID:      billID,
		CustomerID:  in.CustomerID,
		Currency:    currency.Code,
		PeriodStart: in.PeriodStart,
		PeriodEnd:   in.PeriodEnd,
	})
	if err != nil {
		// The key names a bill that already exists. Return it rather than a second.
		if isWorkflowAlreadyStarted(err) {
			// This request lost the race, or repeats a key that already made a bill.
			// Either way the bill exists; compare here as well as in the pre-check,
			// because two concurrent requests can both pass the pre-check before
			// either has created anything.
			snap, readErr := s.readBill(ctx, billID)
			if readErr == nil {
				if !matchesRequest(snap, currency, in) {
					writeKeyReuse(w, billID, snap)
					return
				}
				w.Header().Set("Location", "/bills/"+billID)
				writeJSON(w, http.StatusOK, snap)
				return
			}
			if isWorkflowBusy(readErr) {
				writeUnavailable(w, "the existing bill for this key could not be read: "+readErr.Error())
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
		if isWorkflowBusy(err) {
			// The bill exists - the start succeeded - but its state cannot be read
			// back right now. Reporting a fault would tell the caller its bill was
			// not created, and a caller without an idempotency key that retries on
			// that basis creates a second one. 503 asks for the retry that will
			// return the bill, and creation is idempotent so the retry is safe.
			writeUnavailable(w, "the bill was created but its state could not be read back: "+err.Error())
			return
		}
		writeProblem(w, http.StatusInternalServerError, billflow.ReasonInternal,
			"reading the new bill: "+err.Error())
		return
	}

	// Only the request that actually created the execution reports 201. Without
	// this, concurrent requests sharing a key all claim to have created the bill.
	if !started && !matchesRequest(snap, currency, in) {
		writeKeyReuse(w, billID, snap)
		return
	}
	status := http.StatusOK
	if started {
		status = http.StatusCreated
	}

	w.Header().Set("Location", "/bills/"+billID)
	writeJSON(w, status, snap)
}

// startBillWorkflow starts a bill's workflow and reports whether this request is
// the one that created it.
//
// It calls the service API directly rather than client.ExecuteWorkflow, because
// the SDK helper does not surface that fact. Under any conflict policy it returns
// the existing run handle and a nil error when the workflow is already running,
// so a caller cannot tell creating from attaching - and every concurrent request
// sharing an idempotency key then reports 201 Created. The raw response carries a
// Started flag, which is exactly the missing signal.
//
// USE_EXISTING rather than FAIL: attaching is the ordinary outcome of a retry and
// should not be an error path. REJECT_DUPLICATE still stands, so a key whose bill
// has completed cannot quietly start a second one.
func (s *Service) startBillWorkflow(ctx context.Context, billID string, in billflow.StartBillInput) (bool, error) {
	payload, err := dataConverter.ToPayloads(in)
	if err != nil {
		return false, fmt.Errorf("encoding bill workflow input: %w", err)
	}

	resp, err := s.temporal.WorkflowService().StartWorkflowExecution(ctx,
		&workflowservice.StartWorkflowExecutionRequest{
			Namespace:    temporalNamespace(),
			WorkflowId:   billID,
			WorkflowType: &commonpb.WorkflowType{Name: billflow.WorkflowTypeName},
			TaskQueue:    &taskqueuepb.TaskQueue{Name: billflow.TaskQueue},
			Input:        payload,
			// Identifies this attempt. Distinct per request, so two concurrent
			// requests are two attempts and exactly one of them starts the bill; a
			// shared value would make the server treat them as one retried request.
			RequestId:                newRequestID(),
			WorkflowIdReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			WorkflowIdConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		})
	if err != nil {
		return false, err
	}
	return resp.Started, nil
}

// newRequestID returns a fresh identifier for one start attempt.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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

	if err := validateLineItemRequest(itemID, in); err != nil {
		writeProblem(w, statusForDomainError(err), billflow.ClassifyRejection(err), err.Error())
		return
	}

	if in.CustomerID != "" {
		snap, readErr := s.readBill(ctx, billID)
		switch {
		case readErr == nil && snap.CustomerID != in.CustomerID:
			writeProblem(w, http.StatusUnprocessableEntity, billflow.ReasonCustomerMismatch,
				"this line item states customer "+in.CustomerID+" but bill "+billID+
					" belongs to "+snap.CustomerID)
			return
		case readErr != nil && isWorkflowBusy(readErr):
			writeUnavailable(w, "the bill's customer could not be read to check it: "+readErr.Error())
			return
		}
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

// billIDForCustomer derives a bill id from the customer and its idempotency key.
//
// The key identifies a request within a customer rather than across all of them.
// Hashed globally, "september-2026" named one bill for every caller, so two fee
// engines acting for different customers were handed the same bill and the
// second accrued its charges onto the first customer's invoice.
//
// The boundary between the two fields is encoded, not marked. A customer
// identifier is opaque, so no character can be reserved as a delimiter, and every
// naive scheme has a colliding pair:
//
//	customer + key         ("acme","x") and ("acm","ex")   -> "acmex"
//	customer + ":" + key   ("ac:me","x") and ("ac","me:x") -> "ac:me:x"
//
// Length-prefixing the customer removes both: the digest cannot be reached by
// two different splits of the same bytes.
func billIDForCustomer(customerID, idempotencyKey string) (string, bool) {
	if idempotencyKey == "" {
		return billIDFor("")
	}
	scoped := strconv.Itoa(len(customerID)) + ":" + customerID + idempotencyKey
	return billIDFor(scoped)
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
