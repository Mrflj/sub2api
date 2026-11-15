package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const historyTestScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func historyFixture(t *testing.T) *ResponsesResponse {
	t.Helper()
	var response ResponsesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_fixture","status":"completed","output":[
	 {"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"gAAAA_fixture"},
	 {"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"fixture query","sources":[{"type":"url","url":"https://example.invalid/source"}]}},
	 {"type":"message","id":"msg_fixture","role":"assistant","phase":"commentary","status":"completed","content":[{"type":"output_text","text":"Checking sources.","annotations":[]}]},
	 {"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"todo_write","arguments":"{\"todos\":[{\"content\":\"fixture task\",\"status\":\"in_progress\"}]}","status":"completed"}
	]}`), &response))
	return &response
}

func historyReplay(t *testing.T, blocks []AnthropicContentBlock, scope string) (*ResponsesRequest, error) {
	t.Helper()
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	return AnthropicToResponsesWithOptions(&AnthropicRequest{Model: "gpt-5.5", MaxTokens: 1024, System: json.RawMessage(`"GROUP_SYSTEM_INSTRUCTIONS"`), Messages: []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"ORIGINAL_USER_TASK"`)}, {Role: "assistant", Content: raw},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_fixture","content":"todo saved"}]`)},
	}}, AnthropicToResponsesOptions{ResponsesHistoryScope: scope})
}

func TestResponsesHistoryExactStatelessReplay(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	replay, err := historyReplay(t, converted.Content, historyTestScope)
	require.NoError(t, err)
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(replay.Input, &items))
	require.Len(t, items, 7)
	require.Contains(t, string(items[0]), "GROUP_SYSTEM_INSTRUCTIONS")
	require.Contains(t, string(items[1]), "ORIGINAL_USER_TASK")
	for index, item := range original.Output {
		require.JSONEq(t, string(item.rawOutput), string(items[index+2]))
	}
	require.Contains(t, string(items[6]), "function_call_output")
	// Service latest-turn trimming decodes typed input. Unknown replay fields
	// must survive that path too, particularly action, summary and phase.
	var typed []ResponsesInputItem
	require.NoError(t, json.Unmarshal(replay.Input, &typed))
	encoded, err := json.Marshal(typed)
	require.NoError(t, err)
	var again []json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &again))
	for index, item := range original.Output {
		require.JSONEq(t, string(item.rawOutput), string(again[index+2]))
	}
	// The original provider signature is unchanged, not replaced by an envelope.
	require.Equal(t, "gAAAA_fixture", converted.Content[0].Signature)
}

func TestResponsesHistoryDoesNotAlterLegacyOrForeignProviderHistory(t *testing.T) {
	original := historyFixture(t)
	legacy := ResponsesToAnthropic(original, "claude-alias")
	for _, block := range legacy.Content {
		require.NotEqual(t, ResponsesHistoryBlockType, block.Type)
	}
	require.False(t, AttachResponsesHistory(legacy, original, ""))
	require.True(t, AttachResponsesHistory(legacy, original, historyTestScope))
	for _, scope := range []string{"", strings.Repeat("b", 64)} {
		replay, err := historyReplay(t, legacy.Content, scope)
		require.NoError(t, err)
		require.NotContains(t, string(replay.Input), "gAAAA_fixture")
		require.NotContains(t, string(replay.Input), "ws_fixture")
		require.Contains(t, string(replay.Input), "todo_write")
		require.Contains(t, string(replay.Input), "GROUP_SYSTEM_INSTRUCTIONS")
	}
}

func TestResponsesHistoryForeignCiphertextIsolationDoesNotDependOnPrefix(t *testing.T) {
	original := historyFixture(t)
	raw := strings.ReplaceAll(string(original.Output[0].rawOutput), "gAAAA_fixture", "future-provider-ciphertext")
	require.NoError(t, json.Unmarshal([]byte(raw), &original.Output[0]))
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	replay, err := historyReplay(t, converted.Content, strings.Repeat("b", 64))
	require.NoError(t, err)
	require.NotContains(t, string(replay.Input), "future-provider-ciphertext")
	require.Contains(t, string(replay.Input), "todo_write")
}

