package llm

import (
	"regexp"
	"strconv"
	"sync"
)

// Some upstreams cap the images in one request and reject the whole request
// past the cap (Azure OpenAI: "Exceeded maximum number of images (50) allowed
// in the request"). Coding clients keep every screenshot in the history, so
// once a session crosses the cap every later turn fails the same way. On that
// rejection the gateway keeps the newest N images, replaces the older ones
// with a text placeholder, retries the same account, and remembers N for the
// account so later requests are trimmed before they are sent.

var imageLimitRe = regexp.MustCompile(`(?i)maximum number of images \((\d+)\)`)

// imagePlaceholder stands in for a dropped image so the model knows the
// history had one there.
const imagePlaceholder = "[较早的图片已省略：上游单次请求的图片数量有上限]"

// accountImageLimits maps account id -> learned image cap. Process-local:
// each instance relearns it with one rejected request after a restart.
var accountImageLimits sync.Map

// upstreamImageLimit returns the cap named in an upstream rejection, 0 if the
// body is not an image-count rejection.
func upstreamImageLimit(body []byte) int {
	m := imageLimitRe.FindSubmatch(body)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func learnedImageLimit(account string) int {
	if v, ok := accountImageLimits.Load(account); ok {
		return v.(int)
	}
	return 0
}

// learnImageLimit records the cap for the account. It returns false when the
// same or a lower cap was already known: the request was trimmed to it and
// still rejected, so another retry would fail the same way.
func learnImageLimit(account string, limit int) bool {
	if old := learnedImageLimit(account); old > 0 && old <= limit {
		return false
	}
	accountImageLimits.Store(account, limit)
	return true
}

// imageTextType maps an image part type to the text part type of the same
// dialect: Responses input_image, Chat image_url, Anthropic image.
var imageTextType = map[string]string{
	"input_image": "input_text",
	"image_url":   "text",
	"image":       "text",
}

type imageRef struct {
	parts []any
	idx   int
}

// collectImages lists the image parts under v in document order (oldest
// first).
func collectImages(v any, out []imageRef) []imageRef {
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			if m, ok := e.(map[string]any); ok {
				if typ, _ := m["type"].(string); imageTextType[typ] != "" {
					out = append(out, imageRef{parts: t, idx: i})
					continue
				}
			}
			out = collectImages(e, out)
		}
	case map[string]any:
		for _, e := range t {
			out = collectImages(e, out)
		}
	}
	return out
}

func countRequestImages(body map[string]any) int {
	return len(collectImages(body["input"], collectImages(body["messages"], nil)))
}

// trimRequestImages returns body with only the newest limit images kept and
// how many were dropped; limit 0 means no cap. The caller's body is never
// mutated: a copy is made only when something has to be dropped.
func trimRequestImages(body map[string]any, limit int) (map[string]any, int, error) {
	if limit <= 0 || countRequestImages(body) <= limit {
		return body, 0, nil
	}
	out, err := cloneRequestBody(body)
	if err != nil {
		return nil, 0, err
	}
	refs := collectImages(out["input"], collectImages(out["messages"], nil))
	drop := len(refs) - limit
	for _, r := range refs[:drop] {
		typ, _ := r.parts[r.idx].(map[string]any)["type"].(string)
		r.parts[r.idx] = map[string]any{"type": imageTextType[typ], "text": imagePlaceholder}
	}
	return out, drop, nil
}
