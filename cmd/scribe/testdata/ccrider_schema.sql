-- Minimal ccrider sessions.db fixture schema for tests.
-- Mirrors only the columns scribe actually queries (sessions.go,
-- sync_sessions.go, hook.go, triage.go, session_transcript.go,
-- adoption.go). Never point tests at the real
-- ~/.config/ccrider/sessions.db.
CREATE TABLE sessions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT UNIQUE,
    project_path  TEXT,
    message_count INTEGER DEFAULT 0,
    created_at    TEXT,
    updated_at    TEXT,
    summary       TEXT,
    llm_summary   TEXT,
    -- provider distinguishes claude vs codex sessions; the watcher
    -- (watch.go) filters on it, so the fixture must carry it too.
    provider      TEXT
);

CREATE TABLE messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   INTEGER REFERENCES sessions(id),
    type         TEXT,
    text_content TEXT,
    -- content mirrors ccrider's raw tool-payload column (the verbatim
    -- "message" JSON from the source JSONL line — see
    -- session_transcript.go and adoption.go for what scribe reads out
    -- of it). Additive: existing INSERT INTO messages (session_id,
    -- type, text_content) calls keep working unchanged, defaulting to
    -- NULL, and every query touching this column already uses COALESCE.
    content      TEXT,
    -- sequence mirrors ccrider's per-session monotonic ordinal.
    -- Defaults to 0 so pre-existing fixture inserts that don't set it
    -- still sort deterministically (ORDER BY sequence ASC, id ASC).
    sequence     INTEGER DEFAULT 0
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
