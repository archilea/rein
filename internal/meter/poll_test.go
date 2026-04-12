package meter

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// pollTestFile writes content at path, optionally setting a specific
// mtime so tests can simulate a "file was modified 5 seconds ago"
// scenario without sleeping.
func pollTestFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// waitForReload polls the holder until `check` returns true or the
// deadline is hit, and returns whether check passed. Used to wait for
// the PollLoop goroutine to swap a new pricer in.
func waitForReload(t *testing.T, holder *PricerHolder, check func(*Pricer) bool, deadline time.Duration) bool {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if check(holder.Load()) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestPollLoop_MtimeChangeTriggersReload(t *testing.T) {
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"initial":{"input_per_mtok":1,"output_per_mtok":1}}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 20*time.Millisecond, base, holder)
		close(done)
	}()

	// Give the loop one tick to read the initial mtime.
	time.Sleep(30 * time.Millisecond)

	// Rewrite the file with a new model and a future mtime so the
	// mtime-compare definitely sees a change (Chtimes rounds to the
	// filesystem's resolution, so advance by a full second).
	future := time.Now().Add(2 * time.Second)
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"updated":{"input_per_mtok":2,"output_per_mtok":2}}}}`, future)

	ok := waitForReload(t, holder, func(p *Pricer) bool {
		if p == nil {
			return false
		}
		_, found := p.Cost("openai", "updated", 1, 1)
		return found
	}, 2*time.Second)
	cancel()
	<-done
	if !ok {
		t.Fatalf("poll did not pick up the new 'updated' entry within deadline; log=%s", lb.buf.String())
	}
	// And we expect the 'initial' entry to no longer resolve (new file
	// replaced the old, not merged with it).
	if _, found := holder.Load().Cost("openai", "initial", 1, 1); found {
		t.Error("initial entry still resolvable after poll reload; did the loader append instead of replace?")
	}
}

func TestPollLoop_NoChangeNoReload(t *testing.T) {
	// Happy-path test for the mtime-skip branch: if the file's mtime
	// does not advance, PollLoop must NOT call TryReload, so no
	// "config reload succeeded" log line should be emitted between
	// ticks. This is the branch that protects operators against
	// useless CPU burn on a stable file.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"stable":{"input_per_mtok":1,"output_per_mtok":1}}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 20*time.Millisecond, base, holder)
		close(done)
	}()

	// Let the loop run for 150ms (~7 ticks) without touching the file.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	// Count "config reload succeeded" messages. The initial stat
	// happens BEFORE the first tick, so if the file did not change
	// there should be ZERO success messages. If there are any, the
	// mtime-skip branch is broken.
	successCount := 0
	for _, e := range lb.entries(t) {
		if e["msg"] == "config reload succeeded" {
			successCount++
		}
	}
	if successCount != 0 {
		t.Errorf("stable file should produce 0 'config reload succeeded' lines, got %d: %s",
			successCount, lb.buf.String())
	}
}

func TestPollLoop_StatErrorLogsAndContinues(t *testing.T) {
	// Regression guard for the stat-failure branch: if the file is
	// deleted under a running rein process, PollLoop must log an
	// ERROR and KEEP RUNNING. Dying on a transient stat error would
	// break operators who mv-and-restore a config file mid-edit.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"initial":{"input_per_mtok":1,"output_per_mtok":1}}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 20*time.Millisecond, base, holder)
		close(done)
	}()

	// Let the loop read the initial mtime.
	time.Sleep(40 * time.Millisecond)

	// Delete the file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Let a couple of ticks fail to stat.
	time.Sleep(80 * time.Millisecond)

	// Restore the file with a new mtime + new content.
	future := time.Now().Add(2 * time.Second)
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"restored":{"input_per_mtok":3,"output_per_mtok":3}}}}`, future)

	// PollLoop should pick up the restored file on the next successful
	// stat, proving it didn't exit during the delete window.
	ok := waitForReload(t, holder, func(p *Pricer) bool {
		_, found := p.Cost("openai", "restored", 1, 1)
		return found
	}, 2*time.Second)
	cancel()
	<-done
	if !ok {
		t.Fatalf("poll did not recover after transient file deletion; log=%s", lb.buf.String())
	}

	// And at least one "config poll stat failed" ERROR should have
	// been logged during the delete window.
	statErrorCount := 0
	for _, e := range lb.entries(t) {
		if e["msg"] == "config poll stat failed" {
			statErrorCount++
		}
	}
	if statErrorCount == 0 {
		t.Error("expected at least one 'config poll stat failed' log during delete window")
	}
}

