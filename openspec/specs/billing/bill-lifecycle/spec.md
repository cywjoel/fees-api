# billing/bill-lifecycle Specification

## Purpose
Defines how a bill is opened for a fee period, how it progresses through its lifecycle to a final invoice, and how the two independent close triggers resolve to a single deterministic outcome.

## Requirements

### Requirement: Create a bill for a fee period

The system SHALL create a bill on request, denominated in a single currency, belonging to a single customer, and bounded by a caller-supplied fee period. A newly created bill SHALL be in the `OPEN` state and SHALL have a total of zero in its currency.

The request MUST supply a customer identifier, a currency, a `periodStart`, and a `periodEnd`. The system SHALL reject a request that does not identify a customer. A bill exists to be invoiced to someone, and one that belongs to nobody cannot be.

The customer identifier SHALL be treated as opaque: the system stores, compares and reports it, and SHALL NOT interpret its structure or validate it against any registry. Whatever system owns customers remains the authority on which of them exist.

The system SHALL reject a request whose `periodEnd` is not strictly after its `periodStart`.

The system SHALL additionally reject a request whose `periodEnd` has already passed. A bill exists to accrue charges over a period that is still running; one whose period has ended can never accept a charge, and creating it would report success for a bill that is already closing.

A fee period SHALL be recorded at a resolution the system can durably retain, and the bill SHALL report the period it recorded rather than the one it was sent. A caller that states a period more precisely than the system can store must be told what was actually recorded, because that is the value every later comparison and every later read will use.

#### Scenario: Bill is created successfully

- **WHEN** a client requests a new bill for a customer, with currency `USD`, a `periodStart`, and a later `periodEnd` that has not yet passed
- **THEN** the system responds `201 Created` with a bill identifier, the customer it belongs to, state `OPEN`, currency `USD`, a total of `0.00 USD`, and an empty line item collection

#### Scenario: Creation without a customer is rejected

- **WHEN** a client requests a new bill without identifying a customer, or identifies one with a value containing no non-whitespace character
- **THEN** the system responds `422 Unprocessable Content` and no bill is created
- **AND** a value that is only whitespace is refused for the same reason an absent one is: it identifies nobody

#### Scenario: An identifier's surrounding whitespace is part of it

- **WHEN** a client creates a bill for a customer identifier that carries leading or trailing whitespace alongside other characters
- **THEN** the bill is created and reports that identifier unchanged, because the system does not decide which customers exist and must not alter one it was given

#### Scenario: Customer identifier is stored as given

- **WHEN** a client creates a bill for a customer identifier of any form the system does not recognise
- **THEN** the bill is created and reports that identifier unchanged, because the system does not decide which customers exist

#### Scenario: Period end is not after period start

- **WHEN** a client requests a new bill whose `periodEnd` is equal to or earlier than its `periodStart`
- **THEN** the system responds `422 Unprocessable Content` and no bill is created

#### Scenario: Fee period has already ended

- **WHEN** a client requests a new bill whose `periodEnd` is in the past, even though it is after its `periodStart`
- **THEN** the system responds `422 Unprocessable Content` and no bill is created
- **AND** the system SHALL NOT respond `201 Created` for a bill that has already left `OPEN`

#### Scenario: Fee period is recorded at the resolution it can be retained

- **WHEN** a client creates a bill stating a fee period more precisely than the system can durably record
- **THEN** the bill reports the period actually recorded, and every subsequent retrieval reports that same period
- **AND** a later retry of that creation is recognised as the same request, whether the bill is read from its running workflow or from durable storage

#### Scenario: Unsupported currency is rejected

- **WHEN** a client requests a new bill in a currency the system does not support
- **THEN** the system responds `422 Unprocessable Content` and no bill is created

### Requirement: Bill creation is idempotent

The system SHALL accept an idempotency key on bill creation and SHALL create at most one bill per key **per customer**. A key identifies a request within the customer it was sent for, not across all customers: two callers acting for different customers SHALL be able to choose the same key string without interfering with one another.

A repeated request bearing a key that has already been used for that customer, and whose currency and fee period match the bill that key created, SHALL return the existing bill rather than creating a second one, and SHALL do so whether the existing bill is still open or has already closed.

A request bearing a used key whose currency or fee period differs from the existing bill SHALL be rejected. Returning the existing bill would answer a request the caller did not make, and the caller would then accrue charges against a bill whose currency or period is not the one it asked for.

Exactly one request SHALL be reported as having created the bill. Concurrent requests sharing one key SHALL yield one `201 Created`; every other request SHALL be reported as returning an existing bill.

#### Scenario: The same key for two customers makes two bills

