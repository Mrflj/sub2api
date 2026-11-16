//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const topKResponsesRejectionForTest = "top_k has no equivalent in Responses; use a native Messages endpoint to retain this generation control"
const topKChatRejectionForTest = "top_k has no equivalent in Chat Completions; use a native Messages endpoint to retain this generation control"

func TestAnthropicTopKCompatibilityRetry(t *testing.T) {
	request := `{"model":"fixture","top_k":40,"stop_sequences":["END"],"messages":[{"role":"user","content":"hello"}],"metadata":{"top_k":"keep"}}`
	rejection := `{"type":"error","error":{"type":"invalid_request_error","message":"` + topKResponsesRejectionForTest + `"}}`
	for _, tc := range []struct {
		name, request, rejection string
		status, calls            int
		repeat                   bool
	}{
		{name: "responses rejection", request: request, rejection: rejection, status: 400, calls: 2},
		{name: "chat rejection", request: request, rejection: strings.ReplaceAll(rejection, topKResponsesRejectionForTest, topKChatRejectionForTest), status: 400, calls: 2},
		{name: "repeated rejection stops", request: request, rejection: rejection, status: 400, calls: 2, repeat: true},
		{name: "ordinary bad request", request: request, rejection: `{"error":{"message":"invalid tool input"}}`, status: 400, calls: 1},
		{name: "partial message does not match", request: request, rejection: `{"error":{"message":"unsupported top_k value"}}`, status: 400, calls: 1},
		{name: "unauthorized", request: request, rejection: rejection, status: 401, calls: 1},
		{name: "no top k", request: `{"model":"fixture","stop_sequences":["END"]}`, rejection: rejection, status: 400, calls: 1},
		{name: "malformed response", request: request, rejection: strings.TrimSuffix(rejection, "}"), status: 400, calls: 1},
		{name: "large response", request: request, rejection: rejection + strings.Repeat(" ", 100000), status: 400, calls: 1},
		{name: "native success", request: request, rejection: `{"type":"message","content":[]}`, status: 200, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			resp, stops, err := doAnthropicStopSequenceCompatibleRequest(context.Background(), nil, newAnthropicAPIKeyAccountForTest(), []byte(tc.request), func(body []byte) (*http.Response, error) {
				calls++
				if calls == 1 {
					require.JSONEq(t, tc.request, string(body))
					return stopCompatHTTPResponse(tc.status, tc.rejection), nil
				}
				require.Equal(t, 2, calls)
				require.False(t, gjson.GetBytes(body, "top_k").Exists())
				require.Equal(t, "END", gjson.GetBytes(body, "stop_sequences.0").String())
				require.Equal(t, "hello", gjson.GetBytes(body, "messages.0.content").String())
				require.Equal(t, "keep", gjson.GetBytes(body, "metadata.top_k").String())
				if tc.repeat {
					return stopCompatHTTPResponse(400, tc.rejection), nil
				}
				return stopCompatHTTPResponse(200, `{"type":"message","content":[]}`), nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, tc.calls, calls)
			require.Empty(t, stops)
			payload, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tc.calls == 1 || tc.repeat {
				require.Equal(t, tc.rejection, string(payload))
			} else {
				require.Equal(t, 200, resp.StatusCode)
			}
		})
	}
}

func TestAnthropicTopKAndStopSequenceCompatibilityOrder(t *testing.T) {
	for _, topKFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(topKFirst), func(t *testing.T) {
			calls := 0
			resp, stops, err := doAnthropicStopSequenceCompatibleRequest(context.Background(), nil, newAnthropicAPIKeyAccountForTest(), []byte(`{"top_k":40,"stop_sequences":["END"],"messages":[{"role":"user","content":"hello"}]}`), func(body []byte) (*http.Response, error) {
				calls++
				if calls == 3 {
					require.False(t, gjson.GetBytes(body, "top_k").Exists())
					require.False(t, gjson.GetBytes(body, "stop_sequences").Exists())
					return stopCompatHTTPResponse(200, "done"), nil
				}
				require.LessOrEqual(t, calls, 2)
				message := anthropicStopSequencesUnsupported
				if (calls == 1) == topKFirst {
					message = topKResponsesRejectionForTest
					require.True(t, gjson.GetBytes(body, "top_k").Exists())
				} else {
					require.True(t, gjson.GetBytes(body, "stop_sequences").Exists())
				}
				return stopCompatHTTPResponse(400, `{"error":{"message":"`+message+`"}}`), nil
			})
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 3, calls)
			require.Equal(t, 200, resp.StatusCode)
			require.Equal(t, []string{"END"}, stops)
		})
	}
}

