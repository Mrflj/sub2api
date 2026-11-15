package service

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Configured retries keep pre-answer progress private. Answer content streams
// immediately; configured terminal failures may retry even after partial output.
func openAIStreamConfiguredRetryEnabled(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI ||
		!account.IsPoolMode() || account.GetPoolModeRetryCount() == 0 {
		return false
	}
	codes := account.GetPoolModeRetryStatusCodes()
	if codes == nil {
		codes = defaultPoolModeRetryableStatusCodes
	}
	for _, code := range codes {
		if code >= http.StatusBadRequest {
			return true
		}
	}
	return false
}

// Only configured pool streams defer these events, until answer output or a
// successful terminal. Tool-only success releases the complete tool call then,
// so a failed attempt cannot cause a client to execute a discarded tool call.
func openAIStreamDefersProgressForRetry(data, eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	for _, prefix := range []string{
		"response.reasoning", "response.function_call_arguments.",
		"response.custom_tool_call_input.", "response.web_search_call.",
		"response.file_search_call.", "response.code_interpreter_call.",
		"response.code_interpreter_call_code.", "response.computer_call.",
		"response.mcp_call.", "response.mcp_call_arguments.", "response.mcp_list_tools.",
	} {
		if strings.HasPrefix(eventType, prefix) {
			return true
		}
	}
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		switch gjson.Get(data, "item.type").String() {
		case "reasoning", "compaction", "function_call", "custom_tool_call",
			"web_search_call", "file_search_call", "code_interpreter_call",
			"computer_call", "mcp_call", "mcp_list_tools", "mcp_approval_request":
			return true
		}
	case "response.content_part.added", "response.content_part.done":
		switch gjson.Get(data, "part.type").String() {
		case "reasoning_text", "summary_text":
			return true
		}
	}
	return false
}

// Keep draining detached upstream streams for usage, but never write queued
// output or schedule a replay after the downstream request has been canceled.
func openAIStreamClientCanceled(c *gin.Context) bool {
	return c != nil && c.Request != nil && c.Request.Context().Err() != nil
}

type openAIStreamRetryAccountKey struct{}

const (
	openAIMessagesOutputRetryStateKey = "openai_messages_output_retry_state"
	openAIChatOutputRetryStateKey     = "openai_chat_output_retry_state"
)

// WithOpenAIStreamRetryAccount pins retries after partial output to their owner.
// Previously delivered content cannot be withdrawn or moved to another account.
func WithOpenAIStreamRetryAccount(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, openAIStreamRetryAccountKey{}, accountID)
}

func OpenAIStreamRetryAccountID(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	id, _ := ctx.Value(openAIStreamRetryAccountKey{}).(int64)
	return id
}

func openAIStreamAllowsRetryAfterOutput(c *gin.Context, account *Account, payload []byte, message string, disconnected bool) bool {
	if disconnected || c == nil || c.Request == nil || c.Request.Context().Err() != nil ||
		OpenAIImageGenerationIntentFromContext(c.Request.Context()) ||
		!openAIStreamConfiguredRetryEnabled(account) || account.Type != AccountTypeAPIKey || IsResponseCommitted(c) ||
		!account.IsPoolModeRetryableStatus(openAIStreamFailedEventSemanticStatus(payload, message)) {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) openAIStreamRetryAfterOutputError(c *gin.Context, account *Account, passthrough bool, requestID string, payload []byte, message, model string, headers http.Header, disconnected bool) *UpstreamFailoverError {
	if !openAIStreamAllowsRetryAfterOutput(c, account, payload, message, disconnected) {
		return nil
	}
	err := s.newOpenAIStreamFailoverErrorWithModel(c, account, passthrough, requestID, payload, message, model, headers)
	err.StatusCode = openAIStreamFailedEventSemanticStatus(payload, message)
	err.RetryAfterOutput = true
	err.NextAccountAction = NextAccountStop
	return err
}

func (s *OpenAIGatewayService) openAIStreamRetryLimits() (int, int64) {
	line := config.DefaultOpenAIStreamRetryMaxLineBytes
	buffer := int64(config.DefaultOpenAIStreamRetryMaxBufferBytes)
	if s != nil && s.cfg != nil {
		if n := s.cfg.Gateway.OpenAIStreamRetryMaxLineBytes; n > 0 {
			line = n
		}
		if n := s.cfg.Gateway.OpenAIStreamRetryMaxBufferBytes; n > 0 {
			buffer = n
		}
	}
	return line, buffer
}

func (s *OpenAIGatewayService) newOpenAIStreamOutputStage(account *Account) *openAIFirstOutputStage {
	if !openAIStreamConfiguredRetryEnabled(account) {
		return newDefaultOpenAIFirstOutputStage()
	}
	_, limit := s.openAIStreamRetryLimits()
	return newOpenAIFirstOutputStage(limit)
}

func (s *OpenAIGatewayService) openAIRetryLineLimitError(c *gin.Context, account *Account, requestID string, err error, disconnected, outputStarted bool) *UpstreamFailoverError {
	if !errors.Is(err, bufio.ErrTooLong) || !openAIStreamConfiguredRetryEnabled(account) || disconnected || c == nil || c.Request == nil || c.Request.Context().Err() != nil || openAIStreamClientOutputStarted(c, outputStarted) {
		return nil
	}
	return s.newOpenAIStreamFailoverError(c, account, false, requestID, nil, "OpenAI SSE line exceeds configured maximum before output")
}
