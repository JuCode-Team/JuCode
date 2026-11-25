package llm

import "testing"

func TestGenerationFailoverTreatsDisabledImageGroupAsAccountFailure(t *testing.T) {
	if !generationShouldFailover(403) || !generationShouldFailover(503) {
		t.Fatal("account-level image and upstream failures should fail over")
	}
	if generationShouldFailover(400) {
		t.Fatal("a 400 is the request and must not fail over")
	}
}

func TestOpenAIErrorIncludesRequestID(t *testing.T) {
	got := openAIError("api_error", "upstream_unavailable", "service temporarily unavailable", "req_123")

	if got["request_id"] != "req_123" {
		t.Fatalf("top-level request_id = %v, want req_123", got["request_id"])
	}
	err, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field has type %T", got["error"])
	}
	if err["request_id"] != "req_123" {
		t.Fatalf("nested request_id = %v, want req_123", err["request_id"])
	}
}

func TestAnthropicErrorIncludesRequestID(t *testing.T) {
	got := anthropicError("api_error", "service temporarily unavailable", "req_123")

	if got["request_id"] != "req_123" {
		t.Fatalf("top-level request_id = %v, want req_123", got["request_id"])
	}
	err, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field has type %T", got["error"])
	}
	if err["request_id"] != "req_123" {
		t.Fatalf("nested request_id = %v, want req_123", err["request_id"])
	}
}
