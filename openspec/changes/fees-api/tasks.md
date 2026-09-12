## 1. Project scaffolding

- [x] 1.1 Initialize the Encore Go application and module, and verify `encore run` boots and serves a health endpoint
- [x] 1.2 Add the Temporal Go SDK and an arbitrary-precision decimal dependency, and verify `go build ./...` succeeds with both pinned in `go.sum`
- [x] 1.3 Confirm the local Temporal dev server prerequisite, and verify `temporal server start-dev` exposes gRPC on `:7233` and the Web UI on `:8233`

## 2. Money package (pure domain, no framework)

- [x] 2.1 Implement the `Money` type as an `int64` minor-unit count plus an ISO 4217 currency carrying its own exponent, and verify unit tests assert no `float64` appears in the type or its JSON encoding
- [x] 2.2 Implement the supported-currency registry limited to `GEL` and `USD`, and verify a table test rejects an unsupported code such as `EUR`
- [x] 2.3 Implement addition and summation with a currency-mismatch error, and verify one hundred `0.10 USD` amounts sum to exactly `10.00 USD`
- [x] 2.4 Implement half-up rounding from a decimal to whole minor units, and verify a fee resolving to exactly `4.5` minor units is charged as `5`, with negative amounts and non-tie fractions covered
- [x] 2.5 Implement JSON encoding and decoding for `Money`, and verify `0.10 USD` round-trips to exactly `0.10 USD`

## 3. Bill domain and state machine (pure, no Temporal)

- [x] 3.1 Implement `transition(State, Event) (State, error)` covering `OPEN -> CLOSING -> CLOSED`, and verify a table test accepts both legal transitions and rejects every illegal one, including any attempt to return to `OPEN`
- [x] 3.2 Implement the bill aggregate holding line items keyed by caller-supplied item ID plus a running total, and verify unit tests cover deduplication by ID, `409` on same-ID-different-payload, and rejection of a line item in the wrong currency
- [x] 3.3 Implement the close snapshot that freezes the total and line items and records `closedAt` and `closedBy`, and verify the snapshot is unchanged by any later mutation attempt

## 4. Bill workflow

- [x] 4.1 Define the bill workflow and its input (bill ID, currency, `periodStart`, `periodEnd`), and verify a newly started workflow queries as `OPEN` with a zero total and no line items
- [x] 4.2 Implement the `AddLineItem` update with a validator that rejects when the bill is not `OPEN` and a yield-free handler returning the stored item and running total, and verify the handler contains no activity call, timer, or `Await`
- [x] 4.3 Implement the `CloseBill` update as a yield-free handler that freezes totals and returns the snapshot, and verify it returns state `CLOSING` with `closedBy` of `api_request`
- [x] 4.4 Implement the period-end timer using `workflow.NewTimer` with `workflow.Now` (never `time.Now`), and verify the bill auto-closes with `closedBy` of `period_end`
- [x] 4.5 Implement the selector-based main loop so the first trigger to change state wins and the other is a no-op, and verify the bill closes exactly once under either ordering
- [x] 4.6 Implement the `GetBill` query returning state, currency, period, total, line items, `closedAt`, and `closedBy`, and verify it answers correctly in `OPEN`, `CLOSING`, and `CLOSED`
- [x] 4.7 Sequence the main loop to run `PersistInvoice`, then `EmitInvoice`, then transition to `CLOSED`, and verify a test asserts that ordering so the durable record is never the missing artifact

## 5. Activities and persistence

- [x] 5.1 Declare the Encore Postgres database and write the migration for immutable invoices and their line items, and verify the migration applies cleanly under `encore test`
- [x] 5.2 Implement the `PersistInvoice` activity as an idempotent write, and verify running it twice for one bill leaves exactly one invoice row and one row per line item
- [x] 5.3 Implement the `EmitInvoice` stub activity with a bounded retry policy, and verify a test where it fails twice then succeeds still reaches `CLOSED`
- [x] 5.4 Implement the invoice read repository, and verify a closed bill remains readable after its workflow has completed

