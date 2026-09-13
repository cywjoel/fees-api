## Context

See `proposal.md` — Why, for motivation.

Greenfield service. `openspec/specs/` is empty; there is no existing code, schema, or convention to conform to. The binding constraints come from the brief: Go, Encore, Temporal, a money representation immune to precision error, correct REST semantics for a financial API, and an explicit account of which problems Temporal is actually solving here.

The single most consequential property is that **a bill is a process with a lifetime, not a record that gets mutated**. A fee period may run for a month. During that month the bill accrues writes from concurrent callers, must close on a wall-clock deadline, and must survive deploys and crashes without losing or duplicating a charge. That framing drives nearly every decision below.

## Goals / Non-Goals

**Goals:**

- Make concurrent line-item writes correct without application-level locking.
- Make the close deadline reliable across process restarts and deploys.
- Make every mutation safe to retry, since Temporal delivers updates at-least-once.
- Keep the closed invoice durable independently of Temporal's retention horizon.
- Keep workflow code deployable while bills are mid-period.
- Keep the domain logic testable without Temporal or Encore in the loop.

**Non-Goals** (design-level; see `proposal.md` — Non-goals for scope):

- No horizontal scale target. Correctness and clarity over throughput.
- No continue-as-new handling. Assumed bounded line-item counts per the brief.
- No read-model projection for open bills. Reads are by ID; live detail comes from a Query.
- No authn/authz design. Assumed handled upstream.

## Decisions

### 1. One Temporal workflow instance per bill; the workflow is the writer

```
  +----------------------------------------------------------------+
  |  ENCORE SERVICE  (never writes Postgres — reads only)           |
  |                                                                 |
  |  POST /bills                        -> StartWorkflow            |
  |  PUT  /bills/{id}/line-items/{item} -> Update AddLineItem       |
  |  POST /bills/{id}/close             -> Update CloseBill         |
  |  GET  /bills/{id}                   -> Query | Postgres         |
  +---------------------------------+-------------------------------+
                                    |
  +---------------------------------v-------------------------------+
  |  BILL WORKFLOW    OPEN -> CLOSING -> CLOSED                     |
  |  updates: yield-free & atomic  |  main loop: selector + activity|
  +---------------------------------+-------------------------------+
                                    | activities only
  +---------------------------------v-------------------------------+
  |  POSTGRES (Encore-provisioned): immutable closed invoices        |
  +------------------------------------------------------------------+
```

Bill ID **is** the Temporal workflow ID. This gives uniqueness and create-idempotency from the infrastructure rather than from a dedup table.

*Why:* A Temporal workflow executes as a single-threaded deterministic coroutine scheduler. All mutations to a given bill therefore serialise by construction — no `SELECT ... FOR UPDATE`, no optimistic-concurrency retry loop, no lost updates. This is the concrete answer to "what problem is Temporal solving": it is not a queue here, it is a **serialised, durable, single-writer entity with a built-in month-long timer.**

*Alternatives considered:*

- *Postgres as source of truth, Temporal only for the period timer.* Rejected: reintroduces every concurrency problem the workflow removes, and leaves the timer as the only justification for a heavyweight dependency.
- *Temporal-only, no database.* Rejected — see decision 6.

### 2. Updates, not Signals

A Signal is fire-and-forget: it returns no value and cannot reject. Satisfying "reject line item addition if the bill is closed" with a Signal means either lying to the caller (`202 Accepted`, then silently dropping the item) or signalling and then polling a Query, which is genuinely racy.

A Temporal **Update** runs a validator that can reject synchronously, and a handler that returns a value. That maps cleanly onto HTTP:

| Operation | Mechanism | Success | Rejection |
|---|---|---|---|
| Create bill | `StartWorkflow` | `201` | `200` if already exists |
| Add line item | Update `AddLineItem` | `201` new / `200` existing | `409` not OPEN, `409` id conflict, `422` currency |
| Close bill | Update `CloseBill` | `202` with frozen invoice | `202` if already closing/closed |
| Read bill | Query / Postgres | `200` | `404` |

A rejected update never enters workflow history at all, whereas a failed one does — so validating in the validator is both semantically correct and cheaper.

### 3. Update handlers must be yield-free; the main loop does all waiting

This is the rule that makes the close race disappear rather than merely resolving it.

```
  Update handlers and the main loop run on the SAME thread.
  They interleave ONLY at yield points (activity call, timer, Await).
  => A handler with NO yield points is ATOMIC w.r.t. the timer callback.
```

