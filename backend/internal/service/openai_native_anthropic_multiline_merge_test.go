//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestNativeAnthropicMultilineModelPrivacyAndUsage(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(newline, "\r", "CR"), func(t *testing.T) {
			wire := "id: msg_1\nretry: 1000\nevent: message_start\n" +
				"data: {\ndata: \"type\":\"message_start\",\ndata: \"message\":{\n" +
				"data: \"id\":\"msg_1\",\"model\":\"private-model\",\n" +
				"data: \"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":11}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"keep private-model\"}}\n\n" +
				"event: message_delta\ndata: {\ndata: \"type\":\"message_delta\",\n" +
				"data: \"usage\":{\"output_tokens\":3}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			wire = strings.ReplaceAll(wire, "\n", newline)
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "public-model"))
			resp := newOpenAIUpstreamErrorResponse(http.StatusOK, wire)
			resp.Header.Set("Content-Type", "text/event-stream")
			svc := &OpenAIGatewayService{}
			result, err := svc.handleNativeAnthropicStreamingResponse(c.Request.Context(), resp, c,
				&Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
				"channel-model", "billing-model", "mapped-model", nil, time.Now())
			require.NoError(t, err)
			require.NotNil(t, result)
			out := rec.Body.String()
			require.Contains(t, out, `"model":"public-model"`)
			require.NotContains(t, out, `"model":"private-model"`)
			require.Contains(t, out, "keep private-model", "assistant text must remain untouched")
			require.Contains(t, out, "id: msg_1\nretry: 1000\nevent: message_start\n")
			require.Equal(t, "private-model", result.UpstreamResponseModel, "operations must retain the actual upstream model")
			require.Equal(t, 18, result.Usage.InputTokens)
			require.Equal(t, 11, result.Usage.CacheReadInputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
		})
	}
}

func TestReplaceModelInSSEBodyMultilinePreservesFraming(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		for _, payload := range []string{
			"{\n\"type\":\"message_start\",\n\"message\":{\"model\":\"private\"},\n\"text\":\"private\",\"tool\":{\"model\":\"private\"}}",
			"{\n\"type\":\"response.completed\",\n\"response\":{\"model\":\"private\"},\n\"text\":\"private\",\"tool\":{\"model\":\"private\"}}",
			"{\n\"model\":\"private\",\n\"choices\":[],\"tool\":{\"model\":\"private\"}}",
		} {
			wire := "id: event_1\n: comment\nevent: fixture\nretry: 20\ndata:\t" + strings.ReplaceAll(payload, "\n", "\ndata: ") + "\n\n"
			want := strings.Replace(wire, `"model":"private"`, `"model":"public"`, 1)
			wire, want = strings.ReplaceAll(wire, "\n", newline), strings.ReplaceAll(want, "\n", newline)
			require.Equal(t, want, (&OpenAIGatewayService{}).replaceModelInSSEBody(wire, "mapped", "public"))
		}
	}
	for _, wire := range []string{
		"event: ping\n: heartbeat\n\n",
		"data: {\ndata: \"model\":\"private\",\n\n",
		"data: {\"model\":\"private\"}\ndata: {\"model\":\"private\"}\n\n",
		"data: {\"text\":\"private\",\"tool\":{\"model\":\"private\"}}\n\n",
	} {
		// Independently complete JSON data lines retain the legacy line-wise behavior.
		want := wire
		if strings.Count(wire, `data: {"model":"private"}`) == 2 {
			want = strings.ReplaceAll(wire, `"model":"private"`, `"model":"public"`)
		}
		require.Equal(t, want, (&OpenAIGatewayService{}).replaceModelInSSEBody(wire, "mapped", "public"))
	}
}
