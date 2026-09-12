## 1. Pin the defects with failing tests

- [x] 1.1 Add `billing/errors_test.go` asserting `WorkflowNotReady` and `WorkflowExecutionAlreadyStarted` are not classified as absent, and verify it fails today on both cases while the `NotFound` control passes
- [x] 1.2 Make the existing `409 once the bill is no longer open` case in `e2e/api_test.go` wait for `CLOSED` before offering the late charge, and verify it now fails with `404` where it previously passed against `CLOSING`
- [ ] 1.3 Add a failing case for a line item body with no `amount`, and verify it returns `500` today where `422` is required
- [ ] 1.4 Add a failing case for concurrent creations sharing one idempotency key, and verify every request reports `201` today
- [ ] 1.5 Add a failing case for an idempotency key reused with a different currency and period, and verify the original bill is returned today
- [ ] 1.6 Add a failing case for a fee period whose `periodEnd` has passed, and verify it returns `201` carrying `state: CLOSING` today

## 2. Separate absence from unavailability

- [ ] 2.1 Replace `isWorkflowGone` in `billing/errors.go` with predicates distinguishing absent, completed, transient, and already-started, and verify task 1.1's test passes without loosening its assertions
- [ ] 2.2 Add the transient rejection reason and its `503` mapping to `reasonStatus` and `reasonTitle`, and verify a `503` response carries `Retry-After` and a machine-readable reason
- [ ] 2.3 Narrow `readBill` so storage is consulted only for an absent or completed execution, propagating every other error including the currently discarded decode error, and verify a transient failure yields `503` rather than `404`
- [ ] 2.4 Route `writeUpdateFailure` through the new predicates, and verify a transient failure on a mutation yields `503` while a genuinely unknown bill still yields `404`
- [ ] 2.5 Verify with a live outage that `GET` on an existing open bill returns `503` and not `404`, reproducing the scenario recorded in the proposal

## 3. Answer for a bill whose workflow has finished

- [ ] 3.1 Add `bill.CheckAgainstSnapshot` to `internal/bill`, reusing the comparison `checkLineItem` uses so the deduplication rule is stated once, and verify unit tests cover identical retry, conflicting reuse, and an item absent from the invoice
- [ ] 3.2 Verify `CheckAgainstSnapshot` ignores `AccruedAt`, so a retry carrying a later timestamp is not mistaken for a conflicting charge
- [ ] 3.3 Give `AddLineItem` the storage fallback when the workflow is absent or completed, answering `409 bill_not_open` for a new charge, `409 line_item_conflict` for a reused id with different detail, and `200` for an identical retry, and verify task 1.2's e2e case passes
- [ ] 3.4 Verify by reproduction that `GET`, `PUT` and `POST /close` now agree about whether a closed bill exists, replacing the three-way disagreement recorded in the proposal
- [ ] 3.5 Fix the `CloseBill` fallback so a non-`ErrInvoiceNotFound` storage error reports a server fault rather than falling through to `404`, and verify with an induced storage error

## 4. Creation idempotency

- [ ] 4.1 Change the workflow id conflict policy to `FAIL` and handle `WorkflowExecutionAlreadyStarted` as the existing-bill path, and verify task 1.4's concurrent test yields exactly one `201` and the rest `200`
- [ ] 4.2 Compare the request's currency and fee period against the existing bill in both the pre-check and the already-started path, rejecting a mismatch with `409`, and verify task 1.5 passes
- [ ] 4.3 Verify the period comparison uses `time.Time.Equal`, with a test that retries a creation stating the same instant in a different time zone offset and expects `200`
- [ ] 4.4 Verify a read failure immediately after a successful start no longer reports `500` for a bill that was created

## 5. Request validation

- [ ] 5.1 Reject a line item whose amount carries no currency before dispatching the update, and verify task 1.3's case returns `422` with the `invalid_line_item` reason
- [ ] 5.2 Make `Money.MarshalJSON` reject a zero currency, in a separate commit from 5.1, and verify the existing money suite still passes after auditing marshal sites including log lines
- [ ] 5.3 Reject a creation whose `periodEnd` has already passed, and verify task 1.6's case returns `422` and creates no bill
- [ ] 5.4 Clamp the period timer duration to a non-negative value in `internal/billflow`, and verify a bill cannot be born already closing even if validation is bypassed

## 6. Close reporting

- [ ] 6.1 Make `beginClose` return its error rather than logging it and returning an `OPEN` snapshot, and verify `TestUpdateHandlersAreYieldFree` still passes because no yield point was introduced

## 7. Verify nothing regressed

- [ ] 7.1 Run `go test ./internal/... -race` and verify the domain, workflow and money suites are unchanged and green
- [ ] 7.2 Run `TestReplayCommittedHistories` and verify it passes against the committed fixtures without re-recording them, confirming bills open across the deploy will resume
- [ ] 7.3 Run the full end-to-end suite against a live service and verify every case in the status matrix passes, including the previously failing closed-bill case
- [ ] 7.4 Verify every one of the reproductions recorded in the proposal now returns the specified status, and that no reproduction still yields `404` for a bill that `GET` answers `200` for

## 8. Close the coverage gap that hid this

- [ ] 8.1 Add a CI job that starts a Temporal dev server, runs `encore run` in the background, waits for health, and executes the end-to-end suite with `FEES_API_BASE_URL` set, and verify it fails when the closed-bill fix is reverted