func TestAnthropicTopKCompatibilityBoundsDuplicateKeys(t *testing.T) {
	calls := 0
	rejection := `{"error":{"message":"` + topKResponsesRejectionForTest + `"}}`
	resp, stops, err := doAnthropicStopSequenceCompatibleRequest(context.Background(), nil, newAnthropicAPIKeyAccountForTest(), []byte(`{"top_k":10,"top_k":20,"top_k":30,"top_k":40}`), func([]byte) (*http.Response, error) {
		calls++
		require.LessOrEqual(t, calls, 3)
		return stopCompatHTTPResponse(400, rejection), nil
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.LessOrEqual(t, calls, 3)
	require.Empty(t, stops)
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, rejection, string(payload))
}

func TestGatewayAnthropicTopKCompatibility(t *testing.T) {
	for _, openAINative := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"native success", "top k", "both top k first", "both stop first"} {
				t.Run(fmt.Sprintf("openai=%t/stream=%t/%s", openAINative, stream, mode), func(t *testing.T) {
					model := "claude-sonnet-4-20250514"
					if openAINative {
						model = "k3"
					}
					body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"max_tokens":256,"stream":%t,"top_k":40,"stop_sequences":["END"],"temperature":0.3}`, model, stream))
					upstream := &anthropicStopHTTPUpstream{}
					var rejections []string
					switch mode {
					case "top k":
						rejections = []string{topKResponsesRejectionForTest}
					case "both top k first":
						rejections = []string{topKResponsesRejectionForTest, anthropicStopSequencesUnsupported}
					case "both stop first":
						rejections = []string{anthropicStopSequencesUnsupported, topKResponsesRejectionForTest}
					}
					for _, message := range rejections {
						upstream.responses = append(upstream.responses, stopCompatHTTPResponse(400, `{"type":"error","error":{"type":"invalid_request_error","message":"`+message+`"}}`))
					}
					response := fmt.Sprintf(`{"type":"message","model":%q,"content":[{"type":"text","text":"Hello END hidden"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":9}}`, model)
					if stream {
						response = strings.ReplaceAll(anthropicStopFixtureSSE("Hello E", "ND hidden"), "claude-sonnet-4-20250514", model)
					}
					upstream.responses = append(upstream.responses, stopCompatHTTPResponse(200, response))
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
					var account *Account
					var err error
					var outputTokens int
					if openAINative {
						account = nativeAnthropicTestAccount()
						svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
						result, forwardErr := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
						err = forwardErr
						if result != nil {
							outputTokens = result.Usage.OutputTokens
						}
					} else {
						account = newAnthropicAPIKeyAccountForTest()
						svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}}
						result, forwardErr := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, account, body, model, model, stream, time.Now())
						err = forwardErr
						if result != nil {
							outputTokens = result.Usage.OutputTokens
						}
					}
					require.NoError(t, err)
					require.Equal(t, 200, rec.Code)
					require.Len(t, upstream.bodies, len(rejections)+1)
					for _, accountID := range upstream.accounts {
						require.Equal(t, account.ID, accountID)
					}
					require.Equal(t, int64(40), gjson.GetBytes(upstream.bodies[0], "top_k").Int())
					last := upstream.bodies[len(upstream.bodies)-1]
					require.Equal(t, "hello", gjson.GetBytes(last, "messages.0.content").String())
					require.Equal(t, 0.3, gjson.GetBytes(last, "temperature").Float())
					require.Equal(t, 9, outputTokens)
					if len(rejections) > 0 {
						require.False(t, gjson.GetBytes(last, "top_k").Exists())
					}
					if len(rejections) == 2 {
						require.False(t, gjson.GetBytes(last, "stop_sequences").Exists())
						require.NotContains(t, rec.Body.String(), "hidden")
						require.Contains(t, rec.Body.String(), "stop_sequence")
					} else {
						require.Equal(t, "END", gjson.GetBytes(last, "stop_sequences.0").String())
						require.Contains(t, rec.Body.String(), "hidden")
					}
				})
			}
		}
	}
}

func TestAnthropicTopKDoesNotChangeTokenCounting(t *testing.T) {
	plain := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`)
	withTopK := []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}],"top_k":40}`)
	account := rawChatCompletionsTestAccount()
	baseline, err := prepareOpenAIInputTokensCountRequest(plain, account, "")
	require.NoError(t, err)
	prepared, err := prepareOpenAIInputTokensCountRequest(withTopK, account, "")
	require.NoError(t, err)
	require.Equal(t, baseline, prepared)
	baselineCount, err := estimateAnthropicCountTokensLocally(plain)
	require.NoError(t, err)
	count, err := estimateAnthropicCountTokensLocally(withTopK)
	require.NoError(t, err)
	require.Equal(t, baselineCount, count)
}

func TestGatewayAnthropicTopKCompatibilityPersistsAcrossStatusRetry(t *testing.T) {
	for _, parsedRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(parsedRequest), func(t *testing.T) {
			body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"top_k":40}`)
			upstream := &anthropicStopHTTPUpstream{responses: []*http.Response{
				stopCompatHTTPResponse(400, `{"error":{"message":"`+topKResponsesRejectionForTest+`"}}`),
				stopCompatHTTPResponse(502, `{"error":{"message":"temporary upstream failure"}}`),
				stopCompatHTTPResponse(200, `{"type":"message","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":9}}`),
			}}
			svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			account := newAnthropicAPIKeyAccountForTest()
			account.Credentials["custom_error_codes_enabled"] = true
			account.Credentials["custom_error_codes"] = []any{float64(400)}
			input := anthropicPassthroughForwardInput{
				Body: body, RequestModel: "claude-sonnet-4-20250514", OriginalModel: "claude-sonnet-4-20250514", StartTime: time.Now(),
			}
			if parsedRequest {
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, err)
				input.Parsed = parsed
			}
			result, err := svc.forwardAnthropicAPIKeyPassthroughWithInput(context.Background(), c, account, input)
			require.NoError(t, err)
			require.Equal(t, 200, rec.Code)
			require.Len(t, upstream.bodies, 3)
			require.Equal(t, []int64{account.ID, account.ID, account.ID}, upstream.accounts)
			require.Equal(t, int64(40), gjson.GetBytes(upstream.bodies[0], "top_k").Int())
			for _, sent := range upstream.bodies[1:] {
				require.False(t, gjson.GetBytes(sent, "top_k").Exists())
			}
			if input.Parsed != nil {
				require.False(t, gjson.GetBytes(input.Parsed.Body.Bytes(), "top_k").Exists())
			}
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.True(t, gjson.GetBytes(body, "top_k").Exists())
		})
	}
}
