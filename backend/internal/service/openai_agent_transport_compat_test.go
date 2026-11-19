package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOpenAIAgentWSCompatibleTransportPreservesHistory(t *testing.T) {
	for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, mode := range []string{"ctx_pool", "passthrough", "http_bridge"} {
			for _, mutation := range []string{"intact", "tool_result", "task", "cipher", "precision"} {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, mode, mutation), func(t *testing.T) {
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					svc.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
					manager, _, account := issue715WSManager(t, svc, context.Background())
					defer manager.abort()
					account.Type = kind
					source := []byte(strings.ReplaceAll(strings.ReplaceAll(issue842Source, `,"status":"completed"`, ""), `,"id":"rs_fixture842"`, ""))
					var err error
					source, err = sjson.SetRawBytes(source, "input.2.internal_chat_message_metadata_passthrough", []byte(`{"fixture":true}`))
					require.NoError(t, err)
					source, err = sjson.SetBytes(source, "input.3.name", "python")
					require.NoError(t, err)
					source, err = sjson.SetRawBytes(source, "tools", []byte(`[{"type":"function","name":"python","parameters":{"type":"object","properties":{}}}]`))
					require.NoError(t, err)
					original := append([]byte(nil), source...)
					payload, err := manager.prepareReadable(account, source)
					require.NoError(t, err)
					payload, _, err = normalizeOpenAIResponsesWebSocketCompatibilityBodyWithConfig(svc.cfg, payload, account, false)
					require.NoError(t, err)
					if account.IsOpenAIOAuthLike() {
						payload, _, _, err = aliasOpenAIOAuthReservedToolNamesBody(payload)
						require.NoError(t, err)
					}
					switch mutation {
					case "tool_result":
						payload, err = sjson.SetBytes(payload, "input.4.output", "changed result")
					case "task":
						payload, err = sjson.SetBytes(payload, "input.2.content", "changed task")
					case "cipher":
						payload, err = sjson.SetBytes(payload, "input.0.encrypted_content", "changed cipher")
					case "precision":
						payload, err = sjson.SetRawBytes(payload, "input.2.sequence", []byte("9007199254740992"))
					}
					require.NoError(t, err)
					if mode == "http_bridge" {
						req, requestErr := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", bytes.NewReader(payload))
						require.NoError(t, requestErr)
						req.Header.Set("Authorization", "Bearer fixture")
						err = manager.begin(account, source, payload, req, mode)
					} else {
						err = manager.beginSocket(account, source, payload, "wss://native.example/v1/responses", http.Header{"Authorization": {"Bearer fixture"}}, mode)
					}
					if mutation != "intact" {
						var refusal *OpenAIAgentRecoveryError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, "wire_changed", refusal.Reason)
						return
					}
					require.NoError(t, err)
					require.Equal(t, original, source)
					require.JSONEq(t, gjson.GetBytes(source, "input.0").Raw, gjson.GetBytes(payload, "input.0").Raw)
					require.Equal(t, "9007199254740993", gjson.GetBytes(payload, "input.2.sequence").Raw)
					require.Equal(t, "already recorded842", gjson.GetBytes(payload, "input.4.output").String())
					require.Equal(t, gjson.GetBytes(payload, "input.3.call_id").String(), gjson.GetBytes(payload, "input.4.call_id").String())
					if account.IsOpenAIOAuthLike() {
						require.False(t, gjson.GetBytes(payload, "input.2.internal_chat_message_metadata_passthrough").Exists())
						require.Equal(t, codexPythonToolAlias, gjson.GetBytes(payload, "input.3.name").String())
					}
				})
			}
		}
	}
}
