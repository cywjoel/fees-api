## MODIFIED Requirements

### Requirement: Create a bill for a fee period

The system SHALL create a bill on request, denominated in a single currency and bounded by a caller-supplied fee period. A newly created bill SHALL be in the `OPEN` state and SHALL have a total of zero in its currency.

The request MUST supply a currency, a `periodStart`, and a `periodEnd`. The system SHALL reject a request whose `periodEnd` is not strictly after its `periodStart`.

The system SHALL additionally reject a request whose `periodEnd` has already passed. A bill exists to accrue charges over a period that is still running; one whose period has ended can never accept a charge, and creating it would report success for a bill that is already closing.

A fee period SHALL be recorded at a resolution the system can durably retain, and the bill SHALL report the period it recorded rather than the one it was sent. A caller that states a period more precisely than the system can store must be told what was actually recorded, because that is the value every later comparison and every later read will use.

#### Scenario: Bill is created successfully

- **WHEN** a client requests a new bill with currency `USD`, a `periodStart`, and a later `periodEnd` that has not yet passed
- **THEN** the system responds `201 Created` with a bill identifier, state `OPEN`, currency `USD`, a total of `0.00 USD`, and an empty line item collection

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

The system SHALL accept an idempotency key on bill creation and SHALL create at most one bill per key. A repeated request bearing a key that has already been used, and whose currency and fee period match the bill that key created, SHALL return the existing bill rather than creating a second one, and SHALL do so whether the existing bill is still open or has already closed.

A request bearing a used key whose currency or fee period differs from the existing bill SHALL be rejected. Returning the existing bill would answer a request the caller did not make, and the caller would then accrue charges against a bill whose currency or period is not the one it asked for.

Exactly one request SHALL be reported as having created the bill. Concurrent requests sharing one key SHALL yield one `201 Created`; every other request SHALL be reported as returning an existing bill.

#### Scenario: Retried creation returns the existing open bill

- **WHEN** a client repeats a creation request with an idempotency key that maps to a bill still in `OPEN`, with matching currency and fee period
- **THEN** the system responds `200 OK` with the existing bill and does not create a second bill

#### Scenario: Retried creation returns an already-closed bill

- **WHEN** a client repeats a creation request with an idempotency key that maps to a bill in `CLOSED`, with matching currency and fee period
- **THEN** the system responds `200 OK` with the existing closed bill and its final invoice, and does not create a second bill

#### Scenario: Key reused with different parameters is rejected

- **WHEN** a client sends a creation request with an idempotency key that already created a bill, but with a different currency or a different fee period
- **THEN** the system responds `409 Conflict` and no second bill is created
- **AND** the existing bill is not returned as though it satisfied the request

#### Scenario: Concurrent creations sharing a key report one creation

- **WHEN** several creation requests carrying the same idempotency key and identical parameters are processed concurrently
- **THEN** exactly one responds `201 Created` and the others respond `200 OK`, and exactly one bill exists for that key

#### Scenario: Fee period is compared by instant, not by representation

- **WHEN** a client retries a creation request stating the same fee period in a different time zone offset
- **THEN** the period is treated as matching, because it denotes the same instant, and the system responds `200 OK` with the existing bill

### Requirement: Retrieve a bill by identifier

The system SHALL return a bill by its identifier in any state, reporting its state, currency, fee period, current or final total, and its line items. For a bill in `CLOSING` or `CLOSED`, the response SHALL additionally report `closedAt` and `closedBy`.

A closed bill SHALL remain retrievable indefinitely, and SHALL NOT become unavailable through the passage of time.

The system SHALL distinguish a bill that does not exist from a bill it cannot currently reach. A bill that exists SHALL NOT be reported as absent because the workflow holding its live state is unreachable, unready, or has completed. Where the system cannot determine a bill's state, it SHALL report a retryable failure rather than an absence.

#### Scenario: Open bill is retrieved with its running total

- **WHEN** a client retrieves a bill in `OPEN`
- **THEN** the system responds `200 OK` with state `OPEN`, the line items accrued so far, and their running total

#### Scenario: Closed bill is retrieved with its final invoice

- **WHEN** a client retrieves a bill in `CLOSED`
- **THEN** the system responds `200 OK` with state `CLOSED`, the final total, every line item charged, `closedAt`, and `closedBy`

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
