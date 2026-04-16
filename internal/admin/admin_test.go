package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
)

// errorEnvelope mirrors the api.errorEnvelope shape for test assertions.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const testToken = "test-admin-token"

func newTestServer(t *testing.T) (*Server, *killswitch.Memory, *keys.Memory, *http.ServeMux) {
	t.Helper()
	ks := killswitch.NewMemory()
	store := keys.NewMemory()
	srv := NewServer(testToken, ks, store)
	mux := http.NewServeMux()
	srv.Mount(mux)
	return srv, ks, store, mux
}

func TestAdmin_SetFreeze(t *testing.T) {
	_, ks, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/killswitch",
		strings.NewReader(`{"frozen": true}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Frozen bool `json:"frozen"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Frozen {
		t.Errorf("response frozen: got false want true")
	}

	gotFrozen, _ := ks.IsFrozen(context.Background())
	if !gotFrozen {
		t.Errorf("killswitch state after SetFrozen: got false want true")
	}
}

func TestAdmin_GetFreeze(t *testing.T) {
	_, ks, _, mux := newTestServer(t)
	_ = ks.SetFrozen(context.Background(), true)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/killswitch", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}

	var resp struct {
		Frozen bool `json:"frozen"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Frozen {
		t.Errorf("response frozen: got false want true")
	}
}

func TestAdmin_MissingAuth(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/killswitch",
		strings.NewReader(`{"frozen": true}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d want 401", rec.Code)
	}
}

func TestAdmin_WrongToken(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/killswitch",
		strings.NewReader(`{"frozen": true}`))
	req.Header.Set("Authorization", "Bearer not-the-right-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d want 401", rec.Code)
	}
}

func TestAdmin_InvalidBody(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/killswitch",
		strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body: got %d want 400", rec.Code)
	}
}

func patchAuthed(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func postAuthed(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAdmin_CreateKey(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	body := `{"name":"prod-app","upstream":"openai","upstream_key":"sk-real","daily_budget_usd":10,"month_budget_usd":100}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Token == "" || !strings.HasPrefix(resp.Token, "rein_live_") {
		t.Errorf("token: got %q want rein_live_* prefix", resp.Token)
	}
	if resp.ID == "" || !strings.HasPrefix(resp.ID, "key_") {
		t.Errorf("id: got %q want key_* prefix", resp.ID)
	}
	if resp.Name != "prod-app" || resp.Upstream != "openai" {
		t.Errorf("fields: got name=%q upstream=%q", resp.Name, resp.Upstream)
	}
	if resp.DailyBudgetUSD != 10 || resp.MonthBudgetUSD != 100 {
		t.Errorf("budgets: got daily=%v month=%v", resp.DailyBudgetUSD, resp.MonthBudgetUSD)
	}

	// Verify it is actually persisted and resolvable by token.
	vk, err := store.GetByToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatalf("store lookup: %v", err)
	}
	if vk.UpstreamKey != "sk-real" {
		t.Errorf("upstream_key in store: got %q want sk-real", vk.UpstreamKey)
	}
}

func TestAdmin_CreateKey_Validation(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"missing name", `{"upstream":"openai","upstream_key":"sk-x"}`},
		{"blank name", `{"name":"   ","upstream":"openai","upstream_key":"sk-x"}`},
		{"bad upstream", `{"name":"x","upstream":"cohere","upstream_key":"sk-x"}`},
		{"missing upstream_key", `{"name":"x","upstream":"openai"}`},
		{"negative budget", `{"name":"x","upstream":"openai","upstream_key":"sk-x","daily_budget_usd":-1}`},
		{"malformed json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postAuthed(t, mux, "/admin/v1/keys", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdmin_ListKeys_DoesNotLeakSecrets(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	create := postAuthed(t, mux, "/admin/v1/keys",
		`{"name":"a","upstream":"anthropic","upstream_key":"sk-ant-secret"}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", create.Code, create.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200", rec.Code)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "sk-ant-secret") {
		t.Errorf("list response leaked upstream key: %s", raw)
	}
	if strings.Contains(raw, "rein_live_") {
		t.Errorf("list response leaked rein token: %s", raw)
	}

	var resp struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0].Name != "a" {
		t.Errorf("list: got %+v want one key named 'a'", resp.Keys)
	}
}

