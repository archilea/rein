// Package admin exposes Rein's administrative HTTP endpoints.
//
// Routes are mounted under /admin/v1/* and protected by a single bearer token
// (REIN_ADMIN_TOKEN). Constant-time comparison is used to avoid leaking the
// token via timing side-channels.
//
// Scope today is intentionally minimal: the kill-switch endpoints. Key
// management, budget administration, and usage queries are tracked for
// follow-up iterations.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/archilea/rein/internal/api"
	"github.com/archilea/rein/internal/keys"
	"github.com/archilea/rein/internal/killswitch"
)

// Server exposes Rein's admin endpoints.
type Server struct {
	token      string
	killSwitch killswitch.Switch
	keys       keys.Store
}

// NewServer constructs an admin Server.
// token is compared in constant time against the inbound Authorization bearer.
// keyStore may be nil; key management endpoints are only mounted when non-nil.
func NewServer(token string, ks killswitch.Switch, keyStore keys.Store) *Server {
	return &Server{
		token:      token,
		killSwitch: ks,
		keys:       keyStore,
	}
}

// Mount registers the admin routes on the supplied mux.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.Handle("POST /admin/v1/killswitch", s.withAuth(s.handleSetFreeze))
	mux.Handle("GET /admin/v1/killswitch", s.withAuth(s.handleGetFreeze))
	if s.keys != nil {
		mux.Handle("POST /admin/v1/keys", s.withAuth(s.handleCreateKey))
		mux.Handle("GET /admin/v1/keys", s.withAuth(s.handleListKeys))
		mux.Handle("POST /admin/v1/keys/{id}/revoke", s.withAuth(s.handleRevokeKey))
	}
}

func (s *Server) withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.token
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

func (s *Server) handleGetFreeze(w http.ResponseWriter, r *http.Request) {
	frozen, err := s.killSwitch.IsFrozen(r.Context())
	if err != nil {
		slog.Error("killswitch read", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]bool{"frozen": frozen})
}

func (s *Server) handleSetFreeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Frozen bool `json:"frozen"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := s.killSwitch.SetFrozen(r.Context(), body.Frozen); err != nil {
		slog.Error("killswitch write", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("killswitch state changed", "frozen", body.Frozen)
	api.WriteJSON(w, http.StatusOK, map[string]bool{"frozen": body.Frozen})
}

// keyView is the safe projection of a VirtualKey for list/get responses.
// It never includes the rein token or the upstream API key.
type keyView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Upstream        string     `json:"upstream"`
	DailyBudgetUSD  float64    `json:"daily_budget_usd"`
	MonthBudgetUSD  float64    `json:"month_budget_usd"`
	RPSLimit        int        `json:"rps_limit"`
	RPMLimit        int        `json:"rpm_limit"`
	MaxConcurrent   int        `json:"max_concurrent"`
	UpstreamBaseURL string     `json:"upstream_base_url,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

func viewOf(k *keys.VirtualKey) keyView {
	return keyView{
		ID:              k.ID,
		Name:            k.Name,
		Upstream:        k.Upstream,
		DailyBudgetUSD:  k.DailyBudgetUSD,
		MonthBudgetUSD:  k.MonthBudgetUSD,
		RPSLimit:        k.RPSLimit,
		RPMLimit:        k.RPMLimit,
		MaxConcurrent:   k.MaxConcurrent,
		UpstreamBaseURL: k.UpstreamBaseURL,
		CreatedAt:       k.CreatedAt,
		RevokedAt:       k.RevokedAt,
	}
}

