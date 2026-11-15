package apicompat

import (
	"encoding/json"
	"strings"
)

// responsesAnthropicToolStream serializes only overlapping output items. The
// active tool's argument fragments still stream immediately. Fragments for
// sibling tools wait until their own content block can be opened, so none can
// target a block that was already stopped. Non-tool events otherwise retain
// their original converter and do not wait for response.completed.
type responsesAnthropicToolStream struct {
	pending       []ResponsesStreamEvent
	items         map[int]ResponsesOutput
	closed        map[int]bool
	args          map[int]*strings.Builder
	customStarted map[int]bool
	active        bool
	index         int
}

func newResponsesAnthropicToolStream() *responsesAnthropicToolStream {
	return &responsesAnthropicToolStream{
		items: make(map[int]ResponsesOutput), closed: make(map[int]bool),
		args:          make(map[int]*strings.Builder),
		customStarted: make(map[int]bool),
	}
}

func isResponsesAnthropicToolItem(item *ResponsesOutput) bool {
	return item != nil && (item.Type == "function_call" || item.Type == "custom_tool_call")
}

func isResponsesAnthropicToolAdded(evt *ResponsesStreamEvent) bool {
	return evt.Type == "response.output_item.added" && isResponsesAnthropicToolItem(evt.Item)
}

func isResponsesAnthropicTerminal(evt *ResponsesStreamEvent) bool {
	switch evt.Type {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return true
	}
	return false
}

func responsesAnthropicTerminalHasTools(evt *ResponsesStreamEvent) bool {
	if !isResponsesAnthropicTerminal(evt) || evt.Response == nil {
		return false
	}
	for i := range evt.Response.Output {
		if isResponsesAnthropicToolItem(&evt.Response.Output[i]) {
			return true
		}
	}
	return false
}

func isResponsesAnthropicToolArgs(evt *ResponsesStreamEvent) bool {
	switch evt.Type {
	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	}
	return false
}

func (q *responsesAnthropicToolStream) convert(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if isResponsesAnthropicTerminal(evt) {
		q.recoverTerminalTools(evt)
		events := q.drain(state, evt, true)
		return append(events, resToAnthConvertEvent(evt, state)...)
	}
	copy := *evt
	if evt.Item != nil {
		item := *evt.Item
		copy.Item = &item
	}
	// Keep identities before draining: sibling output_item.added events can be
	// waiting in the queue when their terminal snapshot arrives.
	if isResponsesAnthropicToolAdded(&copy) {
		if _, seen := q.items[copy.OutputIndex]; seen {
			return nil
		}
		q.items[copy.OutputIndex] = *copy.Item
	}
	q.pending = append(q.pending, copy)
	return q.drain(state, nil, false)
}

func (q *responsesAnthropicToolStream) drain(state *ResponsesEventToAnthropicState, terminal *ResponsesStreamEvent, force bool) []AnthropicStreamEvent {
	var events []AnthropicStreamEvent
	for len(q.pending) > 0 || q.active {
		position := 0
		if q.active {
			position = -1
			for i := range q.pending {
				evt := &q.pending[i]
				if evt.OutputIndex == q.index && (isResponsesAnthropicToolArgs(evt) ||
					(evt.Type == "response.output_item.done" && isResponsesAnthropicToolItem(evt.Item))) {
					position = i
					break
				}
			}
			if position < 0 {
				if !force {
					break
				}
				events = append(events, q.finish(q.index, q.terminalArguments(terminal, q.index), state)...)
				continue
			}
		}
		if len(q.pending) == 0 {
			break
		}
		evt := q.pending[position]
		if position == 0 {
			q.pending[0] = ResponsesStreamEvent{}
			q.pending = q.pending[1:]
		} else {
			copy(q.pending[position:], q.pending[position+1:])
			q.pending[len(q.pending)-1] = ResponsesStreamEvent{}
			q.pending = q.pending[:len(q.pending)-1]
		}

		if isResponsesAnthropicToolAdded(&evt) {
			if q.closed[evt.OutputIndex] {
				continue
			}
			q.active, q.index = true, evt.OutputIndex
			events = append(events, resToAnthConvertEvent(&evt, state)...)
			continue
		}
		if isResponsesAnthropicToolArgs(&evt) {
			if q.closed[evt.OutputIndex] || !q.active || evt.OutputIndex != q.index {
				continue
			}
			if strings.HasSuffix(evt.Type, ".done") {
				full := evt.Arguments
				if evt.Type == "response.custom_tool_call_input.done" {
					full = evt.Input
				}
				events = append(events, q.finish(evt.OutputIndex, full, state)...)
			} else {
				if q.args[evt.OutputIndex] == nil {
					q.args[evt.OutputIndex] = &strings.Builder{}
				}
				q.args[evt.OutputIndex].WriteString(evt.Delta)
				events = append(events, q.emitArgumentDelta(evt.OutputIndex, evt.Delta, state)...)
			}
			continue
		}
		if evt.Type == "response.output_item.done" && isResponsesAnthropicToolItem(evt.Item) {
			if q.closed[evt.OutputIndex] {
				continue
			}
			if !q.active {
				// A done-only tool snapshot still contains a complete call.
				added := evt
				added.Type = "response.output_item.added"
				q.items[evt.OutputIndex] = *evt.Item
				q.active, q.index = true, evt.OutputIndex
				events = append(events, resToAnthConvertEvent(&added, state)...)
			}
			events = append(events, q.finish(evt.OutputIndex, responsesAnthropicToolArguments(evt.Item), state)...)
			continue
		}
		events = append(events, resToAnthConvertEvent(&evt, state)...)
	}
	return events
}

