package apicompat

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicTopKCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stops   []string
		convert func(*AnthropicRequest) (any, error)
	}{
		{name: "responses", convert: func(req *AnthropicRequest) (any, error) {
			return AnthropicToResponses(req)
		}},
		{name: "responses with local stops", stops: []string{"END"}, convert: func(req *AnthropicRequest) (any, error) {
			return AnthropicToResponsesWithOptions(req, AnthropicToResponsesOptions{StopSequencesHandledLocally: true})
		}},
		{name: "chat completions", stops: []string{"END"}, convert: func(req *AnthropicRequest) (any, error) {
			return AnthropicToChatCompletionsRequest(req)
		}},
	} {
		for _, topK := range []int{0, 40} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, topK), func(t *testing.T) {
				var req AnthropicRequest
				require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-4.1","max_tokens":128,"system":"Keep context","messages":[{"role":"user","content":"hello"}],"temperature":0.3,"top_p":0.9,"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`), &req))
				req.StopSeqs = tc.stops
				baseline, err := tc.convert(&req)
				require.NoError(t, err)
				baselineJSON, err := json.Marshal(baseline)
				require.NoError(t, err)
				req.TopK = &topK
				original, err := json.Marshal(req)
				require.NoError(t, err)
				converted, err := tc.convert(&req)
				require.NoError(t, err)
				encoded, err := json.Marshal(converted)
				require.NoError(t, err)
				require.JSONEq(t, string(baselineJSON), string(encoded))
				require.NotContains(t, string(encoded), `"top_k"`)
				after, err := json.Marshal(req)
				require.NoError(t, err)
				require.JSONEq(t, string(original), string(after))
			})
		}
	}
}