// createKeyResponse includes the secret token. This is the ONE time the caller
// sees the token: Rein never returns it on list or get.
type createKeyResponse struct {
	keyView
	Token string `json:"token"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name            string  `json:"name"`
		Upstream        string  `json:"upstream"`
		UpstreamKey     string  `json:"upstream_key"`
		DailyBudgetUSD  float64 `json:"daily_budget_usd"`
		MonthBudgetUSD  float64 `json:"month_budget_usd"`
		RPSLimit        int     `json:"rps_limit"`
		RPMLimit        int     `json:"rpm_limit"`
		MaxConcurrent   int     `json:"max_concurrent"`
		UpstreamBaseURL string  `json:"upstream_base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(name) > 100 {
		http.Error(w, "name must be 100 characters or fewer", http.StatusBadRequest)
		return
	}
	if body.Upstream != keys.UpstreamOpenAI && body.Upstream != keys.UpstreamAnthropic {
		http.Error(w, "upstream must be 'openai' or 'anthropic'", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.UpstreamKey) == "" {
		http.Error(w, "upstream_key is required", http.StatusBadRequest)
		return
	}
	if body.DailyBudgetUSD < 0 || body.MonthBudgetUSD < 0 {
		http.Error(w, "budgets must be non-negative", http.StatusBadRequest)
		return
	}
	if body.RPSLimit < 0 || body.RPMLimit < 0 {
		http.Error(w, "rate limits must be non-negative", http.StatusBadRequest)
		return
	}
	if body.MaxConcurrent < 0 {
		http.Error(w, "max_concurrent must be non-negative", http.StatusBadRequest)
		return
	}
	// Per-key upstream base URL override. Only meaningful for the OpenAI
	// adapter in 0.2 (no known Anthropic-compatible providers in the wild),
	// so reject it on anthropic keys up front rather than silently ignoring.
	canonicalBaseURL := ""
	if strings.TrimSpace(body.UpstreamBaseURL) != "" {
		if body.Upstream != keys.UpstreamOpenAI {
			api.WriteError(w, http.StatusBadRequest,
				keys.ErrCodeInvalidBaseURL,
				"upstream_base_url is only supported when upstream is 'openai'")
			return
		}
		canonical, err := keys.ValidateUpstreamBaseURL(body.UpstreamBaseURL)
		if err != nil {
			if bue := keys.AsBaseURLError(err); bue != nil {
				api.WriteError(w, http.StatusBadRequest, bue.Code, bue.Message)
				return
			}
			api.WriteError(w, http.StatusBadRequest,
				keys.ErrCodeInvalidBaseURL, "invalid upstream_base_url")
			return
		}
		canonicalBaseURL = canonical
	}

	id, err := keys.GenerateID()
	if err != nil {
		slog.Error("generate key id", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, err := keys.GenerateToken()
	if err != nil {
		slog.Error("generate key token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	vk := &keys.VirtualKey{
		ID:              id,
		Token:           token,
		Name:            name,
		Upstream:        body.Upstream,
		UpstreamKey:     body.UpstreamKey,
		DailyBudgetUSD:  body.DailyBudgetUSD,
		MonthBudgetUSD:  body.MonthBudgetUSD,
		RPSLimit:        body.RPSLimit,
		RPMLimit:        body.RPMLimit,
		MaxConcurrent:   body.MaxConcurrent,
		UpstreamBaseURL: canonicalBaseURL,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.keys.Create(r.Context(), vk); err != nil {
		slog.Error("create virtual key", "err", err)
		http.Error(w, "failed to create key", http.StatusInternalServerError)
		return
	}

	slog.Info("virtual key created", "id", vk.ID, "name", vk.Name, "upstream", vk.Upstream)
	api.WriteJSON(w, http.StatusCreated, createKeyResponse{
		keyView: viewOf(vk),
		Token:   vk.Token,
	})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	all, err := s.keys.List(r.Context())
	if err != nil {
		slog.Error("list virtual keys", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]keyView, 0, len(all))
	for _, k := range all {
		out = append(out, viewOf(k))
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !keys.ValidID(id) {
		// Reject anything that is not the exact "key_" + 16 hex format before
		// it reaches the keystore or any log line. A true from ValidID is a
		// hard guarantee that the remainder of the handler cannot log or
		// render an attacker-controlled string.
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}
	if err := s.keys.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, keys.ErrNotFound) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		slog.Error("revoke virtual key", "err", err, "id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	vk, err := s.keys.GetByID(r.Context(), id)
	if err != nil {
		slog.Error("get revoked key", "err", err, "id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("virtual key revoked", "id", vk.ID)
	api.WriteJSON(w, http.StatusOK, viewOf(vk))
}
