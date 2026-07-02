// heartbeat_test.go — tests for the progress-heartbeat seam (issue #19).
//
// heartbeatInterval and heartbeatWriter are package vars specifically so
// tests can shrink the tick period and capture output without sleeping
// real 30s intervals or redirecting the real stderr file descriptor.
// Every test that mutates either var saves and restores it via
// t.Cleanup so package-global state never leaks across tests — none of
// these use t.Parallel() for that reason (mirrors the stub-LLM harness
// convention in llm_stub_test.go).
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// heartbeatBuf is a concurrency-safe io.Writer wrapper around bytes.Buffer.
// The heartbeat goroutine writes from a background goroutine while the
// test reads/polls from the main goroutine, so a plain bytes.Buffer would
// race.
type heartbeatBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *heartbeatBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *heartbeatBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *heartbeatBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

// waitForCondition polls cond until it returns true or timeout elapses,
// failing the test on timeout. Used instead of a fixed sleep so the
// emits-a-line test isn't flaky under load while still bounding runtime.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestHeartbeatWriter_DefaultsToStderr(t *testing.T) {
	if heartbeatWriter != io.Writer(os.Stderr) {
		t.Errorf("expected heartbeatWriter to default to os.Stderr; got %v", heartbeatWriter)
	}
}

// TestStartHeartbeat_EmitsProgressLine shrinks heartbeatInterval to
// milliseconds and captures heartbeatWriter, then asserts the goroutine
// actually writes a line mentioning the op label once a tick fires.
func TestStartHeartbeat_EmitsProgressLine(t *testing.T) {
	origInterval, origWriter := heartbeatInterval, heartbeatWriter
	t.Cleanup(func() { heartbeatInterval, heartbeatWriter = origInterval, origWriter })

	heartbeatInterval = time.Millisecond
	buf := &heartbeatBuf{}
	heartbeatWriter = buf

	stop := startHeartbeat(context.Background(), "test-op")
	defer stop()

	waitForCondition(t, 2*time.Second, func() bool { return buf.Len() > 0 })

	out := buf.String()
	if !strings.Contains(out, "test-op") {
		t.Errorf("expected heartbeat line to mention op label %q; got %q", "test-op", out)
	}
	if !strings.Contains(out, "heartbeat") {
		t.Errorf("expected heartbeat line to contain %q; got %q", "heartbeat", out)
	}
}

// TestStartHeartbeat_StopBeforeTickEmitsNothing sets a long interval (so
// no real tick can fire during the test) and verifies stop() called
// immediately after start produces zero output.
func TestStartHeartbeat_StopBeforeTickEmitsNothing(t *testing.T) {
	origInterval, origWriter := heartbeatInterval, heartbeatWriter
	t.Cleanup(func() { heartbeatInterval, heartbeatWriter = origInterval, origWriter })

	heartbeatInterval = time.Hour
	buf := &heartbeatBuf{}
	heartbeatWriter = buf

	stop := startHeartbeat(context.Background(), "test-op")
	stop()

	if buf.Len() != 0 {
		t.Errorf("expected no output when stopped before first tick; got %q", buf.String())
	}
}

// TestStartHeartbeat_StopBlocksAndGoroutineExits proves stop() is a true
// blocking join, not a fire-and-forget signal: it (a) returns within a
// bounded timeout — a leaked/deadlocked goroutine would hang forever and
// fail this via the select/timeout — and (b) the writer receives no more
// output after stop() returns, even after waiting past several more
// intervals, which would only be possible if the ticking goroutine had
// truly exited.
//
// goleak isn't a dependency of this module (module graph is deliberately
// lean — see CLAUDE.md), so this done-channel + timeout pattern is the
// substitute for a leak assertion.
func TestStartHeartbeat_StopBlocksAndGoroutineExits(t *testing.T) {
	origInterval, origWriter := heartbeatInterval, heartbeatWriter
	t.Cleanup(func() { heartbeatInterval, heartbeatWriter = origInterval, origWriter })

	heartbeatInterval = time.Millisecond
	buf := &heartbeatBuf{}
	heartbeatWriter = buf

	stop := startHeartbeat(context.Background(), "test-op")
	waitForCondition(t, 2*time.Second, func() bool { return buf.Len() > 0 })

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2s — heartbeat goroutine leaked")
	}

	n := buf.Len()
	time.Sleep(50 * time.Millisecond)
	if buf.Len() != n {
		t.Errorf("writer kept receiving output after stop() returned (%d -> %d bytes) — goroutine did not actually exit", n, buf.Len())
	}
}

// TestStartHeartbeat_ContextCancelStopsGoroutine verifies ctx cancellation
// alone (no explicit stop() call racing it) also lets the goroutine exit
// — stop() must still be safe and prompt to call afterward, matching the
// "defer stop()" call pattern used at every call site even when the
// call's own context deadline fired first.
func TestStartHeartbeat_ContextCancelStopsGoroutine(t *testing.T) {
	origInterval := heartbeatInterval
	t.Cleanup(func() { heartbeatInterval = origInterval })
	heartbeatInterval = time.Hour // never ticks during this test

	ctx, cancel := context.WithCancel(context.Background())
	stop := startHeartbeat(ctx, "test-op")
	cancel()

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after ctx cancellation — heartbeat goroutine leaked")
	}
}

// TestStartHeartbeat_StopIsIdempotent guards the sync.Once inside stop():
// callers that both defer stop() and (in some error path) call it again
// explicitly must not panic on a double close of the internal channel.
func TestStartHeartbeat_StopIsIdempotent(t *testing.T) {
	origInterval := heartbeatInterval
	t.Cleanup(func() { heartbeatInterval = origInterval })
	heartbeatInterval = time.Hour

	stop := startHeartbeat(context.Background(), "test-op")
	stop()
	stop()
}
