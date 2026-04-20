package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/archilea/rein/internal/keys"
)

// streamMeter wraps an SSE response body so we can parse token usage as it
// flows to the client without buffering the full stream. Bytes pass through
// untouched; parsing happens on a copy of the byte stream. When the underlying
// body is closed (normal EOF or client disconnect), onDone is called once with
// whatever usage fields were captured.
//
// For both upstreams the final usage is only meaningful at stream end:
//   - OpenAI emits a single trailing chunk with a populated usage object
//     (requires stream_options.include_usage: true, which Rein auto-injects).
//   - Anthropic emits input_tokens in message_start and running output_tokens
//     in each message_delta; the last message_delta carries the final total.
//
// streamMeter also handles the per-key upstream timeout (issue #30): if ctx
// carries a context.DeadlineExceeded at the moment the underlying body Read
// fails, a final SSE comment line is flushed to the client and the stream
// is closed cleanly, so the 200 status already flushed at the start of the
// stream is not retroactively changed to 504 (which is impossible over HTTP).
// The partial-metering invariant from docs/architecture.md:42 still holds:
// Close() fires onDone with whatever usage was parsed before the cancel.
type streamMeter struct {
	body      io.ReadCloser
	upstream  string
	partial   []byte // bytes from the previous Read that didn't end in '\n'
	model     string
	inputTok  int
	outputTok int
	reported  bool
	onDone    func(model string, inputTokens, outputTokens int)
	// ctx is the request context. Nil means "do not inject a timeout
	// trailer" (pre-existing non-timeout callers keep working). Callers
	// that set a per-key upstream timeout pass the request ctx so the
	// meter can detect context.DeadlineExceeded on the failing Read.
	ctx context.Context
	// pending holds the timeout trailer bytes after the deadline fires,
	// drained across as many Reads as the caller needs before EOF. Using
	// a single buffer means io.Copy flushes the trailer even when the
	// caller's buffer is smaller than the trailer.
	pending []byte
	// ended is set once the underlying body has errored or EOFed so we
	// do not try to Read past the trailer.
	ended bool
}

func newStreamMeter(body io.ReadCloser, upstream string, onDone func(string, int, int), ctx context.Context) *streamMeter {
	return &streamMeter{body: body, upstream: upstream, onDone: onDone, ctx: ctx}
}

// Read satisfies io.Reader. It delegates to the underlying body, then tees the
// bytes into a line-oriented parser. On context.DeadlineExceeded (per-key
// upstream timeout), Read drains any already-read bytes, then injects a final
// SSE comment trailer before reporting io.EOF. The surrounding ReverseProxy
// sees a clean end-of-stream rather than a cancel error, so it does not emit
// its own 500.
func (s *streamMeter) Read(p []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		if len(s.pending) == 0 {
			return n, io.EOF
		}
		return n, nil
	}
	if s.ended {
		return 0, io.EOF
	}
	n, err := s.body.Read(p)
	if n > 0 {
		s.consume(p[:n])
	}
	if err == nil {
		return n, nil
	}
	if s.ctx != nil && errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
		seconds := 0
		if vk := VKeyFromContext(s.ctx); vk != nil {
			seconds = vk.UpstreamTimeoutSeconds
		}
		s.pending = buildTimeoutTrailer(seconds)
		s.ended = true
		// Return already-read bytes now; next Read drains the trailer,
		// then reports io.EOF. Suppressing err here is deliberate: the
		// client already sees a graceful close via the trailer, and
		// ReverseProxy's error handler does not fire on a nil err path.
		return n, nil
	}
	return n, err
}

// buildTimeoutTrailer returns the SSE comment line written to the client
// when a streaming response is cut short by the per-key upstream timeout.
// SSE comments (lines that start with a colon) are legal and ignored by
// compliant parsers, so strict clients see a no-op followed by a clean
// close. Clients that expect a `[DONE]` sentinel will not see one; that
// is correct, because we cannot honestly claim "done" on a canceled call.
func buildTimeoutTrailer(seconds int) []byte {
	if seconds > 0 {
		return []byte(fmt.Sprintf(": rein upstream timeout after %d seconds\n\n", seconds))
	}
	return []byte(": rein upstream timeout\n\n")
}

// Close finalizes parsing and reports the captured usage. It is safe to call
// multiple times and will only fire the callback once.
func (s *streamMeter) Close() error {
	if !s.reported {
		s.reported = true
		if s.onDone != nil && (s.inputTok > 0 || s.outputTok > 0) {
			s.onDone(s.model, s.inputTok, s.outputTok)
		}
	}
	return s.body.Close()
}

// consume buffers incoming bytes and parses any complete SSE lines found.
func (s *streamMeter) consume(chunk []byte) {
	s.partial = append(s.partial, chunk...)
	for {
		idx := bytes.IndexByte(s.partial, '\n')
		if idx < 0 {
			return
		}
		line := bytes.TrimRight(s.partial[:idx], "\r")
		s.partial = s.partial[idx+1:]
		s.parseLine(line)
	}
}

// parseLine extracts usage from a single `data: {...}` SSE line.
func (s *streamMeter) parseLine(line []byte) {
	const dataPrefix = "data: "
	if !bytes.HasPrefix(line, []byte(dataPrefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(dataPrefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	switch s.upstream {
	case keys.UpstreamOpenAI:
		s.parseOpenAI(payload)
	case keys.UpstreamAnthropic:
		s.parseAnthropic(payload)
	}
}

func (s *streamMeter) parseOpenAI(payload []byte) {
	var chunk struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if s.model == "" && chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.inputTok = chunk.Usage.PromptTokens
		s.outputTok = chunk.Usage.CompletionTokens
	}
}

func (s *streamMeter) parseAnthropic(payload []byte) {
	var chunk struct {
		Type    string `json:"type"`
		Message *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	// message_start carries the initial input + output token counts
	// plus the model name. message_delta updates running output_tokens.
	switch chunk.Type {
	case "message_start":
		if chunk.Message != nil {
			if chunk.Message.Model != "" {
				s.model = chunk.Message.Model
			}
			if chunk.Message.Usage != nil {
				s.inputTok = chunk.Message.Usage.InputTokens
				s.outputTok = chunk.Message.Usage.OutputTokens
			}
		}
	case "message_delta":
		if chunk.Usage != nil && chunk.Usage.OutputTokens > 0 {
			s.outputTok = chunk.Usage.OutputTokens
		}
	}
}
