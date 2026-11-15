package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIConfiguredStreamRetryAfterOutputPolicy(t *testing.T) {
	for _, policy := range []string{"non_pool", "zero_retries", "unconfigured_status", "oauth", "canceled", "committed_error", "image_request"} {
		for _, path := range []string{"native", "passthrough", "messages", "chat"} {
			t.Run(path+"/"+policy, func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				c, rec := newOpenAIConfiguredRetryContext()
				switch policy {
				case "non_pool":
					account.Credentials["pool_mode"] = false
				case "zero_retries":
					account.Credentials["pool_mode_retry_count"] = float64(0)
				case "unconfigured_status":
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusServiceUnavailable)}
				case "oauth":
					account.Type = AccountTypeOAuth
				case "canceled":
					ctx, cancel := context.WithCancel(c.Request.Context())
					cancel()
					c.Request = c.Request.WithContext(ctx)
				case "committed_error":
					MarkResponseCommitted(c)
				case "image_request":
					c.Request = c.Request.WithContext(WithOpenAIImageGenerationIntent(c.Request.Context()))
				}
				body := configuredRetryOutput("text") + configuredRetryEvent(messagesBPSFailure)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{}, c, resp, account, path)
				var retry *UpstreamFailoverError
				if policy == "canceled" {
					require.Empty(t, rec.Body.String())
				}
				if errors.As(err, &retry) {
					require.False(t, retry.RetryAfterOutput, "excluded policies must not permit replay after client output")
				}
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryAfterOutputKeepsErrorFraming(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			c.Request.URL.Path = path
			c.Header("Content-Type", "text/event-stream")
			_, err := c.Writer.WriteString(": existing stream\n\n")
			require.NoError(t, err)
			c.Writer.Flush()
			PrepareOpenAIStreamRetry(c, 59328)
			PrepareOpenAIStreamRetry(c, 59328)
			require.Equal(t, int64(59328), OpenAIStreamRetryAccountID(c.Request.Context()))
			c.JSON(http.StatusBadRequest, map[string]any{"error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_value", "message": "rejected parameter",
			}})
			require.Contains(t, rec.Body.String(), "data: ")
			require.Contains(t, rec.Body.String(), "rejected parameter")
			require.True(t, IsResponseCommitted(c))
			require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
			if path == "/v1/responses" {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
				require.Contains(t, rec.Body.String(), "invalid_value")
			} else if path == "/v1/messages" {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
			} else {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "data: [DONE]"))
			}
		})
	}
}