func (q *responsesAnthropicToolStream) finish(index int, full string, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	var events []AnthropicStreamEvent
	// A done snapshot can contain a suffix missing from the deltas. Recover
	// only a proven extension, never repeat or rewrite arguments already sent.
	if delivered := q.args[index]; delivered != nil && delivered.Len() > 0 &&
		strings.HasPrefix(full, delivered.String()) && len(full) > delivered.Len() {
		tail := full[delivered.Len():]
		events = append(events, q.emitArgumentDelta(index, tail, state)...)
	}
	if q.items[index].Type == "custom_tool_call" {
		if q.customStarted[index] {
			events = append(events, resToAnthConvertEvent(&ResponsesStreamEvent{
				Type: "response.function_call_arguments.delta", OutputIndex: index, Delta: `"}`,
			}, state)...)
		}
		full = responsesAnthropicCustomToolInput(full)
	}
	events = append(events, resToAnthHandleFuncArgsDone(&ResponsesStreamEvent{
		Type: "response.function_call_arguments.done", OutputIndex: index, Arguments: full,
	}, state)...)
	q.closed[index] = true
	delete(q.args, index)
	delete(q.customStarted, index)
	q.active = false
	return events
}

func (q *responsesAnthropicToolStream) emitArgumentDelta(index int, delta string, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if q.items[index].Type == "custom_tool_call" {
		// Messages tools use object input, matching customToolInputSchema.
		// Escape each raw fragment inside one JSON string while keeping the
		// first custom tool streaming; ordinary function arguments are untouched.
		quoted, _ := json.Marshal(delta)
		delta = string(quoted[1 : len(quoted)-1])
		if !q.customStarted[index] {
			delta = `{"input":"` + delta
			q.customStarted[index] = true
		}
	}
	return resToAnthConvertEvent(&ResponsesStreamEvent{
		Type: "response.function_call_arguments.delta", OutputIndex: index, Delta: delta,
	}, state)
}

func responsesAnthropicCustomToolInput(input string) string {
	encoded, _ := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: input})
	return string(encoded)
}

func responsesAnthropicToolArguments(item *ResponsesOutput) string {
	if item.Type == "custom_tool_call" {
		return item.Input
	}
	return item.Arguments
}

func (q *responsesAnthropicToolStream) terminalArguments(evt *ResponsesStreamEvent, index int) string {
	if evt == nil || evt.Response == nil {
		return ""
	}
	known := q.items[index]
	for i := range evt.Response.Output {
		item := &evt.Response.Output[i]
		if !isResponsesAnthropicToolItem(item) {
			continue
		}
		if (known.ID != "" && item.ID == known.ID) ||
			(known.CallID != "" && item.CallID == known.CallID) ||
			(i == index && known.ID == "" && known.CallID == "") {
			return responsesAnthropicToolArguments(item)
		}
	}
	return ""
}

func (q *responsesAnthropicToolStream) recoverTerminalTools(evt *ResponsesStreamEvent) {
	if evt.Response == nil {
		return
	}
	for index := range evt.Response.Output {
		item := evt.Response.Output[index]
		if !isResponsesAnthropicToolItem(&item) {
			continue
		}
		seen := false
		for _, known := range q.items {
			if (item.ID != "" && item.ID == known.ID) ||
				(item.CallID != "" && item.CallID == known.CallID) {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		if _, occupied := q.items[index]; occupied {
			// A compatibility upstream may compact the terminal output array.
			// Explicit call/item identity wins over that different array index.
			if item.ID == "" && item.CallID == "" {
				continue
			}
			for {
				index++
				if _, occupied = q.items[index]; !occupied {
					break
				}
			}
		}
		q.items[index] = item
		q.pending = append(q.pending, ResponsesStreamEvent{
			Type: "response.output_item.added", OutputIndex: index, Item: &item,
		})
	}
}
