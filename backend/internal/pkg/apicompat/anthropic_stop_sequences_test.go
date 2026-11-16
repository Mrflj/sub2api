package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicToResponsesRequiresLocalStopSequenceHandling(t *testing.T) {
	request := &AnthropicRequest{Model: "gpt-4.1", MaxTokens: 128, StopSeqs: []string{"END"}}
	_, err := AnthropicToResponses(request)
	require.ErrorContains(t, err, "stop_sequences")
	converted, err := AnthropicToResponsesWithOptions(request, AnthropicToResponsesOptions{StopSequencesHandledLocally: true})
	require.NoError(t, err)
	encoded, err := json.Marshal(converted)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "stop_sequences")
	require.Equal(t, []string{"END"}, request.StopSeqs)
	request.StopSeqs = []string{""}
	_, err = AnthropicToResponsesWithOptions(request, AnthropicToResponsesOptions{StopSequencesHandledLocally: true})
	require.ErrorContains(t, err, "non-empty")
}
