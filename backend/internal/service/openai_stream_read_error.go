package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/net/http2"
)

const (
	// OpenAIUpstreamHTTP2StreamErrorCode is returned to OpenAI-compatible clients
	// when an upstream HTTP/2 response stream is reset after the request started.
	OpenAIUpstreamHTTP2StreamErrorCode     = "upstream_http2_stream_error"
	OpenAIUpstreamStreamReadErrorCode      = "upstream_stream_read_error"
	openAIUpstreamStreamInterruptedMessage = "Upstream response stream was interrupted"
	// OpenAIUpstreamStreamTruncatedCode is returned when an upstream SSE stream
	// closes *cleanly* before delivering any terminal signal. A clean EOF carries
	// no transport error, so without this classification a truncated generation is
	// indistinguishable from a successful one.
	OpenAIUpstreamStreamTruncatedCode = "upstream_stream_truncated"
)

// ErrOpenAIUpstreamStreamTruncated marks an upstream SSE stream that ended at
// EOF — without a read error — before any terminal signal arrived.
var ErrOpenAIUpstreamStreamTruncated = errors.New("upstream stream ended before any terminal chunk")

type openAIUpstreamStreamReadError struct {
	cause         error
	clientCode    string
	clientMessage string
}

func (e *openAIUpstreamStreamReadError) Error() string {
	return fmt.Sprintf("stream usage incomplete: %v", e.cause)
}

func (e *openAIUpstreamStreamReadError) Unwrap() error { return e.cause }

func newOpenAIUpstreamStreamReadError(err error) error {
	code, message := classifyOpenAIUpstreamStreamReadError(err)
	return &openAIUpstreamStreamReadError{
		cause:         err,
		clientCode:    code,
		clientMessage: message,
	}
}

// shouldClassifyOpenAIUpstreamStreamReadError excludes cancellation and
// response-size enforcement from upstream retry.
func shouldClassifyOpenAIUpstreamStreamReadError(err error, contexts ...context.Context) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
		return false
	}
	for _, ctx := range contexts {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
	}
	return true
}

// OpenAIUpstreamStreamReadErrorDetails returns the stable, sanitized client
// classification attached to an upstream stream read failure.
func OpenAIUpstreamStreamReadErrorDetails(err error) (code, message string, ok bool) {
	var streamErr *openAIUpstreamStreamReadError
	if !errors.As(err, &streamErr) || streamErr == nil {
		return "", "", false
	}
	return streamErr.clientCode, streamErr.clientMessage, true
}

func classifyOpenAIUpstreamStreamReadError(err error) (code, message string) {
	if reason, ok := openAIHTTP2PeerResetReason(err); ok {
		return OpenAIUpstreamHTTP2StreamErrorCode, openAIHTTP2PeerResetMessage(reason)
	}
	if err != nil {
		if errors.Is(err, ErrOpenAIUpstreamStreamTruncated) {
			return OpenAIUpstreamStreamTruncatedCode, "Upstream response stream ended before completion"
		}
		lower := strings.ToLower(err.Error())
		// net/http's HTTP/2 stream error is unexported. Its stable text contains
		// "stream error: stream ID ..."; match only the transport signature and
		// never pass the original text to the client.
		if strings.Contains(lower, "stream error: stream id ") ||
			(strings.Contains(lower, "http2:") && strings.Contains(lower, "stream")) {
			return OpenAIUpstreamHTTP2StreamErrorCode, "Upstream HTTP/2 stream failed"
		}
	}
	return OpenAIUpstreamStreamReadErrorCode, openAIUpstreamStreamInterruptedMessage
}

// net/http uses an unexported copy of http2.StreamError. Match its complete
// peer-reset signature, not an arbitrary mention of HTTP/2 in an error body.
var openAIHTTP2PeerResetPattern = regexp.MustCompile(`(?:^|: )stream error: stream ID [0-9]+; ([^;\r\n]+); received from peer$`)

func safeHTTP2ResetReason(reason string) string {
	switch reason {
	case "NO_ERROR", "PROTOCOL_ERROR", "INTERNAL_ERROR", "FLOW_CONTROL_ERROR", "SETTINGS_TIMEOUT", "STREAM_CLOSED", "FRAME_SIZE_ERROR", "REFUSED_STREAM", "CANCEL", "COMPRESSION_ERROR", "CONNECT_ERROR", "ENHANCE_YOUR_CALM", "INADEQUATE_SECURITY", "HTTP_1_1_REQUIRED":
		return reason
	default:
		return ""
	}
}

