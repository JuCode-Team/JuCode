package llm

import (
	"encoding/json"
	"testing"
)

func TestUpstreamImageLimit(t *testing.T) {
	azure := []byte(`{ "error": { "message": "Exceeded maximum number of images (50) allowed in the request", "type": "invalid_request_error", "param": "input", "code": null } }`)
	if got := upstreamImageLimit(azure); got != 50 {
		t.Fatalf("limit = %d, want 50", got)
	}
	if got := upstreamImageLimit([]byte(`{"error":{"message":"Invalid image data."}}`)); got != 0 {
		t.Fatalf("limit = %d, want 0", got)
	}
}

func responsesBodyWithImages(n int) map[string]any {
	var input []any
	for i := 0; i < n; i++ {
		input = append(input, map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "look"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,IMG" + string(rune('a'+i))},
			},
		})
	}
	return map[string]any{"model": "m", "input": input}
}

func TestTrimRequestImagesKeepsNewest(t *testing.T) {
	body := responsesBodyWithImages(5)
	before, _ := json.Marshal(body)

	out, dropped, err := trimRequestImages(body, 3)
	if err != nil || dropped != 2 {
		t.Fatalf("dropped = %d err = %v, want 2", dropped, err)
	}
	if after, _ := json.Marshal(body); string(after) != string(before) {
		t.Fatal("caller's body was mutated")
	}
	if got := countRequestImages(out); got != 3 {
		t.Fatalf("images left = %d, want 3", got)
	}
	items := out["input"].([]any)
	for i, want := range []string{"input_text", "input_text", "input_image", "input_image", "input_image"} {
		part := items[i].(map[string]any)["content"].([]any)[1].(map[string]any)
		if part["type"] != want {
			t.Fatalf("item %d part type = %v, want %s", i, part["type"], want)
		}
	}
	last := items[4].(map[string]any)["content"].([]any)[1].(map[string]any)
	if last["image_url"] != "data:image/png;base64,IMGe" {
		t.Fatalf("newest image not kept: %v", last)
	}
}

func TestTrimRequestImagesChatAndAnthropic(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:a"}},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "b"}},
		}},
	}}
	out, dropped, err := trimRequestImages(body, 1)
	if err != nil || dropped != 1 {
		t.Fatalf("dropped = %d err = %v, want 1", dropped, err)
	}
	parts := out["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if p := parts[0].(map[string]any); p["type"] != "text" || p["text"] != imagePlaceholder {
		t.Fatalf("placeholder = %v", p)
	}
	if p := parts[1].(map[string]any); p["type"] != "image" {
		t.Fatalf("newest image dropped: %v", p)
	}
}

func TestTrimRequestImagesUnderLimitIsNoop(t *testing.T) {
	body := responsesBodyWithImages(2)
	for _, limit := range []int{0, 2, 50} {
		out, dropped, err := trimRequestImages(body, limit)
		if err != nil || dropped != 0 {
			t.Fatalf("limit %d: dropped = %d err = %v", limit, dropped, err)
		}
		if out["input"] == nil || len(out["input"].([]any)) != 2 {
			t.Fatalf("limit %d: body changed", limit)
		}
	}
}

func TestLearnImageLimitStopsRepeatRetry(t *testing.T) {
	acct := "test-account-learn"
	t.Cleanup(func() { accountImageLimits.Delete(acct) })
	if !learnImageLimit(acct, 50) {
		t.Fatal("first cap must be learned")
	}
	if learnImageLimit(acct, 50) {
		t.Fatal("same cap again must not trigger another retry")
	}
	if !learnImageLimit(acct, 20) {
		t.Fatal("a lower cap must be learned")
	}
	if got := learnedImageLimit(acct); got != 20 {
		t.Fatalf("learned = %d, want 20", got)
	}
}