## 6. Encore API layer

- [x] 6.1 Implement `POST /bills` mapping `Idempotency-Key` to the workflow ID with `REJECT_DUPLICATE` reuse and `USE_EXISTING` conflict policies, and verify `201` for a new key, `200` for a key still in flight, and `200` for a key whose bill already closed
- [x] 6.2 Implement `PUT /bills/{billId}/line-items/{itemId}`, and verify the full matrix: `201` new, `200` identical retry, `409` same ID with different payload, `409` bill not `OPEN`, `422` currency mismatch
- [x] 6.3 Implement `POST /bills/{billId}/close`, and verify `202` with the frozen invoice on first call and `202` with the true `closedBy` when the bill is already `CLOSING` or `CLOSED`
- [x] 6.4 Implement `GET /bills/{billId}` routing to the workflow query while open and to Postgres once closed, and verify an open bill returns its running total, a closed bill returns its final invoice, and an unknown ID returns `404`
- [x] 6.5 Implement request validation for `periodEnd` strictly after `periodStart` and for supported currencies, and verify each rejection returns `422` with no bill created
- [x] 6.6 Implement a consistent error response shape across all endpoints, and verify every documented status code carries a machine-readable reason distinguishing `409` contradictory state from `422` malformed input

## 7. Worker lifecycle

- [x] 7.1 Start the Temporal worker from an Encore service struct's `initService`, and verify the worker registers the workflow and activities and polls its task queue when `encore run` starts
- [x] 7.2 Implement graceful worker shutdown, and verify stopping the service drains without panic or leaked goroutines

## 8. Workflow tests with time skipping

- [x] 8.1 Establish the `testsuite` harness with mocked activities, and verify a 30-day fee period executes in under a second of real time
- [x] 8.2 Test create, add three line items, close early, and verify the total is correct, all three items are present, and `closedBy` is `api_request`
- [x] 8.3 Test create, add items, never close, and verify the timer fires and the bill auto-closes with `closedBy` of `period_end`
- [x] 8.4 Test a close scheduled at `periodEnd - 1ns` against one at `periodEnd + 1ns`, and verify `closedBy` differs deterministically across the two runs
- [x] 8.5 Test adding a line item after close, and verify the update is rejected by the validator rather than failing in the handler, and that the rejection does not enter workflow history
- [x] 8.6 Test submitting the same item ID twice, and verify one line item exists, the total is unchanged, and the existing item is returned
- [x] 8.7 Test a `GEL` line item on a `USD` bill, and verify it is rejected with the currency-mismatch reason and the total is unchanged
- [x] 8.8 Test `EmitInvoice` failing twice then succeeding, and verify the bill remains observable in `CLOSING` throughout and still reaches `CLOSED`

## 9. Replay and determinism

- [x] 9.1 Export a real workflow history with `temporal workflow show --output json` and commit it under `testdata/`, and verify the fixture covers a bill that accrued items and closed
- [x] 9.2 Add a `worker.WorkflowReplayer` test over the committed fixtures, and verify it passes against current code and fails when an activity call is deliberately reordered
- [x] 9.3 Wire the replay test into CI, and verify a non-deterministic workflow change fails the build rather than surfacing at deploy time

## 10. Integration

- [x] 10.1 Add one end-to-end test against a running Temporal dev server, and verify HTTP request, workflow update, activity execution, and Postgres write all connect on a single happy path

## 11. Documentation

- [x] 11.1 Write the README run instructions, and verify a reader can start `temporal server start-dev`, run `encore run`, and reach the API from a clean checkout
- [x] 11.2 Add a curl walkthrough using a short fee period, and verify a reviewer can create a bill with `periodEnd` seconds away, add line items, and watch it auto-close in the Temporal Web UI
- [x] 11.3 Document why Temporal is used — serialised writes, durable period timer, activity retry, replayable history — and record the Encore/Temporal worker-lifecycle seam, and verify the rationale maps to the tests that demonstrate each property
