// images.go: helpers that operate on the request/response body
// shapes carrying images. Lives in biz/ so the policy scan and the
// gateway share it without a biz→llm import cycle.
//
// Three image-bearing shapes we recognize:
//
//  1. OpenAI Chat Completions input
//     messages[].content[].type == "image_url"
//     .image_url.url  = "https://..."  OR  "data:image/png;base64,XXXX"
//
//  2. OpenAI Responses input
//     input[].content[].type == "input_image"
//     .image_url       = "data:image/..." OR https URL
//     .detail          = "auto" | "low" | "high"
//
//  3. Anthropic Messages input
//     messages[].content[].type == "image"
//     .source.type == "base64"
//     .source.media_type = "image/png" / "image/jpeg" / ...
//     .source.data       = base64 string
//     .source.type == "url"
//     .source.url        = https URL
//
// For POLICY SCAN we'd waste cycles AC-matching megabytes of base64,
// so we replace each image's "data"/"url" string with a single literal
// "<image>" sentinel before the scanner runs. The sentinel has no
// chars that overlap with any sensible keyword.
package biz

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const imageScanSentinel = "<image>"

// RedactImagesForScan returns a string suitable for feeding into the
// policy AC scanner: the original JSON with every image payload
// replaced by a short sentinel. Falls back to the raw input if the
// body isn't valid JSON (the scanner can deal with raw bytes; this
// helper is opportunistic).
func RedactImagesForScan(raw []byte) string {
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return string(raw)
	}
	redactAnyImages(body)
	out, err := json.Marshal(body)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// redactAnyImages walks the parsed JSON tree in place. Object keys
// matching known image-carrying field names get their value clamped
// to imageScanSentinel.
func redactAnyImages(v any) {
	switch t := v.(type) {
	case map[string]any:
		// Anthropic source.{data,url}
		if src, ok := t["source"].(map[string]any); ok {
			if _, has := src["data"]; has {
				src["data"] = imageScanSentinel
			}
			if _, has := src["url"]; has {
				src["url"] = imageScanSentinel
			}
		}
		// OpenAI chat image_url.url
		if iu, ok := t["image_url"].(map[string]any); ok {
			if _, has := iu["url"]; has {
				iu["url"] = imageScanSentinel
			}
		}
		// OpenAI responses input_image.image_url (string)
		if typ, _ := t["type"].(string); typ == "input_image" {
			if _, has := t["image_url"]; has {
				t["image_url"] = imageScanSentinel
			}
		}
		for _, child := range t {
			redactAnyImages(child)
		}
	case []any:
		for _, child := range t {
			redactAnyImages(child)
		}
	}
}

// OutputImage represents one image emitted by the model in the
// assistant's response. SourceKind tells the caller whether Data
// carries the decoded bytes (base64 case) or whether URL is the
// canonical reference (url case — typically a temporary upstream
// CDN link). When SourceKind == "url", Data is empty.
type OutputImage struct {
	Index       int    // 0-based position within the request's image outputs
	ContentType string // e.g. "image/png"; empty when unknown
	SizeBytes   int
	SHA256      string
	SourceKind  string // "base64" | "url"
	URL         string // populated when SourceKind == "url"
	Data        []byte // raw decoded bytes when SourceKind == "base64"
}

// ExtractOutputImages walks one decoded SSE / JSON frame and pulls
// every image-bearing block that looks like a model OUTPUT (as
// opposed to an input). Three patterns are recognized; unknown ones
// are silently ignored so a new vendor variant just behaves like
// "no images for this frame":
//
//	OpenAI Chat (gpt-image-1):
//	    choices[].delta.content   = [{type:"image_url", image_url:{url:"data:..."}}]
//	    choices[].message.content = same shape on non-stream
//
//	OpenAI Responses:
//	    response.output[].content[].type == "output_image" with .image_url (string)
//	    response.output[].type == "image_generation_call" with .result (base64)
//	    Streaming event:
//	      type == "response.output_item.added"
//	      item.content[].type == "output_image" / image_url string
//	    Or:
//	      type == "response.image_generation_call.partial_image" with .b64_json
//
//	Anthropic (future):
//	    content_block_start with content_block.type == "image" + base64 source
//
// Returns refs in document order. Empty result on no images.
func ExtractOutputImages(obj map[string]any) []OutputImage {
	var refs []OutputImage
	walkAndExtractOutputs(obj, &refs)
	return refs
}