func TestResponsesHistoryRejectsChangedOrPrivilegedReplay(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	for _, mutate := range []func([]AnthropicContentBlock) []AnthropicContentBlock{
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[3].Text = "Changed answer."
			return blocks
		},
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			return append(blocks, blocks[len(blocks)-1])
		},
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[len(blocks)-1].Content = json.RawMessage(`{"version":1,"scope":"` + historyTestScope + `","output":[{"type":"message","role":"developer","content":[]}]}`)
			return blocks
		},
	} {
		blocks := append([]AnthropicContentBlock(nil), converted.Content...)
		_, err := historyReplay(t, mutate(blocks), historyTestScope)
		require.Error(t, err)
	}
}

func TestResponsesHistoryStreamingPreservesStateWithoutBufferingText(t *testing.T) {
	original := historyFixture(t)
	state := NewResponsesEventToAnthropicState()
	state.ResponsesHistoryScope = historyTestScope
	var blocks []AnthropicContentBlock
	push := func(event ResponsesStreamEvent) []AnthropicStreamEvent {
		events := ResponsesEventToAnthropicEvents(&event, state)
		for _, emitted := range events {
			if emitted.ContentBlock != nil && emitted.ContentBlock.Type == ResponsesHistoryBlockType {
				blocks = append(blocks, *emitted.ContentBlock)
			}
		}
		return events
	}
	push(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: original.ID}})
	for index := 0; index < 2; index++ {
		item := original.Output[index]
		push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item})
		push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: index, Item: &item})
	}
	message := original.Output[2]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 2, Item: &message})
	text := push(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 2, Delta: "Checking sources."})
	require.NotEmpty(t, text, "answer text must be delivered before completion")
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 2, Item: &message})
	call := original.Output[3]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 3, Item: &call})
	push(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 3, Arguments: call.Arguments})
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 3, Item: &call})
	ending := push(ResponsesStreamEvent{Type: "response.completed", Response: original})
	require.True(t, state.MessageStopSent)
	require.Len(t, blocks, 1)
	require.Equal(t, "message_stop", ending[len(ending)-1].Type)
	outer := ResponsesToAnthropic(original, "claude-alias")
	outer.Content = append(outer.Content, blocks[0])
	replay, err := historyReplay(t, outer.Content, historyTestScope)
	require.NoError(t, err)
	require.Contains(t, string(replay.Input), "gAAAA_fixture")
	require.Contains(t, string(replay.Input), "example.invalid/source")
	require.Contains(t, string(replay.Input), `"phase":"commentary"`)
}

