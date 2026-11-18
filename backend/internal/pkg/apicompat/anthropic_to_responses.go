package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// AnthropicToResponses converts an Anthropic Messages request directly into
// a Responses API request. This preserves fields that would be lost in a
// Chat Completions intermediary round-trip (e.g. thinking, cache_control,
// structured system prompts).
func AnthropicToResponses(req *AnthropicRequest) (*ResponsesRequest, error) {
	return AnthropicToResponsesWithMinOutputTokens(req, DefaultMinMaxOutputTokens)
}

// AnthropicToResponsesWithMinOutputTokens converts an Anthropic request using
// the supplied max_output_tokens floor.
func AnthropicToResponsesWithMinOutputTokens(req *AnthropicRequest, minOutputTokens int) (*ResponsesRequest, error) {
	return AnthropicToResponsesWithOptions(req, AnthropicToResponsesOptions{MinOutputTokens: minOutputTokens})
}

// AnthropicToResponsesOptions controls conversion and optional deferred effort
// validation. Callers deferring validation must validate raw selections before
// conversion and the final effort after applying their policy.
type AnthropicToResponsesOptions struct {
	MinOutputTokens                int
	DeferReasoningEffortValidation bool
	ResponsesHistoryScope          string
	// The caller must enforce stop sequences on the converted response.
	StopSequencesHandledLocally bool
}

