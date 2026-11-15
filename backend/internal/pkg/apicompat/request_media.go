package apicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// UnsupportedRequestConversionError is a client request incompatibility, not an
// upstream failure. Gateways must return 400 before making an upstream request.
type UnsupportedRequestConversionError struct{ Message string }

func (e *UnsupportedRequestConversionError) Error() string { return e.Message }

// IsUnsupportedRequestConversion recognizes a 400 conversion failure through wrappers.
func IsUnsupportedRequestConversion(err error) bool {
	var unsupported *UnsupportedRequestConversionError
	return errors.As(err, &unsupported)
}

func unsupportedRequestConversion(format string, args ...any) error {
	return &UnsupportedRequestConversionError{Message: fmt.Sprintf(format, args...)}
}

func isHTTPMediaURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func anthropicDocumentToResponses(block AnthropicContentBlock) (ResponsesContentPart, error) {
	part := ResponsesContentPart{Type: "input_file", Filename: block.Title}
	if block.Source == nil {
		return part, unsupportedRequestConversion("document source is required for OpenAI conversion")
	}
	source := block.Source
	switch source.Type {
	case "base64", "":
		if source.MediaType != "application/pdf" || strings.TrimSpace(source.Data) == "" {
			return part, unsupportedRequestConversion("only application/pdf base64 documents can be converted to OpenAI input_file")
		}
		part.FileData = "data:application/pdf;base64," + source.Data
		if part.Filename == "" {
			part.Filename = "document.pdf"
		}
	case "url":
		if !isHTTPMediaURL(source.URL) {
			return part, unsupportedRequestConversion("document URL must be an absolute HTTP(S) URL")
		}
		part.FileURL = source.URL
	default:
		return part, unsupportedRequestConversion("document source type %q cannot be converted to OpenAI; provide PDF file data or an HTTP(S) URL", source.Type)
	}
	return part, nil
}

func responsesMediaToChatPart(part ResponsesContentPart) (ChatContentPart, error) {
	out := ChatContentPart{PromptCacheBreakpoint: part.PromptCacheBreakpoint}
	switch part.Type {
	case "input_image", "image_url":
		if part.ImageURL == "" {
			return out, unsupportedRequestConversion("input_image without image_url cannot be converted to Chat Completions; file_id image references are provider-specific")
		}
		detail := part.Detail
		if detail == "original" {
			// Chat has no original value. This is the official Agents SDK's
			// closest supported quality hint, with image bytes/URL unchanged.
			detail = "high"
		}
		out.Type, out.ImageURL = "image_url", &ChatImageURL{URL: part.ImageURL, Detail: detail}
	case "input_file":
		if part.FileData == "" && part.FileID == "" {
			return out, unsupportedRequestConversion("input_file requires file_data or file_id for Chat Completions; file_url is not supported by this target protocol")
		}
		out.Type = "file"
		out.File = &ChatFile{Filename: part.Filename, FileData: part.FileData}
		if out.File.FileData == "" {
			out.File.FileID = part.FileID
		}
	default:
		return out, unsupportedRequestConversion("content type %q has no Chat Completions media conversion", part.Type)
	}
	return out, nil
}

func responsesMediaToAnthropicBlock(part ResponsesContentPart) (AnthropicContentBlock, error) {
	switch part.Type {
	case "input_image":
		source := dataURIToAnthropicImageSource(part.ImageURL)
		if source == nil && isHTTPMediaURL(part.ImageURL) {
			source = &AnthropicImageSource{Type: "url", URL: part.ImageURL}
		}
		if source == nil {
			return AnthropicContentBlock{}, unsupportedRequestConversion("input_image requires a data URI or HTTP(S) image_url for Messages; OpenAI file_id image references cannot be used by another provider")
		}
		return AnthropicContentBlock{Type: "image", Source: source}, nil
	case "input_file":
		if part.FileData != "" {
			source := dataURIToAnthropicFileSource(part.FileData)
			if source == nil || source.MediaType != "application/pdf" || strings.TrimSpace(source.Data) == "" {
				return AnthropicContentBlock{}, unsupportedRequestConversion("input_file requires application/pdf data URI content for Messages")
			}
			return AnthropicContentBlock{Type: "document", Source: source, Title: part.Filename}, nil
		}
		if isHTTPMediaURL(part.FileURL) {
			return AnthropicContentBlock{Type: "document", Source: &AnthropicImageSource{Type: "url", URL: part.FileURL}, Title: part.Filename}, nil
		}
		return AnthropicContentBlock{}, unsupportedRequestConversion("input_file requires PDF file_data or an HTTP(S) file_url for Messages; OpenAI file_id references cannot be used by another provider")
	case "video_url", "input_video", "video", "input_audio", "audio":
		return AnthropicContentBlock{}, unsupportedRequestConversion("content type %q cannot be converted to Messages; this target protocol has no equivalent media input", part.Type)
	}
	return AnthropicContentBlock{}, unsupportedRequestConversion("content type %q has no Messages media conversion", part.Type)
}