func TestAdmin_RevokeKey(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	create := postAuthed(t, mux, "/admin/v1/keys",
		`{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)
	var created createKeyResponse
	_ = json.Unmarshal(create.Body.Bytes(), &created)

	rec := postAuthed(t, mux, "/admin/v1/keys/"+created.ID+"/revoke", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	vk, err := store.GetByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if !vk.IsRevoked() {
		t.Errorf("key was not marked revoked in store")
	}
}

func TestAdmin_RevokeKey_NotFound(t *testing.T) {
	_, _, _, mux := newTestServer(t)
	// Use a syntactically valid ID (matches keys.ValidID) that does not
	// exist in the fresh store. This exercises the 404-from-store path,
	// not the 400-from-format-check path.
	rec := postAuthed(t, mux, "/admin/v1/keys/key_0000000000000000/revoke", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d want 404", rec.Code)
	}
}

// TestAdmin_RevokeKey_InvalidFormat confirms that a malformed key ID in the
// URL path is rejected with 400 before any keystore lookup or log line is
// emitted. This is the first-line defense that makes the gosec G706
// exclusion in .golangci.yml unconditionally safe.
func TestAdmin_RevokeKey_InvalidFormat(t *testing.T) {
	_, _, _, mux := newTestServer(t)
	cases := []string{
		"key_nope",                        // too short
		"token_a1b2c3d4e5f60718",          // wrong prefix
		"key_A1B2C3D4E5F60718",            // uppercase hex rejected by our regex
		"key_a1b2c3d4e5f60718extra",       // too long
		"key_a1b2c3d4e5f6071%0A",          // encoded newline injection attempt
		"key_a1b2c3d4e5f60718%2F..%2Fetc", // path traversal attempt
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			rec := postAuthed(t, mux, "/admin/v1/keys/"+id+"/revoke", "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("id=%q: got %d want 400; body=%s", id, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdmin_CreateKey_WithUpstreamBaseURL(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	body := `{"name":"groq","upstream":"openai","upstream_key":"gsk-real","upstream_base_url":"https://api.groq.com/"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UpstreamBaseURL != "https://api.groq.com" {
		t.Errorf("upstream_base_url in response: got %q want canonical form https://api.groq.com", resp.UpstreamBaseURL)
	}

	vk, err := store.GetByToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatalf("store lookup: %v", err)
	}
	if vk.UpstreamBaseURL != "https://api.groq.com" {
		t.Errorf("upstream_base_url in store: got %q want https://api.groq.com", vk.UpstreamBaseURL)
	}
}

func TestAdmin_CreateKey_UpstreamBaseURLRejectsAnthropic(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	body := `{"name":"x","upstream":"anthropic","upstream_key":"sk-ant","upstream_base_url":"https://api.example.com"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != keys.ErrCodeInvalidBaseURL {
		t.Errorf("envelope code: got %q want %q", env.Error.Code, keys.ErrCodeInvalidBaseURL)
	}
}

func TestAdmin_CreateKey_InvalidUpstreamBaseURL(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	cases := []struct {
		name     string
		url      string
		wantCode string
	}{
		{"non-loopback http", "http://api.example.com", keys.ErrCodeInvalidBaseURLScheme},
		{"ftp scheme", "ftp://api.example.com", keys.ErrCodeInvalidBaseURLScheme},
		{"query included", "https://api.example.com?foo=bar", keys.ErrCodeInvalidBaseURLQuery},
		{"fragment included", "https://api.example.com#f", keys.ErrCodeInvalidBaseURLFragment},
		{"userinfo included", "https://user:pass@api.example.com", keys.ErrCodeInvalidBaseURLHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"name":"x","upstream":"openai","upstream_key":"sk-x","upstream_base_url":"` + tc.url + `"}`
			rec := postAuthed(t, mux, "/admin/v1/keys", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("envelope code: got %q want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.Message == "" {
				t.Errorf("envelope message should be non-empty")
			}
		})
	}
}

