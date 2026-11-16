package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only these explicit conversion rejections permit changing a native request.
const (
	anthropicStopSequencesUnsupported = "stop_sequences has no equivalent in Responses; use a native Messages or Chat Completions endpoint to retain this generation control"
	anthropicTopKResponsesUnsupported = "top_k has no equivalent in Responses; use a native Messages endpoint to retain this generation control"
	anthropicTopKChatUnsupported      = "top_k has no equivalent in Chat Completions; use a native Messages endpoint to retain this generation control"
)

func doAnthropicStopSequenceCompatibleRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	send func([]byte) (*http.Response, error),
) (*http.Response, []string, error) {
	var stops []string
	// Limit compatibility retries even if a request repeats the same JSON key.
	for compatibilityRetries := 0; ; compatibilityRetries++ {
		resp, err := send(body)
		if err != nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusBadRequest || compatibilityRetries >= 2 {
			return resp, stops, err
		}
		hasTopK := gjson.GetBytes(body, "top_k").Exists()
		var currentStops []string
		validStops := json.Unmarshal([]byte(gjson.GetBytes(body, "stop_sequences").Raw), &currentStops) == nil && len(currentStops) > 0
		for _, stop := range currentStops {
			if stop == "" {
				validStops = false
				break
			}
		}
		if !hasTopK && !validStops {
			return resp, stops, nil
		}
		// Restore every byte for ordinary 400 handling, including a failed read.
		const errorLimit = 64 << 10
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, errorLimit+1))
		upstreamBody := resp.Body
		resp.Body = &anthropicStopReplayBody{ReadCloser: upstreamBody, replay: bytes.NewReader(payload), readErr: readErr}
		if readErr != nil || len(payload) > errorLimit || !json.Valid(payload) {
			return resp, stops, nil
		}
		var parameter, message string
		switch gjson.GetBytes(payload, "error.message").String() {
		case anthropicStopSequencesUnsupported:
			if !validStops {
				return resp, stops, nil
			}
			parameter = "stop_sequences"
			message = "Upstream rejected stop_sequences; retrying with local stop-sequence handling"
		case anthropicTopKResponsesUnsupported, anthropicTopKChatUnsupported:
			if !hasTopK {
				return resp, stops, nil
			}
			parameter = "top_k"
			message = "Upstream rejected top_k; retrying without top_k in compatibility mode"
		default:
			return resp, stops, nil
		}
		stripped, err := sjson.DeleteBytes(body, parameter)
		if err != nil {
			return resp, stops, nil
		}
		if err := ctx.Err(); err != nil {
			return resp, stops, err
		}
		if parameter == "stop_sequences" {
			stops = currentStops
		}
		_ = resp.Body.Close()
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
			UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
			Kind: "compatibility_retry", Message: message,
		})
		body = stripped
	}
}

type anthropicStopReplayBody struct {
	io.ReadCloser
	replay  *bytes.Reader
	readErr error
}

func (r *anthropicStopReplayBody) Read(p []byte) (int, error) {
	if r.replay.Len() > 0 {
		return r.replay.Read(p)
	}
	if r.readErr != nil {
		return 0, r.readErr
	}
	return r.ReadCloser.Read(p)
}

type anthropicStopMatcher struct {
	stops    []string
	prefixes [][]int
	pending  string
}

// push retains only a suffix that could still become a stop sequence.
func (m *anthropicStopMatcher) push(text string) (string, string) {
	text = m.pending + text
	m.pending = ""
	first, end, matched := len(text), len(text)+1, ""
	for _, stop := range m.stops {
		if stop == "" {
			continue
		}
		if i := strings.Index(text, stop); i >= 0 && (i+len(stop) < end || (i+len(stop) == end && i < first)) {
			first, end, matched = i, i+len(stop), stop
		}
	}
	if matched != "" {
		return text[:first], matched
	}
	if m.prefixes == nil {
		m.prefixes = make([][]int, len(m.stops))
		for i, stop := range m.stops {
			prefix := make([]int, len(stop))
			for j, n := 1, 0; j < len(stop); j++ {
				for n > 0 && stop[j] != stop[n] {
					n = prefix[n-1]
				}
				if stop[j] == stop[n] {
					n++
				}
				prefix[j] = n
			}
			m.prefixes[i] = prefix
		}
	}
	hold := 0
	for i, stop := range m.stops {
		if len(stop) <= 1 {
			continue
		}
		// KMP bounds suffix checks for long, repetitive user-supplied stops.
		n := 0
		for j := max(0, len(text)-len(stop)+1); j < len(text); j++ {
			for n > 0 && text[j] != stop[n] {
				n = m.prefixes[i][n-1]
			}
			if text[j] == stop[n] {
				n++
			}
		}
		hold = max(hold, n)
	}
	m.pending = text[len(text)-hold:]
	return text[:len(text)-hold], ""
}

