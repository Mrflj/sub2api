package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

func openAIMessagesResponseHistoryScope(c *gin.Context, account *Account, model string) string {
	if c == nil || account == nil || account.Platform != PlatformOpenAI || !strings.EqualFold(strings.TrimSpace(c.GetHeader(apicompat.ResponsesHistoryHeader)), "v1") {
		return ""
	}
	// Scope provider ciphertext to its account, routed model and
	// caller. A routing change keeps visible history but cannot borrow state.
	payload, _ := json.Marshal([]any{"responses-history-v1", account.ID, getAPIKeyIDFromContext(c), strings.TrimSpace(model)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
