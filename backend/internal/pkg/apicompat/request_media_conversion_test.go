package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const fixturePDF = "data:application/pdf;base64,JVBERi0="
const fixtureImageURL = "https://example.invalid/image.png?asset=fixture"

func TestRequestMediaMessagesPDFAndURLImagePreserved(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "tool_result"}[nested], func(t *testing.T) {
			content := `[{"type":"text","text":"read fixture"},{"type":"document","title":"fixture.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},{"type":"image","source":{"type":"url","url":"` + fixtureImageURL + `"}}]`
			if nested {
				content = `[{"type":"tool_result","tool_use_id":"call_fixture","is_error":true,"content":` + content + `}]`
			}
			req := &AnthropicRequest{Model: "gpt-4.1", Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(content)}}}
			if nested {
				req.Messages = append([]AnthropicMessage{{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_fixture","name":"read","input":{}}]`)}}, req.Messages...)
			}
			before, _ := json.Marshal(req)
			responses, err := AnthropicToResponses(req)
			require.NoError(t, err)
			require.Contains(t, string(responses.Input), fixturePDF)
			require.Contains(t, string(responses.Input), fixtureImageURL)
			require.Contains(t, string(responses.Input), `"filename":"fixture.pdf"`)
			chat, err := AnthropicToChatCompletionsRequest(req)
			require.NoError(t, err)
			chatJSON, err := json.Marshal(chat)
			require.NoError(t, err)
			require.Contains(t, string(chatJSON), fixturePDF)
			require.Contains(t, string(chatJSON), fixtureImageURL)
			if nested {
				require.Equal(t, "call_fixture", gjson.GetBytes(chatJSON, `messages.#(role=="tool").tool_call_id`).String())
				require.JSONEq(t, `{"is_error":true,"content":"read fixture"}`, gjson.GetBytes(chatJSON, `messages.#(role=="tool").content`).String())
			}
			after, _ := json.Marshal(req)
			require.JSONEq(t, string(before), string(after), "converters must not mutate the caller")
		})
	}
}

func TestRequestMediaPDFURLOnlyUsesSupportedTarget(t *testing.T) {
	req := &AnthropicRequest{Model: "gpt-4.1", Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`[{"type":"document","source":{"type":"url","url":"https://example.invalid/report.pdf"}}]`)}}}
	responses, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.Contains(t, string(responses.Input), `"file_url":"https://example.invalid/report.pdf"`)
	_, err = AnthropicToChatCompletionsRequest(req)
	require.ErrorContains(t, err, "file_url")
	require.True(t, IsUnsupportedRequestConversion(err))
	back, err := ResponsesToAnthropicRequest(responses)
	require.NoError(t, err)
	require.Contains(t, string(back.Messages[0].Content), `"type":"url"`)
	require.Contains(t, string(back.Messages[0].Content), "https://example.invalid/report.pdf")
}

func TestRequestMediaResponsesFilesAndImageDetailToChat(t *testing.T) {
	for _, content := range []string{
		`[{"type":"input_text","text":"inspect"},{"type":"input_file","filename":"inline.pdf","file_data":"` + fixturePDF + `"},{"type":"input_file","file_id":"file_fixture","filename":"uploaded.pdf"},{"type":"input_image","image_url":"` + fixtureImageURL + `","detail":"low"}]`,
		`{"type":"input_file","filename":"inline.pdf","file_data":"` + fixturePDF + `"}`,
	} {
		req := &ResponsesRequest{Model: "gpt-4.1", Input: json.RawMessage(`[{"type":"message","role":"user","content":` + content + `}]`)}
		chat, err := ResponsesToChatCompletionsRequest(req)
		require.NoError(t, err)
		require.Len(t, chat.Messages, 1)
		require.Contains(t, string(chat.Messages[0].Content), fixturePDF)
		if content[0] == '[' {
			require.Contains(t, string(chat.Messages[0].Content), `"file_id":"file_fixture"`)
			require.Contains(t, string(chat.Messages[0].Content), `"detail":"low"`)
		}
	}
	for _, content := range []string{
		`[{"type":"input_file","file_url":"https://example.invalid/report.pdf"}]`,
		`[{"type":"input_image","file_id":"file_image"}]`,
	} {
		_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{Input: json.RawMessage(`[{"role":"user","content":` + content + `}]`)})
		require.Error(t, err)
		require.True(t, IsUnsupportedRequestConversion(err))
	}
}

