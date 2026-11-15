package service

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// PrepareOpenAIStreamRetry is called only when a partial-output retry was
// admitted. Later HTTP errors must terminate the existing SSE stream, even if
// the upstream changes from SSE to a JSON error on the next attempt.
func PrepareOpenAIStreamRetry(c *gin.Context, accountID int64) {
	if c == nil || c.Request == nil || c.Writer == nil || accountID <= 0 {
		return
	}
	c.Request = c.Request.WithContext(WithOpenAIStreamRetryAccount(c.Request.Context(), accountID))
	if _, ok := c.Writer.(*openAIOutputRetryWriter); !ok {
		c.Writer = &openAIOutputRetryWriter{ResponseWriter: c.Writer, c: c}
	}
}

type openAIOutputRetryWriter struct {
	gin.ResponseWriter
	c          *gin.Context
	statusCode int
}

func (w *openAIOutputRetryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *openAIOutputRetryWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *openAIOutputRetryWriter) Write(p []byte) (int, error) {
	if !json.Valid(p) || !gjson.GetBytes(p, "error").IsObject() {
		return w.ResponseWriter.Write(p)
	}
	message := extractOpenAISSEErrorMessage(p)
	if message == "" {
		message = "Upstream request failed"
	}
	errType := gjson.GetBytes(p, "error.type").String()
	if errType == "" {
		errType = "upstream_error"
	}
	var frame string
	switch {
	case strings.HasSuffix(w.c.Request.URL.Path, "/messages"):
		frame = buildAnthropicStreamErrorSSE(errType, message)
	case strings.HasSuffix(w.c.Request.URL.Path, "/chat/completions"):
		frame = "data: " + string(p) + "\n\ndata: [DONE]\n\n"
	default:
		frame = buildOpenAIResponseFailedSSE("", "", 0, p, message)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	MarkResponseCommitted(w.c)
	MarkOpsStreamFailure(w.c, errType, gjson.GetBytes(p, "error.code").String(), message, w.statusCode)
	n, err := w.ResponseWriter.Write([]byte(frame))
	if err != nil {
		return 0, err
	}
	if n != len(frame) {
		return 0, io.ErrShortWrite
	}
	w.ResponseWriter.Flush()
	return len(p), nil
}

func (w *openAIOutputRetryWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}