func AnthropicToResponsesWithOptions(req *AnthropicRequest, options AnthropicToResponsesOptions) (*ResponsesRequest, error) {
	if err := validateAnthropicRequestMedia(req, "responses"); err != nil {
		return nil, err
	}
	// Compatibility mode omits top_k because Responses has no equivalent.
	if len(req.StopSeqs) > 0 && !options.StopSequencesHandledLocally {
		return nil, unsupportedRequestConversion("stop_sequences has no equivalent in Responses; use a native Messages or Chat Completions endpoint to retain this generation control")
	}
	if options.StopSequencesHandledLocally {
		for _, stop := range req.StopSeqs {
			if stop == "" {
				return nil, unsupportedRequestConversion("stop_sequences must contain non-empty strings")
			}
		}
	}
	if !options.DeferReasoningEffortValidation {
		if err := openai.ValidateGPT61ReasoningEffort(req.Model, anthropicReasoningEffort(req)); err != nil {
			return nil, err
		}
	}
	input, err := convertAnthropicToResponsesInputWithHistory(req.System, req.Messages, options.ResponsesHistoryScope)
	if err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	out := &ResponsesRequest{
		Model:   req.Model,
		Input:   inputJSON,
		Stream:  req.Stream,
		Include: []string{"reasoning.encrypted_content"},
	}

	// Reasoning models (gpt-5.x) served via the Responses API do not accept
	// sampling parameters. Sending temperature or top_p causes a 400
	// "Unsupported parameter" error, so we only forward them for non-reasoning
	// models.
	if !isReasoningModel(req.Model) {
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}

	storeFalse := false
	out.Store = &storeFalse
	parallelToolCalls := anthropicParallelToolCalls(req.ToolChoice)
	out.ParallelToolCalls = &parallelToolCalls
	out.Text = &ResponsesText{Verbosity: "medium"}
	if req.OutputConfig != nil && len(req.OutputConfig.Format) > 0 {
		format, err := anthropicOutputFormatToResponsesTextFormat(req.OutputConfig.Format)
		if err != nil {
			return nil, err
		}
		out.Text.Format = format
	}

	if req.MaxTokens > 0 {
		v := req.MaxTokens
		if v < options.MinOutputTokens {
			v = options.MinOutputTokens
		}
		out.MaxOutputTokens = &v
	}

	if len(req.Tools) > 0 {
		out.Tools = convertAnthropicToolsToResponses(req.Tools)
	}

	// An explicit thinking disable takes precedence over output_config.effort.
	effort := anthropicReasoningEffort(req)
	if (openai.IsGPT61SolModelSpelling(req.Model) || openai.IsGPT61TerraModelSpelling(req.Model)) && req.OutputConfig != nil && req.OutputConfig.Effort == "max" && effort != "none" {
		effort = "max"
	}
	out.Reasoning = &ResponsesReasoning{
		Effort: effort,
	}
	if effort != "none" {
		out.Reasoning.Summary = "auto"
	}

	// Convert tool_choice
	if len(req.ToolChoice) > 0 {
		tc, err := convertAnthropicToolChoiceToResponses(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("convert tool_choice: %w", err)
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// convertAnthropicToolChoiceToResponses maps Anthropic tool_choice to Responses format.
//
//	{"type":"auto"}            → "auto"
//	{"type":"any"}             → "required"
//	{"type":"none"}            → "none"
//	{"type":"tool","name":"X"} → {"type":"function","name":"X"}
func convertAnthropicToolChoiceToResponses(raw json.RawMessage) (json.RawMessage, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(map[string]any{
			"type": "function",
			"name": tc.Name,
		})
	default:
		// Pass through unknown types as-is
		return raw, nil
	}
}

// anthropicParallelToolCalls preserves the client's serial-tool constraint.
// Anthropic carries it inside tool_choice rather than as a request field.
// Absence retains the bridge's existing parallel-by-default behavior.
func anthropicParallelToolCalls(raw json.RawMessage) bool {
	var choice struct {
		DisableParallelToolUse bool `json:"disable_parallel_tool_use"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &choice) != nil {
		return true
	}
	return !choice.DisableParallelToolUse
}

// convertAnthropicToResponsesInput builds the Responses API input items array
// from the Anthropic system field and message list.
func convertAnthropicToResponsesInput(system json.RawMessage, msgs []AnthropicMessage) ([]ResponsesInputItem, error) {
	return convertAnthropicToResponsesInputWithHistory(system, msgs, "")
}

func convertAnthropicToResponsesInputWithHistory(system json.RawMessage, msgs []AnthropicMessage, scope string) ([]ResponsesInputItem, error) {
	var out []ResponsesInputItem
	var historyCalls map[string]struct{ id, outputType string }

	// System prompt → developer role input item. ChatGPT Codex SSE behaves like
	// Codex CLI here: keeping Anthropic system text in input preserves the
	// conversation/cache shape better than moving it into instructions.
	if len(system) > 0 {
		sysParts, err := parseAnthropicSystemContentParts(system)
		if err != nil {
			return nil, err
		}
		if len(sysParts) > 0 {
			content, _ := json.Marshal(sysParts)
			out = append(out, ResponsesInputItem{
				Type:    "message",
				Role:    "developer",
				Content: content,
			})
		}
	}

	for _, m := range msgs {
		if m.Role == "assistant" {
			var blocks []AnthropicContentBlock
			if json.Unmarshal(m.Content, &blocks) == nil {
				items, restored, err := restoreResponsesHistory(blocks, scope)
				if err != nil {
					return nil, err
				}
				if restored {
					for _, item := range items {
						if item.Type != "function_call" && item.Type != "custom_tool_call" {
							continue
						}
						if historyCalls == nil {
							historyCalls = make(map[string]struct{ id, outputType string })
						}
						// Messages exposes legacy IDs without their old prefix and
						// represents custom calls as tool_use. Restore each result's
						// original ID and output kind along with its validated call.
						historyCalls[fromResponsesCallID(item.CallID)] = struct{ id, outputType string }{item.CallID, item.Type + "_output"}
					}
					out = append(out, items...)
					continue
				}
			}
		}
		items, err := anthropicMsgToResponsesItems(m)
		if err != nil {
			return nil, err
		}
		for index := range items {
			item := &items[index]
			switch item.Type {
			case "function_call", "custom_tool_call":
				delete(historyCalls, item.CallID)
			case "function_call_output":
				if original, ok := historyCalls[item.CallID]; ok {
					delete(historyCalls, item.CallID)
					item.CallID, item.Type = original.id, original.outputType
				}
			}
		}
		out = append(out, items...)
	}
	return out, nil
}

// parseAnthropicSystemContentParts handles the Anthropic system field which can
// be a plain string or an array of text blocks. Claude Code may include an
// x-anthropic-billing-header block; airgate drops it before sending to Codex.
func parseAnthropicSystemContentParts(raw json.RawMessage) ([]ResponsesContentPart, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if isAnthropicBillingHeaderText(s) || s == "" {
			return nil, nil
		}
		return []ResponsesContentPart{{Type: "input_text", Text: s}}, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var parts []ResponsesContentPart
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" && !isAnthropicBillingHeaderText(b.Text) {
			parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
		}
	}
	return parts, nil
}

func isAnthropicBillingHeaderText(text string) bool {
	return strings.HasPrefix(text, "x-anthropic-billing-header: ")
}

// anthropicMsgToResponsesItems converts a single Anthropic message into one
// or more Responses API input items.
func anthropicMsgToResponsesItems(m AnthropicMessage) ([]ResponsesInputItem, error) {
	switch m.Role {
	case "user":
		return anthropicUserToResponses(m.Content)
	case "assistant":
		return anthropicAssistantToResponses(m.Content)
	case "system":
		// Mid-conversation system turns (e.g. hook output) keep system-level
		// authority: Responses uses developer for them, not user.
		items, err := anthropicUserToResponses(m.Content)
		for i := range items {
			if items[i].Role == "user" {
				items[i].Role = "developer"
			}
		}
		return items, err
	default:
		return anthropicUserToResponses(m.Content)
	}
}

// anthropicUserToResponses handles an Anthropic user message. Content can be a
// plain string or an array of blocks. tool_result blocks are extracted into
// function_call_output items. Image blocks are converted to input_image parts.
func anthropicUserToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parts := []ResponsesContentPart{{Type: "input_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Type: "message", Role: "user", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var out []ResponsesInputItem
	var toolResultImageParts []ResponsesContentPart

	// Extract tool_result blocks → function_call_output items.
	// Images inside tool_results are extracted separately because the
	// Responses API function_call_output.output only accepts strings.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		outputText, imageParts := convertToolResultOutput(b)
		out = append(out, ResponsesInputItem{
			Type:   "function_call_output",
			CallID: toResponsesCallID(b.ToolUseID),
			Output: outputText,
		})
		toolResultImageParts = append(toolResultImageParts, imageParts...)
	}

	// Remaining text + image blocks → user message with content parts.
	// Also include images extracted from tool_results so the model can see them.
	var parts []ResponsesContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		case "document":
			part, err := anthropicDocumentToResponses(b)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
	}
	parts = append(parts, toolResultImageParts...)

	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		out = append(out, ResponsesInputItem{Type: "message", Role: "user", Content: content})
	}

	return out, nil
}

// anthropicAssistantToResponses handles an Anthropic assistant message.
// Text content → assistant message with output_text parts.
// tool_use blocks → function_call items.
// thinking blocks with signature → reasoning items (encrypted_content) so
// multi-turn Grok/Codex prompt cache can reuse prior reasoning prefixes.
// thinking without signature remains ignored (not accepted as plain text input).
func anthropicAssistantToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parts := []ResponsesContentPart{{Type: "output_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Type: "message", Role: "assistant", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var items []ResponsesInputItem

	// Preserve turn order: reasoning → assistant text → tool calls. xAI/Codex
	// multi-turn cache and tool continuations expect reasoning before the
	// assistant message that followed it.
	for _, b := range blocks {
		if b.Type != "thinking" {
			continue
		}
		sig := strings.TrimSpace(b.Signature)
		// Only replay provider ciphertext. Skip GPT/Codex-style gAAAA blobs and
		// empty placeholders — xAI returns 400 on decrypt for foreign signatures.
		if sig == "" || strings.HasPrefix(sig, "gAAAA") {
			continue
		}
		// CC Switch carries a JSON reasoning item in this versioned envelope,
		// not provider ciphertext. Ignore the foreign state when resuming directly.
		if strings.HasPrefix(sig, "ccswitch-openai-reasoning-v1:") {
			continue
		}
		items = append(items, ResponsesInputItem{
			Type:             "reasoning",
			EncryptedContent: sig,
		})
	}

	// Text content → assistant message with output_text content parts.
	text := extractAnthropicTextFromBlocks(blocks)
	if text != "" {
		parts := []ResponsesContentPart{{Type: "output_text", Text: text}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		items = append(items, ResponsesInputItem{Type: "message", Role: "assistant", Content: partsJSON})
	}

	// tool_use → function_call items.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 {
			args = string(b.Input)
		}
		fcID := toResponsesCallID(b.ID)
		items = append(items, ResponsesInputItem{
			Type:      "function_call",
			CallID:    fcID,
			Name:      b.Name,
			Arguments: args,
		})
	}

	return items, nil
}

// toResponsesCallID preserves Anthropic tool IDs as Responses call_id values.
// Claude Code sends tool_result.tool_use_id back verbatim, and ChatGPT Codex
// continuation expects that call_id to match the original tool_use id.
func toResponsesCallID(id string) string {
	return id
}

// fromResponsesCallID reverses old prefixed IDs while preserving current IDs.
func fromResponsesCallID(id string) string {
	if after, ok := strings.CutPrefix(id, "fc_"); ok {
		// Only strip if the remainder doesn't look like it was already "fc_" prefixed.
		// E.g. "fc_toolu_xxx" → "toolu_xxx", "fc_call_xxx" → "call_xxx"
		if strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_") {
			return after
		}
	}
	return id
}

// anthropicImageToDataURI converts an AnthropicImageSource to a data URI string.
// Returns "" if the source is nil or has no data.
func anthropicImageToDataURI(src *AnthropicImageSource) string {
	if src != nil && src.Type == "url" && isHTTPMediaURL(src.URL) {
		return src.URL
	}
	if src == nil || src.Data == "" {
		return ""
	}
	mediaType := src.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + src.Data
}

// convertToolResultOutput extracts text and image content from a tool_result
// block. Returns the text as a string for the function_call_output Output
// field, plus any image parts that must be sent in a separate user message
// (the Responses API output field only accepts strings).
func convertToolResultOutput(b AnthropicContentBlock) (text string, images []ResponsesContentPart) {
	// OpenAI function results have no is_error field. Preserve the execution
	// status as result data, without changing successful tool results or images.
	defer func() {
		if b.IsError {
			text = encodeToolResultError(text)
		}
	}()
	if len(b.Content) == 0 {
		return "(empty)", nil
	}

	// Try plain string content.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		if s == "" {
			s = "(empty)"
		}
		return s, nil
	}

	// Array of content blocks — may contain text and/or images.
	var inner []AnthropicContentBlock
	if err := json.Unmarshal(b.Content, &inner); err != nil {
		return "(empty)", nil
	}

	// Separate text (for function_call_output) from images (for user message).
	var textParts []string
	var imageParts []ResponsesContentPart
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			if ib.Text != "" {
				textParts = append(textParts, ib.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(ib.Source); uri != "" {
				imageParts = append(imageParts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		case "document":
			// Request validation already rejected unsupported document sources.
			if part, err := anthropicDocumentToResponses(ib); err == nil {
				imageParts = append(imageParts, part)
			}
		}
	}

	text = strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageParts
}

func encodeToolResultError(content string) string {
	// Conversion chains can encounter an already encoded error. Only our exact
	// two-field envelope is recognized; arbitrary JSON tool output stays intact.
	var existing map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &existing) == nil && len(existing) == 2 {
		var isError bool
		var message string
		contentJSON := strings.TrimSpace(string(existing["content"]))
		if json.Unmarshal(existing["is_error"], &isError) == nil && isError &&
			strings.HasPrefix(contentJSON, `"`) &&
			json.Unmarshal(existing["content"], &message) == nil {
			return content
		}
	}
	encoded, _ := json.Marshal(struct {
		IsError bool   `json:"is_error"`
		Content string `json:"content"`
	}{IsError: true, Content: content})
	return string(encoded)
}

// extractAnthropicTextFromBlocks joins all text blocks, ignoring thinking/
// tool_use/tool_result blocks.
func extractAnthropicTextFromBlocks(blocks []AnthropicContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// anthropicReasoningEffort resolves the Anthropic request preference for both
// OpenAI bridges. Explicitly disabled thinking overrides output_config.effort;
// otherwise the bridge keeps its medium default.
func anthropicReasoningEffort(req *AnthropicRequest) string {
	if req.Thinking != nil && req.Thinking.Type == "disabled" {
		return "none"
	}
	effort := "medium"
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		effort = req.OutputConfig.Effort
	}
	return mapAnthropicEffortToResponses(effort)
}

// mapAnthropicEffortToResponses maps shared effort levels directly and maps
// Anthropic's max to OpenAI's xhigh.
func mapAnthropicEffortToResponses(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort // low→low, medium→medium, high→high, unknown→passthrough
}

// convertAnthropicToolsToResponses maps Anthropic tool definitions to
// Responses API tools. Server-side tools like web_search are mapped to their
// OpenAI equivalents; regular tools become function tools.
func convertAnthropicToolsToResponses(tools []AnthropicTool) []ResponsesTool {
	var out []ResponsesTool
	for _, t := range tools {
		// Anthropic server tools like "web_search_20250305" → OpenAI {"type":"web_search"}
		if strings.HasPrefix(t.Type, "web_search") {
			out = append(out, ResponsesTool{Type: "web_search"})
			continue
		}
		out = append(out, ResponsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  normalizeToolParameters(t.InputSchema),
			Strict:      boolPtr(false),
		})
	}
	return out
}

func boolPtr(v bool) *bool {
	return &v
}

// isReasoningModel reports whether model is a reasoning model that does not
// support sampling parameters (temperature, top_p) via the Responses API.
// GPT-5 and every later generation are reasoning-only; the Responses API
// returns "Unsupported parameter: temperature" if these fields are present.
//
// Keyed on the generation number instead of a "gpt-5" prefix: pinning the
// prefix meant each new family (gpt-6-astra and whatever follows) silently
// fell through to the sampling branch and failed upstream on every compat
// request until someone edited this line.
func isReasoningModel(model string) bool {
	major, ok := openAIModelGeneration(model)
	return (ok && major >= 5) || openai.IsGPT6SolOrLunaModelSpelling(model)
}

// openAIModelGeneration extracts N from a "gpt-N[.M][-suffix]" model id.
// ok is false for non-GPT ids and for GPT families that carry no numeric
// generation (gpt-image-1, gpt-audio, ...).
func openAIModelGeneration(model string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-")
	if !ok {
		return 0, false
	}
	major, digits := 0, 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		major = major*10 + int(r-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	return major, true
}

// normalizeToolParameters ensures the tool parameter schema is valid for
// OpenAI's Responses API, which requires "properties" on object schemas.
//
//   - nil/empty → {"type":"object","properties":{}}
//   - type=object without properties → adds "properties": {}
//   - otherwise → returned unchanged
func normalizeToolParameters(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(schema, &m); err != nil {
		return schema
	}

	typ := m["type"]
	if string(typ) != `"object"` {
		return schema
	}

	if _, ok := m["properties"]; ok {
		return schema
	}

	m["properties"] = json.RawMessage(`{}`)
	out, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return out
}