func TestRequestMediaImageDetailRoundTrip(t *testing.T) {
	chat := &ChatCompletionsRequest{Model: "gpt-4.1", Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"` + fixtureImageURL + `","detail":"low"}}]`)}}}
	responses, err := ChatCompletionsToResponses(chat)
	require.NoError(t, err)
	require.Contains(t, string(responses.Input), `"detail":"low"`)
	back, err := ResponsesToChatCompletionsRequest(responses)
	require.NoError(t, err)
	require.Contains(t, string(back.Messages[0].Content), `"detail":"low"`)
	anthropic, err := ResponsesToAnthropicRequest(responses)
	require.NoError(t, err)
	require.Contains(t, string(anthropic.Messages[0].Content), fixtureImageURL)
	require.Contains(t, string(anthropic.Messages[0].Content), `"source":{"type":"url"`)
}

func TestRequestMediaVideoCannotDisappearInConversions(t *testing.T) {
	for _, role := range []string{"user", "assistant", "tool"} {
		_, err := ChatCompletionsToResponses(&ChatCompletionsRequest{Model: "fixture", Messages: []ChatMessage{{Role: role, Content: json.RawMessage(`[{"type":"text","text":"inspect video"},{"type":"video_url","video_url":{"url":"https://example.invalid/clip.mp4"}}]`)}}})
		require.ErrorContains(t, err, "video_url")
		require.True(t, IsUnsupportedRequestConversion(err))
	}
	for _, typ := range []string{"video_url", "input_video"} {
		req := &ResponsesRequest{Model: "fixture", Input: json.RawMessage(`[{"role":"user","content":[{"type":"` + typ + `","video_url":{"url":"https://example.invalid/clip.mp4"}}]}]`)}
		_, err := ResponsesToChatCompletionsRequest(req)
		require.ErrorContains(t, err, typ)
		_, err = ResponsesToAnthropicRequest(req)
		require.ErrorContains(t, err, typ)
	}
}

func TestRequestMediaToolOutputPDFAndURLImageRetainPairing(t *testing.T) {
	input := `[{"type":"function_call","call_id":"call_pdf","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_pdf","output":[{"type":"input_text","text":"report"},{"type":"input_file","filename":"report.pdf","file_data":"` + fixturePDF + `"},{"type":"input_image","image_url":"` + fixtureImageURL + `","detail":"original"}]}]`
	req := &ResponsesRequest{Model: "gpt-4.1", Input: json.RawMessage(input)}
	chat, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	wire, _ := json.Marshal(chat)
	require.Contains(t, string(wire), fixturePDF)
	require.Contains(t, string(wire), `"detail":"high"`)
	require.Equal(t, "call_pdf", chat.Messages[1].ToolCallID)
	anthropic, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)
	wire, _ = json.Marshal(anthropic)
	require.Contains(t, string(wire), `"tool_use_id":"call_pdf"`)
	require.Contains(t, string(wire), `"type":"document"`)
	require.Contains(t, string(wire), fixtureImageURL)
	var native any
	require.NoError(t, json.Unmarshal(req.Input, &native))
	lifted, changed := LiftResponsesToolOutputMedia(native)
	require.True(t, changed)
	wire, _ = json.Marshal(lifted)
	require.Contains(t, string(wire), fixturePDF, "shared media lifting must not drop newly supported file parts")
	require.Contains(t, string(wire), `"detail":"original"`, "native Responses lifting must preserve its original detail hint")
}

func TestRequestGenerationControlsMapWithoutLosingZeroValues(t *testing.T) {
	var chat ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"fixture"}],"temperature":0,"top_p":0,"max_completion_tokens":256,"logprobs":true,"top_logprobs":0,"verbosity":"low","response_format":{"type":"json_schema","json_schema":{"name":"fixture","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}}}`), &chat))
	responses, err := ChatCompletionsToResponsesWithMinOutputTokens(&chat, 0)
	require.NoError(t, err)
	require.Equal(t, 0.0, *responses.Temperature)
	require.Equal(t, 0, *responses.TopLogprobs)
	require.Equal(t, "low", responses.Text.Verbosity)
	require.Contains(t, responses.Include, "message.output_text.logprobs")
	back, err := ResponsesToChatCompletionsRequest(responses)
	require.NoError(t, err)
	require.True(t, *back.Logprobs)
	require.Equal(t, "low", back.Verbosity)
	require.Equal(t, 0, *back.TopLogprobs)

	for _, model := range []string{"claude-fixture", "claude-sonnet-5-5"} {
		req := &ResponsesRequest{Model: model, Input: json.RawMessage(`"fixture"`), Text: &ResponsesText{Format: json.RawMessage(`{"type":"json_schema","name":"fixture","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}`)}}
		anthropic, err := ResponsesToAnthropicRequest(req)
		require.NoError(t, err)
		require.NotNil(t, anthropic.OutputConfig)
		require.Equal(t, "json_schema", gjson.GetBytes(anthropic.OutputConfig.Format, "type").String())
		responses, err := AnthropicToResponses(anthropic)
		require.NoError(t, err)
		require.Equal(t, "string", gjson.GetBytes(responses.Text.Format, "schema.properties.answer.type").String())
	}
	_, err = ResponsesToAnthropicRequest(responses)
	require.ErrorContains(t, err, "logprobs")
}

func TestRequestGenerationUnsupportedControlsAreExplicit(t *testing.T) {
	for _, field := range []string{`"frequency_penalty":0`, `"presence_penalty":0`, `"seed":0`, `"logit_bias":{"1":0}`, `"n":2`, `"stop":["END"]`} {
		var req ChatCompletionsRequest
		require.NoError(t, json.Unmarshal([]byte(`{"model":"fixture","messages":[],`+field+`}`), &req))
		_, err := ChatCompletionsToResponses(&req)
		require.Error(t, err)
		require.True(t, IsUnsupportedRequestConversion(err))
	}
	var req AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-4.1","max_tokens":256,"messages":[{"role":"user","content":"fixture"}],"stop_sequences":["END"]}`), &req))
	chat, err := AnthropicToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.JSONEq(t, `["END"]`, string(chat.Stop))
	_, err = AnthropicToResponses(&req)
	require.ErrorContains(t, err, "stop_sequences")
}