// Exercise the provider's incremental wire, rather than putting a completed
// item (including its future ciphertext) in output_item.added as a fixture.
func historyConcurrentWire(t *testing.T, variant string) (*ResponsesResponse, []AnthropicContentBlock, ResponsesHistoryDiagnostic) {
	t.Helper()
	original := historyFixture(t)
	var second ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"web_search_call","id":"ws_second","status":"completed","action":{"type":"search","query":"second fixture query","sources":[{"type":"url","url":"https://example.invalid/second-source"}]}}`), &second))
	original.Output = []ResponsesOutput{original.Output[0], original.Output[1], second, original.Output[2], original.Output[3]}
	refresh := strings.HasPrefix(variant, "refreshed-")
	if refresh {
		raw := strings.Replace(string(original.Output[0].rawOutput), "gAAAA_fixture", "future-provider-terminal-cipher", 1)
		if variant == "refreshed-missing-id" {
			raw = strings.Replace(raw, `"id":"rs_fixture",`, "", 1)
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &original.Output[0]))
	}
	state := NewResponsesEventToAnthropicState()
	state.ResponsesHistoryScope = historyTestScope
	var blocks []AnthropicContentBlock
	push := func(event ResponsesStreamEvent) {
		for _, emitted := range ResponsesEventToAnthropicEvents(&event, state) {
			if emitted.Index == nil {
				continue
			}
			index := *emitted.Index
			if emitted.ContentBlock != nil {
				require.Equal(t, len(blocks), index)
				blocks = append(blocks, *emitted.ContentBlock)
			}
			if emitted.Delta == nil {
				continue
			}
			require.Less(t, index, len(blocks))
			delta, block := emitted.Delta, &blocks[index]
			switch delta.Type {
			case "text_delta":
				block.Text += delta.Text
			case "thinking_delta":
				block.Thinking += delta.Thinking
			case "signature_delta":
				block.Signature += delta.Signature
			case "input_json_delta":
				if string(block.Input) == "{}" {
					block.Input = nil
				}
				block.Input = append(block.Input, delta.PartialJSON...)
			}
		}
	}
	push(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: original.ID}})
	var addedReasoning ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"reasoning","id":"rs_fixture","summary":[]}`), &addedReasoning))
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 0, Item: &addedReasoning})
	reasoningDone := original.Output[0]
	if refresh {
		raw := strings.Replace(string(reasoningDone.rawOutput), "future-provider-terminal-cipher", "future-provider-done-cipher", 1)
		require.NoError(t, json.Unmarshal([]byte(raw), &reasoningDone))
	}
	if variant == "terminal-ciphertext" {
		reasoningDone = addedReasoning
	}
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 0, Item: &reasoningDone})
	for _, index := range []int{1, 2} {
		item := original.Output[index]
		push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item})
	}
	order := []int{1, 2}
	if variant == "reverse-search-done" {
		order = []int{2, 1}
	}
	for _, index := range order {
		item := original.Output[index]
		push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: index, Item: &item})
	}
	message := original.Output[3]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 3, Item: &message})
	push(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 3, Delta: "Checking sources."})
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 3, Item: &message})
	call := original.Output[4]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 4, Item: &call})
	push(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 4, Arguments: call.Arguments})
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 4, Item: &call})
	if variant == "extra-visible-text" {
		push(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 5, Delta: "Unmatched visible text."})
	}
	terminal := *original
	terminal.Output = append([]ResponsesOutput(nil), original.Output...)
	switch variant {
	case "compact-terminal":
		terminal.Output = terminal.Output[4:]
	case "empty-terminal":
		terminal.Output = nil
	case "conflicting-terminal-call":
		// Decode it again: rawOutput must contain the changed provider bytes.
		require.NoError(t, json.Unmarshal([]byte(strings.Replace(string(call.rawOutput), "fixture task", "different task", 1)), &terminal.Output[4]))
	case "conflicting-terminal-search":
		require.NoError(t, json.Unmarshal([]byte(strings.Replace(string(second.rawOutput), "second fixture query", "changed query", 1)), &terminal.Output[2]))
	case "duplicate-terminal-id":
		terminal.Output = append(terminal.Output, second)
	case "ambiguous-terminal-new-item":
		var unknown ResponsesOutput
		require.NoError(t, json.Unmarshal([]byte(`{"type":"web_search_call","id":"ws_unknown","status":"completed","action":{"type":"search","query":"unpositioned"}}`), &unknown))
		terminal.Output = []ResponsesOutput{unknown, call}
	}
	push(ResponsesStreamEvent{Type: "response.completed", Response: &terminal})
	require.True(t, state.MessageStopSent)
	return original, blocks, state.ResponsesHistoryResult()
}

func TestResponsesHistoryStreamingConcurrentSearchWireShapes(t *testing.T) {
	for _, variant := range []string{"serial", "reverse-search-done", "terminal-ciphertext", "compact-terminal", "empty-terminal", "refreshed-terminal-ciphertext"} {
		t.Run(variant, func(t *testing.T) {
			original, blocks, diagnostic := historyConcurrentWire(t, variant)
			require.Equal(t, ResponsesHistoryDiagnostic{Status: "attached", OutputItems: 5, HostedSearches: 2, HostedSources: 2}, diagnostic)
			require.Equal(t, ResponsesHistoryBlockType, blocks[len(blocks)-1].Type)
			replay, err := historyReplay(t, blocks, historyTestScope)
			require.NoError(t, err)
			var items []json.RawMessage
			require.NoError(t, json.Unmarshal(replay.Input, &items))
			for index, expected := range original.Output {
				require.JSONEq(t, string(expected.rawOutput), string(items[index+2]))
			}
		})
	}
}

