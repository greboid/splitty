-- +goose Up
CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT UNIQUE,
    name          TEXT NOT NULL,
    password_hash TEXT,
    is_guest      BOOLEAN NOT NULL DEFAULT FALSE,
    invited_by    INTEGER REFERENCES users(id),
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE groups (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'group' CHECK (kind IN ('group', 'direct')),
    created_by      INTEGER NOT NULL REFERENCES users(id),
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE invites (
    id          INTEGER PRIMARY KEY,
    email       TEXT NOT NULL,
    group_id    INTEGER REFERENCES groups(id) ON DELETE SET NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    created_by  INTEGER NOT NULL REFERENCES users(id),
    expires_at  TIMESTAMP NOT NULL,
    accepted_by INTEGER REFERENCES users(id),
    accepted_at TIMESTAMP
);

CREATE TABLE memberships (
    group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);

CREATE TABLE expenses (
    id           INTEGER PRIMARY KEY,
    group_id     INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    description  TEXT NOT NULL,
    notes        TEXT NOT NULL DEFAULT '',
    category     TEXT NOT NULL DEFAULT 'general',
    date         DATE NOT NULL,
    is_payment   BOOLEAN NOT NULL DEFAULT FALSE,
    split_mode   TEXT NOT NULL DEFAULT 'even',
    receipt_file TEXT,
    created_by   INTEGER NOT NULL REFERENCES users(id),
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at   TIMESTAMP
);
CREATE INDEX idx_expenses_group ON expenses(group_id, date);

CREATE TABLE expense_shares (
    expense_id INTEGER NOT NULL REFERENCES expenses(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    paid       INTEGER NOT NULL DEFAULT 0,
    owed       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (expense_id, user_id)
);
CREATE INDEX idx_expense_shares_user ON expense_shares(user_id);

CREATE TABLE expense_items (
    id          INTEGER PRIMARY KEY,
    expense_id  INTEGER NOT NULL REFERENCES expenses(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    description TEXT NOT NULL,
    amount      INTEGER NOT NULL
);
CREATE INDEX idx_expense_items_expense ON expense_items(expense_id, position);

CREATE TABLE item_shares (
    item_id INTEGER NOT NULL REFERENCES expense_items(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, user_id)
);

CREATE TABLE activity (
    id         INTEGER PRIMARY KEY,
    group_id   INTEGER REFERENCES groups(id) ON DELETE CASCADE,
    expense_id INTEGER REFERENCES expenses(id) ON DELETE SET NULL,
    actor_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    type       TEXT NOT NULL,
    detail     TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_activity_group ON activity(group_id, id DESC);

CREATE TABLE receipts (
    name         TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    data         BLOB NOT NULL,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
