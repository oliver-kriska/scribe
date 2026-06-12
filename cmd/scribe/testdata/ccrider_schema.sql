-- Minimal ccrider sessions.db fixture schema for tests.
-- Mirrors only the columns scribe actually queries (sessions.go,
-- sync_sessions.go, hook.go, triage.go). Never point tests at the real
-- ~/.config/ccrider/sessions.db.
CREATE TABLE sessions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT UNIQUE,
    project_path  TEXT,
    message_count INTEGER DEFAULT 0,
    created_at    TEXT,
    updated_at    TEXT,
    summary       TEXT,
    llm_summary   TEXT
);

CREATE TABLE messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   INTEGER REFERENCES sessions(id),
    type         TEXT,
    text_content TEXT
);

-- External-content FTS5 tables; tests insert rows manually with
-- INSERT INTO messages_fts(rowid, text_content) so the rowid matches
-- messages.id, the join key scribe's queries rely on.
CREATE VIRTUAL TABLE messages_fts USING fts5(
    text_content,
    content='messages',
    content_rowid='id'
);

CREATE VIRTUAL TABLE messages_fts_code USING fts5(
    text_content,
    content='messages',
    content_rowid='id'
);