```
  UPDATE HANDLER (pure, no yields)     MAIN LOOP (does all the waiting)
  -------------------------------      -------------------------------
  validator: state == OPEN ?           sel.AddFuture(periodTimer, func(){
    no  -> reject                        if state == OPEN {
    yes -> admit                             beginClose(period_end)
                                         }
  handler:                             })
    freeze totals                      sel.Select(ctx)
    state = CLOSING                    workflow.Await(state != OPEN)
    closedBy = api_request             ExecuteActivity(PersistInvoice)
    return snapshot  -> 202 body       ExecuteActivity(EmitInvoice)
                                       state = CLOSED
```

Whichever trigger touches `state` first wins; the loser is a no-op reading the same variable on the same thread.

*The trap this avoids:* if the close handler itself called `ExecuteActivity`, it would yield mid-transition and the timer could fire inside that window, producing a bill that is half-closed. The handler freezes and returns; the main loop runs the activities.

### 4. Closing an already-closing bill returns `202`, not `409`

```
  timer fires at 23:59:59.998
  client's close lands at 23:59:59.999
  -> caller asked to close; bill IS closing; they simply did not cause it
```

The response carries `closedBy: "period_end" | "api_request"` and `closedAt`, so the caller learns what happened without being handed an error for a state they wanted.

*Rule:* `409` is reserved for **contradictory** state (adding a line item to a closed bill; reusing an item ID with a different payload), never for **already-achieved** state. This keeps the error semantics consistent with the idempotency model throughout.

`202` rather than `200` because the resource is genuinely not in its terminal state — the totals are final, the bill is not.

### 5. Idempotency: client-supplied item IDs under `PUT`; `Idempotency-Key` for create

Temporal's update delivery is at-least-once. Without deduplication, a client timeout-and-retry double-charges:

```
  client --X (timed out, but it landed) --> workflow: +$5.00
  client retries                       --> workflow: +$5.00   = $10 charged
```

**Line items** are keyed by a client-supplied item ID, deduplicated inside the workflow against a `map[itemID]LineItem`. Chosen over an `Idempotency-Key` header because:

- A fee line item is *caused* by something that already has an ID — a transaction, transfer, or card authorisation. So the dedup key is not a retry token bolted on; it expresses a real business invariant: **the fee for transaction `X` appears at most once on this bill.**
- It is permanent and part of the resource, so it doubles as the audit key. `Idempotency-Key` caches conventionally expire (Stripe: 24h).
- It makes `PUT` the correct verb, so idempotency follows from RFC 9110's definition of the method rather than from a header convention.

Same ID with a *different* payload is a contradiction, not a retry: `409`. Accrued line items are immutable.

**Bill creation** uses `POST` with an `Idempotency-Key` mapped deterministically to the workflow ID, since clients should not have to invent bill IDs.

*Discovered while implementing:* the item ID must **not** be reused as the Temporal **update ID**, tempting as that is. Temporal deduplicates by update ID *before* the validator runs, so a second request reusing an item ID with a different amount is answered from the first result and the conflict is never detected. The bill stays correct — the stored item is never overwritten — but the caller is told its charge was accepted when a contradictory one was silently discarded, which is precisely the failure mode idempotency is meant to prevent. Deduplication therefore stays with the domain, which can tell an identical retry from a conflicting reuse. Nothing is lost by letting each request reach the workflow: updates are delivered at least once and the handler is idempotent by construction.

### 6. Postgres holds closed invoices; a Query is not a database

Temporal Queries work against closed workflow executions, but only while history is retained (namespace default: 30 days). A financial artifact must outlive the process that produced it.

```
  GET /bills/{id}
       |
    status? --OPEN----> Query the running workflow  (strongly consistent)
       |
       +------CLOSED---> Postgres                   (durable, permanent)
```

The `CLOSING -> CLOSED` boundary already requires an activity for the invoice hand-off, so persisting the invoice there is nearly free. The API layer never writes Postgres — only activities do — which preserves the single-writer property from decision 1 and avoids a dual-write problem.

*Alternative considered — Temporal only, no database.* Simpler, and not broken: reads work right up until the retention horizon, at which point the invoice silently disappears. Unacceptable for a bank. Treating a Query as durable storage is the most common Temporal anti-pattern, and explicitly not doing it is a deliberate part of this design.

### 7. Money: `int64` minor units, rounded per line item

`Money{ minorUnits int64, currency Currency }`, where the currency carries its own ISO 4217 exponent. Rate arithmetic (e.g. 0.35% of a transaction) uses an arbitrary-precision decimal library and rounds to minor units **at line-item creation**, using **half-up** rounding. The bill total is then an exact integer sum.

