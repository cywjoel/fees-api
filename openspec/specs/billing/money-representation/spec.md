# billing/money-representation Specification

## Purpose
Defines how monetary amounts are represented, rounded, and constrained across bills and line items, so that no charge is subject to floating-point drift and every bill totals exactly.

## Requirements

### Requirement: Monetary amounts are exact

The system SHALL represent every monetary amount as an exact integer count of the currency's minor units together with an ISO 4217 currency code. The system SHALL NOT represent, transport, or store a monetary amount as a binary floating-point number at any point.

The number of minor units in a major unit SHALL be determined by the currency, not assumed to be uniform across currencies.

#### Scenario: Amount round-trips without drift

- **WHEN** a client submits a line item of `0.10 USD` and later retrieves it
- **THEN** the amount returned is exactly `0.10 USD`

#### Scenario: Repeated addition does not accumulate error

- **WHEN** a bill accrues one hundred line items of `0.10 USD` each
- **THEN** the bill's total is exactly `10.00 USD`

#### Scenario: Amount is expressed with its currency

- **WHEN** a client retrieves any monetary amount from the system
- **THEN** the amount is accompanied by an ISO 4217 currency code identifying the currency it is denominated in

### Requirement: Supported currencies

The system SHALL support `GEL` and `USD`. The system SHALL reject any request denominating a bill or a line item in a currency it does not support.

#### Scenario: Supported currency is accepted

- **WHEN** a client creates a bill in `GEL` or in `USD`
- **THEN** the bill is created in that currency

#### Scenario: Unsupported currency is rejected

- **WHEN** a client submits a bill or a line item denominated in a currency outside the supported set
- **THEN** the system responds `422 Unprocessable Content` and the request has no effect

### Requirement: A bill is denominated in exactly one currency

A bill's currency SHALL be fixed when the bill is created and SHALL NOT change thereafter. Every line item on a bill SHALL be denominated in that bill's currency.

#### Scenario: Bill currency is fixed at creation

- **WHEN** a client retrieves a bill at any point in its lifecycle
- **THEN** its currency is the same currency it was created with

#### Scenario: Line item in a different currency is rejected

- **WHEN** a client adds a line item denominated in `GEL` to a bill denominated in `USD`
- **THEN** the system responds `422 Unprocessable Content`, no line item is added, and the bill's total is unchanged

#### Scenario: Currency mismatch is distinguishable from a closed bill

- **WHEN** a client adds a line item in the wrong currency to a bill in `OPEN`
- **THEN** the system responds `422 Unprocessable Content` rather than `409 Conflict`, distinguishing a malformed charge from a charge arriving too late

### Requirement: Line item amounts are whole minor units

Each line item amount SHALL be a whole number of minor units in the bill's currency. Where a fee is derived from a rate applied to a base amount, the derived amount SHALL be rounded to whole minor units at the point the line item is created, and the rounded amount SHALL be the amount charged and reported.

The system SHALL round half-up: a derived amount SHALL be rounded to the nearest whole minor unit, and an amount falling exactly halfway SHALL be rounded away from zero. The system SHALL apply this rule consistently to every such derivation.

#### Scenario: Derived fee is rounded when the line item is created

- **WHEN** a fee derived from a rate resolves to a fractional number of minor units
- **THEN** the line item is stored and reported as a whole number of minor units, and that rounded amount is what the customer is charged

#### Scenario: Fee falling exactly halfway rounds away from zero

- **WHEN** a fee derived from a rate resolves to exactly `4.5` minor units
- **THEN** the line item is charged as `5` minor units

#### Scenario: Reported amount is the charged amount

- **WHEN** a client retrieves any line item
- **THEN** the amount reported is exactly the amount contributing to the bill's total, with no undisclosed fractional remainder

### Requirement: A bill total is the exact sum of its line items

A bill's total SHALL equal the arithmetic sum of its line item amounts, in the bill's currency, with no further rounding applied at the total. This SHALL hold for the running total of an open bill and for the frozen total of a closed one.

#### Scenario: Total equals the sum of line items

- **WHEN** a client retrieves a bill carrying line items
- **THEN** the reported total equals the exact sum of those line item amounts

#### Scenario: Frozen total equals the sum of the charged items

- **WHEN** a bill is closed
- **THEN** the final total equals the exact sum of every line item reported on that bill, and no rounding difference exists between them

#### Scenario: Empty bill totals zero

- **WHEN** a client retrieves a bill that has accrued no line items
- **THEN** the reported total is zero in the bill's currency
