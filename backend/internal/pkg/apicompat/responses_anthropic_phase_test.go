package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesMessagePhaseWireAndNegotiatedMessages(t *testing.T) {
	var response ResponsesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_phase","status":"completed","output":[
	 {"type":"message","id":"m1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Progress."}]},
	 {"type":"message","id":"m2","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer."}]}]}`), &response))
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"phase":"commentary"`)
	require.Contains(t, string(raw), `"phase":"final_answer"`)
	legacy, err := json.Marshal(ResponsesToAnthropic(&response, "alias"))
	require.NoError(t, err)
	require.NotContains(t, string(legacy), `"phase"`)
	negotiated := ResponsesToAnthropicWithPhase(&response, "alias")
	require.Equal(t, "commentary", negotiated.Content[0].Phase)
	require.Equal(t, "final_answer", negotiated.Content[1].Phase)
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "negotiated"}[enabled], func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()
			if enabled {
				state.ResponsesHistoryScope = strings.Repeat("a", 64)
			}
			var observed []string
			push := func(event ResponsesStreamEvent) {
				for _, converted := range ResponsesEventToAnthropicEvents(&event, state) {
					if converted.ContentBlock != nil && converted.ContentBlock.Phase != "" {
						observed = append(observed, "phase="+converted.ContentBlock.Phase)
					}
					if converted.Delta != nil && converted.Delta.Type == "text_delta" {
						observed = append(observed, "text="+converted.Delta.Text)
					}
				}
			}
			push(ResponsesStreamEvent{Type: "response.created", Response: &response})
			for index := range response.Output {
				item := response.Output[index]
				push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item})
				push(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: index, Delta: item.Content[0].Text})
				push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: index, Item: &item})
			}
			push(ResponsesStreamEvent{Type: "response.completed", Response: &response})
			if enabled {
				require.Equal(t, []string{"phase=commentary", "text=Progress.", "phase=final_answer", "text=Answer."}, observed)
			} else {
				require.Equal(t, []string{"text=Progress.", "text=Answer."}, observed)
			}
		})
	}
}

func TestResponsesMessagePhaseOnlyAndTerminalTextFallback(t *testing.T) {
	for _, phase := range []string{"commentary", "final_answer", "unsupported"} {
		state := NewResponsesEventToAnthropicState()
		state.ResponsesHistoryScope = strings.Repeat("a", 64)
		item := ResponsesOutput{Type: "message", ID: "m", Role: "assistant", Phase: phase}
		events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.output_item.added", Item: &item}, state)
		if phase == "unsupported" {
			require.Empty(t, events)
		} else {
			require.Len(t, events, 1)
			require.Equal(t, phase, events[0].ContentBlock.Phase)
			require.Empty(t, events[0].ContentBlock.Text)
		}
		require.False(t, state.MessageStopSent)
	}
	state := NewResponsesEventToAnthropicState()
	state.ResponsesHistoryScope = strings.Repeat("b", 64)
	response := &ResponsesResponse{ID: "resp_terminal", Status: "completed", Output: []ResponsesOutput{{Type: "message", ID: "m", Role: "assistant", Phase: "final_answer", Content: []ResponsesContentPart{{Type: "output_text", Text: "Recovered."}}}}}
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.created", Response: response}, state)
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{Type: "response.completed", Response: response}, state)
	var phaseAt, textAt = -1, -1
	for index, event := range events {
		if event.ContentBlock != nil && event.ContentBlock.Phase == "final_answer" {
			phaseAt = index
		}
		if event.Delta != nil && event.Delta.Text == "Recovered." {
			textAt = index
		}
	}
	require.GreaterOrEqual(t, phaseAt, 0)
	require.Greater(t, textAt, phaseAt)
}