type anthropicStopFilter struct {
	stops   []string
	matched string
	blocks  map[int]string
	text    map[int]*anthropicStopMatcher
}

func (f *anthropicStopFilter) message(body []byte) ([]byte, error) {
	if gjson.GetBytes(body, "type").String() != "message" {
		return body, nil
	}
	content := gjson.GetBytes(body, "content").Array()
	for i, block := range content {
		if block.Get("type").String() != "text" {
			continue
		}
		m := anthropicStopMatcher{stops: f.stops}
		text, matched := m.push(block.Get("text").String())
		if matched == "" {
			continue
		}
		updated, err := sjson.Set(block.Raw, "text", text)
		if err != nil {
			return nil, err
		}
		blocks := make([]json.RawMessage, 0, i+1)
		for _, prior := range content[:i] {
			blocks = append(blocks, json.RawMessage(prior.Raw))
		}
		blocks = append(blocks, json.RawMessage(updated))
		body, err = sjson.SetBytes(body, "content", blocks)
		if err == nil {
			body, err = sjson.SetBytes(body, "stop_reason", "stop_sequence")
		}
		if err == nil {
			body, err = sjson.SetBytes(body, "stop_sequence", matched)
		}
		return body, err
	}
	return body, nil
}

func (f *anthropicStopFilter) flushText() [][]byte {
	indices := make([]int, 0, len(f.text))
	for index := range f.text {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var out [][]byte
	for _, index := range indices {
		if data := f.flushBlock(index); data != nil {
			out = append(out, data)
		}
	}
	return out
}

func (f *anthropicStopFilter) flushBlock(index int) []byte {
	m := f.text[index]
	if m == nil || m.pending == "" || f.matched != "" {
		return nil
	}
	data, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]string{"type": "text_delta", "text": m.pending},
	})
	m.pending = ""
	return data
}

func (f *anthropicStopFilter) event(data []byte) ([][]byte, error) {
	if !json.Valid(data) {
		return [][]byte{data}, nil
	}
	if f.blocks == nil {
		f.blocks = make(map[int]string)
		f.text = make(map[int]*anthropicStopMatcher)
	}
	typ := gjson.GetBytes(data, "type").String()
	index := int(gjson.GetBytes(data, "index").Int())
	var prefix [][]byte
	switch typ {
	case "content_block_start":
		if f.matched != "" {
			return nil, nil
		}
		f.blocks[index] = gjson.GetBytes(data, "content_block.type").String()
		if f.blocks[index] == "text" {
			m := &anthropicStopMatcher{stops: f.stops}
			f.text[index] = m
			text, matched := m.push(gjson.GetBytes(data, "content_block.text").String())
			f.matched = matched
			var err error
			data, err = sjson.SetBytes(data, "content_block.text", text)
			if err != nil {
				return nil, err
			}
		}
	case "content_block_delta":
		if gjson.GetBytes(data, "delta.type").String() == "text_delta" {
			if f.matched != "" {
				return nil, nil
			}
			m := f.text[index]
			if m == nil {
				return [][]byte{data}, nil
			}
			text, matched := m.push(gjson.GetBytes(data, "delta.text").String())
			f.matched = matched
			if text == "" {
				return nil, nil
			}
			var err error
			data, err = sjson.SetBytes(data, "delta.text", text)
			if err != nil {
				return nil, err
			}
		} else if _, open := f.blocks[index]; f.matched != "" && !open {
			return nil, nil
		}
	case "content_block_stop":
		if _, open := f.blocks[index]; !open && f.matched != "" {
			return nil, nil
		}
		if tail := f.flushBlock(index); tail != nil {
			prefix = append(prefix, tail)
		}
		delete(f.blocks, index)
		delete(f.text, index)
	case "message_delta":
		if reason := gjson.GetBytes(data, "delta.stop_reason"); reason.Type == gjson.String && reason.String() != "" {
			prefix = f.flushText()
			if f.matched != "" {
				var err error
				data, err = sjson.SetBytes(data, "delta.stop_reason", "stop_sequence")
				if err == nil {
					data, err = sjson.SetBytes(data, "delta.stop_sequence", f.matched)
				}
				if err != nil {
					return nil, err
				}
			}
		}
	case "message_stop", "error":
		prefix = f.flushText()
	}
	return append(prefix, data), nil
}

