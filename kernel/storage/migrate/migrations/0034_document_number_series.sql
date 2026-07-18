-- WP-1.4 invoicing: gapless document number sequences (INV-F6). One counter per
-- (tenant, series); next_value is the number the *next* accepted document will
-- receive. Numbers are allocated only at server acceptance (a document post),
-- inside the accepting transaction, with a row-locking UPDATE … next_value + 1 —
-- so concurrent posts serialize on the row (no dup, no gap) and a post that
-- fails before allocation consumes nothing. Drafts never touch this table.
-- tenant_id is the first PK column (tenancy commandment); RLS is added in 0035.
CREATE TABLE document_number_series (
	tenant_id TEXT NOT NULL,
	series TEXT NOT NULL,
	next_value BIGINT NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (tenant_id, series)
);
