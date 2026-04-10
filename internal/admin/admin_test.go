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
	rec := postAuthed(t, mux, "/admin/v1/keys/key_nope/revoke", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d want 404", rec.Code)
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
