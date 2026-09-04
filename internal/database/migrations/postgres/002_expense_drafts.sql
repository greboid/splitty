-- Draft expenses: one saved in-progress form per user and group. Clicking
-- "Add expense" opens (creating if needed) the draft, and the form autosaves
-- to form_body (a urlencoded snapshot of the expense form fields), so a
-- refresh or process kill on mobile never loses entered data. receipt_file
-- is lifted out of the body so scanned receipt images can be authorised to
-- the draft's owner before the expense exists.
-- +goose Up
CREATE TABLE expense_drafts (
    id           TEXT PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id     BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    form_body    TEXT NOT NULL DEFAULT '',
    receipt_file TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, group_id)
);
CREATE INDEX idx_expense_drafts_receipt ON expense_drafts(receipt_file);