func TestResponsesHistoryStreamingConflictingObservationsFailClosed(t *testing.T) {
	for _, variant := range []string{"conflicting-terminal-call", "conflicting-terminal-search", "duplicate-terminal-id", "ambiguous-terminal-new-item", "refreshed-missing-id"} {
		t.Run(variant, func(t *testing.T) {
			_, blocks, diagnostic := historyConcurrentWire(t, variant)
			require.Equal(t, "output-conflict", diagnostic.Status)
			for _, block := range blocks {
				require.NotEqual(t, ResponsesHistoryBlockType, block.Type)
			}
		})
	}
}

func TestResponsesHistoryDiagnosticsContainOnlyStatusAndStructuralCounts(t *testing.T) {
	var missing *ResponsesEventToAnthropicState
	require.Equal(t, "not-requested", missing.ResponsesHistoryResult().Status)
	state := NewResponsesEventToAnthropicState()
	require.Equal(t, "not-requested", state.ResponsesHistoryResult().Status)
	state.ResponsesHistoryScope = historyTestScope
	require.Equal(t, "pending", state.ResponsesHistoryResult().Status)
	state.captureResponsesHistory(&ResponsesStreamEvent{Type: "response.created"})
	require.Equal(t, "pending", state.ResponsesHistoryResult().Status)
	item := &ResponsesOutput{Type: "reasoning", EncryptedContent: strings.Repeat("x", maxResponsesHistoryBytes+1)}
	state.captureResponsesHistory(&ResponsesStreamEvent{Type: "response.output_item.done", Item: item})
	require.Equal(t, "oversize", state.ResponsesHistoryResult().Status)
	_, blocks, diagnostic := historyConcurrentWire(t, "extra-visible-text")
	require.Equal(t, ResponsesHistoryDiagnostic{Status: "projection-mismatch", OutputItems: 5, HostedSearches: 2, HostedSources: 2}, diagnostic)
	for _, block := range blocks {
		require.NotEqual(t, ResponsesHistoryBlockType, block.Type)
	}
	encoded, err := json.Marshal(diagnostic)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "fixture query")
	require.NotContains(t, string(encoded), "gAAAA")
	require.NotContains(t, string(encoded), "example.invalid")
}

func TestResponsesHistoryHostedPairIdentityAndContentsRemainStrict(t *testing.T) {
	original := historyFixture(t)
	for name, mutate := range map[string]func([]AnthropicContentBlock) []AnthropicContentBlock{
		"changed query": func(b []AnthropicContentBlock) []AnthropicContentBlock {
			b[1].Input = json.RawMessage(`{"query":"changed"}`)
			return b
		},
		"wrong result id": func(b []AnthropicContentBlock) []AnthropicContentBlock { b[2].ToolUseID = "srvtoolu_other"; return b },
		"changed result content": func(b []AnthropicContentBlock) []AnthropicContentBlock {
			b[2].Content = json.RawMessage(`[{"type":"web_search_result","url":"https://example.invalid/changed"}]`)
			return b
		},
		"duplicate use":    func(b []AnthropicContentBlock) []AnthropicContentBlock { return append(b, b[1]) },
		"duplicate result": func(b []AnthropicContentBlock) []AnthropicContentBlock { return append(b, b[2]) },
		"foreign native signature": func(b []AnthropicContentBlock) []AnthropicContentBlock {
			b[0].Signature = "ACTUAL_NATIVE_SIGNATURE_NOT_IN_PROVIDER_STATE"
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			converted := ResponsesToAnthropic(original, "claude-alias")
			require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
			_, err := historyReplay(t, mutate(converted.Content), historyTestScope)
			require.Error(t, err)
		})
	}
}

func TestResponsesHistoryLargeToolArgumentsRemainExact(t *testing.T) {
	var item ResponsesOutput
	require.NoError(t, json.Unmarshal([]byte(`{"type":"function_call","id":"fc_exact","call_id":"call_exact","name":"fixture","arguments":"{\"value\":9007199254740993}"}`), &item))
	original := &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{item}}
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	converted.Content[0].Input = json.RawMessage(`{"value":9007199254740992}`)
	_, restored, err := restoreResponsesHistory(converted.Content, historyTestScope)
	require.False(t, restored)
	require.Error(t, err)
}