*Rounding mode:* half-up — ties round away from zero. Chosen for explicability: a customer disputing a single line item can be told the rule in one sentence. The trade-off is accepted knowingly — every tie resolves in the biller's favour, so across high fee volume this accrues a small systematic surplus to the platform, where half-even would split ties evenly. The mode is a single constant in the money package and is the only place this policy is expressed.

*Why round per item:* a line item is a discrete charge a customer can see and dispute. It must be a real chargeable amount — nobody can be billed $12.55003. Storing digits that can never be charged makes the type lie about what it represents and defers the rounding question rather than answering it.

*Alternative considered — a fixed higher scale (e.g. `$12.55 -> 1255000` at 1e-5) to leave room for FX.* Rejected on two grounds. First, it does not achieve its aim: FX precision lives in the **rate**, not the amount.

```
  Money{1255, USD}  x  Rate{2.704318, USD->GEL, asOf: t, source: ...}
        |                       |                            |
   exact, 2dp,            6-8dp decimal,              Money{3393, GEL}
   chargeable            timestamped, sourced          rounded ONCE, here,
                                                       with an explicit mode
```

Second, a hardcoded scale of 5 contradicts ISO 4217, which assigns exponents of 0 (JPY), 2 (USD, GEL), 3 (KWD) and 4. Carrying the exponent on the currency handles all of them.

*Where sub-minor scaling would be right:* continuous accrual — daily interest, per-second metering — where fractions accumulate all period and round once at the end. Transaction fees are discrete charges, so it does not apply here.

### 8. Mono-currency bills

A bill's currency is fixed at creation. A line item in a different currency is rejected with `422`, which also yields a second integrity rejection distinct in kind from the closed-bill `409`.

Cross-currency **settlement** is a different bounded context and is out of scope:

```
  BILLING                        |  SETTLEMENT (a separate entity)
  bill.currency = USD            |  payment.settlementCurrency = GEL
  line items in USD              |  payment.rate = {2.704318, asOf, source}
  invoice total = USD 1234.56    |  payment.spread, payment.quoteExpiresAt
                                 |  payment.debited = GEL 3338.34
```

*Extension seam:* a future `Payment` entity references the invoice and carries its own rate snapshot. Critically, **`Money` does not change** when that arrives — conversion produces a new `Money` in the target currency. So decision 7 does not foreclose it.

### 9. Workflow ID reuse policy — a silent-failure landmine

Because bill ID is workflow ID, the default reuse policy breaks create-idempotency invisibly:

```
  WorkflowIDReusePolicy default = ALLOW_DUPLICATE
        |
        v
  bill closed -> workflow COMPLETED -> duplicate POST with the same key
  starts a BRAND NEW RUN under the same ID -> silent second bill, no error
```

Required configuration:

- `WorkflowIDReusePolicy: REJECT_DUPLICATE` — a completed ID cannot be reused.
- `WorkflowIdConflictPolicy: USE_EXISTING` — a still-running ID returns the existing handle instead of erroring.

Yielding:

| Request | Outcome |
|---|---|
| `POST /bills`, new key | `201 Created` |
| `POST /bills`, key in flight | `200 OK`, existing bill (conflict policy) |
| `POST /bills`, key completed | `200 OK`, read from Postgres (reuse policy rejects) |

Called out explicitly because it is invisible in testing and produces duplicate bills in production.

### 10. Period bounds supplied by the client

`periodStart` and `periodEnd` are provided at creation. The timer is `workflow.NewTimer(ctx, periodEnd.Sub(workflow.Now(ctx)))` — `workflow.Now`, never `time.Now`, which would be non-deterministic on replay.

*Why client-supplied:* it keeps billing-calendar policy out of this service, and it makes the system demonstrable — a reviewer can set `periodEnd = now + 30s` and watch the timer fire live. A hardcoded calendar month would make the timer path impossible to exercise by hand.

### 11. State machine as a pure function

```go
func transition(s State, e Event) (State, error)
```

Kept outside workflow code so the lifecycle rules are testable with no framework in the loop. If a transition is only reachable by executing a workflow, it is needlessly hard to test.

### 12. Test strategy

```
  +--------------------------------------------------------------+
  | 5. MANUAL / DEMO       README + Temporal Web UI :8233         |
  +--------------------------------------------------------------+
  | 4. INTEGRATION         encore test + temporal server start-dev|
  +--------------------------------------------------------------+
  | 3. REPLAY              WorkflowReplayer vs committed history  |
  +--------------------------------------------------------------+
  | 2. WORKFLOW            testsuite, time-skipped, mocked acts   |
  +--------------------------------------------------------------+
  | 1. DOMAIN              pure Go: Money, currency, transitions  |
  +--------------------------------------------------------------+
```

