// Package api provides shared HTTP response helpers used by both the admin
// and proxy surfaces. Keeping the envelope shape in one place guarantees
// that every Rein HTTP error looks the same to machine clients.
package api

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope is the standard error shape returned by all Rein HTTP
// endpoints. The nested structure matches the idiom used by most modern
// APIs and was first shipped on the admin surface in 0.2.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes a structured JSON error response. It sets
// Content-Type to application/json, writes the status code, and encodes
// the envelope. Callers that need additional headers (e.g. Retry-After)
// must set them before calling WriteError.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Code: code, Message: message},
	})
}

// WriteJSON writes an arbitrary value as a JSON response with the given
// status code. Used for success responses across both admin and proxy.
func WriteJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