func openAIHTTP2PeerResetReason(err error) (string, bool) {
	if !shouldClassifyOpenAIUpstreamStreamReadError(err) {
		return "", false
	}
	// Semantic HTTP/SSE errors may contain arbitrary upstream text, including
	// an exact transport signature. They cannot create trusted reset evidence.
	var imageErr *OpenAIImagesUpstreamError
	var httpErr *UpstreamFailoverError
	if errors.As(err, &imageErr) || errors.As(err, &httpErr) {
		return "", false
	}
	var streamErr http2.StreamError
	if errors.As(err, &streamErr) && streamErr.Cause != nil && streamErr.Cause.Error() == "received from peer" {
		return safeHTTP2ResetReason(streamErr.Code.String()), true
	}
	match := openAIHTTP2PeerResetPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return "", false
	}
	return safeHTTP2ResetReason(match[1]), true
}

func openAIHTTP2PeerResetMessage(reason string) string {
	protocol := "HTTP/2"
	if reason = safeHTTP2ResetReason(reason); reason != "" {
		protocol += " " + reason
	}
	return "Upstream connection reset by peer (" + protocol + "). Content may be incomplete. Do not automatically retry the same request."
}

// OpsTransportFailure is trusted local evidence, never inferred from SSE text.
// Keeping it separate from HTTP status preserves transport attribution when a
// pool uses the effective 502 status for its request-local retry policy.
type OpsTransportFailure struct {
	Protocol    string `json:"protocol,omitempty"`
	ResetReason string `json:"reset_reason,omitempty"`
}

func (f *OpsTransportFailure) errorCode() string {
	if f.Protocol == "TCP" {
		return OpenAIUpstreamStreamReadErrorCode
	}
	return OpenAIUpstreamHTTP2StreamErrorCode
}

func (f *OpsTransportFailure) errorMessage() string {
	if f.Protocol == "TCP" {
		return openAIUpstreamStreamInterruptedMessage
	}
	return openAIHTTP2PeerResetMessage(f.ResetReason)
}

func (f *OpsTransportFailure) ErrorFields() map[string]any {
	protocol, reason := "HTTP/2", safeHTTP2ResetReason(f.ResetReason)
	if f.Protocol == "TCP" {
		protocol, reason = "TCP", "CONNECTION_RESET"
	}
	return map[string]any{"type": "upstream_error", "code": f.errorCode(),
		"category": "transport_error", "protocol": protocol, "reset_reason": reason,
		"message": f.errorMessage()}
}

func openAITCPReadReset(err error) bool {
	if !shouldClassifyOpenAIUpstreamStreamReadError(err) {
		return false
	}
	var semantic *UpstreamFailoverError
	var image *OpenAIImagesUpstreamError
	if errors.As(err, &semantic) || errors.As(err, &image) {
		return false
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "read" && strings.HasPrefix(op.Net, "tcp") && isOpenAITCPResetError(err)
}

func openAIStreamTransportRetryAllowed(c *gin.Context, account *Account, websocket bool) bool {
	if websocket || c == nil || c.Request == nil || c.Request.Context().Err() != nil ||
		GetOpenAIClientTransport(c) == OpenAIClientTransportWS || account == nil ||
		account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		!openAIStreamConfiguredRetryEnabled(account) || !account.IsPoolModeRetryableStatus(http.StatusBadGateway) ||
		IsResponseCommitted(c) || OpenAIImageGenerationIntentFromContext(c.Request.Context()) {
		return false
	}
	return !strings.Contains(strings.TrimRight(c.Request.URL.Path, "/"), "/images/")
}

func openAIStreamTransportResetFailure(c *gin.Context, account *Account, cause error, websocket bool) *OpsTransportFailure {
	if reason, ok := openAIHTTP2PeerResetReason(cause); ok {
		return &OpsTransportFailure{ResetReason: reason}
	}
	// A verified TCP read reset gains only the explicitly configured pool retry
	// path. Opt-outs and other protocols retain their existing error handling.
	if openAITCPReadReset(cause) && openAIStreamTransportRetryAllowed(c, account, websocket) {
		return &OpsTransportFailure{Protocol: "TCP", ResetReason: "CONNECTION_RESET"}
	}
	return nil
}

// handleOpenAIStreamTransportReset applies configured pool retries at the read boundary.
// The caller must discard any incomplete staged event before invoking it.
// Other accounts and transports retain the terminal, non-replay behavior.
func handleOpenAIStreamTransportReset(c *gin.Context, account *Account, requestID string, cause error, writeWSFrame ...func([]byte) error) (error, bool) {
	if c == nil || c.Request == nil || !shouldClassifyOpenAIUpstreamStreamReadError(cause, c.Request.Context()) {
		return nil, false
	}
	failure := openAIStreamTransportResetFailure(c, account, cause, len(writeWSFrame) > 0)
	if failure == nil {
		return nil, false
	}
	classified := newOpenAIUpstreamStreamReadError(cause)
	if marked, exists := GetOpsStreamError(c); exists && marked.TransportFailure != nil && marked.Turn == c.GetInt(OpsStreamTurnKey) {
		return classified, true
	}
	StopOpenAICompactSSEKeepaliveCommitted(c)
	StopOpenAIImagesJSONKeepaliveCommitted(c)
	message := failure.errorMessage()
	c.Set(OpsUpstreamStatusCodeKey, 0)
	c.Set(OpsUpstreamErrorMessageKey, message)
	c.Set(OpsUpstreamErrorDetailKey, "")
	event := OpsUpstreamErrorEvent{Kind: "transport_error", Stage: "response_stream", Reason: failure.ResetReason,
		Message: message, UpstreamRequestID: requestID, ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account)}
	if account != nil {
		event.Platform, event.AccountID, event.AccountName = account.Platform, account.ID, account.Name
	}
	appendOpsUpstreamError(c, event)
	if openAIStreamTransportRetryAllowed(c, account, len(writeWSFrame) > 0) {
		return &UpstreamFailoverError{
			StatusCode: http.StatusBadGateway, RetryableOnSameAccount: true,
			ConfiguredStreamRetry: true, RetryAfterOutput: c.Writer.Written(),
			RequestScopedTransient: true, NextAccountAction: NextAccountStop,
			TransportFailure: failure,
		}, true
	}
	writeOpenAIStreamTransportFailure(c, failure, writeWSFrame...)
	return classified, true
}