// Validate nested tool_result documents too: moving media into a follow-up user
// message must never silently omit an unsupported document or video.
func validateAnthropicRequestMedia(req *AnthropicRequest, target string) error {
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			var blocks []AnthropicContentBlock
			_ = json.Unmarshal(message.Content, &blocks)
			for _, block := range blocks {
				if block.Type == "document" || block.Type == "image" {
					return unsupportedRequestConversion("Messages assistant media type %q cannot be converted to %s without losing content", block.Type, target)
				}
			}
		}
		if err := validateAnthropicMediaContent(message.Content, target); err != nil {
			return err
		}
	}
	return nil
}

func validateChatRequestMedia(req *ChatCompletionsRequest) error {
	for _, message := range req.Messages {
		var parts []ChatContentPart
		if json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			switch part.Type {
			case "video_url", "video", "input_video":
				return unsupportedRequestConversion("Chat content type %q cannot be converted to Responses or Messages; use a native media-capable endpoint", part.Type)
			case "audio", "input_audio":
				return unsupportedRequestConversion("Chat audio content type %q is not implemented by this conversion; use a native audio-capable endpoint", part.Type)
			case "file", "image_url":
				if message.Role == "assistant" {
					return unsupportedRequestConversion("Chat media type %q in role %q cannot be converted without losing content", part.Type, message.Role)
				}
			}
		}
	}
	return nil
}

func validateResponsesToolOutputMedia(input json.RawMessage) error {
	var items []map[string]json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return nil
	}
	for _, item := range items {
		if rawString(item["type"]) != "function_call_output" && rawString(item["type"]) != "custom_tool_call_output" {
			continue
		}
		value, ok := decodeToolOutputJSON(item["output"])
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			if nested, valid := decodeToolOutputJSON([]byte(text)); valid {
				value = nested
			}
		}
		if err := validateToolOutputMediaValue(value); err != nil {
			return err
		}
	}
	return nil
}

func validateToolOutputMediaValue(value any) error {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if err := validateToolOutputMediaValue(item); err != nil {
				return err
			}
		}
	case map[string]any:
		switch value["type"] {
		case "input_file":
			fileData, _ := value["file_data"].(string)
			fileID, _ := value["file_id"].(string)
			if _, err := responsesMediaToChatPart(ResponsesContentPart{Type: "input_file", FileData: fileData, FileID: fileID}); err != nil {
				return err
			}
		case "input_image":
			if _, ok := recognizedToolOutputImageURL(value); !ok {
				return unsupportedRequestConversion("tool output input_image requires image_url for Chat conversion; file_id image references are not supported by this target")
			}
		case "video_url", "input_video", "video", "input_audio", "audio":
			return unsupportedRequestConversion("tool output media type %q has no equivalent in this Chat conversion", value["type"])
		}
		if content, ok := value["content"]; ok {
			return validateToolOutputMediaValue(content)
		}
	}
	return nil
}

func validateAnthropicMediaContent(raw json.RawMessage, target string) error {
	var blocks []AnthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil // plain text and malformed input are handled by the converter
	}
	for _, block := range blocks {
		switch block.Type {
		case "document":
			part, err := anthropicDocumentToResponses(block)
			if err != nil {
				return err
			}
			if target == "chat" {
				if _, err = responsesMediaToChatPart(part); err != nil {
					return err
				}
			}
		case "image":
			if block.Source != nil && ((block.Source.Type == "url" && !isHTTPMediaURL(block.Source.URL)) || block.Source.FileID != "" || (block.Source.Type != "" && block.Source.Type != "url" && block.Source.Type != "base64")) {
				return unsupportedRequestConversion("image source type %q cannot be converted to %s; provide image data or an HTTP(S) URL", block.Source.Type, target)
			}
		case "video", "video_url", "input_video", "audio", "input_audio":
			return unsupportedRequestConversion("Messages content type %q has no %s conversion; media was not forwarded", block.Type, target)
		case "tool_result":
			if err := validateAnthropicMediaContent(block.Content, target); err != nil {
				return err
			}
		}
	}
	return nil
}