// Installed only for conversions or an explicit upstream incompatibility.
// Native successful requests retain their existing writer and wire format.
type anthropicStopSequenceWriter struct {
	gin.ResponseWriter
	filter anthropicStopFilter
	stream bool
	frame  []byte
	err    error
}

const anthropicLocalStopWriterKey = "anthropic_local_stop_writer"

func installAnthropicStopSequenceWriter(c *gin.Context, stops []string, stream bool) (*anthropicStopSequenceWriter, func()) {
	if len(stops) == 0 || c == nil {
		return nil, func() {}
	}
	base := c.Writer
	w := &anthropicStopSequenceWriter{ResponseWriter: base, filter: anthropicStopFilter{stops: stops}, stream: stream}
	if stream {
		// A permitted post-output retry must not resume text after a stop that
		// was already matched by the preceding attempt on the pinned account.
		if c.Request != nil && OpenAIStreamRetryAccountID(c.Request.Context()) > 0 {
			if previous, ok := c.Get(anthropicLocalStopWriterKey); ok {
				if prior, ok := previous.(*anthropicStopSequenceWriter); ok {
					for _, stop := range stops {
						if stop != "" && stop == prior.filter.matched {
							w.filter.matched = stop
						}
					}
				}
			}
		}
		c.Set(anthropicLocalStopWriterKey, w)
	}
	c.Writer = w
	return w, func() { c.Writer = base }
}

func (w *anthropicStopSequenceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *anthropicStopSequenceWriter) WriteHeaderNow() {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeaderNow()
}

func (w *anthropicStopSequenceWriter) Flush() {
	w.Header().Del("Content-Length")
	w.ResponseWriter.Flush()
}

func (w *anthropicStopSequenceWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.Status() >= http.StatusBadRequest {
		return w.ResponseWriter.Write(data)
	}
	w.Header().Del("Content-Length")
	if !w.stream {
		out, err := w.filter.message(data)
		if err == nil {
			err = w.write(out)
		}
		w.err = err
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
	w.frame = append(w.frame, data...)
	for {
		end, size := bytes.Index(w.frame, []byte("\n\n")), 2
		if crlf := bytes.Index(w.frame, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
			end, size = crlf, 4
		}
		if end < 0 {
			break
		}
		if err := w.writeFrame(w.frame[:end+size]); err != nil {
			w.err = err
			return 0, err
		}
		w.frame = w.frame[end+size:]
	}
	return len(data), nil
}

func (w *anthropicStopSequenceWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *anthropicStopSequenceWriter) write(data []byte) error {
	n, err := w.ResponseWriter.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (w *anthropicStopSequenceWriter) writeFrame(frame []byte) error {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\n"), "\n")
	var data []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 {
		return w.write(frame)
	}
	payload := []byte(strings.Join(data, "\n"))
	out, err := w.filter.event(payload)
	if err != nil {
		return err
	}
	if len(out) == 1 && bytes.Equal(out[0], payload) {
		return w.write(frame)
	}
	for i, event := range out {
		var buf bytes.Buffer
		if i == len(out)-1 {
			for _, line := range lines {
				if !strings.HasPrefix(line, "data:") && !strings.HasPrefix(line, "event:") {
					buf.WriteString(line + "\n")
				}
			}
		}
		buf.WriteString("event: " + gjson.GetBytes(event, "type").String() + "\ndata: ")
		if err := json.Compact(&buf, event); err != nil {
			return err
		}
		buf.WriteString("\n\n")
		if err := w.write(buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (w *anthropicStopSequenceWriter) finish() error {
	if w == nil {
		return nil
	}
	if w.err == nil && len(w.frame) > 0 {
		w.err = w.writeFrame(w.frame)
		w.frame = nil
	}
	if w.stream && w.Written() {
		w.ResponseWriter.Flush()
	}
	return w.err
}
