package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatCompletionsToResponses_GenerationControlsCompatibility(t *testing.T) {
	const request = `{"model":"gpt-4.1","messages":[{"role":"system","content":"Keep the task"},{"role":"user","content":"Continue END"},{"role":"assistant","tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"read","arguments":"{\"stop\":\"END\"}"}}]},{"role":"tool","tool_call_id":"call_fixture","content":"tool result"}],"temperature":0,"top_p":0.9,"max_completion_tokens":256,"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"stop":{"type":"string"}}}}}],"response_format":{"type":"json_object"}}`
	for _, converter := range []struct {
		name string
		run  func(*ChatCompletionsRequest) (*ResponsesRequest, error)
	}{
		{"default", ChatCompletionsToResponses},
		{"configured floor", func(req *ChatCompletionsRequest) (*ResponsesRequest, error) {
			return ChatCompletionsToResponsesWithMinOutputTokens(req, 16)
		}},
	} {
		t.Run(converter.name, func(t *testing.T) {
			var baselineRequest ChatCompletionsRequest
			require.NoError(t, json.Unmarshal([]byte(request), &baselineRequest))
			baseline, err := converter.run(&baselineRequest)
			require.NoError(t, err)
			for _, tc := range []struct{ name, fields string }{
				{"frequency zero", `"frequency_penalty":0`},
				{"frequency nonzero", `"frequency_penalty":0.5`},
				{"presence zero", `"presence_penalty":0`},
				{"presence nonzero", `"presence_penalty":-0.5`},
				{"seed zero", `"seed":0`},
				{"seed large integer", `"seed":9007199254740993`},
				{"logit bias", `"logit_bias":{"1":0,"2":-100}`},
				{"empty logit bias", `"logit_bias":{}`},
				{"one choice", `"n":1`},
				{"multiple choices", `"n":3`},
				{"stop string", `"stop":"END"`},
				{"stop array", `"stop":["END","STOP"]`},
				{"empty stops", `"stop":[]`},
				{"null controls", `"frequency_penalty":null,"presence_penalty":null,"seed":null,"logit_bias":null,"n":null,"stop":null`},
				{"all controls", `"frequency_penalty":0.5,"presence_penalty":-0.5,"seed":42,"logit_bias":{"1":-100},"n":2,"stop":["END"]`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var req ChatCompletionsRequest
					require.NoError(t, json.Unmarshal([]byte(request), &req))
					require.NoError(t, json.Unmarshal([]byte("{"+tc.fields+"}"), &req))
					before, err := json.Marshal(req)
					require.NoError(t, err)
					converted, err := converter.run(&req)
					require.NoError(t, err)
					require.Equal(t, baseline, converted, "compatibility fields must not change messages, tools or supported controls")
					after, err := json.Marshal(req)
					require.NoError(t, err)
					require.JSONEq(t, string(before), string(after), "conversion must not mutate the caller's request")
				})
			}
		})
	}
}
