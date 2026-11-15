package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type anthropicToolTrace struct {
	open  map[int]bool
	ids   map[int]string
	args  map[string]*strings.Builder
	text  strings.Builder
	stops int
}

func newAnthropicToolTrace() *anthropicToolTrace {
	return &anthropicToolTrace{open: make(map[int]bool), ids: make(map[int]string), args: make(map[string]*strings.Builder)}
}

func (trace *anthropicToolTrace) record(t *testing.T, events []AnthropicStreamEvent) {
	t.Helper()
	for _, event := range events {
		index := -1
		if event.Index != nil {
			index = *event.Index
		}
		switch event.Type {
		case "content_block_start":
			for _, open := range trace.open {
				require.False(t, open, "a new block must not stop an unfinished sibling tool")
			}
			require.False(t, trace.open[index])
			trace.open[index] = true
			if block := event.ContentBlock; block != nil && block.Type == "tool_use" {
				trace.ids[index] = block.ID
				trace.args[block.ID] = &strings.Builder{}
			}
		case "content_block_delta":
			require.True(t, trace.open[index], "delta must target an OPEN block, not just a previously started block")
			if event.Delta.Type == "input_json_delta" {
				trace.args[trace.ids[index]].WriteString(event.Delta.PartialJSON)
			} else if event.Delta.Type == "text_delta" {
				trace.text.WriteString(event.Delta.Text)
			}
		case "content_block_stop":
			require.True(t, trace.open[index], "content block must stop exactly once")
			trace.open[index] = false
		case "message_stop":
			trace.stops++
		}
	}
}

func TestResponsesAnthropicInterleavedToolArgumentsRemainOpen(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	trace := newAnthropicToolTrace()
	send := func(event ResponsesStreamEvent) []AnthropicStreamEvent {
		events := ResponsesEventToAnthropicEvents(&event, state)
		trace.record(t, events)
		return events
	}
	send(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "resp_interleaved", Model: "fixture"}})
	require.NotEmpty(t, send(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 0, Delta: "first token"}))
	require.Equal(t, "first token", trace.text.String(), "normal text must stream before tool completion")
	send(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", CallID: "call_a", Name: "mcp__fixture__a"}})
	require.NotEmpty(t, send(ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: `{"a":`}))
	require.Equal(t, `{"a":`, trace.args["call_a"].String(), "the first tool must stream its arguments immediately")
	require.Empty(t, send(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 2, Item: &ResponsesOutput{Type: "function_call", CallID: "call_b", Name: "mcp__fixture__b"}}))
	send(ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 2, Delta: `{"b":2}`})
	send(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 2, Arguments: `{"b":2}`})
	send(ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: `1}`})
	send(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 1, Arguments: `{"a":1}`})
	require.JSONEq(t, `{"a":1}`, trace.args["call_a"].String())
	require.JSONEq(t, `{"b":2}`, trace.args["call_b"].String())
	// A redundant old done event must not close another tool's block.
	send(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", CallID: "call_a", Name: "mcp__fixture__a", Arguments: `{"a":1}`}})
	send(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 3, Delta: " done"})
	require.Equal(t, "first token done", trace.text.String(), "queued tools must not wait until response.completed")
	send(ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed", Usage: &ResponsesUsage{InputTokens: 11, OutputTokens: 7}}})
	require.Equal(t, 1, trace.stops)
	require.Equal(t, 11, state.InputTokens)
	require.Equal(t, 7, state.OutputTokens)
	require.Empty(t, send(ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: "late"}))
}

func TestResponsesAnthropicInterleavedToolSnapshotAndRead(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done"} {
		t.Run(terminal, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			trace := newAnthropicToolTrace()
			for _, event := range []ResponsesStreamEvent{
				{Type: "response.created", Response: &ResponsesResponse{ID: "snapshot", Model: "fixture"}},
				{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "function_call", CallID: "call_read", Name: "Read"}},
				{Type: "response.function_call_arguments.delta", OutputIndex: 0, Delta: `{"file_path":"a.go",`},
				{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_custom", Name: "apply_patch"}},
				{Type: "response.custom_tool_call_input.delta", OutputIndex: 1, Delta: `{"input":`},
				{Type: terminal, Response: &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{
					{Type: "function_call", CallID: "call_read", Name: "Read", Arguments: `{"file_path":"a.go","pages":""}`},
					{Type: "custom_tool_call", CallID: "call_custom", Name: "apply_patch", Input: `{"input":"fixture"}`},
				}}},
			} {
				trace.record(t, ResponsesEventToAnthropicEvents(&event, state))
			}
			require.JSONEq(t, `{"file_path":"a.go"}`, trace.args["call_read"].String())
			var customInput struct {
				Input string `json:"input"`
			}
			require.NoError(t, json.Unmarshal([]byte(trace.args["call_custom"].String()), &customInput))
			require.Equal(t, `{"input":"fixture"}`, customInput.Input)
			require.Equal(t, 1, trace.stops)
		})
	}
}

func TestResponsesAnthropicInterleavedToolsFlushOnEOF(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	trace := newAnthropicToolTrace()
	for _, event := range []ResponsesStreamEvent{
		{Type: "response.created", Response: &ResponsesResponse{ID: "eof", Model: "fixture"}},
		{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "function_call", CallID: "call_a", Name: "a"}},
		{Type: "response.output_item.added", OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", CallID: "call_b", Name: "b"}},
		{Type: "response.function_call_arguments.delta", OutputIndex: 1, Delta: `{"b":2}`},
		{Type: "response.function_call_arguments.delta", OutputIndex: 0, Delta: `{"a":1}`},
	} {
		trace.record(t, ResponsesEventToAnthropicEvents(&event, state))
	}
	trace.record(t, FinalizeResponsesAnthropicStream(state))
	require.JSONEq(t, `{"a":1}`, trace.args["call_a"].String())
	require.JSONEq(t, `{"b":2}`, trace.args["call_b"].String())
	require.Equal(t, 1, trace.stops)
	require.Empty(t, FinalizeResponsesAnthropicStream(state))
}

