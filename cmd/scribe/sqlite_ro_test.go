package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenSQLiteRO_RefusesWrites is the regression for the bare
// `?mode=ro` DSN: without the `file:` prefix go-sqlite3 dropped the
// parameter and CREATE TABLE succeeded on what every caller believed was
// a read-only handle.
func TestOpenSQLiteRO_RefusesWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "with space #1", "sessions.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.ExecContext(ctx, `CREATE TABLE sessions (session_id TEXT); INSERT INTO sessions VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	ro, err := openSQLiteRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read through the RO handle: n=%d err=%v", n, err)
	}
	for _, stmt := range []string{
		`INSERT INTO sessions VALUES ('b')`,
		`CREATE TABLE scribe_wrote_this (x)`,
		`DELETE FROM sessions`,
	} {
		if _, err := ro.ExecContext(ctx, stmt); err == nil {
			t.Errorf("%q succeeded on a read-only handle", stmt)
		} else if !strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%q: unexpected error %v", stmt, err)
		}
	}
}

// TestOpenSQLiteRO_DoesNotCreateMissingDB: the old DSN opened with
// SQLITE_OPEN_CREATE, so `scribe status` on a machine without ccrider
// left an empty sessions.db behind.
func TestOpenSQLiteRO_DoesNotCreateMissingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing.db")
	db, err := openSQLiteRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err == nil {
		t.Error("ping of a missing database must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("read-only open must not create the file")
	}
}
