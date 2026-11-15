package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ccUpstreamStreamError preserves the semantic payload separately from read
// failures; upstream text must never impersonate a trusted transport reset.
type ccUpstreamStreamError struct {
	payload   []byte
	eventType string
	message   string
}

func newCCUpstreamStreamError(payload []byte, eventType string) *ccUpstreamStreamError {
	message := extractOpenAISSEErrorMessage(payload)
	if message == "" {
		message = "Upstream Chat Completions request failed"
	}
	return &ccUpstreamStreamError{payload: append([]byte(nil), payload...), eventType: eventType, message: message}
}

func (e *ccUpstreamStreamError) Error() string { return e.message }

// Keep CC lifecycle-only preambles private until actual model output or a
// validated successful terminal. This lets a pre-output failure use the normal
// failover policy without exposing an empty assistant turn from that attempt.
type ccStreamWriteState struct {
	preamble     *openAIFirstOutputStage
	started      bool
	disconnected bool
	err          error
}

func (s *OpenAIGatewayService) newCCStreamWriteState(account *Account) *ccStreamWriteState {
	return &ccStreamWriteState{preamble: s.newOpenAIStreamOutputStage(account)}
}

func (w *ccStreamWriteState) write(c *gin.Context, headers func(), data string, release bool) {
	if w.disconnected || w.err != nil {
		return
	}
	if openAIStreamClientCanceled(c) {
		w.disconnected = true
		return
	}
	if !w.started && !release {
		_, w.err = w.preamble.WriteString(data)
		return
	}
	headers()
	if !w.started {
		if err := w.preamble.CommitTo(c.Writer); err != nil {
			w.disconnected, w.err = true, err
			return
		}
		w.started = true
	}
	if _, err := io.WriteString(c.Writer, data); err != nil {
		w.disconnected, w.err = true, err
	}
}

// Both CC bridge endpoints use the same failure verdict. Before any delivery,
// eligible upstream failures can use the existing bounded retry/failover policy.
// Once bytes have reached a client, report an endpoint-native error only: never
// finalize a tool or return a replayable account-switch error for that attempt.
func (s *OpenAIGatewayService) handleCCStreamScanError(
	c *gin.Context, account *Account, resp *http.Response, upstreamModel string,
	cause error, disconnected bool, writeHTTP compatErrorWriter,
	writeSSE func([]byte, string),
) error {
	if cause == nil {
		return nil
	}
	if openAIStreamClientCanceled(c) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("stream usage incomplete: %w", cause)
	}
	// HTTP/2 reset handling already wrote the precisely classified error. Do
	// not emit a second event or turn a submitted reset into an account retry.
	if _, _, classified := OpenAIUpstreamStreamReadErrorDetails(cause); classified {
		return cause
	}
	var semantic *ccUpstreamStreamError
	payload, message := []byte(nil), ""
	retryEligible := errors.As(cause, &semantic)
	if semantic != nil {
		payload, message = semantic.payload, semantic.message
	} else {
		code, clientMessage := classifyOpenAIUpstreamStreamReadError(cause)
		message = clientMessage
		payload, _ = json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
		retryEligible = errors.Is(cause, ErrOpenAIUpstreamStreamTruncated) || errors.Is(cause, errOpenAIFirstOutputStageLimit)
	}
	shouldFailover := openAIStreamErrorEventShouldFailover(payload, message)
	if semantic != nil && semantic.eventType == "response.failed" {
		shouldFailover = openAIStreamFailedEventShouldFailover(payload, message)
	}
	// Respect configured same-account 502 retries for server-error envelopes,
	// while retaining request/policy failures as non-retryable.
	if openAIStreamConfiguredRetryEnabled(account) && openAIStreamFailedEventRetryableOnSameAccount(account, payload, message) &&
		openAIStreamFailedEventShouldFailover(payload, message) {
		shouldFailover = true
	}
	requestID := resp.Header.Get("x-request-id")
	if retryEligible && !disconnected && !c.Writer.Written() && shouldFailover {
		return s.newOpenAIStreamFailoverErrorWithModel(c, account, false, requestID, payload, message, upstreamModel, resp.Header)
	}
	message = s.recordOpenAIStreamUpstreamError(c, account, false, requestID, "http_error", payload, message)
	if !disconnected && !openAIStreamClientCanceled(c) {
		if c.Writer.Written() {
			writeSSE(payload, message)
		} else {
			errType := "api_error"
			status := openAIStreamFailureStatus(payload, message)
			if status == http.StatusTooManyRequests {
				errType = "rate_limit_error"
			}
			writeHTTP(c, status, errType, message)
		}
	}
	return fmt.Errorf("stream usage incomplete: %w", cause)
}

func ccStreamChunkHasOutput(chunkContent, reasoning *string) bool {
	return chunkContent != nil && *chunkContent != "" || reasoning != nil && *reasoning != ""
}