**Layer 1 — domain.** Table-driven tests over money arithmetic, rounding at the `.5` boundary, currency mismatch, and every legal and illegal transition. No infrastructure.

**Layer 2 — workflow.** `testsuite.TestWorkflowEnvironment` fast-forwards timers, so a 30-day period executes in milliseconds:

```
  real time:  |--5ms--|
  workflow:   |------------------ 30 days ------------------|
              ^        ^                ^                  ^
              start    delayed cb       delayed cb         timer fires
                       AddLineItem      AddLineItem        auto-close
```

| # | Test | Asserts |
|---|---|---|
| 1 | create -> add 3 -> close early | total correct, 3 items in result, `closedBy: api_request` |
| 2 | create -> add -> never close | timer fires, auto-closes, `closedBy: period_end` |
| 3 | close scheduled at `periodEnd - 1ns` vs `+1ns` | `closedBy` differs deterministically — *proves* the race resolution |
| 4 | add line item after close | rejected **by the validator**, not failed in the handler |
| 5 | same item ID twice | one item, total unchanged, existing item returned |
| 6 | GEL item on a USD bill | rejected, `422` |
| 7 | `EmitInvoice` fails twice then succeeds | bill still reaches `CLOSED` |

Test 3 is the direct evidence for the Temporal-role constraint. Test 7 is where `CLOSING` earns its keep: the bill sits in `CLOSING` while Temporal retries, then completes — a scenario that is genuinely painful to build without a durable execution engine.

**Layer 3 — replay.** Bills live for a month, so deploys land mid-period. A non-deterministic code change breaks every in-flight bill:

```
  bill opened   deploy v2   ...bill still running...
      |             |
      v             v
  [-------- 30 day period --------]
                |
                +--> replay history against v2 -> NonDeterministicError
```

Guard: export a real history (`temporal workflow show --output json`), commit it under `testdata/`, and run `worker.WorkflowReplayer` against it in CI. A refactor that reorders an activity call then fails the build instead of a customer's bill.

**Layer 4 — integration.** Deliberately thin: one end-to-end path proving HTTP -> update -> workflow -> activity -> Postgres wires together. Layers 1–3 carry the real coverage.

**Layer 5 — demo.** `temporal server start-dev` plus `encore run`, a curl walkthrough, and the Temporal Web UI at `localhost:8233` so a reviewer can watch the update events, the timer, and the activity retries in the actual history.

## Risks / Trade-offs

- **Encore and Temporal do not compose automatically.** Temporal is not Encore-provisioned infrastructure, so the worker needs a long-lived goroutine owned by an Encore service struct, and `encore test` needs an external Temporal dev server alongside it. → Attach worker lifecycle to `initService`; document the seam in the README rather than papering over it; keep integration coverage thin so the awkward layer carries little weight.

- **Temporal is a heavyweight dependency for a small API.** → Justified only by the four properties in decision 1 (serialised writes, durable month-long timer, activity retry, replayable audit history). If those were not required, this design would be wrong.

- **Workflow non-determinism breaks in-flight bills.** The highest-severity failure mode, and it fails at deploy time rather than at test time. → Layer 3 replay tests in CI with committed history fixtures.

- **`CLOSING` can stall if `EmitInvoice` keeps failing.** The bill is correctly frozen but never reaches `CLOSED`. → Bounded retry policy; the state is externally visible via `GET`, so it is observable rather than silent. Alerting on time-in-`CLOSING` is the production follow-up.

- **List/search is impossible without a read model.** Ruled out of scope, but if `GET /bills?status=open` is ever wanted, it forces an index row written by an activity at every transition. → Noted so the decision is revisited deliberately rather than discovered.

- **History growth is unbounded in principle.** The brief assumes no upper limit on line items is reached. A genuinely high-volume bill would need continue-as-new, which complicates carrying accrued items forward. → Accepted per the brief; recorded here so the assumption is explicit.

- **Postgres write at close is a second failure domain.** If `PersistInvoice` succeeds but `EmitInvoice` never does, the durable record exists while the workflow has not completed. → Ordering is deliberate: persist first, emit second, so the durable artifact is never the thing that is missing. Both are idempotent activities.

## Migration Plan

Not applicable. Greenfield service with no existing data, no consumers, and no prior version to roll back to.

## Open Questions

- Retry policy bounds for `EmitInvoice` (max attempts, backoff ceiling) — tunable without structural change once the real invoice implementation exists.
