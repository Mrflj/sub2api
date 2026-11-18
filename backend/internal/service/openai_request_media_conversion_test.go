//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRequestMediaConversionUnsupportedReturns400BeforeUpstream(t *testing.T) {
	video := `[{"role":"user","content":[{"type":"text","text":"inspect fixture"},{"type":"video_url","video_url":{"url":"https://example.invalid/clip.mp4"}}]}]`
	for _, tc := range []struct{ name, endpoint, upstream, body, diagnostic string }{
		{"chat_video_responses", "chat/completions", APIProtocolResponses, `{"model":"gpt-4.1","messages":` + video + `}`, "video_url"},
		{"chat_video_messages", "chat/completions", APIProtocolAnthropic, `{"model":"gpt-4.1","messages":` + video + `}`, "video_url"},
		{"messages_document_url_chat", "messages", APIProtocolChatCompletions, `{"model":"gpt-4.1","max_tokens":128,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.invalid/report.pdf"}}]}]}`, "file_url"},
		{"messages_document_file_id_responses", "messages", APIProtocolResponses, `{"model":"gpt-4.1","max_tokens":128,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"anthropic_fixture"}}]}]}`, "document source"},
		{"responses_file_url_chat", "responses", APIProtocolChatCompletions, `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_file","file_url":"https://example.invalid/report.pdf"}]}]}`, "file_url"},
		{"responses_file_id_messages", "responses", APIProtocolAnthropic, `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_file","file_id":"openai_fixture"}]}]}`, "file_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			account := rawChatCompletionsTestAccount()
			account.Credentials["api_protocol"] = tc.upstream
			if tc.upstream == APIProtocolAnthropic {
				account.Platform = PlatformKimi // provider exposing a native Messages endpoint
			}
			mode := openai_compat.ResponsesSupportModeForceResponses
			if tc.upstream == APIProtocolChatCompletions {
				mode = openai_compat.ResponsesSupportModeForceChatCompletions
			}
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(mode)}
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := []byte(tc.body)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+tc.endpoint, bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			var result *OpenAIForwardResult
			var err error
			switch tc.endpoint {
			case "messages":
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			case "chat/completions":
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			case "responses":
				result, err = svc.Forward(context.Background(), c, account, body)
			}
			require.Nil(t, result)
			require.ErrorContains(t, err, tc.diagnostic)
			require.True(t, apicompat.IsUnsupportedRequestConversion(err))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), tc.diagnostic)
			require.Empty(t, upstream.requests, "unsupported media must fail without contacting a model or changing protocol/model")
		})
	}
}

func TestRequestMediaNativeChatPassthroughPreservesVideoAndControls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"fixture-video","messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"https://example.invalid/clip.mp4","fps":2}}]}],"seed":0,"frequency_penalty":0,"presence_penalty":0.25,"logit_bias":{"1":-100},"n":2,"stop":["END"],"provider_fixture_control":{"preserve":true}}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","model":"fixture-video","choices":[{"index":0,"message":{"role":"assistant","content":"fixture accepted"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := rawChatCompletionsTestAccount()
	account.Credentials["api_protocol"] = APIProtocolChatCompletions
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://example.invalid/clip.mp4", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.video_url.url").String())
	require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "messages.0.content.0.video_url.fps").Int())
	require.True(t, gjson.GetBytes(upstream.lastBody, "seed").Exists())
	require.True(t, gjson.GetBytes(upstream.lastBody, "frequency_penalty").Exists())
	require.Equal(t, 0.0, gjson.GetBytes(upstream.lastBody, "frequency_penalty").Float())
	require.Equal(t, 0.25, gjson.GetBytes(upstream.lastBody, "presence_penalty").Float())
	require.JSONEq(t, `{"1":-100}`, gjson.GetBytes(upstream.lastBody, "logit_bias").Raw)
	require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "n").Int())
	require.Equal(t, "END", gjson.GetBytes(upstream.lastBody, "stop.0").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "provider_fixture_control.preserve").Bool())
}

func TestRequestMediaMessagesPDFActuallyReachesResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-4.1","max_tokens":128,"messages":[{"role":"user","content":[{"type":"document","title":"fixture.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},{"type":"image","source":{"type":"url","url":"https://example.invalid/fixture.png"}}]}]}`)
	frames := `data: {"type":"response.completed","response":{"id":"resp_fixture","status":"completed","model":"gpt-4.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture read"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\ndata: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, string(upstream.lastBody), `"file_data":"data:application/pdf;base64,JVBERi0="`)
	require.Contains(t, string(upstream.lastBody), `"image_url":"https://example.invalid/fixture.png"`)
}
