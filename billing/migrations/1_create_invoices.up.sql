-- Closed invoices and the line items that comprise them.
--
-- This is the system of record for a bill once its fee period has ended. A
-- Temporal query answers for a bill that is still running, but a query is a view
-- of live process state, not storage: workflow history is retained for a limited
-- window and then discarded. A financial artifact has to outlive the process that
-- produced it, so the invoice is written here at close and read from here
-- thereafter.
--
-- Rows are written only by workflow activities, never by the API layer, which
-- keeps the workflow the single writer for a bill.

CREATE TABLE invoice (
    bill_id           TEXT        PRIMARY KEY,
    state             TEXT        NOT NULL,
    currency          TEXT        NOT NULL,
    period_start      TIMESTAMPTZ NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,

    -- Money is stored as an exact integer count of the currency's minor units.
    -- BIGINT, never NUMERIC or DOUBLE PRECISION: the value that lands here is the
    -- value that was charged, and no representation in this column can round it.
    total_minor_units BIGINT      NOT NULL,

    closed_at         TIMESTAMPTZ NOT NULL,
    closed_by         TEXT        NOT NULL,
    persisted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT invoice_state_is_closing_or_closed
        CHECK (state IN ('CLOSING', 'CLOSED')),
    CONSTRAINT invoice_closed_by_is_known
        CHECK (closed_by IN ('api_request', 'period_end')),
    CONSTRAINT invoice_period_ends_after_it_begins
        CHECK (period_end > period_start)
);

CREATE TABLE invoice_line_item (
    bill_id            TEXT        NOT NULL REFERENCES invoice (bill_id) ON DELETE CASCADE,

    -- Supplied by the caller and unique within the bill. This is the same key the
    -- workflow deduplicates on, so the invariant "the fee for a given source event
    -- appears at most once on this bill" is enforced by the schema as well as by
    -- the domain.
    item_id            TEXT        NOT NULL,

    amount_minor_units BIGINT      NOT NULL,
    currency           TEXT        NOT NULL,
    description        TEXT        NOT NULL,
    accrued_at         TIMESTAMPTZ NOT NULL,

    -- Accrual order, so the invoice lists its charges the way the bill accrued
    -- them rather than in whatever order the database returns.
    seq                INTEGER     NOT NULL,

    PRIMARY KEY (bill_id, item_id)
);

CREATE INDEX invoice_line_item_by_bill_in_order
    ON invoice_line_item (bill_id, seq);
