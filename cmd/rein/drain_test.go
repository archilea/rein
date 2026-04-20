package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/archilea/rein/internal/concurrency"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
	"github.com/archilea/rein/internal/meter"
	"github.com/archilea/rein/internal/proxy"
	"github.com/archilea/rein/internal/rates"
)

// newTestProxyForReadyz builds a bare-bones proxy just so the readyzHandler
// can close over its IsDraining flag. Dependencies are all in-memory; no
// network, no SQLite.
func newTestProxyForReadyz(t *testing.T) *proxy.Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(keys.NewMemory(), killswitch.NewMemory(),
		meter.NewMemory(), rates.NewMemory(), concurrency.NewMemory(),
		meter.NewPricerHolder(pricer),
		"https://api.openai.com", "https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadyzHandler_ReadyBeforeDrain covers the default branch: a
// process that has not received SIGTERM returns 200 + ready. Required
// by #76 so Kubernetes readinessProbe marks the pod healthy at boot.
func TestReadyzHandler_ReadyBeforeDrain(t *testing.T) {
	p := newTestProxyForReadyz(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	readyzHandler(p)(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", ct)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ready" {
		t.Errorf("status: got %q want ready", body.Status)
	}
}

// TestReadyzHandler_DrainingAfterSetDraining pins the 503/draining
// branch. The same Proxy instance whose flag the signal handler flips
// is the one the handler reads, so this test is a regression fence for
// the factory closure.
func TestReadyzHandler_DrainingAfterSetDraining(t *testing.T) {
	p := newTestProxyForReadyz(t)
	p.SetDraining(true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyzHandler(p)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", rec.Code)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "draining" {
		t.Errorf("status: got %q want draining", body.Status)
	}
}

// TestHealthz_DoesNotFlipOnDrain is the load-bearing split between
// liveness and readiness for Kubernetes. Flipping /healthz during
// drain would cause the livenessProbe to fail, triggering a container
// restart, which would kill in-flight requests — the exact opposite
// of what the drain window is for. #76 acceptance criterion.
func TestHealthz_DoesNotFlipOnDrain(t *testing.T) {
	// handleHealthz is a package-scope function, unaffected by the
	// draining flag. The test confirms that claim by calling it with
	// and without the flag set; both calls must return 200.
	p := newTestProxyForReadyz(t)

	for _, label := range []string{"before_drain", "after_drain"} {
		if label == "after_drain" {
			p.SetDraining(true)
		}
		t.Run(label, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			handleHealthz(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("/healthz %s: got %d want 200", label, rec.Code)
			}
		})
	}
}

// fakeConn is a minimal net.Conn for exercising inflightConnTracker
// without opening real sockets. Only the pointer identity matters to
// the tracker; none of the I/O methods are ever called.
type fakeConn struct{ net.Conn }

// TestInflightConnTracker_ActiveIncrementsThenDecrements covers the
// happy-path lifecycle of a single connection: StateNew → StateActive
// (inflight=1) → StateIdle (inflight=0). StateNew itself must NOT
// increment since it fires before the first byte of the request.
func TestInflightConnTracker_ActiveIncrementsThenDecrements(t *testing.T) {
	tr := newInflightConnTracker()
	c := &fakeConn{}

	tr.observe(c, http.StateNew)
	if got := tr.inflight(); got != 0 {
		t.Errorf("StateNew inflight: got %d want 0", got)
	}
	tr.observe(c, http.StateActive)
	if got := tr.inflight(); got != 1 {
		t.Errorf("StateActive inflight: got %d want 1", got)
	}
	tr.observe(c, http.StateIdle)
	if got := tr.inflight(); got != 0 {
		t.Errorf("StateIdle inflight: got %d want 0", got)
	}
}

// TestInflightConnTracker_IdleWithoutActiveIsNoOp guards the
// double-decrement failure mode. A StateIdle transition on a
// connection we never saw in StateActive must not push the counter
// negative. Without the sync.Map guard the count would read -1 here.
func TestInflightConnTracker_IdleWithoutActiveIsNoOp(t *testing.T) {
	tr := newInflightConnTracker()
	c := &fakeConn{}

	tr.observe(c, http.StateIdle)
	if got := tr.inflight(); got != 0 {
		t.Errorf("spurious StateIdle inflight: got %d want 0", got)
	}
	tr.observe(c, http.StateClosed)
	if got := tr.inflight(); got != 0 {
		t.Errorf("spurious StateClosed inflight: got %d want 0", got)
	}
}

// TestInflightConnTracker_KeepAliveReuse covers a connection that
// serves multiple requests (StateActive → StateIdle → StateActive →
// StateClosed). The counter must reach 1 twice and end at 0.
func TestInflightConnTracker_KeepAliveReuse(t *testing.T) {
	tr := newInflightConnTracker()
	c := &fakeConn{}

	tr.observe(c, http.StateActive)
	if got := tr.inflight(); got != 1 {
		t.Errorf("first Active: got %d want 1", got)
	}
	tr.observe(c, http.StateIdle)
	if got := tr.inflight(); got != 0 {
		t.Errorf("first Idle: got %d want 0", got)
	}
	tr.observe(c, http.StateActive)
	if got := tr.inflight(); got != 1 {
		t.Errorf("second Active: got %d want 1", got)
	}
	tr.observe(c, http.StateClosed)
	if got := tr.inflight(); got != 0 {
		t.Errorf("final Closed: got %d want 0", got)
	}
}

// TestInflightConnTracker_ActiveHijackedIsDecremented covers the
// WebSocket / SSE hijack path. A hijacked connection is no longer
// managed by net/http so it must be removed from the tracker;
// otherwise the inflight count would drift up across every upgrade.
func TestInflightConnTracker_ActiveHijackedIsDecremented(t *testing.T) {
	tr := newInflightConnTracker()
	c := &fakeConn{}

	tr.observe(c, http.StateActive)
	tr.observe(c, http.StateHijacked)
	if got := tr.inflight(); got != 0 {
		t.Errorf("Active→Hijacked: got %d want 0", got)
	}
}

// TestInflightConnTracker_MultipleConcurrentConns pins the parallel
// case: two connections active at the same time should show inflight=2.
// Also confirms the counter decrements correctly when they close out
// in reverse order.
func TestInflightConnTracker_MultipleConcurrentConns(t *testing.T) {
	tr := newInflightConnTracker()
	a, b := &fakeConn{}, &fakeConn{}

	tr.observe(a, http.StateActive)
	tr.observe(b, http.StateActive)
	if got := tr.inflight(); got != 2 {
		t.Errorf("two active conns: got %d want 2", got)
	}
	tr.observe(b, http.StateClosed)
	if got := tr.inflight(); got != 1 {
		t.Errorf("after b closed: got %d want 1", got)
	}
	tr.observe(a, http.StateClosed)
	if got := tr.inflight(); got != 0 {
		t.Errorf("after a closed: got %d want 0", got)
	}
}