func TestPollLoop_ContextCancelExits(t *testing.T) {
	// Cancelling the context must cause the goroutine to return.
	// Proving this via a timeout on close(done) is the clean shape.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 100*time.Millisecond, base, holder)
		close(done)
	}()

	// Give the loop a moment to start up.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good — the goroutine exited.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("PollLoop did not exit within 500ms of ctx cancel")
	}

	// "config poll stopped" log line should be present.
	stopFound := false
	for _, e := range lb.entries(t) {
		if e["msg"] == "config poll stopped" {
			stopFound = true
			break
		}
	}
	if !stopFound {
		t.Errorf("expected 'config poll stopped' log line on ctx cancel")
	}
}

func TestPollLoop_FileMissingAtStart(t *testing.T) {
	// If the path does not exist when PollLoop starts — common in K8s
	// where the ConfigMap volume may mount a few ms after the
	// container starts — the loop must NOT crash, must log stat
	// errors on each tick until the file appears, and then reload
	// correctly once it does.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	// File does NOT exist yet.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 20*time.Millisecond, base, holder)
		close(done)
	}()

	// Let a tick or two fail to find the file.
	time.Sleep(60 * time.Millisecond)

	// Now create the file.
	future := time.Now().Add(2 * time.Second)
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"late-arrival":{"input_per_mtok":4,"output_per_mtok":4}}}}`, future)

	ok := waitForReload(t, holder, func(p *Pricer) bool {
		_, found := p.Cost("openai", "late-arrival", 1, 1)
		return found
	}, 2*time.Second)
	cancel()
	<-done
	if !ok {
		t.Fatalf("poll did not pick up late-arriving file; log=%s", lb.buf.String())
	}
}

func TestPollLoop_ReloadFailureKeepsOldSnapshotAndContinues(t *testing.T) {
	// A bad file (unknown version, for example) under a running poll
	// must not: (a) crash the loop, (b) clobber the holder with nil,
	// or (c) stop picking up future valid changes.
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)
	initialHolder := holder.Load()

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"good-one":{"input_per_mtok":1,"output_per_mtok":1}}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	done := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 20*time.Millisecond, base, holder)
		close(done)
	}()

	// Advance mtime + write a BAD file (unknown version).
	time.Sleep(30 * time.Millisecond)
	future := time.Now().Add(2 * time.Second)
	pollTestFile(t, path, `{"version":"999","models":{"openai":{}}}`, future)
	time.Sleep(80 * time.Millisecond)

	// Holder should still point at the original base pricer.
	if holder.Load() != initialHolder {
		t.Error("bad reload replaced the holder; previous snapshot was supposed to stay active")
	}

	// Now write a GOOD file with an even later mtime.
	laterFuture := time.Now().Add(4 * time.Second)
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"recovered":{"input_per_mtok":5,"output_per_mtok":5}}}}`, laterFuture)

	ok := waitForReload(t, holder, func(p *Pricer) bool {
		_, found := p.Cost("openai", "recovered", 1, 1)
		return found
	}, 2*time.Second)
	cancel()
	<-done
	if !ok {
		t.Fatalf("poll did not recover after a bad-version reload; log=%s", lb.buf.String())
	}
}

// TestPollLoop_ConcurrentReadersDuringSwap is a race-detector regression
// guard. Under -race, any unsynchronized access to the Pricer during a
// Swap would trip the detector. This test drives PollLoop alongside a
// reader goroutine that continuously calls Load().Cost() to prove the
// publication is safe.
func TestPollLoop_ConcurrentReadersDuringSwap(t *testing.T) {
	base := embeddedForTests(t)
	holder := NewPricerHolder(base)

	dir := t.TempDir()
	path := filepath.Join(dir, "rein.json")
	pollTestFile(t, path, `{"version":"1","models":{"openai":{"m0":{"input_per_mtok":1,"output_per_mtok":1}}}}`, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := newJSONLogBuffer()
	pollDone := make(chan struct{})
	go func() {
		PollLoop(ctx, lb.logger, path, 10*time.Millisecond, base, holder)
		close(pollDone)
	}()

	readerDone := make(chan struct{})
	var reads atomic.Int64
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				p := holder.Load()
				if p != nil {
					_, _ = p.Cost("openai", "gpt-4o", 1000, 500)
				}
				reads.Add(1)
			}
		}
	}()

	// Drive several reloads by advancing mtime repeatedly.
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		future := time.Now().Add(time.Duration(2*(i+1)) * time.Second)
		pollTestFile(t, path,
			`{"version":"1","models":{"openai":{"m`+string(rune('0'+i))+`":{"input_per_mtok":1,"output_per_mtok":1}}}}`,
			future)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-pollDone
	<-readerDone

	if reads.Load() == 0 {
		t.Errorf("reader goroutine did not run; test harness bug")
	}
}
