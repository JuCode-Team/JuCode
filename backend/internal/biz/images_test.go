package biz

import (
	"encoding/base64"
	"testing"
)

func TestExtractOutputImagesResponsesImageGenerationCall(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3, 4}
	obj := map[string]any{
		"output": []any{
			map[string]any{
				"type":   "image_generation_call",
				"result": base64.StdEncoding.EncodeToString(png),
			},
		},
	}

	imgs := ExtractOutputImages(obj)
	if len(imgs) != 1 {
		t.Fatalf("len(imgs) = %d, want 1", len(imgs))
	}
	if imgs[0].SourceKind != "base64" || imgs[0].ContentType != "image/png" || imgs[0].SizeBytes != len(png) {
		t.Fatalf("image summary = %+v", imgs[0])
	}
}

func TestRewriteImageOutputsWithNoticeSuppressesImageGenerationCall(t *testing.T) {
	obj := map[string]any{
		"output": []any{
			map[string]any{
				"type":   "image_generation_call",
				"status": "completed",
				"result": base64.StdEncoding.EncodeToString([]byte("image")),
			},
		},
	}

	n := RewriteImageOutputsWithNotice(obj)
	if n != 1 {
		t.Fatalf("rewrites = %d, want 1", n)
	}
	item := obj["output"].([]any)[0].(map[string]any)
	if _, ok := item["result"]; ok {
		t.Fatalf("result was not removed: %+v", item)
	}
	if item["suppressed"] != true {
		t.Fatalf("suppressed marker missing: %+v", item)
	}
}