func walkAndExtractOutputs(v any, refs *[]OutputImage) {
	switch t := v.(type) {
	case map[string]any:
		// OpenAI chat content block: {type:"image_url", image_url:{url:"data:..."}}
		if typ, _ := t["type"].(string); typ == "image_url" {
			if iu, ok := t["image_url"].(map[string]any); ok {
				url := strOf(iu["url"])
				if url != "" {
					*refs = append(*refs, decodeOutputImageURL(url))
				}
			}
		}
		// OpenAI Responses content block: {type:"output_image", image_url:"..."}
		if typ, _ := t["type"].(string); typ == "output_image" {
			url := strOf(t["image_url"])
			if url != "" {
				*refs = append(*refs, decodeOutputImageURL(url))
			} else if b64 := strOf(t["b64_json"]); b64 != "" {
				*refs = append(*refs, decodeOutputImageBase64("", b64))
			}
		}
		// Responses built-in image generation tool:
		// {type:"image_generation_call", result:"<base64 png>"}
		if typ, _ := t["type"].(string); typ == "image_generation_call" {
			if b64 := strOf(t["result"]); b64 != "" {
				*refs = append(*refs, decodeOutputImageBase64("", b64))
			}
		}
		// Responses streaming image-gen partial frames:
		// {type:"response.image_generation_call.partial_image", partial_image_b64:"…"}
		if typ, _ := t["type"].(string); typ == "response.image_generation_call.partial_image" {
			if b64 := strOf(t["partial_image_b64"]); b64 != "" {
				*refs = append(*refs, decodeOutputImageBase64("", b64))
			}
		}
		// Anthropic image content block on output (rare today, future-proof):
		// {type:"image", source:{type:"base64", media_type, data}}
		if typ, _ := t["type"].(string); typ == "image" {
			if src, ok := t["source"].(map[string]any); ok {
				switch strOf(src["type"]) {
				case "base64":
					*refs = append(*refs, decodeOutputImageBase64(strOf(src["media_type"]), strOf(src["data"])))
				case "url":
					if url := strOf(src["url"]); url != "" {
						*refs = append(*refs, decodeOutputImageURL(url))
					}
				}
			}
		}
		for _, child := range t {
			walkAndExtractOutputs(child, refs)
		}
	case []any:
		for _, child := range t {
			walkAndExtractOutputs(child, refs)
		}
	}
}

func decodeOutputImageURL(url string) OutputImage {
	if strings.HasPrefix(url, "data:") {
		// data:image/png;base64,XXXX
		if comma := strings.Index(url, ","); comma > 0 {
			head := url[:comma]
			body := url[comma+1:]
			mt := ""
			if mtStripped := strings.TrimPrefix(head, "data:"); mtStripped != "" {
				if semi := strings.Index(mtStripped, ";"); semi > 0 {
					mt = mtStripped[:semi]
				} else {
					mt = mtStripped
				}
			}
			return decodeOutputImageBase64(mt, body)
		}
	}
	// Remote URL — keep as reference; size + hash derived from the URL string itself.
	sum := sha256.Sum256([]byte(url))
	return OutputImage{
		SourceKind: "url",
		URL:        url,
		SHA256:     hex.EncodeToString(sum[:]),
	}
}

func decodeOutputImageBase64(mediaType, b64 string) OutputImage {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// keep the base64 string verbatim and hash that instead
		sum := sha256.Sum256([]byte(b64))
		return OutputImage{
			SourceKind:  "base64",
			ContentType: mediaType,
			SHA256:      hex.EncodeToString(sum[:]),
			SizeBytes:   len(b64),
		}
	}
	sum := sha256.Sum256(decoded)
	if mediaType == "" {
		mediaType = sniffImageMediaType(decoded)
	}
	return OutputImage{
		SourceKind:  "base64",
		ContentType: mediaType,
		SizeBytes:   len(decoded),
		SHA256:      hex.EncodeToString(sum[:]),
		Data:        decoded,
	}
}

// sniffImageMediaType inspects the first few bytes for the common image
// magic numbers. Used when the upstream didn't tag the media type
// explicitly (some `b64_json` payloads omit it). Returns "" on no match
// — caller stays neutral and stores no content_type, which is fine.
func sniffImageMediaType(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	switch {
	case bytesPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case bytesPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytesPrefix(b, []byte{'G', 'I', 'F', '8'}):
		return "image/gif"
	case bytesPrefix(b, []byte{'R', 'I', 'F', 'F'}) && len(b) >= 12 &&
		bytesPrefix(b[8:], []byte{'W', 'E', 'B', 'P'}):
		return "image/webp"
	}
	return ""
}

func bytesPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

// ImageOutputDisabledNotice is the literal text we splice into the
// model's response in place of every image block when an operator
// has set SiteSettings.AllowImageOutput = false. End-users see this
// text inside the assistant turn; the original image bytes are
// discarded (never stored, never billed).
const ImageOutputDisabledNotice = "您的账号暂时无法使用图片生成功能"

// RewriteImageOutputsWithNotice walks a single SSE-frame / non-stream
// response payload (already JSON-unmarshalled to map[string]any) and
// replaces every model-output image block with a text block carrying
// ImageOutputDisabledNotice. Returns the number of replacements made
// — callers can use that as "did this frame contain images" without
// re-scanning.
//
// Replacement rules mirror ExtractOutputImages so the gate covers
// every shape we recognize as an OUTPUT image:
//
//	{type:"image_url", image_url:{...}}      → {type:"text", text:NOTICE}
//	{type:"output_image", image_url:"…"}     → {type:"output_text", text:NOTICE}
//	{type:"output_image", b64_json:"…"}      → {type:"output_text", text:NOTICE}
//	{type:"image_generation_call", result:"…"}
//	                                        → result removed, status kept
//	{type:"response.image_generation_call.partial_image", ...}
//	                                        → struct emptied, type kept so SDKs don't choke
//	{type:"image", source:{...}}             → {type:"text", text:NOTICE}
//
// Note we use "text" for chat/anthropic blocks and "output_text" for
// the Responses API shape — the two SDKs key off different type
// strings.
func RewriteImageOutputsWithNotice(v any) int {
	c := &rewriteCounter{}
	walkAndRewrite(v, c)
	return c.n
}

// FlattenChatContentArrayToString collapses an OpenAI Chat-Completions
// `choices[*].delta.content` or `choices[*].message.content` that's
// shaped as an array-of-text-blocks back into a single string. Used
// AFTER RewriteImageOutputsWithNotice so the downstream chat-chunk
// parser (which strictly expects content:string) can still decode
// the rewritten frame.
//
// Only collapses arrays whose entries are uniformly {type:"text",text:...}
// — mixed arrays are left intact (someone downstream presumably knows
// what to do).
func FlattenChatContentArrayToString(payload map[string]any) {
	choices, _ := payload["choices"].([]any)
	for _, c := range choices {
		ch, _ := c.(map[string]any)
		if ch == nil {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			m, _ := ch[key].(map[string]any)
			if m == nil {
				continue
			}
			if arr, ok := m["content"].([]any); ok {
				if s, ok := tryJoinTextArray(arr); ok {
					m["content"] = s
				}
			}
		}
	}
}

func tryJoinTextArray(arr []any) (string, bool) {
	var sb strings.Builder
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			return "", false
		}
		if t, _ := m["type"].(string); t != "text" {
			return "", false
		}
		s, _ := m["text"].(string)
		sb.WriteString(s)
	}
	return sb.String(), true
}

type rewriteCounter struct{ n int }

func walkAndRewrite(v any, c *rewriteCounter) {
	switch t := v.(type) {
	case map[string]any:
		if typ, _ := t["type"].(string); typ == "image_url" {
			delete(t, "image_url")
			t["type"] = "text"
			t["text"] = ImageOutputDisabledNotice
			c.n++
			return
		}
		if typ, _ := t["type"].(string); typ == "output_image" {
			delete(t, "image_url")
			delete(t, "b64_json")
			t["type"] = "output_text"
			t["text"] = ImageOutputDisabledNotice
			c.n++
			return
		}
		if typ, _ := t["type"].(string); typ == "image_generation_call" {
			delete(t, "result")
			t["status"] = "completed"
			t["suppressed"] = true
			c.n++
			return
		}
		if typ, _ := t["type"].(string); typ == "response.image_generation_call.partial_image" {
			delete(t, "partial_image_b64")
			delete(t, "b64_json")
			t["type"] = "response.image_generation_call.partial_image_suppressed"
			c.n++
			return
		}
		if typ, _ := t["type"].(string); typ == "image" {
			delete(t, "source")
			t["type"] = "text"
			t["text"] = ImageOutputDisabledNotice
			c.n++
			return
		}
		for _, child := range t {
			walkAndRewrite(child, c)
		}
	case []any:
		for _, child := range t {
			walkAndRewrite(child, c)
		}
	}
}

func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