func TestResponsesAnthropicToolDoneOnlyAndTerminalOnly(t *testing.T) {
	for _, mode := range []string{"item done", "terminal snapshot", "custom done"} {
		t.Run(mode, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			trace := newAnthropicToolTrace()
			trace.record(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "snapshot"}}, state))
			item := ResponsesOutput{Type: "function_call", CallID: "call_only", Name: "mcp__fixture__lookup", Arguments: `{"query":"fixture"}`}
			if mode == "item done" {
				trace.record(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.done", Item: &item}, state))
			} else if mode == "custom done" {
				item.Type, item.Input = "custom_tool_call", item.Arguments
				trace.record(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.added", Item: &item}, state))
				trace.record(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.done", Input: item.Input}, state))
			}
			trace.record(t, ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{item}}}, state))
			if mode == "custom done" {
				var input struct {
					Input string `json:"input"`
				}
				require.NoError(t, json.Unmarshal([]byte(trace.args["call_only"].String()), &input))
				require.Equal(t, `{"query":"fixture"}`, input.Input)
			} else {
				require.JSONEq(t, `{"query":"fixture"}`, trace.args["call_only"].String())
			}
			require.Equal(t, 1, trace.stops)
		})
	}
}

func TestResponsesAnthropicCustomRawInputRemainsUnchanged(t *testing.T) {
	const raw = "*** Begin Patch\n+printf(\"中文 fixture\")\n*** End Patch"
	for _, mode := range []string{"single", "custom first", "function first"} {
		t.Run(mode, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			trace := newAnthropicToolTrace()
			send := func(event ResponsesStreamEvent) {
				trace.record(t, ResponsesEventToAnthropicEvents(&event, state))
			}
			send(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "raw-custom"}})
			customIndex, functionIndex := 0, 1
			custom := ResponsesOutput{Type: "custom_tool_call", CallID: "call_raw", Name: "apply_patch", Input: raw}
			function := ResponsesOutput{Type: "function_call", CallID: "call_json", Name: "mcp__fixture__lookup", Arguments: `{"query":"fixture"}`}
			if mode == "function first" {
				customIndex, functionIndex = 1, 0
				send(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: functionIndex, Item: &function})
			}
			send(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: customIndex, Item: &custom})
			send(ResponsesStreamEvent{Type: "response.custom_tool_call_input.delta", OutputIndex: customIndex, Delta: raw[:12]})
			if mode == "single" {
				send(ResponsesStreamEvent{Type: "response.custom_tool_call_input.done", OutputIndex: customIndex, Input: raw})
				send(ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed"}})
			} else {
				if mode == "custom first" {
					send(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: functionIndex, Item: &function})
				}
				send(ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: functionIndex, Delta: function.Arguments})
				send(ResponsesStreamEvent{Type: "response.custom_tool_call_input.delta", OutputIndex: customIndex, Delta: raw[12:]})
				output := []ResponsesOutput{custom, function}
				if mode == "function first" {
					output = []ResponsesOutput{function, custom}
				}
				send(ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed", Output: output}})
				require.JSONEq(t, function.Arguments, trace.args[function.CallID].String())
			}
			var input struct {
				Input string `json:"input"`
			}
			require.NoError(t, json.Unmarshal([]byte(trace.args[custom.CallID].String()), &input), "Messages tool input must be a JSON object")
			require.Equal(t, raw, input.Input, "freeform input must survive the function compatibility wrapper without mixing with its sibling")
			require.Equal(t, 1, trace.stops)
		})
	}
}

func TestResponsesAnthropicCustomNonStreamingInputObject(t *testing.T) {
	const raw = "printf(\"中文\")\nnext line"
	response := ResponsesToAnthropic(&ResponsesResponse{ID: "custom", Status: "completed", Output: []ResponsesOutput{
		{Type: "custom_tool_call", CallID: "call_raw", Name: "apply_patch", Input: raw},
		{Type: "function_call", CallID: "call_function", Name: "apply_patch", Arguments: `{"input":"already lowered"}`},
	}}, "fixture")
	require.Len(t, response.Content, 2)
	var input struct {
		Input string `json:"input"`
	}
	require.NoError(t, json.Unmarshal(response.Content[0].Input, &input))
	require.Equal(t, raw, input.Input)
	require.Equal(t, "call_raw", response.Content[0].ID)
	require.JSONEq(t, `{"input":"already lowered"}`, string(response.Content[1].Input), "function-shaped custom fallback must not be wrapped twice")
}

func TestResponsesAnthropicTerminalSnapshotDoesNotDuplicateToolIdentity(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	trace := newAnthropicToolTrace()
	for _, event := range []ResponsesStreamEvent{
		{Type: "response.created", Response: &ResponsesResponse{ID: "compacted-terminal"}},
		{Type: "response.output_item.added", OutputIndex: 3, Item: &ResponsesOutput{Type: "function_call", CallID: "call_once", Name: "lookup"}},
		{Type: "response.function_call_arguments.delta", OutputIndex: 3, Delta: `{"a":`},
		{Type: "response.completed", Response: &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{
			{Type: "function_call", CallID: "call_once", Name: "lookup", Arguments: `{"a":1}`},
		}}},
	} {
		trace.record(t, ResponsesEventToAnthropicEvents(&event, state))
	}
	require.Len(t, trace.args, 1)
	require.JSONEq(t, `{"a":1}`, trace.args["call_once"].String())
	require.Equal(t, 1, trace.stops)
}