- **WHEN** two clients each create a bill with the idempotency key `september-2026`, one for customer `acme` and one for customer `globex`
- **THEN** two distinct bills exist, each belonging to the customer it was created for
- **AND** neither client receives the other's bill

#### Scenario: Retried creation returns the existing open bill

- **WHEN** a client repeats a creation request for the same customer with an idempotency key that maps to a bill still in `OPEN`, with matching currency and fee period
- **THEN** the system responds `200 OK` with the existing bill and does not create a second bill

#### Scenario: Retried creation returns an already-closed bill

- **WHEN** a client repeats a creation request for the same customer with an idempotency key that maps to a bill in `CLOSED`, with matching currency and fee period
- **THEN** the system responds `200 OK` with the existing closed bill and its final invoice, and does not create a second bill

#### Scenario: Key reused with different parameters is rejected

- **WHEN** a client sends a creation request for a customer with an idempotency key that already created a bill for that customer, but with a different currency or a different fee period
- **THEN** the system responds `409 Conflict` and no second bill is created
- **AND** the existing bill is not returned as though it satisfied the request

#### Scenario: Concurrent creations sharing a key report one creation

- **WHEN** several creation requests carrying the same customer, the same idempotency key and identical parameters are processed concurrently
- **THEN** exactly one responds `201 Created` and the others respond `200 OK`, and exactly one bill exists for that customer and key

#### Scenario: Fee period is compared by instant, not by representation

- **WHEN** a client retries a creation request stating the same fee period in a different time zone offset
- **THEN** the period is treated as matching, because it denotes the same instant, and the system responds `200 OK` with the existing bill

### Requirement: Bill lifecycle states

A bill SHALL occupy exactly one of three states: `OPEN`, `CLOSING`, or `CLOSED`. The system SHALL permit only the transitions `OPEN -> CLOSING` and `CLOSING -> CLOSED`.

A bill in `OPEN` SHALL accept line items. A bill in `CLOSING` SHALL have a frozen total and line item collection, SHALL NOT accept line items, and SHALL have a pending invoice hand-off. A bill in `CLOSED` SHALL be immutable and its invoice hand-off complete.

#### Scenario: Bill is observable in each state

- **WHEN** a client retrieves a bill
- **THEN** the response reports its current state as exactly one of `OPEN`, `CLOSING`, or `CLOSED`

#### Scenario: Totals are frozen on entry to CLOSING

- **WHEN** a bill transitions from `OPEN` to `CLOSING`
- **THEN** its total and line item collection are fixed and SHALL NOT change for the remainder of the bill's existence

### Requirement: Close a bill on request

The system SHALL close an open bill on explicit request. The close SHALL transition the bill from `OPEN` to `CLOSING`, freeze its total and line items, and record the time of closure and the trigger that caused it.

The system SHALL respond `202 Accepted`, indicating that the totals are final while the invoice hand-off remains pending.

#### Scenario: Open bill is closed on request

- **WHEN** a client requests closure of a bill in `OPEN`
- **THEN** the system responds `202 Accepted` with state `CLOSING`, `closedBy` of `api_request`, and a `closedAt` timestamp

#### Scenario: Bill reaches CLOSED after the invoice hand-off succeeds

- **WHEN** the invoice hand-off for a bill in `CLOSING` completes successfully
- **THEN** the bill transitions to `CLOSED` and subsequent retrievals report state `CLOSED` with an unchanged total and line item collection

#### Scenario: Bill remains in CLOSING while the invoice hand-off is retried

- **WHEN** the invoice hand-off for a bill in `CLOSING` fails transiently
- **THEN** the bill remains in `CLOSING` with its frozen total, the hand-off is retried, and the bill's state remains externally observable throughout

### Requirement: Close a bill when the fee period ends

The system SHALL close a bill automatically when its `periodEnd` is reached, without requiring a client request. The automatic close SHALL freeze the total and line items identically to a requested close and SHALL record its trigger as `period_end`.

The close SHALL occur even if the system restarts, is redeployed, or crashes at any point during the fee period.

#### Scenario: Bill closes automatically at period end

- **WHEN** a bill in `OPEN` reaches its `periodEnd` without any close request
- **THEN** the bill transitions to `CLOSING` with `closedBy` of `period_end` and its total and line items are frozen

#### Scenario: Period end close survives a restart

- **WHEN** the system restarts or is redeployed at any point between a bill's creation and its `periodEnd`
- **THEN** the bill still closes at its `periodEnd`

### Requirement: Concurrent close triggers resolve deterministically

When a close request and the period-end deadline occur close together, the system SHALL apply exactly one of them. The bill SHALL be closed exactly once, its total SHALL be frozen exactly once, and `closedBy` SHALL report the trigger that actually caused the closure.

