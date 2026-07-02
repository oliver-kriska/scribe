// heartbeat.go — progress heartbeat for long-running LLM calls (issue #19).
//
// Long envelope applies (dream, deep ingest) go silent for 2+ minutes
// between the "prompt N chars" log line and the result. That makes a
// hung run and a slow run indistinguishable in a cron log until the
// per-op timeout finally fires. startHeartbeat fills the gap: while a
// blocking LLM call is in flight, a background goroutine writes one
// progress line every heartbeatInterval so cron-log forensics can tell
// "still working" from "stuck".
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// heartbeatWriter is where startHeartbeat writes its progress lines. It's
// a package var — not a hardcoded os.Stderr reference inside the
// goroutine — purely so tests can swap in a buffer instead of capturing a
// real file descriptor. Production code never reassigns it.
//
// This must stay stderr, never stdout: SCRIBE_LOG_FORMAT=json makes
// stdout strict NDJSON (see logging.go's slog.NewJSONHandler(os.Stdout,
// ...)), and downstream consumers parse every stdout line as one JSON
// record. A heartbeat line on stdout would corrupt that stream, so this
// intentionally bypasses logMsg/slog rather than routing through it.
var heartbeatWriter io.Writer = os.Stderr

// heartbeatInterval is how often startHeartbeat emits a progress line. A
// package var (not a const) so tests can shrink it to milliseconds
// instead of sleeping 30 real seconds per assertion. Production code
// never reassigns it.
var heartbeatInterval = 30 * time.Second

// startHeartbeat starts a background goroutine that writes one progress
// line to heartbeatWriter every heartbeatInterval for as long as the
// returned stop func hasn't been called (or ctx hasn't been canceled),
// and returns that stop func.
//
// Callers should start the heartbeat immediately before a blocking LLM
// call and defer the stop func so it fires the moment the call returns:
//
//	defer startHeartbeat(ctx, op)()
//	resp, err := client.Do(req)
//
// stop() blocks until the goroutine has actually exited. This is a hard
// requirement, not a nicety: callers that fire off many concurrent LLM
// calls (contextualize's worker pool, pass-1 chapter fan-out) would leak
// one goroutine per call forever if stop() returned before the goroutine
// observed the signal.
//
// op is a short label — the same op label already plumbed via
// withOpLabel/opLabelFromContext for the cost ledger — so a heartbeat
// line reads "heartbeat: absorb-pass1-chapter still running (90s)"
// instead of something generic.
func startHeartbeat(ctx context.Context, op string) (stop func()) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	started := time.Now()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(started).Round(time.Second)
				fmt.Fprintf(heartbeatWriter, "[%s] heartbeat: %s still running (%s)\n",
					time.Now().Format("2006-01-02 15:04:05"), op, elapsed)
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() { close(stopCh) })
		<-doneCh
	}
}
