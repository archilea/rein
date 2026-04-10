package proxy

import (
	"bytes"
	"encoding/json"
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
type streamMeter struct {
	body      io.ReadCloser
	upstream  string
	partial   []byte // bytes from the previous Read that didn't end in '\n'
	model     string
	inputTok  int
	outputTok int
	reported  bool
	onDone    func(model string, inputTokens, outputTokens int)
}

func newStreamMeter(body io.ReadCloser, upstream string, onDone func(string, int, int)) *streamMeter {
	return &streamMeter{body: body, upstream: upstream, onDone: onDone}
}

// Read satisfies io.Reader. It delegates to the underlying body, then tees the
// bytes into a line-oriented parser.
func (s *streamMeter) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.consume(p[:n])
	}
	return n, err
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