#### Scenario: Close request arriving before the deadline wins

- **WHEN** a close request is applied at any instant strictly before a bill's `periodEnd`
- **THEN** the bill closes with `closedBy` of `api_request`, and the subsequent arrival of the period-end deadline has no further effect

#### Scenario: Deadline reached before a close request wins

- **WHEN** a bill's `periodEnd` is reached before a close request is applied
- **THEN** the bill closes with `closedBy` of `period_end`, and the later close request does not close the bill a second time

### Requirement: Closing an already-closing or closed bill is not an error

A close request against a bill that is already in `CLOSING` or `CLOSED` SHALL be treated as satisfied rather than as a conflict. The system SHALL respond `202 Accepted` with the frozen invoice and SHALL report the `closedBy` trigger that actually caused the closure, so that a caller can distinguish having caused the close from finding it already done.

#### Scenario: Close request against a bill already closed by the deadline

- **WHEN** a client requests closure of a bill that has already closed because its `periodEnd` was reached
- **THEN** the system responds `202 Accepted` with the frozen total and line items and `closedBy` of `period_end`

#### Scenario: Repeated close request

- **WHEN** a client repeats a close request for a bill that its own earlier request already closed
- **THEN** the system responds `202 Accepted` with the same frozen total, the same line items, the same `closedAt`, and `closedBy` of `api_request`

### Requirement: Closed bills are terminal

The system SHALL NOT reopen a bill once it has entered `CLOSING` or `CLOSED`. No request SHALL cause a bill to return to `OPEN`, and no request SHALL alter the total or line items of a bill that has left `OPEN`.

#### Scenario: Reopening is not offered

- **WHEN** a client attempts to return a bill in `CLOSING` or `CLOSED` to `OPEN`
- **THEN** the system rejects the attempt and the bill's state, total, and line items are unchanged

### Requirement: A closed bill reports its total and every line item charged

A closed bill SHALL report the total amount being charged and the complete collection of line items comprising that total. Both SHALL be available in the response to the close request itself and on every subsequent retrieval of the bill.

#### Scenario: Close response carries the full invoice

- **WHEN** a bill carrying line items is closed
- **THEN** the response contains the total amount, its currency, and every line item included in that total

#### Scenario: Bill with no line items closes with a zero total

- **WHEN** a bill that has accrued no line items is closed
- **THEN** the bill closes successfully with a total of zero in its currency and an empty line item collection

### Requirement: Retrieve a bill by identifier

The system SHALL return a bill by its identifier in any state, reporting the customer it belongs to, its state, currency, fee period, current or final total, and its line items. For a bill in `CLOSING` or `CLOSED`, the response SHALL additionally report `closedAt` and `closedBy`.

A closed bill SHALL remain retrievable indefinitely, and SHALL NOT become unavailable through the passage of time.

The system SHALL distinguish a bill that does not exist from a bill it cannot currently reach. A bill that exists SHALL NOT be reported as absent because the workflow holding its live state is unreachable, unready, or has completed. Where the system cannot determine a bill's state, it SHALL report a retryable failure rather than an absence.

#### Scenario: Open bill is retrieved with its running total

- **WHEN** a client retrieves a bill in `OPEN`
- **THEN** the system responds `200 OK` with state `OPEN`, the customer it belongs to, the line items accrued so far, and their running total

#### Scenario: Closed bill is retrieved with its final invoice

- **WHEN** a client retrieves a bill in `CLOSED`
- **THEN** the system responds `200 OK` with state `CLOSED`, the customer it belongs to, the final total, every line item charged, `closedAt`, and `closedBy`

#### Scenario: The customer survives the workflow

- **WHEN** a client retrieves a bill whose workflow has completed, so that the invoice is read from durable storage
- **THEN** the response reports the same customer it reported while the bill was open

#### Scenario: Closed bill remains retrievable long after closure

- **WHEN** a client retrieves a bill that closed an arbitrarily long time ago
- **THEN** the system responds `200 OK` with the same final total and line items it reported at closure

#### Scenario: Unknown bill

- **WHEN** a client retrieves a bill identifier that does not exist
- **THEN** the system responds `404 Not Found`

#### Scenario: Existing open bill during a workflow outage

- **WHEN** a client retrieves a bill in `OPEN` while the workflow service is unreachable
- **THEN** the system responds `503 Service Unavailable` with a `Retry-After` header
- **AND** the system SHALL NOT respond `404 Not Found`, because the bill exists and its charges are intact

#### Scenario: Existing bill whose workflow is momentarily unready

- **WHEN** a client retrieves a bill whose workflow is running but not currently able to answer
- **THEN** the system responds `503 Service Unavailable` with a `Retry-After` header rather than reporting the bill as absent