func TestAdmin_CreateKey_UpstreamBaseURLWithPathPrefix(t *testing.T) {
	// Real OpenAI-compatible providers (Groq at /openai, OpenRouter at
	// /api, Fireworks at /inference) mount under a path prefix. The
	// validator accepts a non-empty path, and httputil.ProxyRequest.SetURL
	// joins the base path with the incoming /v1/... so routing works with
	// no adapter change.
	_, _, store, mux := newTestServer(t)

	body := `{"name":"groq-prefix","upstream":"openai","upstream_key":"gsk-real","upstream_base_url":"https://api.groq.com/openai/"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.UpstreamBaseURL != "https://api.groq.com/openai" {
		t.Errorf("canonical form with path: got %q want https://api.groq.com/openai", resp.UpstreamBaseURL)
	}
	vk, err := store.GetByToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatal(err)
	}
	if vk.UpstreamBaseURL != "https://api.groq.com/openai" {
		t.Errorf("stored path: got %q want https://api.groq.com/openai", vk.UpstreamBaseURL)
	}
}

func TestAdmin_CreateKey_LoopbackHTTPAccepted(t *testing.T) {
	_, _, _, mux := newTestServer(t)
	body := `{"name":"local","upstream":"openai","upstream_key":"sk-local","upstream_base_url":"http://127.0.0.1:11434"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_CreateKey_OmittedBaseURLStaysEmpty(t *testing.T) {
	_, _, store, mux := newTestServer(t)
	body := `{"name":"plain","upstream":"openai","upstream_key":"sk-plain"}`
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.UpstreamBaseURL != "" {
		t.Errorf("omitted base url in response: got %q want empty", resp.UpstreamBaseURL)
	}
	vk, err := store.GetByToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatal(err)
	}
	if vk.UpstreamBaseURL != "" {
		t.Errorf("omitted base url in store: got %q want empty", vk.UpstreamBaseURL)
	}
}

func TestAdmin_Keys_RequireAuth(t *testing.T) {
	_, _, _, mux := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d want 401", rec.Code)
	}
}

func TestAdmin_CreateKey_NegativeRateLimitRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"bad","upstream":"openai","upstream_key":"sk-x","rps_limit":-1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative rps_limit: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_CreateKey_RateLimitsInResponse(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"rl-key","upstream":"openai","upstream_key":"sk-x","rps_limit":10,"rpm_limit":300}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RPSLimit int `json:"rps_limit"`
		RPMLimit int `json:"rpm_limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RPSLimit != 10 {
		t.Errorf("rps_limit: got %d want 10", resp.RPSLimit)
	}
	if resp.RPMLimit != 300 {
		t.Errorf("rpm_limit: got %d want 300", resp.RPMLimit)
	}
}

func TestAdmin_CreateKey_NegativeMaxConcurrentRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"bad","upstream":"openai","upstream_key":"sk-x","max_concurrent":-1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative max_concurrent: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_CreateKey_MaxConcurrentInResponse(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"mc-key","upstream":"openai","upstream_key":"sk-x","max_concurrent":10}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxConcurrent int `json:"max_concurrent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MaxConcurrent != 10 {
		t.Errorf("max_concurrent: got %d want 10", resp.MaxConcurrent)
	}
}

func TestAdmin_CreateKey_MaxConcurrentOmittedDefaultsToZero(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"default","upstream":"openai","upstream_key":"sk-x"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MaxConcurrent int `json:"max_concurrent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MaxConcurrent != 0 {
		t.Errorf("omitted max_concurrent: got %d want 0 (unlimited)", resp.MaxConcurrent)
	}
}

func TestAdmin_ListAndRevokeIncludeMaxConcurrent(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	createReq := httptest.NewRequest(http.MethodPost, "/admin/v1/keys",
		strings.NewReader(`{"name":"mc-list","upstream":"openai","upstream_key":"sk-x","max_concurrent":7}`))
	createReq.Header.Set("Authorization", "Bearer "+testToken)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: got %d want 201; body=%s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	listReq.Header.Set("Authorization", "Bearer "+testToken)
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200; body=%s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Keys []struct {
			ID            string `json:"id"`
			MaxConcurrent int    `json:"max_concurrent"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var listed *struct {
		ID            string `json:"id"`
		MaxConcurrent int    `json:"max_concurrent"`
	}
	for i := range list.Keys {
		if list.Keys[i].ID == created.ID {
			listed = &list.Keys[i]
			break
		}
	}
	if listed == nil {
		t.Fatalf("created key %q missing from list response", created.ID)
	}
	if listed.MaxConcurrent != 7 {
		t.Errorf("list: max_concurrent=%d want 7", listed.MaxConcurrent)
	}

	revokeReq := httptest.NewRequest(http.MethodPost, "/admin/v1/keys/"+created.ID+"/revoke", nil)
	revokeReq.Header.Set("Authorization", "Bearer "+testToken)
	revokeRec := httptest.NewRecorder()
	mux.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d want 200; body=%s", revokeRec.Code, revokeRec.Body.String())
	}
	var revoked struct {
		MaxConcurrent int `json:"max_concurrent"`
	}
	if err := json.Unmarshal(revokeRec.Body.Bytes(), &revoked); err != nil {
		t.Fatalf("decode revoke: %v", err)
	}
	if revoked.MaxConcurrent != 7 {
		t.Errorf("revoke: max_concurrent=%d want 7", revoked.MaxConcurrent)
	}
}

