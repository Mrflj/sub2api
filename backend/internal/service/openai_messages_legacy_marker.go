package service

import (
	"encoding/json"
	"strings"
)

// Retain recognition of requests emitted by older bridge versions. New requests
// must preserve client instructions rather than adding a todo-management prompt.
const openAICompatClaudeCodeTodoGuardMarker = "<sub2api-claude-code-todo-guard>"

func inputContainsText(input []any, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, item := range input {
		b, err := json.Marshal(item)
		if err == nil && strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}
