# Fees API

Accrues fees against a customer over a billing period and produces an invoice when
that period ends.

Each bill is a **Temporal workflow execution** rather than a row in a table. A fee
period may run for a month, during which the bill takes concurrent writes, must
close on a wall-clock deadline, and must survive deploys and crashes without
losing or duplicating a charge. Modelling it as a durable process addresses all
four directly; the reasoning is in [Why Temporal](#why-temporal).

```
  POST /bills                        ->  start workflow (id = bill id)
  PUT  /bills/{id}/line-items/{item} ->  update  AddLineItem   201 | 200 | 409 | 422
  POST /bills/{id}/close             ->  update  CloseBill     202
  GET  /bills/{id}                   ->  query, or Postgres once closed
```

---

## Running it

Three prerequisites: **Go 1.26+**, the **Temporal CLI**, and **Encore** (which
provisions a local Postgres via Docker, so Docker must be running).

```bash
brew install temporal encoredev/tap/encore
```

Start the Temporal dev server — this owns the durable timers and workflow history:

```bash
temporal server start-dev
```

In a second terminal, start the API:

```bash
encore run
```

That gives you the API on `http://127.0.0.1:4000`, Encore's dashboard on
`http://127.0.0.1:9400`, and Temporal's Web UI on `http://127.0.0.1:8233`. Keep
the Temporal UI open — watching a bill's history is the clearest way to see what
the workflow is doing.

```bash
curl -s http://127.0.0.1:4000/health
```

`TEMPORAL_HOSTPORT` and `TEMPORAL_NAMESPACE` override the defaults
(`127.0.0.1:7233`, `default`) if your dev server runs elsewhere.

---

## Walkthrough

The fee period is supplied by the caller, so you can set one **30 seconds** long
and watch the bill close itself. That is the same durable timer a month-long
period uses — just short enough to observe by hand.

**1. Open a bill.** The `Idempotency-Key` is optional but makes creation safe to
retry:

```bash
curl -s -X POST http://127.0.0.1:4000/bills \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: demo-$(date +%s)" \
  -d "{\"currency\":\"USD\",
       \"periodStart\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
       \"periodEnd\":\"$(date -u -v+30S +%Y-%m-%dT%H:%M:%SZ)\"}"
```

Returns `201` with the bill. Copy its `id` into `BILL`:

```bash
BILL=bill_...
```

**2. Accrue some fees.** `PUT`, because you supply the line item's id and the
operation is idempotent — which is what `PUT` means:

```bash
curl -s -X PUT "http://127.0.0.1:4000/bills/$BILL/line-items/txn_001" \
  -H 'Content-Type: application/json' \
  -d '{"amount":{"minorUnits":1234,"currency":"USD"},"description":"card fee"}'
```

`201`, with the created item and the bill's running total. Add another with
`txn_002`, then try these three refusals:

```bash
# Same id, same detail -> 200, already accrued. A retry cannot double-charge.
curl -si -X PUT "http://127.0.0.1:4000/bills/$BILL/line-items/txn_001" \
  -H 'Content-Type: application/json' \
  -d '{"amount":{"minorUnits":1234,"currency":"USD"},"description":"card fee"}' | head -1

# Same id, different amount -> 409. Two charges cannot claim one identity.
curl -si -X PUT "http://127.0.0.1:4000/bills/$BILL/line-items/txn_001" \
  -H 'Content-Type: application/json' \
  -d '{"amount":{"minorUnits":9999,"currency":"USD"},"description":"card fee"}' | head -1

# GEL on a USD bill -> 422. Malformed, not contradictory.
curl -si -X PUT "http://127.0.0.1:4000/bills/$BILL/line-items/txn_gel" \
  -H 'Content-Type: application/json' \
  -d '{"amount":{"minorUnits":500,"currency":"GEL"},"description":"lari fee"}' | head -1
```

**3. Either close it, or let the period end.**

```bash
curl -s -X POST "http://127.0.0.1:4000/bills/$BILL/close"
```

`202 Accepted`, carrying the frozen total and every line item charged.
`202` rather than `200` because the totals are final but the bill is not: the
invoice hand-off is still running. `closedBy` says `api_request`.

Or add nothing further and wait 30 seconds — the bill closes itself with
`closedBy: period_end`. Watch it happen:

```bash
watch -n1 "curl -s http://127.0.0.1:4000/bills/$BILL | python3 -m json.tool | head -20"
```

**4. Read the invoice.**

```bash
curl -s "http://127.0.0.1:4000/bills/$BILL" | python3 -m json.tool
```

While the bill is open this is answered by the workflow; once closed it comes from
Postgres, so it stays readable long after Temporal has discarded the history.

Now open `http://127.0.0.1:8233`, find the workflow by its bill id, and read the
history. The updates, the timer, and the three activities are all there — and the
rejected `409` is **not**, because a validator rejection never enters history.

---

## API

| Method | Path | Success | Refusals |
|---|---|---|---|
| `POST` | `/bills` | `201` new, `200` if the idempotency key already made one | `422` bad period or currency |
| `PUT` | `/bills/{billId}/line-items/{itemId}` | `201` accrued, `200` identical retry | `409` not open / id conflict, `422` currency or malformed, `404` no such bill |
| `POST` | `/bills/{billId}/close` | `202` with the frozen invoice | `404` no such bill |
| `GET` | `/bills/{billId}` | `200` | `404` |

Every refusal carries a machine-readable `reason`, and the split is consistent:

- **`409`** — the request *contradicts* the bill's state or an immutable fact:
  `bill_not_open`, `line_item_conflict`
- **`422`** — the request is well-formed but *cannot be acted on as stated*:
  `currency_mismatch`, `invalid_period`, `unsupported_currency`, `invalid_line_item`

`409` is never used for an *already-achieved* state. Closing a bill that is
already closing returns `202` with the real `closedBy`, because the caller wanted
the bill closed and it is — erroring on a millisecond race would be hostile.

### Money

Amounts are an exact `int64` count of the currency's minor units, with the ISO 4217
exponent carried on the currency rather than assumed uniform. On the wire:

```json
{ "amount": "12.55", "currency": "USD", "minorUnits": 1255 }
```

`amount` is a **JSON string**, never a number — a number invites the consumer's
decoder to route it through a float64, which is the precision loss the whole
representation exists to prevent. `minorUnits` is the exact integer form.

Rate-derived fees round **half-up** at line-item creation, so every stored amount
is a real chargeable amount. The trade-off is accepted knowingly: every tie
resolves in the biller's favour, where half-even would split them. Supported
currencies are `GEL` and `USD`.

---

## Why Temporal

Not as a queue. As a **serialised, durable, single-writer entity with a built-in
month-long timer**. Four specific problems, each with the test that demonstrates
it:

**1. Concurrent writes, without locks.** A workflow executes as a single-threaded
deterministic coroutine scheduler, so concurrent additions to one bill are ordered
by construction — no row lock, no optimistic-concurrency retry, no lost update.

**2. A deadline that survives everything.** The period timer is durable. Restart
the worker mid-period and the bill still closes on time.

**3. The close race stops existing.** Two triggers can close a bill: a request and
the deadline. Both run on the same thread, so whichever reaches the state first
wins and the loser is a no-op. This rests on update handlers being **yield-free** —
they validate, mutate in memory, and return, while all waiting happens in the main
loop. A handler that called an activity would suspend mid-transition and the timer
could fire inside that window, leaving a bill half-closed.

  `TestCloseRaceAgainstPeriodEndResolvesDeterministically` proves the outcome
  flips one nanosecond either side of the deadline. `TestUpdateHandlersAreYieldFree`
  parses the source and fails if a handler ever reaches a yield point.

**4. Reliable hand-off.** Invoice emission is an activity with a bounded retry
policy. While it fails the bill sits visibly in `CLOSING` with totals already
frozen — which is why `CLOSING` is a state and not an instant.

  `TestInvoiceHandOffIsRetriedUntilItSucceeds` fails the hand-off twice and
  asserts the bill still reaches `CLOSED` with its total intact.

And one Temporal-specific hazard worth naming: **update delivery is at-least-once**,
so a client retry would double-charge without deduplication. Line items are keyed
by a caller-supplied id — a fee is caused by something that already has an
identity, so the invariant is real: *the fee for a given source event appears at
most once on this bill*.

The id is deliberately **not** reused as the Temporal update id. Temporal
deduplicates by update id *before* the validator runs, so a conflicting reuse
would be answered from the first result and the `409` would never be raised. The
caller would be told its charge was accepted while a contradictory one was
silently discarded. Deduplication belongs to the domain, which can tell an
identical retry from a conflicting reuse.

### A query is not a database

Temporal answers `GET` while a bill is open. Once closed, the invoice is read from
Postgres, written by an activity at the close. Workflow history is retained for a
limited window — the local dev server defaults to **24 hours** — and a financial
artifact has to outlive the process that produced it. Treating a query as durable
storage is the most common Temporal anti-pattern, and not doing it is deliberate.

The API layer never writes Postgres; only activities do. The workflow stays the
single writer.

---

## Testing

```bash
go test ./internal/...                      # domain, workflow, replay — no infrastructure
encore test ./...                           # adds the database tests
FEES_API_BASE_URL=http://127.0.0.1:4000 \
  go test ./e2e/ -v                         # end-to-end, needs `encore run`
```

| Layer | Where | Tests | Needs |
|---|---|---|---|
| Domain | `internal/money`, `internal/bill` | 121 | nothing |
| Workflow | `internal/billflow` | 28 | nothing (time is skipped) |
| Persistence | `billing` | 8 | `encore test` |
| End-to-end | `e2e` | 19 | `encore run` + Temporal |

**A 30-day fee period runs in ~40ms.** The Temporal test environment skips timers
rather than waiting on them, so the full month-long lifecycle — accrual, deadline,
close, hand-off — is exercised in every run.

### Replay tests

The one that matters most for a workflow this long-lived. Bills stay open across
deploys, so new code will routinely resume executions that older code started.
Temporal reconstructs a running workflow by replaying its history; if the code now
issues a different command sequence, replay diverges and the execution is stuck.

`internal/billflow/testdata/` holds histories exported from real executions:

```bash
temporal workflow show --workflow-id <bill-id> --output json > testdata/<name>.json
```

CI replays them against current code, so a non-deterministic change fails the
build instead of appearing at deploy time on live bills. Verified by reordering
the activities: replay fails with `TMPRL1100 nondeterministic workflow`.

Refresh the fixtures deliberately when the workflow's structure is *meant* to
change — never to make a red test go green. The real fix for an intentional change
is `workflow.GetVersion`.

---

## Where Encore and Temporal rub

Two honest seams, neither papered over.

**The worker needs an owner.** Encore provisions its own infrastructure from
declarations in code, but Temporal is not one of its resources, so the worker is
an ordinary long-lived goroutine. An Encore service struct owns it: `initService`
starts it, `Shutdown` drains it. See `billing/service.go`.

**Status codes needed raw endpoints.** Encore's typed endpoints always return
`200` on success and have no `422` in their error-code table, so `201`, `202`, and
`422` are unreachable through them. Since the status codes here are load-bearing —
`202` is what distinguishes "totals final, hand-off pending" from "done" — the bill
endpoints are `//encore:api raw` and write their own responses. The cost is manual
JSON handling and no auto-generated client; the alternative was an API that could
not express its own state machine.

A third, smaller one: `billing` cannot be tested with plain `go test`, because
`sqldb.NewDatabase` panics outside the Encore runtime. CI runs that package under
`encore test` and the rest under `go test`.

---

## Layout

```
billing/                 Encore service — raw HTTP endpoints, activities, worker, Postgres
  api.go                 the four endpoints
  service.go             service struct: Temporal client + worker lifecycle
  activities.go          PersistInvoice, EmitInvoice (stub), FinalizeInvoice
  db.go, migrations/     durable invoices
internal/bill/           pure domain: state machine, aggregate, money invariants
internal/billflow/       the Temporal workflow — no Encore dependency
  testdata/              committed history fixtures for replay
internal/money/          exact money: int64 minor units + ISO 4217
e2e/                     end-to-end HTTP tests
openspec/changes/fees-api/   proposal, specs, design, tasks
```

`internal/bill` and `internal/money` import neither Temporal nor Encore, which is
why most of the suite runs with no infrastructure at all.

### Scope

Invoice *delivery* is out of scope — `EmitInvoice` is a stub, but the seam and its
retry behaviour are real. Also excluded: reopening a closed bill, listing or
searching bills (read is by id), cross-currency settlement, and payment capture.

Full reasoning, alternatives considered, and the requirements this was built
against are in `openspec/changes/fees-api/` — `design.md` carries the twelve
decisions and their rejected alternatives.
