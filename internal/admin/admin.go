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

// immutableFieldError is the structured error code returned when a PATCH
// request attempts to change an immutable field (id, token, upstream,
// upstream_key, created_at, revoked_at).
const immutableFieldError = "immutable_field"

// Structured error codes for the expires_at field. Separate codes let
// machine clients distinguish "your RFC3339 string did not parse" from
// "your RFC3339 string was in the past"; both still return 400.
const (
	errCodeInvalidExpiresAt = "invalid_expires_at"
	errCodeExpiresInPast    = "expires_in_past"
)

// expiresInFutureSkew is the minimum delta between now and the supplied
// expires_at for a create/update to be accepted. Matches the #77 spec:
// "more than 1 second from now". Rejecting sub-second deltas keeps the
// sweeper from racing a key that was already practically expired at
// creation time.
const expiresInFutureSkew = 1 * time.Second

// maxUpstreamTimeoutSeconds is the upper bound on upstream_timeout_seconds.
// One hour covers every realistic LLM request (reasoning models, extended
// thinking, multi-turn tool use) and protects operators from typos such as
// 86400 ("I meant a day" when the admin API takes seconds).
const maxUpstreamTimeoutSeconds = 3600

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
		mux.Handle("PATCH /admin/v1/keys/{id}", s.withAuth(s.handleUpdateKey))
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
	ID                     string     `json:"id"`
	Name                   string     `json:"name"`
	Upstream               string     `json:"upstream"`
	DailyBudgetUSD         float64    `json:"daily_budget_usd"`
	MonthBudgetUSD         float64    `json:"month_budget_usd"`
	RPSLimit               int        `json:"rps_limit"`
	RPMLimit               int        `json:"rpm_limit"`
	MaxConcurrent          int        `json:"max_concurrent"`
	UpstreamTimeoutSeconds int        `json:"upstream_timeout_seconds"`
	UpstreamBaseURL        string     `json:"upstream_base_url,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	RevokedAt              *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
}

func viewOf(k *keys.VirtualKey) keyView {
	return keyView{
		ID:                     k.ID,
		Name:                   k.Name,
		Upstream:               k.Upstream,
		DailyBudgetUSD:         k.DailyBudgetUSD,
		MonthBudgetUSD:         k.MonthBudgetUSD,
		RPSLimit:               k.RPSLimit,
		RPMLimit:               k.RPMLimit,
		MaxConcurrent:          k.MaxConcurrent,
		UpstreamTimeoutSeconds: k.UpstreamTimeoutSeconds,
		UpstreamBaseURL:        k.UpstreamBaseURL,
		CreatedAt:              k.CreatedAt,
		RevokedAt:              k.RevokedAt,
		ExpiresAt:              k.ExpiresAt,
	}
}

// messageForExpiresAtCode maps the structured error code for an
// expires_at validation failure to a stable human message. Keeping the
// mapping small and centralized prevents the messages from drifting
// between create and PATCH.
func messageForExpiresAtCode(code string) string {
	switch code {
	case errCodeExpiresInPast:
		return "expires_at must be in the future"
	case errCodeInvalidExpiresAt:
		return "expires_at must be an RFC3339 UTC timestamp"
	default:
		return "invalid expires_at"
	}
}

// parseExpiresAt parses an expires_at string under the same RFC3339
// contract as CreatedAt and RevokedAt: RFC3339 or RFC3339Nano, UTC
// preferred but any offset is converted. The future-skew check uses the
// caller-supplied `now` so tests can exercise the boundary deterministically.
// Returns (utc time, nil) on success; (_, api code) where api code is one
// of errCodeInvalidExpiresAt / errCodeExpiresInPast on failure.
func parseExpiresAt(raw string, now time.Time) (time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errCodeInvalidExpiresAt
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errCodeInvalidExpiresAt
	}
	t = t.UTC()
	if !t.After(now.Add(expiresInFutureSkew)) {
		return time.Time{}, errCodeExpiresInPast
	}
	return t, ""
}

// createKeyResponse includes the secret token. This is the ONE time the caller
// sees the token: Rein never returns it on list or get.
type createKeyResponse struct {
	keyView
	Token string `json:"token"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name                   string  `json:"name"`
		Upstream               string  `json:"upstream"`
		UpstreamKey            string  `json:"upstream_key"`
		DailyBudgetUSD         float64 `json:"daily_budget_usd"`
		MonthBudgetUSD         float64 `json:"month_budget_usd"`
		RPSLimit               int     `json:"rps_limit"`
		RPMLimit               int     `json:"rpm_limit"`
		MaxConcurrent          int     `json:"max_concurrent"`
		UpstreamTimeoutSeconds int     `json:"upstream_timeout_seconds"`
		UpstreamBaseURL        string  `json:"upstream_base_url"`
		ExpiresAt              string  `json:"expires_at"`
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
	if body.UpstreamTimeoutSeconds < 0 || body.UpstreamTimeoutSeconds > maxUpstreamTimeoutSeconds {
		http.Error(w, "upstream_timeout_seconds must be between 0 and 3600", http.StatusBadRequest)
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

	// Optional auto-revocation expiry. Omitted means no expiry. Present
	// and malformed or in the past returns a structured envelope.
	var expiresAt *time.Time
	if strings.TrimSpace(body.ExpiresAt) != "" {
		parsed, code := parseExpiresAt(body.ExpiresAt, time.Now().UTC())
		if code != "" {
			api.WriteError(w, http.StatusBadRequest, code, messageForExpiresAtCode(code))
			return
		}
		expiresAt = &parsed
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
		ID:                     id,
		Token:                  token,
		Name:                   name,
		Upstream:               body.Upstream,
		UpstreamKey:            body.UpstreamKey,
		DailyBudgetUSD:         body.DailyBudgetUSD,
		MonthBudgetUSD:         body.MonthBudgetUSD,
		RPSLimit:               body.RPSLimit,
		RPMLimit:               body.RPMLimit,
		MaxConcurrent:          body.MaxConcurrent,
		UpstreamTimeoutSeconds: body.UpstreamTimeoutSeconds,
		UpstreamBaseURL:        canonicalBaseURL,
		CreatedAt:              time.Now().UTC(),
		ExpiresAt:              expiresAt,
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

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !keys.ValidID(id) {
		http.Error(w, "invalid key id", http.StatusBadRequest)
		return
	}

	var body struct {
		// Mutable fields. Nil means "leave unchanged".
		Name                   *string  `json:"name,omitempty"`
		DailyBudgetUSD         *float64 `json:"daily_budget_usd,omitempty"`
		MonthBudgetUSD         *float64 `json:"month_budget_usd,omitempty"`
		RPSLimit               *int     `json:"rps_limit,omitempty"`
		RPMLimit               *int     `json:"rpm_limit,omitempty"`
		MaxConcurrent          *int     `json:"max_concurrent,omitempty"`
		UpstreamTimeoutSeconds *int     `json:"upstream_timeout_seconds,omitempty"`
		UpstreamBaseURL        *string  `json:"upstream_base_url,omitempty"`

		// ExpiresAt is tri-state. Non-pointer json.RawMessage is used
		// deliberately: *json.RawMessage collapses "absent" and "null"
		// to the same nil pointer, so we cannot distinguish the two
		// meanings. A bare json.RawMessage leaves the slice empty when
		// the field is absent and stores []byte("null") when the
		// client sends an explicit null.
		//   absent field    -> leave unchanged (len == 0)
		//   explicit null   -> clear (bytes == "null")
		//   RFC3339 string  -> set
		ExpiresAt json.RawMessage `json:"expires_at,omitempty"`

		// Immutable field probes. Non-nil means the client attempted to
		// change an immutable field, which is rejected with 400.
		ID          *json.RawMessage `json:"id,omitempty"`
		Token       *json.RawMessage `json:"token,omitempty"`
		Upstream    *json.RawMessage `json:"upstream,omitempty"`
		UpstreamKey *json.RawMessage `json:"upstream_key,omitempty"`
		CreatedAt   *json.RawMessage `json:"created_at,omitempty"`
		RevokedAt   *json.RawMessage `json:"revoked_at,omitempty"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if body.ID != nil || body.Token != nil || body.Upstream != nil ||
		body.UpstreamKey != nil || body.CreatedAt != nil || body.RevokedAt != nil {
		api.WriteError(w, http.StatusBadRequest, immutableFieldError,
			"id, token, upstream, upstream_key, created_at, and revoked_at cannot be changed")
		return
	}

	// Validate mutable fields using the same rules as handleCreateKey.
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			http.Error(w, "name must not be blank", http.StatusBadRequest)
			return
		}
		if len(name) > 100 {
			http.Error(w, "name must be 100 characters or fewer", http.StatusBadRequest)
			return
		}
		body.Name = &name
	}
	if (body.DailyBudgetUSD != nil && *body.DailyBudgetUSD < 0) ||
		(body.MonthBudgetUSD != nil && *body.MonthBudgetUSD < 0) {
		http.Error(w, "budgets must be non-negative", http.StatusBadRequest)
		return
	}
	if (body.RPSLimit != nil && *body.RPSLimit < 0) ||
		(body.RPMLimit != nil && *body.RPMLimit < 0) {
		http.Error(w, "rate limits must be non-negative", http.StatusBadRequest)
		return
	}
	if body.MaxConcurrent != nil && *body.MaxConcurrent < 0 {
		http.Error(w, "max_concurrent must be non-negative", http.StatusBadRequest)
		return
	}
	if body.UpstreamTimeoutSeconds != nil &&
		(*body.UpstreamTimeoutSeconds < 0 || *body.UpstreamTimeoutSeconds > maxUpstreamTimeoutSeconds) {
		http.Error(w, "upstream_timeout_seconds must be between 0 and 3600", http.StatusBadRequest)
		return
	}

	// upstream_base_url validation requires the key's immutable upstream field.
	var canonicalBaseURL *string
	if body.UpstreamBaseURL != nil {
		raw := strings.TrimSpace(*body.UpstreamBaseURL)
		if raw != "" {
			current, err := s.keys.GetByID(r.Context(), id)
			if err != nil {
				if errors.Is(err, keys.ErrNotFound) {
					http.Error(w, "key not found", http.StatusNotFound)
					return
				}
				slog.Error("get key for update validation", "err", err, "id", id)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if current.Upstream != keys.UpstreamOpenAI {
				api.WriteError(w, http.StatusBadRequest,
					keys.ErrCodeInvalidBaseURL,
					"upstream_base_url is only supported when upstream is 'openai'")
				return
			}
			canonical, err := keys.ValidateUpstreamBaseURL(raw)
			if err != nil {
				if bue := keys.AsBaseURLError(err); bue != nil {
					api.WriteError(w, http.StatusBadRequest, bue.Code, bue.Message)
					return
				}
				api.WriteError(w, http.StatusBadRequest,
					keys.ErrCodeInvalidBaseURL, "invalid upstream_base_url")
				return
			}
			raw = canonical
		}
		canonicalBaseURL = &raw
	}

	// Tri-state expires_at: null clears, RFC3339 string sets, absent leaves
	// the field alone.
	var expiresAtPtr *time.Time
	clearExpiresAt := false
	if len(body.ExpiresAt) > 0 {
		raw := strings.TrimSpace(string(body.ExpiresAt))
		switch {
		case raw == "null":
			clearExpiresAt = true
		case strings.HasPrefix(raw, "\""):
			var s string
			if err := json.Unmarshal(body.ExpiresAt, &s); err != nil {
				api.WriteError(w, http.StatusBadRequest,
					errCodeInvalidExpiresAt, messageForExpiresAtCode(errCodeInvalidExpiresAt))
				return
			}
			parsed, code := parseExpiresAt(s, time.Now().UTC())
			if code != "" {
				api.WriteError(w, http.StatusBadRequest, code, messageForExpiresAtCode(code))
				return
			}
			expiresAtPtr = &parsed
		default:
			api.WriteError(w, http.StatusBadRequest,
				errCodeInvalidExpiresAt, messageForExpiresAtCode(errCodeInvalidExpiresAt))
			return
		}
	}

	patch := keys.KeyPatch{
		Name:                   body.Name,
		DailyBudgetUSD:         body.DailyBudgetUSD,
		MonthBudgetUSD:         body.MonthBudgetUSD,
		RPSLimit:               body.RPSLimit,
		RPMLimit:               body.RPMLimit,
		MaxConcurrent:          body.MaxConcurrent,
		UpstreamTimeoutSeconds: body.UpstreamTimeoutSeconds,
		UpstreamBaseURL:        canonicalBaseURL,
		ExpiresAt:              expiresAtPtr,
		ClearExpiresAt:         clearExpiresAt,
	}

	updated, err := s.keys.Update(r.Context(), id, patch)
	if err != nil {
		if errors.Is(err, keys.ErrNotFound) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, keys.ErrRevoked) {
			api.WriteError(w, http.StatusConflict, "key_revoked",
				"cannot update a revoked key")
			return
		}
		slog.Error("update virtual key", "err", err, "id", id)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	slog.Info("virtual key updated", "id", updated.ID)
	api.WriteJSON(w, http.StatusOK, viewOf(updated))
}
