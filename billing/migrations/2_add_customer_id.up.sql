-- Every invoice belongs to a customer.
--
-- NOT NULL with no default and no backfill, which is only possible because no
-- real billing data exists yet: every row before this migration was produced by
-- the acceptance walkthrough, the end-to-end suite and the review reproductions.
-- The store is reset rather than migrated.
--
-- The alternatives were rejected for the same reason they were tempting. A
-- nullable column is honest about bills that genuinely had no owner, but leaves
-- "an invoice with no payee" representable in the schema forever, long after the
-- rows that justified it are gone. A backfilled sentinel records something that
-- was never true, and sentinel customers outlive the migrations that introduce
-- them.
--
-- This option stops being available the first time this service bills anyone.
ALTER TABLE invoice ADD COLUMN customer_id TEXT NOT NULL;

-- Reading a customer's invoices is the query this column exists to serve.
CREATE INDEX invoice_by_customer ON invoice (customer_id);

-- Deliberately NOT added to invoice_line_item. An item's customer is its bill's,
-- and storing it twice invites the two to disagree - which is the defect the
-- line item's optional customer check exists to catch, not to enshrine.
