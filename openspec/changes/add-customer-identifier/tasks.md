## 1. Pin the collision before fixing it

- [ ] 1.1 Add a failing test in `billing/` asserting that the same idempotency key used for two different customers yields two distinct bills, and verify it fails today because both hash to one id
- [ ] 1.2 Add a failing test asserting the customer scoping encodes a boundary, using the adjacent pair `("ac","me:x")` and `("acme","x")`, and verify it fails today and would still fail under naive concatenation

## 2. Domain

- [ ] 2.1 Add the customer identifier to `bill.Bill` and `bill.Snapshot`, and verify a snapshot reports the customer it was created with
- [ ] 2.2 Take the customer as a parameter to `bill.New` **without** rejecting an empty value, and verify a unit test constructs a bill with no customer successfully - the leniency is deliberate and protects replay, so a later "tightening" must fail this test rather than pass silently
- [ ] 2.3 Verify the domain test suite still passes with no other behavioural change

## 3. Workflow input

- [ ] 3.1 Add the customer to `billflow.StartBillInput`, and verify the workflow passes it to `bill.New` without branching on it anywhere
- [ ] 3.2 Verify `TestReplayCommittedHistories` passes against the committed fixtures **unmodified** - if it fails, the change has introduced a branch on the field's absence and the fix is to remove that branch, never to re-record
- [ ] 3.3 Add a replay fixture recorded before this change is not required; instead verify by inspection that no `workflow.GetVersion` was introduced, since the workflow's command sequence is unchanged and there is nothing to version

## 4. API

- [ ] 4.1 Require a customer identifier on `POST /bills`, rejecting its absence with `422`, and verify the response carries a machine-readable reason distinct from the period and currency rejections
- [ ] 4.2 Report the customer on every bill representation - creation, retrieval, and the close response - and verify it is present in each
- [ ] 4.3 Scope the idempotency key by hashing the customer with it, length-prefixing the customer so no pair of values can span the boundary, and verify tasks 1.1 and 1.2 now pass
- [ ] 4.4 Verify an identifier the system does not recognise is stored and returned unchanged, confirming it is treated as opaque

## 5. Storage, and the reset

- [ ] 5.1 Add a migration introducing `customer_id TEXT NOT NULL` on `invoice`, and verify it applies cleanly under `encore test`
- [ ] 5.2 Reset the local database with `encore db reset bills` before applying the migration, and verify the 143 pre-existing invoices are gone - the `NOT NULL` column is only available because none of that data is real, and the migration cannot apply while those rows exist
- [ ] 5.3 Persist and read back the customer in `saveInvoice` and `loadInvoice`, and verify a closed bill read from storage reports the same customer it reported while open
- [ ] 5.4 Verify `invoice_line_item` gained no customer column, so the item's customer cannot disagree with the bill's

## 6. The line item checksum

- [ ] 6.1 Accept an optional customer on the line item request, and verify an addition stating the bill's customer is accepted exactly as one stating none
- [ ] 6.2 Reject an addition stating a different customer with `422` and its own reason, and verify the reason is distinguishable from a currency mismatch and from a bill that is not open
- [ ] 6.3 Verify the rejection leaves the bill's total and line items unchanged

## 7. Verify nothing regressed

- [ ] 7.1 Run `go test ./internal/... -race` and verify the domain, workflow and money suites are green
- [ ] 7.2 Run `TestReplayCommittedHistories` once more and verify the fixtures are byte-identical to those committed on `main`, confirming no re-recording happened during implementation
- [ ] 7.3 Run the end-to-end suite against a live service and verify every case passes, including the new customer cases, and that the case count guard in CI is raised to match
- [ ] 7.4 Verify by reproduction that two customers using the idempotency key `september-2026` receive two different bills, and that neither can read the other's
