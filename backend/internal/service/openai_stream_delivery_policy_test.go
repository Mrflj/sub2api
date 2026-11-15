package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func configuredRetryWebSearchOutput() string {
	return configuredRetryEvent(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_attempt","type":"web_search_call","status":"in_progress"}}`) +
		configuredRetryEvent(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ws_attempt","type":"web_search_call","status":"completed","action":{"type":"search","query":"normal search query"}}}`)
}

func configuredRetryFailurePayload(status int) string {
	if status == http.StatusTooManyRequests {
		return `{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later","type":"tokens"}`
	}
	return `{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"}`
}

func TestOpenAIConfiguredStreamRetryClassifiesErrorsBeforeDelivery(t *testing.T) {
	for _, streamPath := range []string{"native", "passthrough", "messages", "chat"} {
		for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
			for _, envelope := range []string{"response.failed", "misleading_delta", "error_without_type"} {
				t.Run(fmt.Sprintf("%s/%d/%s", streamPath, status, envelope), func(t *testing.T) {
					account := newConfiguredStreamRetryAccount()
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(status)}
					failure := fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":%s}}`, configuredRetryFailurePayload(status))
					switch envelope {
					case "misleading_delta":
						failure = fmt.Sprintf(`{"type":"response.output_text.delta","delta":"must_not_escape","error":%s}`, configuredRetryFailurePayload(status))
					case "error_without_type":
						failure = fmt.Sprintf(`{"error":%s}`, configuredRetryFailurePayload(status))
					}
					preamble := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_private","status":"in_progress"}}`) +
						configuredRetryOpaquePreamble() + configuredRetryEvent(`{"type":"response.output_text.delta","delta":""}`)
					c, rec := newOpenAIConfiguredRetryContext()
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(preamble + configuredRetryEvent(failure)))}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, streamPath)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.RetryableOnSameAccount)
					require.Equal(t, status, failover.StatusCode)
					require.Empty(t, rec.Body.String(), "classify the error before publishing its apparent output or private metadata")
					require.False(t, c.Writer.Written())
				})
			}
		}
	}
}

func TestOpenAIConfiguredStreamRetryKeepsOrdinaryErrorText(t *testing.T) {
	for _, streamPath := range []string{"native", "passthrough", "messages", "chat"} {
		for _, terminal := range []string{"response.completed", "response.done"} {
			t.Run(streamPath+"/"+terminal, func(t *testing.T) {
				const answer = "The ordinary error handling answer contains error: file not found."
				body := configuredRetryEvent(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"`+answer+`"}`) +
					configuredRetryEvent(fmt.Sprintf(`{"type":%q,"response":{"id":"resp_success","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3}}}`, terminal))
				c, rec := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), streamPath)
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), answer)
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryDoesNotBufferNormalOutput(t *testing.T) {
	for _, streamPath := range []string{"native", "passthrough", "messages", "chat"} {
		for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", streamPath, status), func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(status)}
				tail := strings.Repeat("normal-tail", 2048)
				body := configuredRetryOutput("text") + configuredRetryEvent(fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":%q}`, tail)) +
					configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_success","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3}}}`)
				c, rec := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIStreamRetryMaxBufferBytes: 1024}}}
				err := runConfiguredRetryStream(svc, c, resp, account, streamPath)
				require.NoError(t, err, "normal output must not accumulate against the private preamble buffer limit")
				require.Contains(t, rec.Body.String(), "private answer")
				require.Contains(t, rec.Body.String(), tail)
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryDoesNotReplayLateFailure(t *testing.T) {
	for _, streamPath := range []string{"native", "passthrough", "messages", "chat"} {
		for _, output := range []string{"text", "reasoning", "tool"} {
			for _, status := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
				t.Run(fmt.Sprintf("%s/%s/%d", streamPath, output, status), func(t *testing.T) {
					account := newConfiguredStreamRetryAccount()
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(status)}
					body := configuredRetryOutput(output) + configuredRetryEvent(fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":%s}}`, configuredRetryFailurePayload(status)))
					c, rec := newOpenAIConfiguredRetryContext()
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, streamPath)
					var failover *UpstreamFailoverError
					require.Error(t, err)
					require.NotErrorAs(t, err, &failover, "delivered content cannot be withdrawn and replaced with another attempt")
					require.Contains(t, rec.Body.String(), map[string]string{"text": "private answer", "reasoning": "private reasoning", "tool": "lookup"}[output])
				})
			}
		}
	}
}
