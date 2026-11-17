//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const ccCompletionTool = `{"id":"cc-fixture","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"mcp__fixture__read","arguments":"{\"path\":\"a\"}"}}]}}]}`
const ccCompletionFinish = `{"id":"cc-fixture","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
const ccCompletionFailure = `{"error":{"type":"server_error","code":"upstream_unavailable","message":"fixture upstream explicitly failed"}}`
const ccCompletionPreamble = `{"id":"cc-fixture","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`

func ccCompletionSSE(frames []string, done bool) string {
	result := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"
	if done {
		result += "data: [DONE]\n\n"
	}
	return result
}

func runCCCompletionFixture(t *testing.T, endpoint string, account *Account, reader io.ReadCloser, ctx context.Context) (*httptest.ResponseRecorder, *gin.Context, *httpUpstreamRecorder, *OpenAIForwardResult, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":true,"max_tokens":128,"tools":[{"name":"mcp__fixture__read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"fixture"}]}`)
	if endpoint == "responses" {
		body = []byte(`{"model":"gpt-5.4","stream":true,"input":"fixture"}`)
	}
	if account == nil {
		account = forceChatMessagesFallbackAccount()
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"cc-completion-fixture"}}, Body: reader}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	var result *OpenAIForwardResult
	var err error
	if endpoint == "messages" {
		result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "fixture-cache", "")
	} else {
		result, err = svc.Forward(ctx, c, account, body)
	}
	return recorder, c, upstream, result, err
}

// These are permanent assertions for the four cases first demonstrated by the
// external audit overlay. Both users of scanCCStream must preserve the verdict.
func TestCCFallbackStreamExplicitFailureSkipsSuccessfulFinalize(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		for _, tc := range []struct {
			name   string
			frames []string
			done   bool
			ok     bool
		}{
			{"tool_then_error_EOF", []string{ccCompletionTool, ccCompletionFailure}, false, false},
			{"tool_then_error_DONE", []string{ccCompletionTool, ccCompletionFailure}, true, false},
			{"error_only_EOF", []string{ccCompletionFailure}, false, false},
			{"finished_tool_usage_EOF", []string{ccCompletionTool, ccCompletionFinish, `{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":5}}`}, false, true},
			{"tool_without_finish_EOF", []string{ccCompletionTool}, false, false},
			{"tool_without_finish_DONE", []string{ccCompletionTool}, true, false},
			{"finished_tool_then_error", []string{ccCompletionTool, ccCompletionFinish, ccCompletionFailure}, false, false},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var account *Account
				if len(tc.frames) > 0 && tc.frames[0] == ccCompletionTool {
					account = forceChatMessagesFallbackAccount()
					for key, value := range newConfiguredStreamRetryAccount().Credentials {
						account.Credentials[key] = value
					}
				}
				rec, _, upstream, result, err := runCCCompletionFixture(t, endpoint, account, io.NopCloser(strings.NewReader(ccCompletionSSE(tc.frames, tc.done))), context.Background())
				require.NotNil(t, result)
				require.Len(t, upstream.requests, 1, "the submitted stream must never be replayed internally")
				out := rec.Body.String()
				if tc.ok {
					require.NoError(t, err)
					require.Equal(t, 4, result.Usage.InputTokens)
					require.Equal(t, 5, result.Usage.OutputTokens)
					if endpoint == "messages" {
						require.Contains(t, out, `"stop_reason":"tool_use"`)
						require.Contains(t, out, "event: message_stop")
					} else {
						require.Contains(t, out, "event: response.completed")
					}
					return
				}
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover, "a tool already streamed must not switch accounts")
				require.NotContains(t, out, "event: message_stop")
				require.NotContains(t, out, `"stop_reason":"tool_use"`)
				require.NotContains(t, out, "event: response.completed")
				require.NotContains(t, out, "data: [DONE]")
				if rec.Code == http.StatusOK {
					require.Contains(t, out, "event: ", "committed failures must contain an explicit protocol error")
					require.True(t, strings.Contains(out, "event: error") || strings.Contains(out, "event: response.failed"), out)
				} else {
					require.GreaterOrEqual(t, rec.Code, 400)
				}
			})
		}
	}
}

func TestCCFallbackStreamPreOutputFailuresRemainRetryableAndPrivate(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		for _, failure := range []string{ccCompletionFailure, ""} {
			t.Run(endpoint+"/"+failure, func(t *testing.T) {
				account := forceChatMessagesFallbackAccount()
				for key, value := range newConfiguredStreamRetryAccount().Credentials {
					account.Credentials[key] = value
				}
				frames := []string{ccCompletionPreamble}
				if failure != "" {
					frames = append(frames, failure)
				}
				rec, c, upstream, result, err := runCCCompletionFixture(t, endpoint, account, io.NopCloser(strings.NewReader(ccCompletionSSE(frames, false))), context.Background())
				require.NotNil(t, result)
				require.Nil(t, result.FirstTokenMs, "role and empty content are not first model output")
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, failover.RetryableOnSameAccount)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Empty(t, rec.Body.String())
				require.False(t, c.Writer.Written())
				require.Len(t, upstream.requests, 1)
			})
		}
	}
}