// --- PATCH /admin/v1/keys/{id} tests ---

// createTestKey is a helper that mints a key and returns its ID.
func createTestKey(t *testing.T, mux *http.ServeMux, body string) string {
	t.Helper()
	rec := postAuthed(t, mux, "/admin/v1/keys", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return resp.ID
}

func TestAdmin_UpdateKey_PartialUpdate(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"original","upstream":"openai","upstream_key":"sk-x","daily_budget_usd":10,"month_budget_usd":100,"rps_limit":5,"rpm_limit":300,"max_concurrent":20}`)

	// Patch only the name and daily budget.
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
		`{"name":"updated","daily_budget_usd":50}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp keyView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if resp.Name != "updated" {
		t.Errorf("name: got %q want updated", resp.Name)
	}
	if resp.DailyBudgetUSD != 50 {
		t.Errorf("daily_budget_usd: got %v want 50", resp.DailyBudgetUSD)
	}
	// Unpatched fields must be preserved.
	if resp.MonthBudgetUSD != 100 {
		t.Errorf("month_budget_usd: got %v want 100 (preserved)", resp.MonthBudgetUSD)
	}
	if resp.RPSLimit != 5 {
		t.Errorf("rps_limit: got %d want 5 (preserved)", resp.RPSLimit)
	}
	if resp.RPMLimit != 300 {
		t.Errorf("rpm_limit: got %d want 300 (preserved)", resp.RPMLimit)
	}
	if resp.MaxConcurrent != 20 {
		t.Errorf("max_concurrent: got %d want 20 (preserved)", resp.MaxConcurrent)
	}

	// Verify the store matches the response.
	vk, err := store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if vk.Name != "updated" || vk.DailyBudgetUSD != 50 || vk.MonthBudgetUSD != 100 {
		t.Errorf("store mismatch: name=%q daily=%v month=%v",
			vk.Name, vk.DailyBudgetUSD, vk.MonthBudgetUSD)
	}
}

func TestAdmin_UpdateKey_ZeroMeansUnlimited(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"capped","upstream":"openai","upstream_key":"sk-x","daily_budget_usd":100,"rps_limit":10,"max_concurrent":5}`)

	// Explicitly set to zero to remove the cap.
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
		`{"daily_budget_usd":0,"rps_limit":0,"max_concurrent":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	vk, err := store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if vk.DailyBudgetUSD != 0 {
		t.Errorf("daily_budget_usd: got %v want 0 (unlimited)", vk.DailyBudgetUSD)
	}
	if vk.RPSLimit != 0 {
		t.Errorf("rps_limit: got %d want 0 (unlimited)", vk.RPSLimit)
	}
	if vk.MaxConcurrent != 0 {
		t.Errorf("max_concurrent: got %d want 0 (unlimited)", vk.MaxConcurrent)
	}
}

func TestAdmin_UpdateKey_ImmutableFieldsRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)

	cases := []struct {
		name string
		body string
	}{
		{"id", `{"id":"key_aaaaaaaaaaaaaaaa"}`},
		{"token", `{"token":"rein_live_fake"}`},
		{"upstream", `{"upstream":"anthropic"}`},
		{"upstream_key", `{"upstream_key":"sk-new"}`},
		{"created_at", `{"created_at":"2026-01-01T00:00:00Z"}`},
		{"revoked_at", `{"revoked_at":"2026-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != immutableFieldError {
				t.Errorf("error code: got %q want %q", env.Error.Code, immutableFieldError)
			}
		})
	}
}

func TestAdmin_UpdateKey_UnknownFieldRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"totally_fake_field":42}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_UpdateKey_RevokedKeyRejected(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)

	// Revoke the key first.
	revRec := postAuthed(t, mux, "/admin/v1/keys/"+id+"/revoke", "")
	if revRec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d want 200", revRec.Code)
	}

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"name":"new-name"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Code != "key_revoked" {
		t.Errorf("error code: got %q want key_revoked", env.Error.Code)
	}
}

func TestAdmin_UpdateKey_NotFound(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	rec := patchAuthed(t, mux, "/admin/v1/keys/key_0000000000000000", `{"name":"new"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_UpdateKey_InvalidFormat(t *testing.T) {
	_, _, _, mux := newTestServer(t)
	cases := []string{
		"key_nope",
		"token_a1b2c3d4e5f60718",
		"key_A1B2C3D4E5F60718",
		"key_a1b2c3d4e5f60718extra",
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"name":"new"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("id=%q: got %d want 400", id, rec.Code)
			}
		})
	}
}

