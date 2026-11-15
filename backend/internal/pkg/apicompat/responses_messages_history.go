package apicompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// This extension is explicitly negotiated by the local adapter. It is NOT a
// native Anthropic block or a fabricated thinking signature. Other clients
// receive the ordinary Messages response without this block.
const ResponsesHistoryHeader = "X-Sub2api-Responses-History"
const ResponsesHistoryBlockType = "sub2api_responses_history"
const maxResponsesHistoryBytes = 4 * 1024 * 1024
const maxResponsesHistoryItems = 256

type responsesHistoryEnvelope struct {
	Version    int                         `json:"version"`
	Scope      string                      `json:"scope"`
	Output     []json.RawMessage           `json:"output"`
	Signatures []responsesHistorySignature `json:"reasoning_signatures,omitempty"`
}

// Only a hash of a signature actually emitted from an observed done snapshot
// is retained. It associates that older wire signature with the same explicit
// reasoning item when the terminal snapshot refreshes opaque ciphertext.
type responsesHistorySignature struct {
	ItemID string `json:"item_id"`
	SHA256 string `json:"sha256"`
}

func (signature *responsesHistorySignature) UnmarshalJSON(raw []byte) error {
	type plain responsesHistorySignature
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*signature = responsesHistorySignature(value)
	return nil
}

func responsesHistorySignatureHash(signature string) string {
	digest := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(digest[:])
}

