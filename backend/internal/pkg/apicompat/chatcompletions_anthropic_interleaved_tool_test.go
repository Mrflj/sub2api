package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatAnthropicInterleavedToolArgumentsRemainOpen(t *testing.T) {
	for _, finish := range []bool{true, false} {
		name := "finish reason"
		if !finish {
			name = "EOF"
		}
		t.Run(name, func(t *testing.T) {
			state := NewChatCompletionsToAnthropicStreamState("fixture")
			trace := newAnthropicToolTrace()
			send := func(delta ChatDelta, reason *string) []AnthropicStreamEvent {
				events := ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{
					Choices: []ChatChunkChoice{{Delta: delta, FinishReason: reason}},
				}, state)
				trace.record(t, events)
				return events
			}
			text := "first token"
			send(ChatDelta{Content: &text}, nil)
			require.Equal(t, text, trace.text.String())
			indexA, indexB := 0, 1
			send(ChatDelta{ToolCalls: []ChatToolCall{{Index: &indexA, ID: "call_a", Function: ChatFunctionCall{Name: "mcp__fixture__a", Arguments: `{"a":`}}}}, nil)
			require.Equal(t, `{"a":`, trace.args["call_a"].String(), "first call must stream before finish")
			send(ChatDelta{ToolCalls: []ChatToolCall{{Index: &indexB, ID: "call_b", Function: ChatFunctionCall{Name: "mcp__fixture__b", Arguments: `{"b":`}}}}, nil)
			send(ChatDelta{ToolCalls: []ChatToolCall{{Index: &indexB, Function: ChatFunctionCall{Arguments: `2}`}}}}, nil)
			text = " after tools"
			send(ChatDelta{Content: &text}, nil)
			send(ChatDelta{ToolCalls: []ChatToolCall{{Index: &indexA, Function: ChatFunctionCall{Arguments: `1}`}}}}, nil)
			if finish {
				reason := "tool_calls"
				send(ChatDelta{}, &reason)
				require.JSONEq(t, `{"b":2}`, trace.args["call_b"].String(), "siblings must flush at finish_reason before usage/EOF")
			}
			// Usage is commonly delivered after finish_reason, and must still be
			// included in the sole terminal message_delta.
			trace.record(t, ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{Usage: &ChatUsage{PromptTokens: 13, CompletionTokens: 8}}, state))
			trace.record(t, FinalizeChatCompletionsAnthropicStream(state))
			require.JSONEq(t, `{"a":1}`, trace.args["call_a"].String())
			require.JSONEq(t, `{"b":2}`, trace.args["call_b"].String())
			require.Equal(t, "first token after tools", trace.text.String())
			require.Equal(t, 13, state.InputTokens)
			require.Equal(t, 8, state.OutputTokens)
			require.Equal(t, 1, trace.stops)
			require.Empty(t, FinalizeChatCompletionsAnthropicStream(state))
			require.Empty(t, send(ChatDelta{Content: &text}, nil))
		})
	}
}

func TestChatAnthropicQueuedToolRetainsDelayedNameAndID(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("fixture")
	trace := newAnthropicToolTrace()
	a, b := 0, 1
	for _, call := range []ChatToolCall{
		{Index: &a, ID: "call_a", Function: ChatFunctionCall{Name: "a", Arguments: `{}`}},
		{Index: &b, ID: "call_b", Function: ChatFunctionCall{Arguments: `{"b":`}},
		{Index: &b, Function: ChatFunctionCall{Name: "mcp__fixture__b", Arguments: `true}`}},
	} {
		trace.record(t, ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{call}}}},
		}, state))
	}
	events := FinalizeChatCompletionsAnthropicStream(state)
	trace.record(t, events)
	require.JSONEq(t, `{"b":true}`, trace.args["call_b"].String())
	var found bool
	for _, event := range events {
		if event.ContentBlock != nil && event.ContentBlock.ID == "call_b" {
			found = true
			require.Equal(t, "mcp__fixture__b", event.ContentBlock.Name)
		}
	}
	require.True(t, found)
}
