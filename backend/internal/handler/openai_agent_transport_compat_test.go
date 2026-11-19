//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOpenAIAgentFullHistoryCompatibleTransport(t *testing.T) {
	for _, kind := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
		for _, stream := range []bool{false, true} {
			for _, change := range []string{"namespace", "system", "system_json", "namespace_system", "opaque_tool_result", "tool_alias", "flatten_namespaces"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", kind, stream, change), func(t *testing.T) {
					accounts := issue715AccountsOfType(kind)
					accounts = accounts[:1]
					upstream := &openAIAgentOwnerFollowUpstream{}
					h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
					h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
					status, answer := issue715Request(t, h, issue715Body(false, stream), 99, "fixture-transport-thread", false)
					require.Equal(t, http.StatusOK, status, answer)

					body := issue715Body(true, stream)
					var err error
					if change == "namespace" || change == "namespace_system" {
						body, err = sjson.Set(body, "input.1.namespace", "fixture-message-metadata")
						require.NoError(t, err)
					}
					if change == "system" || change == "system_json" || change == "namespace_system" {
						body, err = sjson.SetRaw(body, "input.-1", `{"type":"message","role":"system","content":"Preserve the fixture ledger guidance"}`)
						require.NoError(t, err)
					}
					if change == "system_json" {
						body, err = sjson.SetRaw(body, "text.format", `{"type":"json_object"}`)
						require.NoError(t, err)
					}
					if change == "opaque_tool_result" {
						body, err = sjson.SetRaw(body, "input.-1", `{"type":"function_call_output","call_id":"fc_inside_opaque_task","output":"Keep the existing opaque task result"}`)
						require.NoError(t, err)
					}
					if change == "tool_alias" {
						body, err = sjson.Set(body, "input.2.name", "python")
						require.NoError(t, err)
						body, err = sjson.SetRaw(body, "tools", `[{"type":"function","name":"python","parameters":{"type":"object","properties":{}}}]`)
						require.NoError(t, err)
					}
					if change == "flatten_namespaces" {
						accounts[0].Extra = map[string]any{"openai_responses_flatten_namespaces": true}
						body, err = sjson.Set(body, "input.2.namespace", "fixture_ledger")
						require.NoError(t, err)
						body, err = sjson.SetRaw(body, "tools", `[{"type":"namespace","name":"fixture_ledger","tools":[{"type":"function","name":"update_ledger","parameters":{"type":"object","properties":{}}}]}]`)
						require.NoError(t, err)
					}
					body, err = sjson.SetRaw(body, "input.1.sequence", "9007199254740993")
					require.NoError(t, err)
					status, answer = issue715Request(t, h, body, 99, "fixture-transport-thread", false)
					require.Equal(t, http.StatusOK, status, answer)
					require.NotContains(t, answer, "encrypted_agent_recovery_wire_changed")
					require.Contains(t, answer, "ledger balance 18")
					require.Equal(t, []int64{accounts[0].ID, accounts[0].ID}, upstream.ids)
					require.Len(t, upstream.fixture.bodies, 2)
					sent := upstream.fixture.bodies[1]
					require.JSONEq(t, issue715Agent, gjson.GetBytes(sent, "input.0").Raw)
					require.Equal(t, "9007199254740993", gjson.GetBytes(sent, "input.1.sequence").Raw)
					require.Equal(t, "Keep ledger balance 11; add 7 exactly once", gjson.GetBytes(sent, "input.1.content").String())
					require.JSONEq(t, `{"add":7}`, gjson.GetBytes(sent, "input.#(type==function_call).arguments").String())
					require.Equal(t, "fc_715_once", gjson.GetBytes(sent, "input.#(type==function_call_output).call_id").String())
					require.Equal(t, "ledger balance 18; applied once", gjson.GetBytes(sent, "input.#(type==function_call_output).output").String())
					if change == "system" || change == "system_json" || change == "namespace_system" {
						require.Contains(t, string(sent), "Preserve the fixture ledger guidance")
						role := "system"
						if kind == service.AccountTypeOAuth {
							role = "developer"
							require.Contains(t, gjson.GetBytes(sent, "instructions").String(), "Preserve the fixture ledger guidance")
						}
						require.Equal(t, role, gjson.GetBytes(sent, "input.5.role").String())
						require.Len(t, gjson.GetBytes(sent, "input").Array(), len(gjson.Get(body, "input").Array()))
					}
					if change == "opaque_tool_result" {
						require.Equal(t, "Keep the existing opaque task result", gjson.GetBytes(sent, "input.#(call_id==fc_inside_opaque_task).output").String())
					}
					if change == "tool_alias" {
						name := "python"
						if kind == service.AccountTypeOAuth {
							name = "python__sub2api"
						}
						require.Equal(t, name, gjson.GetBytes(sent, "input.2.name").String())
					}
				})
			}
		}
	}
}