func validResponsesHistorySignatures(output []json.RawMessage, signatures []responsesHistorySignature) bool {
	if len(signatures) > maxResponsesHistoryItems*3 {
		return false
	}
	ids := map[string]bool{}
	for _, raw := range output {
		var item responsesHistoryIdentity
		if json.Unmarshal(raw, &item) != nil {
			return false
		}
		if item.Type == "reasoning" && item.ID != "" {
			ids[item.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, signature := range signatures {
		if signature.ItemID == "" || len(signature.ItemID) > 2048 || !ids[signature.ItemID] ||
			strings.IndexFunc(signature.ItemID, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 || !validResponsesHistoryScope(signature.SHA256) {
			return false
		}
		key := signature.ItemID + "\x00" + signature.SHA256
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func validResponsesHistoryScope(scope string) bool {
	if len(scope) != 64 {
		return false
	}
	return strings.IndexFunc(scope, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) == -1
}

func validResponsesHistoryOutput(output []json.RawMessage) bool {
	if len(output) == 0 || len(output) > maxResponsesHistoryItems {
		return false
	}
	ids, calls := map[string]bool{}, map[string]bool{}
	for _, raw := range output {
		var item struct {
			Type, Role, ID string
			CallID         string `json:"call_id"`
			Content        []struct{ Type string }
		}
		if json.Unmarshal(raw, &item) != nil {
			return false
		}
		if item.ID != "" {
			if ids[item.ID] {
				return false
			}
			ids[item.ID] = true
		}
		switch item.Type {
		case "message":
			if item.Role != "assistant" {
				return false
			}
			for _, part := range item.Content {
				if part.Type != "output_text" && part.Type != "refusal" {
					return false
				}
			}
		case "web_search_call":
			if item.ID == "" {
				return false
			}
		case "function_call", "custom_tool_call":
			if item.CallID == "" || calls[item.CallID] {
				return false
			}
			calls[item.CallID] = true
		case "reasoning":
		default:
			return false
		}
	}
	return true
}

type responsesHistoryCall struct{ ID, Name, Input string }
type responsesHistoryShape struct {
	Text       string
	Calls      []responsesHistoryCall
	Signatures []string
	Searches   map[string]string
}

func messagesHistoryShape(blocks []AnthropicContentBlock) (responsesHistoryShape, bool) {
	var shape responsesHistoryShape
	uses, results, calls := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			shape.Text += block.Text
		case "thinking":
			if block.Signature != "" {
				shape.Signatures = append(shape.Signatures, block.Signature)
			}
		case "tool_use":
			if block.ID == "" || calls[block.ID] || uses[block.ID] {
				return shape, false
			}
			calls[block.ID] = true
			canonical, ok := historyCanonicalJSON(block.Input)
			if !ok {
				return shape, false
			}
			shape.Calls = append(shape.Calls, responsesHistoryCall{block.ID, block.Name, string(canonical)})
		case "server_tool_use", "web_search_tool_result":
			// Hosted calls can finish concurrently. Their wire completion order
			// is not the provider's output_index order; identity and exact data
			// establish the pair instead. Duplicate or orphan identities fail.
			id, prefix := block.ID, "use:"
			if block.Type == "server_tool_use" {
				if id == "" || uses[id] || calls[id] {
					return shape, false
				}
				uses[id] = true
			} else {
				id, prefix = block.ToolUseID, "result:"
				if id == "" || results[id] {
					return shape, false
				}
				results[id] = true
			}
			// Compare association, query, sources and error payloads as data.
			block.CacheControl = nil
			data, err := json.Marshal(block)
			if err != nil {
				return shape, false
			}
			data, ok := historyCanonicalJSON(data)
			if !ok {
				return shape, false
			}
			if shape.Searches == nil {
				shape.Searches = map[string]string{}
			}
			shape.Searches[prefix+id] = string(data)
		case ResponsesHistoryBlockType:
		default:
			return shape, false
		}
	}
	if !reflect.DeepEqual(uses, results) {
		return shape, false
	}
	return shape, true
}

func historyOutputItems(output []json.RawMessage) ([]ResponsesOutput, bool) {
	if !validResponsesHistoryOutput(output) {
		return nil, false
	}
	items := make([]ResponsesOutput, len(output))
	for index, raw := range output {
		if json.Unmarshal(raw, &items[index]) != nil {
			return nil, false
		}
	}
	return items, true
}

func historyMatchesBlocks(output []json.RawMessage, blocks []AnthropicContentBlock) bool {
	return historyMatchesBlocksWithSignatures(output, blocks, nil)
}

func historyMatchesBlocksWithSignatures(output []json.RawMessage, blocks []AnthropicContentBlock, signatures []responsesHistorySignature) bool {
	items, ok := historyOutputItems(output)
	if !ok || !validResponsesHistorySignatures(output, signatures) {
		return false
	}
	expected := ResponsesToAnthropic(&ResponsesResponse{Status: "completed", Output: items}, "")
	want, ok := messagesHistoryShape(expected.Content)
	if !ok {
		return false
	}
	got, ok := messagesHistoryShape(blocks)
	if !ok {
		return false
	}
	// A terminal response may add ciphertext after the visible thinking
	// block was closed. Preserve that actual provider state without inventing
	// a native signature. Every signature that DID reach the client must
	// match raw output or exact, item-associated provenance captured when
	// a done snapshot emitted an older version of that opaque signature.
	available := map[string]int{}
	observed := map[string]int{}
	for _, signature := range want.Signatures {
		available[signature]++
	}
	for _, signature := range signatures {
		observed[signature.SHA256]++
	}
	for _, signature := range got.Signatures {
		if available[signature] == 0 {
			hash := responsesHistorySignatureHash(signature)
			if observed[hash] == 0 {
				return false
			}
			observed[hash]--
			continue
		}
		available[signature]--
	}
	want.Signatures, got.Signatures = nil, nil
	return reflect.DeepEqual(want, got)
}

// AttachResponsesHistory carries actual upstream output (including opaque
// reasoning ciphertext, hosted tool actions/sources and message phase) exactly
// once. The ordinary visible answer/calls must match before state is attached.
func AttachResponsesHistory(response *AnthropicResponse, original *ResponsesResponse, scope string) bool {
	if response == nil || original == nil || original.Status != "completed" || !validResponsesHistoryScope(scope) {
		return false
	}
	output := make([]json.RawMessage, 0, len(original.Output))
	for _, item := range original.Output {
		raw := item.rawOutput
		if len(raw) == 0 {
			var err error
			raw, err = json.Marshal(item)
			if err != nil {
				return false
			}
		}
		output = append(output, raw)
	}
	return attachResponsesHistoryOutput(response, output, scope)
}

func attachResponsesHistoryOutput(response *AnthropicResponse, output []json.RawMessage, scope string) bool {
	return attachResponsesHistoryOutputWithSignatures(response, output, scope, nil)
}

func attachResponsesHistoryOutputWithSignatures(response *AnthropicResponse, output []json.RawMessage, scope string, signatures []responsesHistorySignature) bool {
	if !validResponsesHistoryScope(scope) || !historyMatchesBlocksWithSignatures(output, response.Content, signatures) {
		return false
	}
	for _, block := range response.Content {
		if block.Type == ResponsesHistoryBlockType {
			return false
		}
	}
	content, err := json.Marshal(responsesHistoryEnvelope{Version: 1, Scope: scope, Output: output, Signatures: signatures})
	if err != nil || len(content) > maxResponsesHistoryBytes {
		return false
	}
	response.Content = append(response.Content, AnthropicContentBlock{Type: ResponsesHistoryBlockType, Content: content})
	return true
}

func restoreResponsesHistory(blocks []AnthropicContentBlock, scope string) ([]ResponsesInputItem, bool, error) {
	var raw json.RawMessage
	for _, block := range blocks {
		if block.Type != ResponsesHistoryBlockType {
			continue
		}
		if raw != nil {
			return nil, false, unsupportedRequestConversion("duplicate Responses history state")
		}
		raw = block.Content
	}
	if raw == nil {
		return nil, false, nil
	}
	if len(raw) > maxResponsesHistoryBytes {
		return nil, false, unsupportedRequestConversion("Responses history state exceeds the supported limit")
	}
	var state responsesHistoryEnvelope
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 || !validResponsesHistoryScope(state.Scope) || !validResponsesHistoryOutput(state.Output) || !validResponsesHistorySignatures(state.Output, state.Signatures) {
		return nil, false, unsupportedRequestConversion("invalid Responses history state")
	}
	// State from another upstream account/model is opaque foreign state. Keep
	// the visible conversation but never send its ciphertext to that account.
	if !validResponsesHistoryScope(scope) || state.Scope != scope {
		// Recognize state by provenance, not by a ciphertext prefix. A provider
		// is free to change its encryption format. Never forward a foreign
		// account's captured signature through the legacy fallback converter.
		signatures := map[string]bool{}
		signatureHashes := map[string]bool{}
		for _, signature := range state.Signatures {
			signatureHashes[signature.SHA256] = true
		}
		for _, raw := range state.Output {
			var item ResponsesOutput
			if json.Unmarshal(raw, &item) == nil && item.Type == "reasoning" && item.EncryptedContent != "" {
				signatures[item.EncryptedContent] = true
			}
		}
		visible := make([]AnthropicContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == ResponsesHistoryBlockType || block.Type == "thinking" && (signatures[block.Signature] || signatureHashes[responsesHistorySignatureHash(block.Signature)]) {
				continue
			}
			visible = append(visible, block)
		}
		content, err := json.Marshal(visible)
		if err != nil {
			return nil, false, err
		}
		items, err := anthropicAssistantToResponses(content)
		return items, true, err
	}
	if !historyMatchesBlocksWithSignatures(state.Output, blocks, state.Signatures) {
		return nil, false, unsupportedRequestConversion("Responses history does not match the assistant message")
	}
	items := make([]ResponsesInputItem, len(state.Output))
	for index, item := range state.Output {
		if err := json.Unmarshal(item, &items[index]); err != nil {
			return nil, false, unsupportedRequestConversion("invalid Responses history item")
		}
	}
	return items, true, nil
}

// ResponsesHistoryMatchesAssistantContent checks only the exact projection
// carried by a negotiated relay message. A transit relay does not know the
// origin's routed-account scope, so this does NOT authenticate that scope or
// authorize replay. The origin still enforces its actual account/model scope.
// It lets the transit relay keep a valid capsule and its visible hosted blocks
// together instead of destroying one side of their consistency contract.
func ResponsesHistoryMatchesAssistantContent(content json.RawMessage) bool {
	if len(content) > maxResponsesHistoryBytes*2 {
		return false
	}
	var blocks []AnthropicContentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	var scope string
	for _, block := range blocks {
		if block.Type == ResponsesHistoryBlockType {
			var envelope responsesHistoryEnvelope
			if json.Unmarshal(block.Content, &envelope) != nil {
				return false
			}
			scope = envelope.Scope
		}
	}
	if !validResponsesHistoryScope(scope) {
		return false
	}
	_, restored, err := restoreResponsesHistory(blocks, scope)
	return restored && err == nil
}

// Capture only original provider items. No model invocation, invented search
// result or synthetic signature is involved in preserving this state.
type responsesHistoryStream struct {
	output              map[int]json.RawMessage
	blocks              map[int]*AnthropicContentBlock
	valid               bool
	bytes               int
	deliveredBytes      int
	result              string
	items               int
	searches            int
	sources             int
	reasoningSignatures map[string]map[string]bool
	signatureCount      int
}

// ResponsesHistoryDiagnostic contains structural counts and a fixed status
// only. It never exposes queries, message text, signatures or provider state.
type ResponsesHistoryDiagnostic struct {
	Status         string `json:"status"`
	OutputItems    int    `json:"output_items"`
	HostedSearches int    `json:"hosted_searches"`
	HostedSources  int    `json:"hosted_sources"`
}

func (state *ResponsesEventToAnthropicState) ResponsesHistoryResult() ResponsesHistoryDiagnostic {
	if state == nil || !validResponsesHistoryScope(state.ResponsesHistoryScope) {
		return ResponsesHistoryDiagnostic{Status: "not-requested"}
	}
	result := ResponsesHistoryDiagnostic{Status: "pending"}
	if state.history == nil {
		return result
	}
	history := state.history
	if history.result != "" {
		result.Status = history.result
	}
	if history.result != "attached" {
		observed := make([]json.RawMessage, 0, len(history.output))
		for _, raw := range history.output {
			observed = append(observed, raw)
		}
		history.recordCounts(observed)
	}
	result.OutputItems, result.HostedSearches, result.HostedSources = history.items, history.searches, history.sources
	return result
}

func (history *responsesHistoryStream) recordCounts(output []json.RawMessage) {
	history.items, history.searches, history.sources = len(output), 0, 0
	for _, raw := range output {
		var item struct {
			Type   string `json:"type"`
			Action struct {
				Sources []json.RawMessage `json:"sources"`
			} `json:"action"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "web_search_call" {
			history.searches++
			history.sources += len(item.Action.Sources)
		}
	}
}

type responsesHistoryIdentity struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	CallID string `json:"call_id"`
}

func historyItemIdentity(raw json.RawMessage) (responsesHistoryIdentity, bool) {
	var identity responsesHistoryIdentity
	err := json.Unmarshal(raw, &identity)
	return identity, err == nil && identity.Type != ""
}

func historySameIdentity(a, b responsesHistoryIdentity) bool {
	return a.Type == b.Type && (a.ID != "" && a.ID == b.ID || a.CallID != "" && a.CallID == b.CallID)
}

func historyJSONEqual(a, b json.RawMessage) bool {
	left, leftOK := historyCanonicalJSON(a)
	right, rightOK := historyCanonicalJSON(b)
	return leftOK && rightOK && bytes.Equal(left, right)
}

func historyCanonicalJSON(raw json.RawMessage) ([]byte, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

// Merge only observations of one already identified output item. Absent
// object fields are complementary observations; overlapping values must be
// equal. The few empty fields that providers populate only at termination are
// explicit below. Conflicting text, arguments, queries or identities never
// become a successful replay by choosing one side.
func mergeResponsesHistoryValue(left, right json.RawMessage, kind, path string, refreshCiphertext bool) (json.RawMessage, bool) {
	if historyJSONEqual(left, right) {
		return left, true
	}
	var leftObject, rightObject map[string]json.RawMessage
	if json.Unmarshal(left, &leftObject) == nil && leftObject != nil && json.Unmarshal(right, &rightObject) == nil && rightObject != nil {
		for key, value := range rightObject {
			previous, exists := leftObject[key]
			if !exists {
				leftObject[key] = value
				continue
			}
			child := key
			if path != "" {
				child = path + "." + key
			}
			merged, ok := mergeResponsesHistoryValue(previous, value, kind, child, refreshCiphertext)
			if !ok {
				return nil, false
			}
			leftObject[key] = merged
		}
		merged, err := json.Marshal(leftObject)
		return merged, err == nil
	}
	if kind == "reasoning" && path == "encrypted_content" {
		var a, b string
		if json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil {
			if a == "" {
				return right, true
			}
			if b == "" {
				return left, true
			}
			// Opaque provider ciphertext is replaceable, not an append-only
			// visible value. Only an explicit matching reasoning item identity
			// permits this refresh; queries/text/tool arguments remain strict.
			if refreshCiphertext {
				return right, true
			}
		}
	}
	if kind == "reasoning" && path == "summary" || kind == "message" && path == "content" || kind == "web_search_call" && path == "action.sources" {
		var a, b []json.RawMessage
		if json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil {
			if len(a) == 0 {
				return right, true
			}
			if len(b) == 0 {
				return left, true
			}
		}
	}
	return nil, false
}

func mergeResponsesHistoryItem(left, right json.RawMessage) (json.RawMessage, bool) {
	a, aOK := historyItemIdentity(left)
	b, bOK := historyItemIdentity(right)
	if !aOK || !bOK || a.Type != b.Type {
		return nil, false
	}
	if a.ID != "" && b.ID != "" && a.ID != b.ID || a.CallID != "" && b.CallID != "" && a.CallID != b.CallID {
		return nil, false
	}
	refreshCiphertext := a.Type == "reasoning" && a.ID != "" && a.ID == b.ID
	return mergeResponsesHistoryValue(left, right, a.Type, "", refreshCiphertext)
}

func (history *responsesHistoryStream) observeDoneSignature(item *ResponsesOutput) bool {
	if item == nil || item.Type != "reasoning" || item.ID == "" || item.EncryptedContent == "" {
		return true
	}
	if history.reasoningSignatures == nil {
		history.reasoningSignatures = map[string]map[string]bool{}
	}
	values := history.reasoningSignatures[item.ID]
	if values == nil {
		values = map[string]bool{}
		history.reasoningSignatures[item.ID] = values
	}
	hash := responsesHistorySignatureHash(item.EncryptedContent)
	if !values[hash] {
		if history.signatureCount >= maxResponsesHistoryItems*3 {
			return false
		}
		values[hash] = true
		history.signatureCount++
	}
	return true
}

func (history *responsesHistoryStream) deliveredSignatureProvenance(output []json.RawMessage, blocks []AnthropicContentBlock) ([]responsesHistorySignature, bool) {
	finalIDs, current := map[string]bool{}, map[string]int{}
	for _, raw := range output {
		var item ResponsesOutput
		if json.Unmarshal(raw, &item) != nil {
			return nil, false
		}
		if item.Type != "reasoning" {
			continue
		}
		if item.ID != "" {
			finalIDs[item.ID] = true
		}
		if item.EncryptedContent != "" {
			current[item.EncryptedContent]++
		}
	}
	var signatures []responsesHistorySignature
	for _, block := range blocks {
		if block.Type != "thinking" || block.Signature == "" {
			continue
		}
		if current[block.Signature] > 0 {
			current[block.Signature]--
			continue
		}
		hash, match := responsesHistorySignatureHash(block.Signature), ""
		for id, observed := range history.reasoningSignatures {
			if !finalIDs[id] || !observed[hash] {
				continue
			}
			if match != "" {
				return nil, false
			}
			match = id
		}
		if match == "" {
			return nil, false
		}
		signatures = append(signatures, responsesHistorySignature{ItemID: match, SHA256: hash})
	}
	return signatures, validResponsesHistorySignatures(output, signatures)
}

func responsesHistoryRawOutput(response *ResponsesResponse) []json.RawMessage {
	if response == nil {
		return nil
	}
	output := make([]json.RawMessage, 0, len(response.Output))
	for _, item := range response.Output {
		raw := item.rawOutput
		if len(raw) == 0 {
			raw, _ = json.Marshal(item)
		}
		output = append(output, raw)
	}
	return output
}

// Some compatibility upstreams compact the final array. Keep the original
// output_index order from done events and identify terminal refinements by
// explicit item/call IDs. A previously unseen terminal item is admitted only
// when the array's positions are established by the existing observations.
func reconcileResponsesHistoryOutput(done map[int]json.RawMessage, terminal []json.RawMessage) ([]json.RawMessage, bool) {
	if len(done) == 0 {
		return terminal, validResponsesHistoryOutput(terminal)
	}
	merged := make(map[int]json.RawMessage, len(done)+len(terminal))
	identities := map[int]responsesHistoryIdentity{}
	maxIndex := -1
	for index, raw := range done {
		identity, ok := historyItemIdentity(raw)
		if !ok {
			return nil, false
		}
		merged[index], identities[index] = raw, identity
		if index > maxIndex {
			maxIndex = index
		}
	}
	if len(terminal) > 0 && !validResponsesHistoryOutput(terminal) {
		return nil, false
	}
	positions := len(terminal) > maxIndex
	matched := make([]int, len(terminal))
	for index, raw := range terminal {
		identity, ok := historyItemIdentity(raw)
		if !ok {
			return nil, false
		}
		matched[index] = -1
		for previousIndex, previous := range identities {
			if !historySameIdentity(previous, identity) {
				continue
			}
			if matched[index] >= 0 {
				return nil, false
			}
			matched[index] = previousIndex
			if previousIndex != index {
				positions = false
			}
		}
	}
	for index, raw := range terminal {
		at := matched[index]
		if at < 0 {
			if !positions {
				return nil, false
			}
			at = index
		}
		if previous, exists := merged[at]; exists {
			value, ok := mergeResponsesHistoryItem(previous, raw)
			if !ok {
				return nil, false
			}
			merged[at] = value
		} else {
			merged[at] = raw
		}
	}
	indexes := make([]int, 0, len(merged))
	for index := range merged {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	output := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		output = append(output, merged[index])
	}
	return output, validResponsesHistoryOutput(output)
}

func (state *ResponsesEventToAnthropicState) captureResponsesHistory(event *ResponsesStreamEvent) {
	if !validResponsesHistoryScope(state.ResponsesHistoryScope) {
		return
	}
	if state.history == nil {
		state.history = &responsesHistoryStream{output: map[int]json.RawMessage{}, blocks: map[int]*AnthropicContentBlock{}, valid: true}
	}
	history := state.history
	if !history.valid {
		return
	}
	if event.Type == "response.output_item.done" && event.Item != nil && event.OutputIndex >= 0 {
		if !history.observeDoneSignature(event.Item) {
			history.valid, history.result = false, "oversize"
			return
		}
		raw := event.Item.rawOutput
		if len(raw) == 0 {
			raw, _ = json.Marshal(event.Item)
		}
		if previous := history.output[event.OutputIndex]; previous != nil {
			var ok bool
			raw, ok = mergeResponsesHistoryItem(previous, raw)
			if !ok {
				history.valid = false
				history.result = "output-conflict"
				return
			}
		}
		history.bytes += len(raw) - len(history.output[event.OutputIndex])
		if len(history.output) >= maxResponsesHistoryItems && history.output[event.OutputIndex] == nil || history.bytes > maxResponsesHistoryBytes {
			history.valid = false
			history.result = "oversize"
			return
		}
		history.output[event.OutputIndex] = append(json.RawMessage(nil), raw...)
	}
}

func (state *ResponsesEventToAnthropicState) withResponsesHistory(event *ResponsesStreamEvent, events []AnthropicStreamEvent) []AnthropicStreamEvent {
	history := state.history
	if history == nil || !history.valid {
		return events
	}
	for _, emitted := range events {
		if emitted.Index == nil {
			continue
		}
		index := *emitted.Index
		switch emitted.Type {
		case "content_block_start":
			if emitted.ContentBlock == nil {
				history.valid = false
				history.result = "projection-mismatch"
				return events
			}
			copy := *emitted.ContentBlock
			history.deliveredBytes += len(copy.Text) + len(copy.Input) + len(copy.Content) + len(copy.Signature)
			if len(history.blocks) >= maxResponsesHistoryItems*3 || history.deliveredBytes > maxResponsesHistoryBytes {
				history.valid = false
				history.result = "oversize"
				return events
			}
			history.blocks[index] = &copy
		case "content_block_delta":
			block, delta := history.blocks[index], emitted.Delta
			if block == nil || delta == nil {
				history.valid = false
				history.result = "projection-mismatch"
				return events
			}
			history.deliveredBytes += len(delta.Text) + len(delta.Signature) + len(delta.PartialJSON)
			if history.deliveredBytes > maxResponsesHistoryBytes {
				history.valid = false
				history.result = "oversize"
				return events
			}
			switch delta.Type {
			case "text_delta":
				block.Text += delta.Text
			case "thinking_delta": // Never copy private thought text into diagnostic state.
			case "signature_delta":
				block.Signature += delta.Signature
			case "input_json_delta":
				if bytes.Equal(bytes.TrimSpace(block.Input), []byte("{}")) {
					block.Input = nil
				}
				block.Input = append(block.Input, delta.PartialJSON...)
			}
		}
	}
	if !state.MessageStopSent || event.Response == nil || event.Response.Status != "completed" {
		return events
	}
	indexes := make([]int, 0, len(history.blocks))
	for index := range history.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	response := &AnthropicResponse{}
	for _, index := range indexes {
		response.Content = append(response.Content, *history.blocks[index])
	}
	output, reconciled := reconcileResponsesHistoryOutput(history.output, responsesHistoryRawOutput(event.Response))
	if !reconciled {
		history.result = "output-conflict"
		return events
	}
	history.recordCounts(output)
	signatures, proven := history.deliveredSignatureProvenance(output, response.Content)
	if !proven || !historyMatchesBlocksWithSignatures(output, response.Content, signatures) {
		history.result = "projection-mismatch"
		return events
	}
	encoded, err := json.Marshal(responsesHistoryEnvelope{Version: 1, Scope: state.ResponsesHistoryScope, Output: output, Signatures: signatures})
	if err != nil || len(encoded) > maxResponsesHistoryBytes {
		history.result = "oversize"
		return events
	}
	if !attachResponsesHistoryOutputWithSignatures(response, output, state.ResponsesHistoryScope, signatures) {
		history.result = "projection-mismatch"
		return events
	}
	block := response.Content[len(response.Content)-1]
	index := state.ContentBlockIndex
	state.ContentBlockIndex++
	additional := []AnthropicStreamEvent{{Type: "content_block_start", Index: &index, ContentBlock: &block}, {Type: "content_block_stop", Index: &index}}
	for pos, emitted := range events {
		if emitted.Type == "message_delta" || emitted.Type == "message_stop" {
			history.result = "attached"
			out := append([]AnthropicStreamEvent(nil), events[:pos]...)
			out = append(out, additional...)
			return append(out, events[pos:]...)
		}
	}
	history.result = "terminal-missing"
	return events
}
