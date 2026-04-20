package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/meter"
)

// newTimeoutTestKey creates a virtual key with an explicit
// UpstreamTimeoutSeconds so tests can assert the hot-path behavior that
// fires once context.DeadlineExceeded propagates through ReverseProxy.
func newTimeoutTestKey(t *testing.T, timeoutSeconds int) (*keys.Memory, *keys.VirtualKey) {
	t.Helper()
	store := keys.NewMemory()
	id, err := keys.GenerateID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	vk := &keys.VirtualKey{
		ID:                     id,
		Token:                  token,
		Name:                   "timeout-test",
		Upstream:               keys.UpstreamOpenAI,
		UpstreamKey:            "sk-real",
		UpstreamTimeoutSeconds: timeoutSeconds,
		CreatedAt:              time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	return store, vk
}

// TestProxy_NonStreamingTimeoutReturns504 is the primary hot-path
// acceptance test for #30: a non-streaming request that exceeds the
// per-key timeout ceiling must terminate with a 504 Gateway Timeout,
// Retry-After: 1, and the structured upstream_timeout code.
func TestProxy_NonStreamingTimeoutReturns504(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block well past the per-key timeout so the deadline fires
		// inside ReverseProxy's io.Copy.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer slow.Close()

	store, vk := newTimeoutTestKey(t, 1)
	p := newTestProxy(t, store, slow.URL, "https://api.anthropic.com")

	rein := httptest.NewServer(p)
	defer rein.Close()

	req, err := http.NewRequest(http.MethodPost, rein.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	elapsed := time.Since(start)
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status: got %d want 504", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q want %q", got, "1")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"upstream_timeout"`) {
		t.Errorf("body missing code: %s", body)
	}
	if !strings.Contains(string(body), "1 seconds") {
		t.Errorf("body should name the timeout value; got %s", body)
	}
	// The deadline is 1 second; a generous upper bound protects against a
	// hung test under CI load but still catches a regression where the
	// timeout stops firing entirely.
	if elapsed > 3*time.Second {
		t.Errorf("request took %v; deadline should have fired closer to 1s", elapsed)
	}
}

// TestProxy_UnlimitedKeyNotAffectedByTimeout confirms a key with
// UpstreamTimeoutSeconds==0 still completes even when the upstream
// takes longer than every other key's deadline. Guards the hot-path
// skip branch in ServeHTTP.
func TestProxy_UnlimitedKeyNotAffectedByTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 ms is well under any realistic test timeout but long
		// enough that a broken implementation wrapping every request
		// in a 0-second deadline would trip.
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	store, vk := newTimeoutTestKey(t, 0)
	p := newTestProxy(t, store, upstream.URL, "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("unlimited key: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProxy_DialTimeoutStillFiresIndependently verifies that the
// transport-level dial timeout is not shadowed by the per-key ceiling:
// an unreachable upstream returns 502 with the generic upstream error
// body, not 504. The two brakes must remain independent.
func TestProxy_DialTimeoutStillFiresIndependently(t *testing.T) {
	store, vk := newTimeoutTestKey(t, 30)
	// 127.0.0.1:1 is guaranteed to refuse connections on every host the
	// test suite runs on. The dial will fail in well under the 10 s
	// transport dial deadline and long before the 30 s per-key deadline.
	p := newTestProxy(t, store, "http://127.0.0.1:1", "https://api.anthropic.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream: got %d want 502; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("dial failure should not be reported as upstream_timeout: %s", rec.Body.String())
	}
}

// TestProxy_StreamingTimeoutCleanCloseAndPartialMetering is the
// load-bearing correctness test for the streaming half of #30. It:
//  1. Streams one data chunk carrying partial usage.
//  2. Hangs until the per-key deadline fires.
//  3. Asserts the client sees the partial chunk + the injected SSE
//     comment trailer followed by a clean EOF (no surrounding HTTP
//     error framing).
//  4. Asserts meter.Record was invoked with the parsed partial usage,
//     proving the docs/architecture.md:42 "background-context Record"
//     invariant survives context cancel.
func TestProxy_StreamingTimeoutCleanCloseAndPartialMetering(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		// First chunk: delivers a populated usage field so the stream
		// meter has something to Record on cancel.
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-x\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		// Now hang. The per-key timeout is 1 second; wait longer so
		// the deadline fires on the client-facing ReverseProxy copy.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	store, vk := newTimeoutTestKey(t, 1)

	recordedMu := sync.Mutex{}
	var recordedCost float64
	var recordedCount int
	m := &recordingMeter{
		onRecord: func(_ context.Context, _ string, cost float64) error {
			recordedMu.Lock()
			defer recordedMu.Unlock()
			recordedCost += cost
			recordedCount++
			return nil
		},
	}
	p := newTestProxyWithMeter(t, store, m, upstream.URL, "https://api.anthropic.com")

	rein := httptest.NewServer(p)
	defer rein.Close()

	req, err := http.NewRequest(http.MethodPost, rein.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Once the stream status has been written, we cannot retroactively
	// change it. The contract is: 200 OK + SSE body containing the
	// partial data chunk + the timeout trailer.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming status: got %d want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"usage":{"prompt_tokens":7`) {
		t.Errorf("partial data chunk missing: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, ": rein upstream timeout after 1 seconds") {
		t.Errorf("SSE timeout trailer missing: %s", bodyStr)
	}
	// Close the response body to fire streamMeter.Close, which records
	// the captured usage via a background-context meter.Record.
	_ = resp.Body.Close()

	// The Record call happens on a detached goroutine path inside
	// wrapStream -> Close -> recordSpend(context.Background()). A short
	// settle window is sufficient; if this flakes in CI, raise it.
	require(t, 500*time.Millisecond, func() bool {
		recordedMu.Lock()
		defer recordedMu.Unlock()
		return recordedCount >= 1 && recordedCost > 0
	})
}

// TestProxy_OpenAIErrorHandlerNon504Path guards the branch where the
// adapter's ErrorHandler receives a non-deadline error: the existing
// 502 fallback must still fire rather than the new 504 envelope. We
// invoke the handler directly so the assertion does not depend on any
// particular wire-level error the transport surfaces.
func TestProxy_OpenAIErrorHandlerNon504Path(t *testing.T) {
	o, err := NewOpenAI("https://api.openai.com", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	o.errorHandler(rec, req, errors.New("boom"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("non-deadline error must not be reported as upstream_timeout: %s", rec.Body.String())
	}
}

// TestProxy_AnthropicErrorHandlerNon504Path mirrors the OpenAI
// coverage on the Anthropic adapter.
func TestProxy_AnthropicErrorHandlerNon504Path(t *testing.T) {
	a, err := NewAnthropic("https://api.anthropic.com", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	a.errorHandler(rec, req, errors.New("boom"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("non-deadline error must not be reported as upstream_timeout: %s", rec.Body.String())
	}
}

// TestProxy_OpenAIErrorHandlerTimeoutPath is the mirror of the
// non-504 test: an injected context.DeadlineExceeded must produce a
// 504 envelope from the adapter ErrorHandler even without the full
// ReverseProxy pipeline.
func TestProxy_OpenAIErrorHandlerTimeoutPath(t *testing.T) {
	o, err := NewOpenAI("https://api.openai.com", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), vkeyContextKey{},
		&keys.VirtualKey{UpstreamTimeoutSeconds: 8}))
	o.errorHandler(rec, req, fmt.Errorf("copy: %w", context.DeadlineExceeded))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status: got %d want 504", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "8 seconds") {
		t.Errorf("message missing seconds: %s", rec.Body.String())
	}
}

// --- unit tests for the helpers in timeout.go and stream.go ---

func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"nil", nil, false},
		{"plain deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("copy: %w", context.DeadlineExceeded), true},
		{"client canceled", context.Canceled, false},
		{"unrelated", errors.New("boom"), false},
		{"wrapped unrelated", fmt.Errorf("outer: %w", errors.New("inner")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUpstreamTimeout(tc.in); got != tc.want {
				t.Errorf("isUpstreamTimeout(%v): got %v want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name     string
		ctx      context.Context
		wantSecs string
	}{
		{
			name:     "no vkey on context",
			ctx:      context.Background(),
			wantSecs: "", // falls back to generic message
		},
		{
			name: "vkey with timeout",
			ctx: context.WithValue(context.Background(), vkeyContextKey{},
				&keys.VirtualKey{UpstreamTimeoutSeconds: 42}),
			wantSecs: "42 seconds",
		},
		{
			name: "vkey with zero timeout",
			ctx: context.WithValue(context.Background(), vkeyContextKey{},
				&keys.VirtualKey{UpstreamTimeoutSeconds: 0}),
			wantSecs: "", // falls back to generic message
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(tc.ctx)
			writeUpstreamTimeout(rec, r)

			if rec.Code != http.StatusGatewayTimeout {
				t.Errorf("status: got %d want 504", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After: got %q want 1", got)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"code":"upstream_timeout"`) {
				t.Errorf("code missing: %s", body)
			}
			if tc.wantSecs != "" && !strings.Contains(body, tc.wantSecs) {
				t.Errorf("message missing %q: %s", tc.wantSecs, body)
			}
			if tc.wantSecs == "" && strings.Contains(body, " seconds") {
				t.Errorf("no-timeout variant should not name a seconds value: %s", body)
			}
		})
	}
}

func TestBuildTimeoutTrailer(t *testing.T) {
	cases := []struct {
		name string
		secs int
		want string
	}{
		{"zero falls back to generic", 0, ": rein upstream timeout\n\n"},
		{"positive includes seconds", 12, ": rein upstream timeout after 12 seconds\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(buildTimeoutTrailer(tc.secs)); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestStreamMeter_TimeoutInjectsTrailerOnCancel exercises the
// stream.go Read path for a deadline context. The underlying body is
// a deadlineBody that blocks until its parent ctx is canceled, at
// which point it returns context.DeadlineExceeded. The meter must
// inject the trailer, suppress the error, and EOF cleanly.
func TestStreamMeter_TimeoutInjectsTrailerOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Wrap in a deadline context so streamMeter sees DeadlineExceeded.
	deadlineCtx, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(20*time.Millisecond))
	defer cancelDeadline()
	defer cancel()
	deadlineCtx = context.WithValue(deadlineCtx, vkeyContextKey{}, &keys.VirtualKey{UpstreamTimeoutSeconds: 5})

	body := newDeadlineBody(deadlineCtx)
	sm := newStreamMeter(body, keys.UpstreamOpenAI, func(string, int, int) {}, deadlineCtx)

	out, err := io.ReadAll(sm)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), "rein upstream timeout after 5 seconds") {
		t.Errorf("trailer missing: %q", out)
	}
	if err := sm.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestStreamMeter_NilCtxUnchanged is the regression fence for
// non-timeout callers: nil ctx must keep the pre-#30 semantics
// (propagate whatever error the body returns).
func TestStreamMeter_NilCtxUnchanged(t *testing.T) {
	body := io.NopCloser(strings.NewReader(""))
	sm := newStreamMeter(body, keys.UpstreamOpenAI, func(string, int, int) {}, nil)
	n, err := sm.Read(make([]byte, 8))
	if n != 0 || err != io.EOF {
		t.Errorf("got (%d, %v) want (0, io.EOF)", n, err)
	}
}

// TestStreamMeter_ReadAfterEndedReturnsEOF covers the latch that
// protects against a caller Reading past the injected trailer's EOF.
// io.ReadAll stops at the first EOF, but any direct caller that
// re-invokes Read would otherwise hit the underlying body again and
// re-trigger the trailer path. The `ended` flag short-circuits to a
// clean EOF instead.
func TestStreamMeter_ReadAfterEndedReturnsEOF(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Millisecond))
	defer cancel()
	ctx = context.WithValue(ctx, vkeyContextKey{}, &keys.VirtualKey{UpstreamTimeoutSeconds: 3})

	body := newDeadlineBody(ctx)
	sm := newStreamMeter(body, keys.UpstreamOpenAI, func(string, int, int) {}, ctx)

	// Drain normally.
	if _, err := io.ReadAll(sm); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Next Read must report EOF via the ended latch.
	n, err := sm.Read(make([]byte, 8))
	if n != 0 || err != io.EOF {
		t.Errorf("post-EOF Read: got (%d, %v) want (0, io.EOF)", n, err)
	}
}

// TestStreamMeter_PendingDrainLargerThanBuffer confirms the trailer
// drains across multiple Read calls when the caller's buffer is
// smaller than the trailer length. io.Copy uses a 32 KiB buffer in
// practice so this case is latent; guarding against future refactors
// that shrink the copy buffer.
func TestStreamMeter_PendingDrainLargerThanBuffer(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Millisecond))
	defer cancel()
	ctx = context.WithValue(ctx, vkeyContextKey{}, &keys.VirtualKey{UpstreamTimeoutSeconds: 7})

	body := newDeadlineBody(ctx)
	sm := newStreamMeter(body, keys.UpstreamOpenAI, func(string, int, int) {}, ctx)

	var out strings.Builder
	buf := make([]byte, 4) // smaller than the trailer
	for {
		n, err := sm.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !strings.Contains(out.String(), "rein upstream timeout after 7 seconds") {
		t.Errorf("trailer not fully drained: %q", out.String())
	}
}

// TestProxy_TimeoutWithAnthropicUpstream mirrors the non-streaming
// 504 case on the Anthropic adapter so both adapters' ErrorHandlers
// are covered.
func TestProxy_TimeoutWithAnthropicUpstream(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer slow.Close()

	store := keys.NewMemory()
	id, _ := keys.GenerateID()
	token, _ := keys.GenerateToken()
	vk := &keys.VirtualKey{
		ID: id, Token: token, Name: "anthropic-timeout",
		Upstream: keys.UpstreamAnthropic, UpstreamKey: "sk-real",
		UpstreamTimeoutSeconds: 1,
		CreatedAt:              time.Now().UTC(),
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, store, "https://api.openai.com", slow.URL)
	rein := httptest.NewServer(p)
	defer rein.Close()

	req, _ := http.NewRequest(http.MethodPost, rein.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Set("Authorization", "Bearer "+vk.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status: got %d want 504", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"upstream_timeout"`) {
		t.Errorf("body missing upstream_timeout code: %s", body)
	}
}

// --- test helpers ---

// deadlineBody is a synthetic io.ReadCloser that blocks on Read until
// its context is canceled, at which point it returns the cancel error.
// Lets us deterministically trigger the DeadlineExceeded branch in
// streamMeter.Read without spinning on a real TCP stream.
type deadlineBody struct {
	ctx    context.Context
	closed atomic.Bool
}

func newDeadlineBody(ctx context.Context) *deadlineBody {
	return &deadlineBody{ctx: ctx}
}

func (d *deadlineBody) Read(_ []byte) (int, error) {
	<-d.ctx.Done()
	return 0, d.ctx.Err()
}

func (d *deadlineBody) Close() error {
	d.closed.Store(true)
	return nil
}

// recordingMeter is a meter.Meter test double that captures every
// Record call. Used only for the partial-metering invariant assertion.
type recordingMeter struct {
	onRecord func(ctx context.Context, keyID string, cost float64) error
}

func (r *recordingMeter) Check(_ context.Context, _ string, _, _ float64) error {
	return nil
}

func (r *recordingMeter) Record(ctx context.Context, keyID string, cost float64) error {
	if r.onRecord != nil {
		return r.onRecord(ctx, keyID, cost)
	}
	return nil
}

// newTestProxyWithMeter builds a Proxy with a caller-supplied meter
// implementation so tests can assert Record was invoked with specific
// values.
func newTestProxyWithMeter(t *testing.T, store keys.Store, m meter.Meter, openaiBase, anthropicBase string) *Proxy {
	t.Helper()
	pricer, err := meter.LoadPricer()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(store, nil, m, nil, nil, meter.NewPricerHolder(pricer), openaiBase, anthropicBase)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// require polls until the predicate returns true or the timeout
// elapses. Keeps partial-metering assertions deterministic without
// relying on hard-coded sleeps.
func require(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !pred() {
		t.Errorf("predicate did not hold within %v", timeout)
	}
}