// WriteOpenAIStreamTransportFailure terminates a configured pool retry only once
// its handler budget is exhausted, preserving the original transport evidence.
func WriteOpenAIStreamTransportFailure(c *gin.Context, failoverErr *UpstreamFailoverError) bool {
	if c == nil || c.Request == nil || failoverErr == nil || failoverErr.TransportFailure == nil {
		return false
	}
	if marked, exists := GetOpsStreamError(c); !exists || marked.TransportFailure == nil || marked.Turn != c.GetInt(OpsStreamTurnKey) {
		writeOpenAIStreamTransportFailure(c, failoverErr.TransportFailure)
	}
	return true
}

func writeOpenAIStreamTransportFailure(c *gin.Context, failure *OpsTransportFailure, writeWSFrame ...func([]byte) error) {
	message := failure.errorMessage()
	c.Set(OpsUpstreamStatusCodeKey, 0)
	c.Set(OpsUpstreamErrorMessageKey, message)
	c.Set(OpsUpstreamErrorDetailKey, "")
	MarkOpsStreamErrorValue(c, OpsStreamError{TransportFailure: failure, ErrType: "upstream_error",
		Code: failure.errorCode(), Message: message, IntendedStatus: http.StatusBadGateway, CountTowardsSLA: true,
		NonStream: len(writeWSFrame) == 0 && !c.GetBool("ops_stream") && !strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream")})
	fields := failure.ErrorFields()
	failedResponse := func() map[string]any {
		// Match buildOpenAIResponseFailedSSE's strict-client field contract.
		return map[string]any{"type": "response.failed", "sequence_number": 0, "response": map[string]any{
			"id": "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "object": "response",
			"created_at": time.Now().Unix(), "model": c.GetString("ops_model"), "output": []any{},
			"status": "failed", "error": fields}}
	}
	// HTTP bridge WebSocket callers own their frame writer. A reset still ends
	// the submitted turn, including before its first visible output.
	if len(writeWSFrame) > 0 {
		data, _ := json.Marshal(failedResponse())
		if err := writeWSFrame[0](data); err != nil {
			_ = c.Error(err)
		}
		MarkResponseCommitted(c)
		return
	}
	if GetOpenAIClientTransport(c) == OpenAIClientTransportWS {
		return
	}
	if c.Writer.Written() && strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
		eventType := "error"
		payload := map[string]any{"error": fields}
		path := strings.TrimRight(c.Request.URL.Path, "/")
		if strings.HasSuffix(path, "/responses") || strings.Contains(path, "/responses/") {
			eventType = "response.failed"
			payload = failedResponse()
		} else if strings.HasSuffix(path, "/messages") || strings.Contains(path, "/images/") {
			payload["type"] = "error"
		}
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(c.Writer, "\n\nevent: %s\ndata: %s\n\n", eventType, data)
		c.Writer.Flush()
	} else {
		status := http.StatusBadGateway
		if c.Writer.Written() {
			status = c.Writer.Status()
		}
		c.Header("Content-Type", "application/json")
		payload := gin.H{"error": fields}
		if strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/messages") {
			payload["type"] = "error"
		}
		c.JSON(status, payload)
	}
	MarkResponseCommitted(c)
}