func TestResponsesHistoryRefreshedCiphertextPreservesOnlyObservedSignatureProvenance(t *testing.T) {
	_, blocks, diagnostic := historyConcurrentWire(t, "refreshed-terminal-ciphertext")
	require.Equal(t, "attached", diagnostic.Status)
	require.Equal(t, "future-provider-done-cipher", blocks[0].Signature, "do not forge a native signature using terminal ciphertext")
	var state responsesHistoryEnvelope
	require.NoError(t, json.Unmarshal(blocks[len(blocks)-1].Content, &state))
	require.Equal(t, []responsesHistorySignature{{ItemID: "rs_fixture", SHA256: responsesHistorySignatureHash("future-provider-done-cipher")}}, state.Signatures)
	require.Contains(t, string(state.Output[0]), "future-provider-terminal-cipher")
	require.NotContains(t, string(blocks[len(blocks)-1].Content), "future-provider-done-cipher", "only its hash is additional provenance")

	// A serialized cache/restart round trip must replay the newest ciphertext,
	// while the original visible/native signature remains the observed one.
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	var reloaded []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	replay, err := historyReplay(t, reloaded, historyTestScope)
	require.NoError(t, err)
	require.Contains(t, string(replay.Input), "future-provider-terminal-cipher")
	require.NotContains(t, string(replay.Input), "future-provider-done-cipher")

	// Scope isolation recognizes the exact old provider signature even when
	// the provider changes its ciphertext prefix. Unrelated native state is
	// untouched rather than broadly dropping every thinking signature.
	reloaded = append(reloaded, AnthropicContentBlock{Type: "thinking", Thinking: "native fixture", Signature: "native_claude_signature"})
	foreign, err := historyReplay(t, reloaded, strings.Repeat("b", 64))
	require.NoError(t, err)
	require.NotContains(t, string(foreign.Input), "future-provider-done-cipher")
	require.NotContains(t, string(foreign.Input), "future-provider-terminal-cipher")
	require.Contains(t, string(foreign.Input), "native_claude_signature")
}

func TestResponsesHistorySignatureProvenanceRejectsInvalidOrUnobservedClaims(t *testing.T) {
	_, originalBlocks, _ := historyConcurrentWire(t, "refreshed-terminal-ciphertext")
	for name, mutate := range map[string]func([]AnthropicContentBlock, *responsesHistoryEnvelope){
		"changed native signature": func(blocks []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			blocks[0].Signature = "unobserved_native_signature"
		},
		"unknown reasoning id": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures[0].ItemID = "rs_unknown"
		},
		"non reasoning id": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures[0].ItemID = "ws_fixture"
		},
		"missing reasoning id": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) { state.Signatures[0].ItemID = "" },
		"invalid hash": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures[0].SHA256 = "not-a-sha256"
		},
		"different signature hash": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures[0].SHA256 = responsesHistorySignatureHash("other_signature")
		},
		"duplicate provenance": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures = append(state.Signatures, state.Signatures[0])
		},
		"oversize provenance": func(_ []AnthropicContentBlock, state *responsesHistoryEnvelope) {
			state.Signatures = make([]responsesHistorySignature, maxResponsesHistoryItems*3+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			blocks := append([]AnthropicContentBlock(nil), originalBlocks...)
			var state responsesHistoryEnvelope
			require.NoError(t, json.Unmarshal(blocks[len(blocks)-1].Content, &state))
			mutate(blocks, &state)
			var err error
			blocks[len(blocks)-1].Content, err = json.Marshal(state)
			require.NoError(t, err)
			_, _, err = restoreResponsesHistory(blocks, historyTestScope)
			require.Error(t, err)
		})
	}
	var signature responsesHistorySignature
	require.Error(t, json.Unmarshal([]byte(`{"item_id":"rs_fixture","sha256":"`+strings.Repeat("a", 64)+`","unrecognized":true}`), &signature))
}
