package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// maxSSEEventSize bounds a single SSE event so one oversized upstream line
// fails the stream instead of silently truncating it.
const maxSSEEventSize = 8 << 20

// ErrStreamTruncated reports an upstream stream that closed before its
// terminal event (message_stop, finishReason or [DONE]).
var ErrStreamTruncated = errors.New("upstream stream truncated")

var errSSEEventTooLarge = errors.New("upstream stream event exceeds size limit")

func streamTruncatedError(providerName string) error {
	return &ProviderError{ProviderName: providerName, Message: ErrStreamTruncated.Error(), Retryable: false}
}

func sseEventTooLargeError(providerName string) error {
	return &ProviderError{
		ProviderName: providerName,
		Message:      fmt.Sprintf("%s (%d bytes)", errSSEEventTooLarge, maxSSEEventSize),
		Retryable:    false,
	}
}

type sseEvent struct {
	Name string
	Data string
}

// sseReader parses Server-Sent Events without bufio.Scanner's token limit.
// Each event is bounded independently, so a large payload does not
// permanently disable the stream.
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 32<<10)}
}

func (r *sseReader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if len(line)+len(chunk) > maxSSEEventSize {
			return nil, errSSEEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// Next returns the next SSE event. A trailing event without a blank-line
// terminator is still returned before io.EOF.
func (r *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data strings.Builder
	have := false

	for {
		line, err := r.readLine()
		if data.Len()+len(line) > maxSSEEventSize {
			return sseEvent{}, errSSEEventTooLarge
		}
		trimmed := strings.TrimRight(string(line), "\r\n")
		switch {
		case trimmed == "":
			if have {
				ev.Data = data.String()
				return ev, nil
			}
		case strings.HasPrefix(trimmed, ":"):
			// comment/heartbeat
		case strings.HasPrefix(trimmed, "event:"):
			ev.Name = strings.TrimSpace(trimmed[len("event:"):])
			have = true
		case strings.HasPrefix(trimmed, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(trimmed[len("data:"):]))
			have = true
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if have {
					ev.Data = data.String()
					return ev, nil
				}
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
	}
}

// providerStreamError recognises an inline {"error":{...}} payload emitted in
// the middle of an SSE stream.
func providerStreamError(providerName string, payload []byte) *ProviderError {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Error) == 0 {
		return nil
	}
	var detail struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Status  string `json:"status"`
		Code    any    `json:"code"`
	}
	_ = json.Unmarshal(envelope.Error, &detail)

	message := detail.Message
	if message == "" {
		message = string(envelope.Error)
	}
	if detail.Type != "" {
		message = detail.Type + ": " + message
	}
	if detail.Status != "" {
		message = detail.Status + ": " + message
	}

	status := 0
	if code, ok := detail.Code.(float64); ok {
		status = int(code)
	}
	return &ProviderError{
		ProviderName: providerName,
		StatusCode:   status,
		Message:      truncateProviderError(message),
		Retryable:    status == 429 || status >= 500,
	}
}

// normalizeBaseURL trims the trailing slash and rejects URLs that would make
// every request fail with an opaque transport error.
func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(raw, "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed, fmt.Errorf("invalid base URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return trimmed, fmt.Errorf("invalid base URL %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return trimmed, fmt.Errorf("invalid base URL %q: missing host", raw)
	}
	return trimmed, nil
}
