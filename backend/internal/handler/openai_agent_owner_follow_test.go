//go:build unit

package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Both synthetic accounts can consume the same fixture history. This models
// a compatible upstream without asserting real-provider ciphertext portability.
type openAIAgentOwnerFollowUpstream struct {
	service.HTTPUpstream
	fixture issue715Upstream
	ids     []int64
	auth    []string
}

func (u *openAIAgentOwnerFollowUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	u.ids = append(u.ids, accountID)
	u.auth = append(u.auth, req.Header.Get("Authorization"))
	return u.fixture.Do(req, proxy, 1, concurrency)
}

func TestOpenAIAgentCompletedHistoryFollowsCurrentRoute(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, change := range []string{"api_key", "base_url", "model_mapping", "account", "passthrough"} {
			name := change + "/json"
			if stream {
				name = change + "/sse"
			}
			t.Run(name, func(t *testing.T) {
				accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
				upstream := &openAIAgentOwnerFollowUpstream{}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
				status, first := issue715Request(t, h, issue715Body(false, stream), 99, "fixture-follow-thread", false)
				require.Equal(t, http.StatusOK, status, first)
				h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
				wantAccountID := int64(1)
				switch change {
				case "api_key":
					accounts[0].Credentials["api_key"] = "fixture-follow-rotated"
				case "base_url":
					accounts[0].Credentials["base_url"] = "https://changed.example.test"
				case "model_mapping":
					accounts[0].Credentials["model_mapping"] = map[string]any{"gpt-5.1": "fixture-mapped-model"}
				case "account":
					reset := time.Now().Add(time.Hour)
					accounts[0].RateLimitResetAt = &reset
					wantAccountID = 2
				case "passthrough":
					accounts[0].Extra = map[string]any{"openai_passthrough": true}
				}
				body := issue715Body(true, stream)
				status, answer := issue715Request(t, h, body, 99, "fixture-follow-thread", false)
				require.Equal(t, http.StatusOK, status, answer)
				require.NotContains(t, answer, "encrypted_agent_recovery_route_changed")
				require.Contains(t, answer, "ledger balance 18")
				require.Equal(t, []int64{1, wantAccountID}, upstream.ids)
				require.Len(t, upstream.fixture.bodies, 2)
				require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.fixture.bodies[1], "input").Raw,
					"account changes must preserve the complete encrypted history and tool results")
				if change == "api_key" {
					require.Equal(t, "Bearer fixture-follow-rotated", upstream.auth[1])
				}
			})
		}
	}
}

func TestOpenAIAgentFullHistorySelectsCompatibleRoute(t *testing.T) {
	for _, protocol := range []string{"chat_completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			accounts[1].Schedulable = false
			upstream := &openAIAgentOwnerFollowUpstream{}
			upstream.fixture.mode = "generated"
			cache := &openAIConfiguredStatusRetryCache{}
			h := newOpenAIResponsesFailoverTestHandlerWithCache(t, upstream, cache, accounts...)
			seed := `{"model":"gpt-5.1","input":"Keep ledger balance 11","stream":false}`
			status, answer := issue715Request(t, h, seed, 99, "fixture-compatible-thread", false)
			require.Equal(t, http.StatusOK, status, answer)
			require.NotEmpty(t, cache.sessions, "seed the actual sticky account before its route changes")
			upstream.fixture.mode = ""
			accounts[1].Schedulable = true
			if protocol == "chat_completions" {
				accounts[0].Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
			} else {
				accounts[0].Extra = map[string]any{"openai_passthrough": true}
			}
			h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
			body := issue715Body(true, false)
			status, answer = issue715Request(t, h, body, 99, "fixture-compatible-thread", false)
			require.Equal(t, http.StatusOK, status, answer)
			wantAccountID := int64(2)
			if protocol == "responses" {
				wantAccountID = 1
			}
			require.Equal(t, []int64{1, wantAccountID}, upstream.ids)
			require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.fixture.bodies[1], "input").Raw)
		})
	}
}

func TestOpenAIAgentFullHistoryCompactionFollowsCurrentRoute(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			upstream := &openAIAgentOwnerFollowUpstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-compaction-follow-thread", false)
			require.Equal(t, http.StatusOK, status, answer)
			h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
			accounts[0].Credentials["api_key"] = "fixture-compaction-rotated"
			accounts[0].Extra = map[string]any{"openai_passthrough": passthrough}
			body := issue715CompactionBody()
			status, answer = issue715Request(t, h, body, 99, "fixture-compaction-follow-thread", false)
			require.Equal(t, http.StatusOK, status, answer)
			require.Contains(t, answer, "fixture-compacted-reference")
			require.Equal(t, []int64{1, 1}, upstream.ids)
			require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.fixture.bodies[1], "input").Raw)
		})
	}
}

func TestOpenAIAgentOwnerFollowingKeepsOrdinaryPoolRetries(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, statusCode := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
					t.Run(fmt.Sprintf("enabled=%t/passthrough=%t/stream=%t/status=%d", enabled, passthrough, stream, statusCode), func(t *testing.T) {
						account := issue715AccountsOfType(service.AccountTypeAPIKey)[0]
						account.Credentials["pool_mode"] = true
						account.Credentials["pool_mode_retry_count"] = 1
						account.Credentials["pool_mode_retry_status_codes"] = []any{float64(statusCode)}
						account.Extra = map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true}
						payload := `{"id":"resp_plain_follow","object":"response","status":"completed","model":"gpt-5.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ordinary request recovered"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
						contentType := "application/json"
						if stream {
							payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
							contentType = "text/event-stream"
						}
						upstream := &openAIConfiguredStatusRetryUpstream{status: http.StatusOK, contentType: contentType, body: payload,
							firstResponse: &http.Response{StatusCode: statusCode, Header: http.Header{"Content-Type": {"application/json"}},
								Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"fixture retryable failure"}}`))}}
						h := newOpenAIResponsesFailoverTestHandler(t, upstream, account)
						h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = enabled
						body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"ordinary request"}`, stream)
						status, answer := issue715Request(t, h, body, 99, "fixture-ordinary-thread", false)
						require.Equal(t, http.StatusOK, status, answer)
						require.Contains(t, answer, "ordinary request recovered")
						require.Equal(t, []int64{1, 1}, upstream.accountIDs, "ordinary requests retain configured same-account retries")
						for _, sent := range upstream.requestBodies {
							require.Equal(t, "ordinary request", gjson.GetBytes(sent, "input").String())
						}
					})
				}
			}
		}
	}
}
