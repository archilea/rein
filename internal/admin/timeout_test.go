package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archilea/rein/internal/keys"
)

// createKeyRoundTrip is a small helper that POSTs a key create payload
// and decodes the response into the exported keyView alongside the
// secret token. Lets the timeout-specific suite stay focused on the
// new field without re-implementing request plumbing.
func createKeyRoundTrip(t *testing.T, mux http.Handler, body string) (int, createKeyResponse, errorEnvelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var success createKeyResponse
	var failure errorEnvelope
	// Best-effort decode: the caller decides which envelope to look at.
	_ = json.Unmarshal(rec.Body.Bytes(), &success)
	_ = json.Unmarshal(rec.Body.Bytes(), &failure)
	return rec.Code, success, failure
}

func patchKeyRoundTrip(t *testing.T, mux http.Handler, id, body string) (int, keyView, errorEnvelope, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/admin/v1/keys/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var success keyView
	var failure errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &success)
	_ = json.Unmarshal(rec.Body.Bytes(), &failure)
	return rec.Code, success, failure, rec.Body.String()
}

// TestAdmin_CreateKey_UpstreamTimeoutAcceptsValidValues walks the
// create-key happy path across the boundary values of the documented
// [0, 3600] range.
func TestAdmin_CreateKey_UpstreamTimeoutAcceptsValidValues(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	cases := []struct {
		name    string
		payload string
		want    int
	}{
		{
			"unlimited default when field omitted",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real"}`,
			0,
		},
		{
			"zero sets unlimited",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":0}`,
			0,
		},
		{
			"small positive",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":5}`,
			5,
		},
		{
			"upper bound 3600",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":3600}`,
			3600,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp, _ := createKeyRoundTrip(t, mux, tc.payload)
			if code != http.StatusCreated {
				t.Fatalf("status: got %d want 201", code)
			}
			if resp.UpstreamTimeoutSeconds != tc.want {
				t.Errorf("upstream_timeout_seconds: got %d want %d", resp.UpstreamTimeoutSeconds, tc.want)
			}
		})
	}
}

// TestAdmin_CreateKey_UpstreamTimeoutRejectsInvalid covers every reject
// path required by the acceptance criteria.
func TestAdmin_CreateKey_UpstreamTimeoutRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			"negative",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":-1}`,
		},
		{
			"one over cap",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":3601}`,
		},
		{
			"day-sized typo",
			`{"name":"k","upstream":"openai","upstream_key":"sk-real","upstream_timeout_seconds":86400}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, mux := newTestServer(t)
			code, _, _ := createKeyRoundTrip(t, mux, tc.payload)
			if code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400", code)
			}
		})
	}
}

// TestAdmin_UpdateKey_UpstreamTimeoutSetsValue confirms the PATCH
// flow accepts the field and the value round-trips back through the
// admin keyView response.
func TestAdmin_UpdateKey_UpstreamTimeoutSetsValue(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	vk := seedTimeoutKey(t, store, 0)
	code, got, _, body := patchKeyRoundTrip(t, mux, vk.ID, `{"upstream_timeout_seconds":30}`)
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", code, body)
	}
	if got.UpstreamTimeoutSeconds != 30 {
		t.Errorf("got %d want 30", got.UpstreamTimeoutSeconds)
	}

	// And clearing back to zero (unlimited) is valid.
	code, got, _, _ = patchKeyRoundTrip(t, mux, vk.ID, `{"upstream_timeout_seconds":0}`)
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200", code)
	}
	if got.UpstreamTimeoutSeconds != 0 {
		t.Errorf("got %d want 0", got.UpstreamTimeoutSeconds)
	}
}

// TestAdmin_UpdateKey_UpstreamTimeoutRejectsInvalid mirrors the
// create-side reject suite on the PATCH surface.
func TestAdmin_UpdateKey_UpstreamTimeoutRejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"negative", `{"upstream_timeout_seconds":-1}`},
		{"one over cap", `{"upstream_timeout_seconds":3601}`},
		{"day-sized typo", `{"upstream_timeout_seconds":86400}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, store, mux := newTestServer(t)
			vk := seedTimeoutKey(t, store, 0)
			code, _, _, body := patchKeyRoundTrip(t, mux, vk.ID, tc.payload)
			if code != http.StatusBadRequest {
				t.Errorf("status: got %d want 400; body=%s", code, body)
			}
		})
	}
}

// TestAdmin_UpdateKey_UpstreamTimeoutOmittedPreserved confirms that a
// PATCH that does not carry upstream_timeout_seconds leaves the
// stored value intact (the nil-pointer "unchanged" contract).
func TestAdmin_UpdateKey_UpstreamTimeoutOmittedPreserved(t *testing.T) {
	_, _, store, mux := newTestServer(t)
	vk := seedTimeoutKey(t, store, 45)

	// PATCH a different field and confirm the timeout survives.
	code, got, _, body := patchKeyRoundTrip(t, mux, vk.ID, `{"name":"renamed"}`)
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", code, body)
	}
	if got.UpstreamTimeoutSeconds != 45 {
		t.Errorf("timeout altered by unrelated patch: got %d want 45", got.UpstreamTimeoutSeconds)
	}
	if got.Name != "renamed" {
		t.Errorf("name: got %q want renamed", got.Name)
	}
}

// TestAdmin_ListKeys_IncludesUpstreamTimeout round-trips through GET
// /admin/v1/keys so we verify the keyView projection serializes the
// field. Guards against a future projection regression that would
// otherwise silently hide the value from operators.
func TestAdmin_ListKeys_IncludesUpstreamTimeout(t *testing.T) {
	_, _, store, mux := newTestServer(t)
	_ = seedTimeoutKey(t, store, 120)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"upstream_timeout_seconds":120`)) {
		t.Errorf("list response missing upstream_timeout_seconds: %s", rec.Body.String())
	}
}

// seedTimeoutKey drops a key into the store directly so tests that
// only care about update/list behavior do not need to replay the
// full create flow.
func seedTimeoutKey(t *testing.T, store *keys.Memory, timeoutSeconds int) *keys.VirtualKey {
	t.Helper()
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
		Name:                   "timeout-seed",
		Upstream:               keys.UpstreamOpenAI,
		UpstreamKey:            "sk-real",
		UpstreamTimeoutSeconds: timeoutSeconds,
	}
	if err := store.Create(context.Background(), vk); err != nil {
		t.Fatalf("create: %v", err)
	}
	return vk
}
