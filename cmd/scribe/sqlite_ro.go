package main

import (
	"database/sql"
	"strings"
)

// openSQLiteRO opens an SQLite database scribe must never write to
// (ccrider's sessions.db, the iMessage chat.db).
//
// go-sqlite3 honors `mode=ro` — and every other URI parameter — ONLY
// when the DSN starts with "file:". A bare `<path>?mode=ro` had the
// query stripped and the file opened READWRITE|CREATE, so scribe held
// writable handles on ccrider's WAL database (checkpoint contention with
// its writer) and `scribe status`/`doctor` on a machine without ccrider
// silently created an empty sessions.db. `_query_only` adds
// PRAGMA query_only as a second lock so an accidental write statement
// fails even if the open mode ever regresses.
//
// sql.Open is lazy: a missing file surfaces as "unable to open database
// file" on the first query, not here — callers already treat query
// errors as "ccrider unavailable".
func openSQLiteRO(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+sqliteURIPath(path)+"?mode=ro&_query_only=1")
}

// sqliteURIPath escapes the characters that would otherwise be parsed
// as URI syntax inside a `file:` DSN. Spaces and other bytes pass
// through untouched — SQLite accepts them literally.
func sqliteURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}
