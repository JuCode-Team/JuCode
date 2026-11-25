package llm

import (
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

func TestRateLimitOutputTokensExcludesAllInput(t *testing.T) {
	usage := biz.TokenUsage{
		TokensIn:              10,
		TokensOut:             5,
		CachedTokensIn:        20,
		CacheCreationTokensIn: 30,
		ImageTokensIn:         40,
		ImageTokensOut:        50,
	}
	if got := rateLimitOutputTokens(usage); got != 5 {
		t.Fatalf("TPM settlement = %d, want 5 text output tokens only", got)
	}
}
