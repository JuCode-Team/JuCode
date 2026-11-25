package llm

import (
	"bytes"
	"encoding/json"
)

// azureUnknownInputFields are Responses input-item fields Azure OpenAI rejects
// with 400 unknown_parameter. Codex 0.164 sends
// internal_chat_message_metadata_passthrough on message items; OpenAI's own
// endpoint accepts it, Azure does not.
var azureUnknownInputFields = []string{"internal_chat_message_metadata_passthrough"}

// adaptAzureOpenAIResponsesBody drops azureUnknownInputFields from every
// input item. Only `input` is re-encoded; a body without them keeps its bytes.
func adaptAzureOpenAIResponsesBody(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"internal_chat_message_metadata_passthrough"`)) {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["input"], &items); err != nil {
		// String input carries no items to clean.
		return body, nil
	}
	changed := false
	for _, item := range items {
		for _, key := range azureUnknownInputFields {
			if _, ok := item[key]; ok {
				delete(item, key)
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	input, err := marshalJSON(items)
	if err != nil {
		return nil, err
	}
	raw["input"] = input
	return marshalJSON(raw)
}
