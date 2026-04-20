package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/archilea/rein/internal/api"
)

// writeUpstreamTimeout writes a structured 504 envelope for a request
// whose per-key upstream_timeout_seconds ceiling fired. The message
// names the configured timeout so operators can correlate with their
// key config without opening Rein's logs. Non-streaming only: on a
// streaming response the 200 status has already been flushed before
// the deadline fires, so stream.go writes a short SSE comment line and
// closes the connection instead.
func writeUpstreamTimeout(w http.ResponseWriter, r *http.Request) {
	seconds := 0
	if vk := VKeyFromContext(r.Context()); vk != nil {
		seconds = vk.UpstreamTimeoutSeconds
	}
	msg := "upstream request timed out"
	if seconds > 0 {
		msg = fmt.Sprintf("upstream request timed out after %d seconds", seconds)
	}
	w.Header().Set("Retry-After", "1")
	api.WriteError(w, http.StatusGatewayTimeout, CodeUpstreamTimeout, msg)
}

// isUpstreamTimeout reports whether err is a context.DeadlineExceeded.
// ReverseProxy surfaces deadline cancels as wrapped *url.Error, so
// errors.Is is the right hook here. Client cancels (context.Canceled)
// are NOT treated as upstream timeouts; ErrorHandler falls through to
// the regular 502 in that case.
func isUpstreamTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