func TestAdmin_UpdateKey_ValidationRejectsInvalid(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"x","upstream":"openai","upstream_key":"sk-x"}`)

	cases := []struct {
		name string
		body string
	}{
		{"blank name", `{"name":"   "}`},
		{"name too long", `{"name":"` + strings.Repeat("a", 101) + `"}`},
		{"negative daily budget", `{"daily_budget_usd":-1}`},
		{"negative month budget", `{"month_budget_usd":-1}`},
		{"negative rps_limit", `{"rps_limit":-1}`},
		{"negative rpm_limit", `{"rpm_limit":-1}`},
		{"negative max_concurrent", `{"max_concurrent":-1}`},
		{"malformed json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdmin_UpdateKey_DoesNotLeakSecrets(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	createRec := postAuthed(t, mux, "/admin/v1/keys",
		`{"name":"secret-check","upstream":"openai","upstream_key":"sk-super-secret"}`)
	var created createKeyResponse
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+created.ID, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "sk-super-secret") {
		t.Errorf("patch response leaked upstream key: %s", raw)
	}
	if strings.Contains(raw, "rein_live_") {
		t.Errorf("patch response leaked rein token: %s", raw)
	}
}

func TestAdmin_UpdateKey_EmptyBodyNoOp(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"stable","upstream":"openai","upstream_key":"sk-x","daily_budget_usd":42}`)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch empty body: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	vk, err := store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if vk.Name != "stable" || vk.DailyBudgetUSD != 42 {
		t.Errorf("no-op patch changed values: name=%q daily=%v", vk.Name, vk.DailyBudgetUSD)
	}
}

func TestAdmin_UpdateKey_UpstreamBaseURL(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"groq","upstream":"openai","upstream_key":"gsk-x","upstream_base_url":"https://api.groq.com"}`)

	// Update base URL.
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
		`{"upstream_base_url":"https://api.fireworks.ai/inference/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp keyView
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.UpstreamBaseURL != "https://api.fireworks.ai/inference" {
		t.Errorf("upstream_base_url: got %q want canonical form", resp.UpstreamBaseURL)
	}

	vk, err := store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if vk.UpstreamBaseURL != "https://api.fireworks.ai/inference" {
		t.Errorf("store: got %q", vk.UpstreamBaseURL)
	}
}

func TestAdmin_UpdateKey_ClearUpstreamBaseURL(t *testing.T) {
	_, _, store, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"groq","upstream":"openai","upstream_key":"gsk-x","upstream_base_url":"https://api.groq.com"}`)

	// Clear by setting to empty string.
	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id, `{"upstream_base_url":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	vk, err := store.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if vk.UpstreamBaseURL != "" {
		t.Errorf("upstream_base_url after clear: got %q want empty", vk.UpstreamBaseURL)
	}
}

func TestAdmin_UpdateKey_InvalidUpstreamBaseURL(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"oai","upstream":"openai","upstream_key":"sk-x"}`)

	cases := []struct {
		name     string
		url      string
		wantCode string
	}{
		{"non-loopback http", "http://api.example.com", keys.ErrCodeInvalidBaseURLScheme},
		{"ftp scheme", "ftp://api.example.com", keys.ErrCodeInvalidBaseURLScheme},
		{"query included", "https://api.example.com?foo=bar", keys.ErrCodeInvalidBaseURLQuery},
		{"fragment included", "https://api.example.com#f", keys.ErrCodeInvalidBaseURLFragment},
		{"userinfo included", "https://user:pass@api.example.com", keys.ErrCodeInvalidBaseURLHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
				`{"upstream_base_url":"`+tc.url+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("error code: got %q want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestAdmin_UpdateKey_UpstreamBaseURLRejectsAnthropic(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	id := createTestKey(t, mux,
		`{"name":"ant","upstream":"anthropic","upstream_key":"sk-ant-x"}`)

	rec := patchAuthed(t, mux, "/admin/v1/keys/"+id,
		`{"upstream_base_url":"https://api.example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != keys.ErrCodeInvalidBaseURL {
		t.Errorf("error code: got %q want %q", env.Error.Code, keys.ErrCodeInvalidBaseURL)
	}
}

func TestAdmin_UpdateKey_RequiresAuth(t *testing.T) {
	_, _, _, mux := newTestServer(t)

	req := httptest.NewRequest(http.MethodPatch, "/admin/v1/keys/key_0000000000000000",
		strings.NewReader(`{"name":"new"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d want 401", rec.Code)
	}
}
