package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamErrorsNeverCountAsModelOutput(t *testing.T) {
	for _, payload := range []string{
		messagesBPSFailure,
		`{"type":"error","message":"failure","delta":"not model output"}`,
		`{"type":"error","error":{"type":"invalid_request_error","message":"blocked"}}`,
		`{"type":"response.error","message":"failure"}`,
		`{"error":{"message":"failed"},"delta":"not model output"}`,
		`{"type":"response.output_text.delta","delta":"not model output","error":{"message":"failed"}}`,
		`{"type":"response.output_text.delta","delta":"not model output","response":{"status":"failed","error":{"message":"failed"}}}`,
		`{"type":"response.completed","response":{"status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"not model output"}]}]}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			eventType := effectiveOpenAISSEEventType([]byte(payload), "")
			require.Contains(t, []string{"error", "response.failed"}, eventType)
			require.False(t, openAIStreamDataStartsClientOutput(payload, eventType))
			require.False(t, openAIStreamDataStartsVisibleOutput(payload, eventType))
			require.False(t, openAIStreamDataStartsSemanticTTFT(payload, eventType))
			require.False(t, openAIStreamDataStartsLegacySemanticTTFT(payload, eventType))
		})
	}
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"error: file not found"}`,
		`{"type":"response.output_text.delta","delta":"normal answer","error":null}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Inspect the error handling."}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"error\":\"data\"}"}`,
	} {
		eventType := effectiveOpenAISSEEventType([]byte(payload), "")
		require.True(t, openAIStreamDataStartsClientOutput(payload, eventType), payload)
		require.True(t, openAIStreamDataStartsVisibleOutput(payload, eventType), payload)
	}
}

func TestOpenAIStreamEmptyOrInvalidDeltasStayPrivate(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":null}`,
		`{"type":"response.output_text.delta","message":"Upstream failed"}`,
		`{"type":"response.output_text.delta","delta":{}}`,
		`{"type":"response.output_text.delta","delta":""}`,
		`{"type":"response.new_kind.delta","delta":""}`,
		`{"type":"response.reasoning_summary_text.delta","delta":null}`,
	} {
		eventType := effectiveOpenAISSEEventType([]byte(payload), "")
		require.False(t, openAIStreamDataStartsClientOutput(payload, eventType), payload)
		require.False(t, openAIStreamDataStartsVisibleOutput(payload, eventType), payload)
	}
}

func TestOpenAIConfiguredStreamRetryBeforeOutputHandlesErrorEnvelopes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	errorBody := `{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"}`
	failures := map[string]string{
		"failed":                   messagesBPSFailure,
		"bare_error":               `{"type":"error","error":` + errorBody + `}`,
		"top_level_error":          `{"type":"error","code":"bps_stream_error","message":"Upstream request failed, please try again later"}`,
		"error_without_type":       `{"error":` + errorBody + `}`,
		"event_header_only":        "event: error\ndata: " + `{"code":"bps_stream_error","message":"Upstream request failed, please try again later"}` + "\n\n",
		"conflicting_error_header": "event: error\ndata: " + `{"type":"response.output_text.delta","delta":"must_not_escape","code":"bps_stream_error","message":"Upstream request failed, please try again later"}` + "\n\n",
		"response_error_alias":     `{"type":"response.error","error":` + errorBody + `}`,
		"misleading_delta_type":    `{"type":"response.output_text.delta","delta":"must_not_escape","error":` + errorBody + `}`,
		"nested_error_with_delta":  `{"type":"response.output_text.delta","delta":"must_not_escape","response":{"status":"failed","error":` + errorBody + `}}`,
	}
	preamble := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_private","status":"in_progress"}}`) +
		configuredRetryEvent(`{"type":"response.output_item.added","item":{"type":"message","content":[]}}`) +
		configuredRetryEvent(`{"type":"response.output_text.delta","delta":""}`) +
		configuredRetryEvent(`{"type":"response.output_text.delta","message":"Upstream failed"}`) +
		configuredRetryOpaquePreamble()
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for name, failure := range failures {
			t.Run(path+"/"+name, func(t *testing.T) {
				c, rec := newOpenAIConfiguredRetryContext()
				frame := configuredRetryEvent(failure)
				if strings.HasPrefix(failure, "event:") {
					frame = failure
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(preamble + frame))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, failover.RetryableOnSameAccount)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Empty(t, rec.Body.String(), "failed attempts must not publish metadata, error text or fake deltas")
				require.False(t, c.Writer.Written())
			})
		}
	}
}

type configuredRetryCancelOnRead struct {
	*strings.Reader
	cancel context.CancelFunc
}

func (r *configuredRetryCancelOnRead) Read(p []byte) (int, error) {
	r.cancel()
	return r.Reader.Read(p)
}

func (*configuredRetryCancelOnRead) Close() error { return nil }

func TestOpenAIConfiguredStreamRetryDoesNotDeliverQueuedOutputAfterCancel(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			body := configuredRetryOutput("text") + configuredRetryEvent(
				`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: &configuredRetryCancelOnRead{
				Reader: strings.NewReader(body), cancel: cancel,
			}}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Empty(t, rec.Body.String())
		})
	}
}

type configuredRetryOutputRecorder struct {
	*httptest.ResponseRecorder
	needle  string
	flushed chan struct{}
}

func (w *configuredRetryOutputRecorder) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), w.needle) {
		select {
		case w.flushed <- struct{}{}:
		default:
		}
	}
}

func TestOpenAIConfiguredStreamRetryFlushesBeforeTerminal(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, kind := range []string{"text", "reasoning", "tool"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				needle := map[string]string{"text": "private answer", "reasoning": "private reasoning", "tool": "lookup"}[kind]
				rec := &configuredRetryOutputRecorder{
					ResponseRecorder: httptest.NewRecorder(), needle: needle, flushed: make(chan struct{}, 1),
				}
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				reader, writer := io.Pipe()
				defer reader.Close()
				deliveredBeforeTerminal := make(chan bool, 1)
				go func() {
					defer writer.Close()
					_, err := io.WriteString(writer, configuredRetryEvent(
						`{"type":"response.created","response":{"id":"resp_live","status":"in_progress"}}`,
					)+configuredRetryOutput(kind))
					if err != nil {
						deliveredBeforeTerminal <- false
						return
					}
					select {
					case <-rec.flushed:
						deliveredBeforeTerminal <- true
					case <-time.After(2 * time.Second):
						deliveredBeforeTerminal <- false
					}
					_, _ = io.WriteString(writer, configuredRetryEvent(
						`{"type":"response.completed","response":{"id":"resp_live","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
					))
				}()
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
				require.NoError(t, err)
				require.True(t, <-deliveredBeforeTerminal, fmt.Sprintf("%s/%s waited for terminal instead of flushing output", path, kind))
				require.Contains(t, rec.Body.String(), needle)
			})
		}
	}
}
