package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

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

func TestOpenAIConfiguredStreamRetryFlushesAnswerBeforeTerminal(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		outputKinds := []string{"text", "reasoning", "tool"}
		if path == "native" || path == "passthrough" {
			outputKinds = append(outputKinds, "web_search")
		}
		for _, kind := range outputKinds {
			t.Run(path+"/"+kind, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					needle := map[string]string{"text": "private answer", "reasoning": "private reasoning", "tool": "lookup", "web_search": "ws_attempt"}[kind]
					output := configuredRetryOutput(kind)
					if kind == "web_search" {
						output = configuredRetryWebSearchOutput()
					}
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
						)+output)
						if err != nil {
							deliveredBeforeTerminal <- false
							return
						}
						synctest.Wait()
						select {
						case <-rec.flushed:
							deliveredBeforeTerminal <- true
						default:
							deliveredBeforeTerminal <- false
						}
						_, _ = io.WriteString(writer, configuredRetryEvent(
							`{"type":"response.completed","response":{"id":"resp_live","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
						))
					}()
					resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
					require.NoError(t, err)
					require.Equal(t, kind == "text", <-deliveredBeforeTerminal, "only answer content commits a configured pool stream before success")
					require.Contains(t, rec.Body.String(), needle)
				})
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryReleasesProgressAtFirstAnswer(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, kind := range []string{"reasoning", "tool", "web_search"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					progress := configuredRetryOutput(kind)
					if kind == "web_search" {
						progress = configuredRetryWebSearchOutput()
					}
					rec := &configuredRetryOutputRecorder{
						ResponseRecorder: httptest.NewRecorder(), needle: "private answer", flushed: make(chan struct{}, 1),
					}
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					reader, writer := io.Pipe()
					defer reader.Close()
					checks := make(chan bool, 2)
					go func() {
						defer writer.Close()
						if _, err := io.WriteString(writer, progress); err != nil {
							return
						}
						synctest.Wait()
						checks <- rec.Body.Len() == 0
						if _, err := io.WriteString(writer, configuredRetryOutput("text")); err != nil {
							return
						}
						synctest.Wait()
						select {
						case <-rec.flushed:
							checks <- true
						default:
							checks <- false
						}
						_, _ = io.WriteString(writer, configuredRetryEvent(messagesBPSFailure))
					}()
					resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
					var failover *UpstreamFailoverError
					require.Error(t, err)
					require.NotErrorAs(t, err, &failover, "published answer must not be replayed")
					require.True(t, <-checks, "progress alone must stay private")
					require.True(t, <-checks, "the first answer must flush before the terminal arrives")
					require.Contains(t, rec.Body.String(), "private answer")
				})
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryToolOnlyTerminalAliases(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, terminal := range []string{"response.completed", "response.done"} {
			t.Run(path+"/"+terminal, func(t *testing.T) {
				body := configuredRetryOutput("tool") + configuredRetryEvent(fmt.Sprintf(`{"type":%q,"response":{"id":"resp_tool_success","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, terminal))
				c, rec := newOpenAIConfiguredRetryContext()
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), "call_private")
				require.Contains(t, rec.Body.String(), "lookup")
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryDefersOnlyProgress(t *testing.T) {
	for _, eventType := range []string{
		"response.reasoning_summary_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_part.done", "response.function_call_arguments.delta",
		"response.custom_tool_call_input.done", "response.web_search_call.completed",
		"response.file_search_call.in_progress", "response.code_interpreter_call_code.delta",
		"response.mcp_call.completed",
	} {
		require.True(t, openAIStreamDefersProgressForRetry(`{}`, eventType), eventType)
	}
	for _, kind := range []string{"reasoning", "compaction", "function_call", "custom_tool_call", "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "mcp_call", "mcp_list_tools", "mcp_approval_request"} {
		for _, eventType := range []string{"response.output_item.added", "response.output_item.done"} {
			require.True(t, openAIStreamDefersProgressForRetry(fmt.Sprintf(`{"item":{"type":%q}}`, kind), eventType), kind)
		}
	}
	for _, eventType := range []string{
		"response.output_text.delta", "response.output_text.done", "response.refusal.delta",
		"response.output_audio.delta", "response.image_generation_call.partial_image",
		"response.completed", "response.done", "response.failed", "error", "future.event",
	} {
		require.False(t, openAIStreamDefersProgressForRetry(`{}`, eventType), eventType)
	}
	require.False(t, openAIStreamDefersProgressForRetry(`{"item":{"type":"message","content":[{"type":"output_text","text":"answer"}]}}`, "response.output_item.done"))
	require.False(t, openAIStreamDefersProgressForRetry(`{"part":{"type":"output_text","text":"answer"}}`, "response.content_part.done"))
	require.True(t, openAIStreamDefersProgressForRetry(`{"part":{"type":"summary_text","text":"reasoning"}}`, "response.content_part.done"))
}