func TestCCFallbackMultilineFailurePreservesPoolRetryAndUsage(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		for _, eventHeader := range []string{"", "event: error\n"} {
			t.Run(endpoint+"/"+eventHeader, func(t *testing.T) {
				account := forceChatMessagesFallbackAccount()
				for key, value := range newConfiguredStreamRetryAccount().Credentials {
					account.Credentials[key] = value
				}
				failure := eventHeader + "data: {\"error\":\ndata: {\"type\":\"server_error\",\"code\":\"bps_stream_error\",\"message\":\"fixture upstream failed\"},\ndata: \"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"
				body := &poolTCPResetBody{Reader: strings.NewReader(ccCompletionSSE([]string{ccCompletionPreamble}, false) + failure), err: poolTCPReadReset()}
				rec, c, _, result, err := runCCCompletionFixture(t, endpoint, account, body, context.Background())
				var retry *UpstreamFailoverError
				require.ErrorAs(t, err, &retry)
				require.True(t, retry.RetryableOnSameAccount)
				require.Nil(t, retry.TransportFailure, "a complete semantic error takes precedence over a later reset")
				require.Equal(t, http.StatusBadGateway, retry.StatusCode)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
				require.Empty(t, rec.Body.String())
				require.False(t, c.Writer.Written())
			})
		}
	}
}

func TestNativeSemanticFailurePreservesClientModelPrivacy(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer.WriteHeaderNow()
	payload := []byte(`{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"reasoning.effort","message":"private-upstream-model does not support reasoning effort minimal"}}`)
	require.Error(t, writeNativeAnthropicSemanticFailure(c, payload, "public-model"))
	require.Contains(t, rec.Body.String(), `"model":"public-model"`)
	require.Contains(t, rec.Body.String(), "unsupported_value")
	require.NotContains(t, rec.Body.String(), "private-upstream-model")
}

func TestCCFallbackStreamDoneWithoutFinishDoesNotSucceed(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			rec, _, _, result, err := runCCCompletionFixture(t, endpoint, nil, io.NopCloser(strings.NewReader("data: [DONE]\n\n")), context.Background())
			require.Error(t, err)
			require.NotNil(t, result)
			require.Nil(t, result.FirstTokenMs)
			require.NotContains(t, rec.Body.String(), "event: message_start")
			require.NotContains(t, rec.Body.String(), "event: message_stop")
			require.NotContains(t, rec.Body.String(), "event: response.completed")
		})
	}
}

func TestCCFallbackStreamErrorEventsAreNotModelChunks(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		for _, failure := range []string{
			`data: {"type":"error","message":"fixture failure"}`,
			`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"fixture failure"}}}`,
			"event: error\ndata: {\"message\":\"fixture failure\"}",
			`data: {"choices":[{"delta":{"content":"not model output"}}],"error":{"type":"invalid_request_error","message":"fixture failure"}}`,
		} {
			t.Run(endpoint+"/"+failure, func(t *testing.T) {
				account := forceChatMessagesFallbackAccount()
				for key, value := range newConfiguredStreamRetryAccount().Credentials {
					account.Credentials[key] = value
				}
				rec, _, _, result, err := runCCCompletionFixture(t, endpoint, account, io.NopCloser(strings.NewReader(failure+"\n\n")), context.Background())
				require.Error(t, err)
				if strings.Contains(failure, "invalid_request_error") {
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover, "pool mode must not turn a client request failure into a retry")
				}
				require.Nil(t, result.FirstTokenMs)
				require.NotContains(t, rec.Body.String(), "not model output")
				require.NotContains(t, rec.Body.String(), "event: message_start")
				require.NotContains(t, rec.Body.String(), "event: response.created")
			})
		}
	}
}

func TestCCFallbackStreamCancellationDoesNotReplayOrFinalize(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &configuredRetryCancelOnRead{Reader: strings.NewReader(ccCompletionSSE([]string{ccCompletionTool, ccCompletionFinish}, true)), cancel: cancel}
			rec, _, _, _, err := runCCCompletionFixture(t, endpoint, nil, reader, ctx)
			require.ErrorIs(t, err, context.Canceled)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Empty(t, rec.Body.String())
		})
	}
}

type ccStreamObservedReader struct {
	io.Reader
	before func()
}

func (r *ccStreamObservedReader) Read(p []byte) (int, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Reader.Read(p)
}

func TestCCFallbackStreamTextFlushesBeforeTerminal(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses"} {
		t.Run(endpoint, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, nil)
			text := `{"id":"cc-fixture","choices":[{"delta":{"content":"ordinary error handling answer"}}]}`
			finish := `{"id":"cc-fixture","choices":[{"delta":{},"finish_reason":"stop"}]}`
			reader := io.MultiReader(strings.NewReader(ccCompletionSSE([]string{ccCompletionPreamble}, false)),
				&ccStreamObservedReader{Reader: strings.NewReader(ccCompletionSSE([]string{text}, false)), before: func() {
					require.Empty(t, recorder.Body.String(), "role-only preamble must remain private")
					require.False(t, c.Writer.Written())
				}},
				&ccStreamObservedReader{Reader: strings.NewReader(ccCompletionSSE([]string{finish}, false)), before: func() {
					require.Contains(t, recorder.Body.String(), "ordinary error handling answer", "first text must flush while upstream is still generating")
					require.NotContains(t, recorder.Body.String(), "event: message_stop")
					require.NotContains(t, recorder.Body.String(), "event: response.completed")
				}})
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(reader)}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
			var err error
			if endpoint == "messages" {
				_, err = svc.streamChatCompletionsAsAnthropic(c, resp, forceChatMessagesFallbackAccount(), "fixture", "fixture", "fixture", nil, nil, time.Now())
			} else {
				_, err = svc.streamChatCompletionsAsResponses(c, resp, forceChatMessagesFallbackAccount(), "fixture", nil, nil, false, nil, "fixture", "fixture", nil, nil, false, time.Now(), nil)
			}
			require.NoError(t, err)
		})
	}
}
