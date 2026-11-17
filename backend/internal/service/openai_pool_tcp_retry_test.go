package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type poolTCPResetBody struct {
	io.Reader
	err error
}

func (b *poolTCPResetBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = b.err
	}
	return n, err
}

func (*poolTCPResetBody) Close() error { return nil }

func poolTCPReadReset() error {
	return fmt.Errorf("body read: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET})
}

func TestPoolTCPReadResetRetriesAtStreamBoundary(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, partial := range []bool{false, true} {
			for _, keepalive := range []int{0, 1} {
				t.Run(fmt.Sprintf("%s/partial=%v/keepalive=%d", path, partial, keepalive), func(t *testing.T) {
					c, rec := newOpenAIConfiguredRetryContext()
					if path == "messages" {
						c.Request.URL.Path = "/v1/messages"
					} else if path == "chat" {
						c.Request.URL.Path = "/v1/chat/completions"
					}
					body := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_tcp"}}`)
					if partial {
						body += configuredRetryOutput("text")
					}
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"tcp-synthetic"}}, Body: &poolTCPResetBody{Reader: strings.NewReader(body), err: poolTCPReadReset()}}
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: keepalive}}}
					err := runConfiguredRetryStream(svc, c, resp, newConfiguredStreamRetryAccount(), path)
					var retry *UpstreamFailoverError
					require.ErrorAs(t, err, &retry)
					require.True(t, retry.RetryableOnSameAccount)
					require.False(t, retry.ShouldRetryNextAccount())
					require.NotNil(t, retry.TransportFailure)
					require.Equal(t, "TCP", retry.TransportFailure.ErrorFields()["protocol"])
					require.NotContains(t, rec.Body.String(), "upstream_stream_read_error", "intermediate resets must not send an error frame")
					_, marked := GetOpsStreamError(c)
					require.False(t, marked)
					if partial {
						require.True(t, retry.RetryAfterOutput)
						require.Contains(t, rec.Body.String(), "private answer")
					}
				})
			}
		}
	}
}

func TestPoolTCPReadResetRequiresTrustedTransportEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped read reset", poolTCPReadReset(), true},
		{"syscall wrapped", &net.OpError{Op: "read", Net: "tcp6", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}, true},
		{"text only", errors.New("read tcp customer-secret: read: connection reset by peer"), false},
		{"write reset", &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}, false},
		{"other network", &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, false},
		{"clean EOF", io.EOF, false},
		{"canceled", errors.Join(poolTCPReadReset(), context.Canceled), false},
		{"deadline", errors.Join(poolTCPReadReset(), context.DeadlineExceeded), false},
		{"body limit", errors.Join(poolTCPReadReset(), ErrUpstreamResponseBodyTooLarge), false},
		{"semantic failure", errors.Join(poolTCPReadReset(), &UpstreamFailoverError{StatusCode: 502}), false},
		{"image failure", errors.Join(poolTCPReadReset(), &OpenAIImagesUpstreamError{StatusCode: 502}), false},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, openAITCPReadReset(tc.err)) })
	}
}

func TestPoolTCPReadResetRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*gin.Context, *Account)
		want   bool
	}{
		{name: "configured", want: true},
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
			account := newConfiguredStreamRetryAccount()
			if tc.change != nil {
				tc.change(c, account)
			}
			err, handled := handleOpenAIStreamTransportReset(c, account, "tcp-synthetic", poolTCPReadReset())
			require.Equal(t, tc.want, handled)
			require.Empty(t, rec.Body.String())
			if !tc.want {
				require.NoError(t, err)
				_, exists := c.Get(OpsUpstreamErrorsKey)
				require.False(t, exists, "opt-outs must retain their existing error path")
				return
			}
			var retry *UpstreamFailoverError
			require.ErrorAs(t, err, &retry)
			require.False(t, retry.ShouldReportAccountScheduleFailure())
			_, _, healthFailure := classifyOpenAIAPIKeyHealthFailure(err)
			require.False(t, healthFailure)
			value, exists := c.Get(OpsUpstreamErrorsKey)
			require.True(t, exists)
			events := value.([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, "transport_error", events[0].Kind)
			require.Equal(t, "CONNECTION_RESET", events[0].Reason)
			require.Zero(t, events[0].UpstreamStatusCode)
			require.True(t, WriteOpenAIStreamTransportFailure(c, retry))
			require.True(t, WriteOpenAIStreamTransportFailure(c, retry))
			require.Equal(t, 1, strings.Count(rec.Body.String(), OpenAIUpstreamStreamReadErrorCode))
			marked, exists := GetOpsStreamError(c)
			require.True(t, exists)
			require.NotNil(t, marked.TransportFailure)
		})
	}
}

func TestPoolTCPReadResetUserDetailsStaySanitized(t *testing.T) {
	status := 502
	entry := &OpsErrorLogDetail{OpsErrorLog: OpsErrorLog{Type: "transport_error", Phase: "network", StatusCode: 200, Message: openAIUpstreamStreamInterruptedMessage},
		UpstreamStatusCode: &status, ErrorBody: `{"error":{"protocol":"TCP","code":"upstream_stream_read_error","reset_reason":"customer-secret","message":"private","headers":{"Authorization":"secret"}}}`}
	require.Equal(t, openAIUpstreamStreamInterruptedMessage, ToUserErrorRequest(&entry.OpsErrorLog).Message)
	detail := ToUserErrorRequestDetail(entry)
	require.Nil(t, detail.UpstreamStatusCode)
	require.Equal(t, "CONNECTION_RESET", detail.ResetReason)
	require.Equal(t, openAIUpstreamStreamInterruptedMessage, detail.Message)
	data, err := json.Marshal(detail)
	require.NoError(t, err)
	require.Contains(t, string(data), "upstream_stream_read_error")
	require.NotContains(t, string(data), "HTTP/2")
	for _, private := range []string{"customer-secret", "private", "Authorization", "secret"} {
		require.NotContains(t, string(data), private)
	}
}
