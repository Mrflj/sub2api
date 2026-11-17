package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestPoolHTTP2PeerResetRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*gin.Context, *Account)
		retry  bool
	}{
		{name: "configured", retry: true},
		{name: "partial output", retry: true, change: func(c *gin.Context, _ *Account) {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString("data: partial\n\n")
			c.Writer.Flush()
		}},
		{name: "non-pool", change: func(_ *gin.Context, a *Account) { a.Credentials["pool_mode"] = false }},
		{name: "zero budget", change: func(_ *gin.Context, a *Account) { a.Credentials["pool_mode_retry_count"] = 0 }},
		{name: "unconfigured 502", change: func(_ *gin.Context, a *Account) { a.Credentials["pool_mode_retry_status_codes"] = []any{429} }},
		{name: "oauth", change: func(_ *gin.Context, a *Account) { a.Type = AccountTypeOAuth }},
		{name: "other platform", change: func(_ *gin.Context, a *Account) { a.Platform = PlatformAnthropic }},
		{name: "images", change: func(c *gin.Context, _ *Account) { c.Request.URL.Path = "/v1/images/generations" }},
		{name: "websocket", change: func(c *gin.Context, _ *Account) { SetOpenAIClientTransport(c, OpenAIClientTransportWS) }},
		{name: "committed", change: func(c *gin.Context, _ *Account) { MarkResponseCommitted(c) }},
		{name: "canceled", change: func(c *gin.Context, _ *Account) {
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 2, "pool_mode_retry_status_codes": []any{502}}}
			if tc.change != nil {
				tc.change(c, account)
			}
			before := rec.Body.String()
			cause := http2.StreamError{StreamID: 123, Code: http2.ErrCodeInternal, Cause: errors.New("received from peer")}
			err, _ := handleOpenAIStreamTransportReset(c, account, "synthetic", cause)
			var retry *UpstreamFailoverError
			require.Equal(t, tc.retry, errors.As(err, &retry))
			if tc.retry {
				require.Equal(t, 502, retry.StatusCode)
				require.True(t, retry.RetryableOnSameAccount)
				require.False(t, retry.ShouldRetryNextAccount())
				require.False(t, retry.ShouldReportAccountScheduleFailure())
				require.Equal(t, before, rec.Body.String(), "intermediate failure must not terminate the client stream")
				_, marked := GetOpsStreamError(c)
				require.False(t, marked, "a retriable reset is not a terminal request outcome")
				_, _, eligible := classifyOpenAIAPIKeyHealthFailure(err)
				require.False(t, eligible)
			}
		})
	}
}

func TestPoolHTTP2PeerResetRetryBeforeOutputPreservesJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	PrepareOpenAIStreamRetry(c, 7)
	require.Equal(t, int64(7), OpenAIStreamRetryAccountID(c.Request.Context()))
	c.JSON(http.StatusOK, gin.H{"id": "resp_synthetic", "status": "completed"})
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"id":"resp_synthetic","status":"completed"}`, rec.Body.String())
	_, marked := GetOpsStreamError(c)
	require.False(t, marked)
}

type poolHTTP2ObservedWriter struct {
	gin.ResponseWriter
	seen chan struct{}
	once sync.Once
}

func (w *poolHTTP2ObservedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && strings.Contains(string(p[:n]), "private answer") {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *poolHTTP2ObservedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func TestPoolHTTP2PeerResetNormalOutputStreamsImmediately(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, _ := newOpenAIConfiguredRetryContext()
			account := newConfiguredStreamRetryAccount()
			PrepareOpenAIStreamRetry(c, account.ID)
			seen := make(chan struct{})
			c.Writer = &poolHTTP2ObservedWriter{ResponseWriter: c.Writer, seen: seen}
			reader, writer := io.Pipe()
			defer reader.Close()
			done := make(chan error, 1)
			go func() {
				defer writer.Close()
				if _, err := io.WriteString(writer, configuredRetryOutput("text")); err != nil {
					done <- err
					return
				}
				select {
				case <-seen:
				case <-time.After(2 * time.Second):
					err := errors.New("normal answer was buffered until the terminal event")
					_ = writer.CloseWithError(err)
					done <- err
					return
				}
				_, err := io.WriteString(writer, configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_synthetic","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
				done <- err
			}()
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
			require.NoError(t, err)
			require.NoError(t, <-done)
		})
	}
}
