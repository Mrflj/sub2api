package apicompat

// Only explicit provider phases understood by Devin are forwarded. Missing or
// unfamiliar phases are never inferred from text, tool calls or stop reasons.
func responsesAssistantPhase(phase string) string {
	if phase == "commentary" || phase == "final_answer" {
		return phase
	}
	return ""
}

func (state *ResponsesEventToAnthropicState) captureResponsesMessagePhase(event *ResponsesStreamEvent) {
	if !validResponsesHistoryScope(state.ResponsesHistoryScope) || event.Item == nil ||
		event.Item.Type != "message" || event.OutputIndex < 0 {
		return
	}
	phase := responsesAssistantPhase(event.Item.Phase)
	if phase == "" {
		return
	}
	if state.messagePhases == nil {
		state.messagePhases = map[int]string{}
		state.phaseStarted = map[int]bool{}
	}
	if _, exists := state.messagePhases[event.OutputIndex]; !exists && len(state.messagePhases) >= maxResponsesHistoryItems {
		return
	}
	// A late item.done cannot overwrite a phase already delivered for that item.
	if state.phaseStarted[event.OutputIndex] && state.messagePhases[event.OutputIndex] != phase {
		return
	}
	state.messagePhases[event.OutputIndex] = phase
}

func (state *ResponsesEventToAnthropicState) responsesMessagePhase(index int) string {
	if !validResponsesHistoryScope(state.ResponsesHistoryScope) {
		return ""
	}
	return state.messagePhases[index]
}

// Send the phase before its message's first text delta, including phase-only
// messages. Multiple output messages keep their own phase and stream order.
func resToAnthStartMessagePhase(outputIndex int, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	phase := state.responsesMessagePhase(outputIndex)
	if phase == "" || state.phaseStarted[outputIndex] {
		return nil
	}
	events := closeCurrentBlock(state)
	index := state.ContentBlockIndex
	state.ContentBlockOpen = true
	state.CurrentBlockType = "text"
	state.textOutputIndex = outputIndex
	state.phaseStarted[outputIndex] = true
	return append(events, AnthropicStreamEvent{
		Type: "content_block_start", Index: &index,
		ContentBlock: &AnthropicContentBlock{Type: "text", Phase: phase},
	})
}
