package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// Codex 0.164 request item, as captured from the Azure 400
// "Unknown parameter: 'input[1].internal_chat_message_metadata_passthrough.content_item_metadata'".
const codex164ResponsesBody = `{"model":"gpt-6-astra","stream":true,"input":[` +
	`{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"content_item_metadata":[{"kind":"text"}]}}` +
	`]}`

func TestAdaptAzureOpenAIResponsesBodyStripsPassthrough(t *testing.T) {
	got, err := adaptAzureOpenAIResponsesBody([]byte(codex164ResponsesBody))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("field kept: %s", got)
	}
	for _, want := range []string{`"model":"gpt-6-astra"`, `"stream":true`, `"text":"hi"`, `"role":"developer"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("lost %s: %s", want, got)
		}
	}
}

func TestAdaptAzureOpenAIResponsesBodyUntouchedBytes(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
		`{"model":"m","input":"internal_chat_message_metadata_passthrough"}`,
	} {
		got, err := adaptAzureOpenAIResponsesBody([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("body rewritten:\n got %s\nwant %s", got, body)
		}
	}
}

// Only accounts of the Azure OpenAI type are cleaned, and only on Responses
// paths; OpenAI's own endpoint keeps receiving the field.
func TestOpenAIProviderStripsPassthroughOnlyForAzure(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cases := []struct {
		definition string
		path       string
		stripped   bool
	}{
		{biz.AzureOpenAIDefinitionID, "/v1/responses", true},
		{biz.AzureOpenAIDefinitionID, "/v1/responses/compact", true},
		{biz.AzureOpenAIDefinitionID, "/v1/chat/completions", false},
		{"", "/v1/responses", false},
	}
	for _, tc := range cases {
		acct := &ent.UpstreamAccount{BaseURL: server.URL, DefinitionID: tc.definition}
		prov := NewOpenAIProviderWithClient(acct, "key", server.Client())
		resp, err := prov.Forward(context.Background(), tc.path, []byte(codex164ResponsesBody), true, http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if has := strings.Contains(got, "internal_chat_message_metadata_passthrough"); has == tc.stripped {
			t.Fatalf("definition=%q path=%s: field present=%v, want stripped=%v", tc.definition, tc.path, has, tc.stripped)
		}
	}
}

func TestAzureOpenAIBuiltinDefinition(t *testing.T) {
	def, ok := biz.BuiltinDefinitionByID(biz.AzureOpenAIDefinitionID)
	if !ok {
		t.Fatal("azure-openai builtin definition missing")
	}
	if def.HeaderRules.Set["api-key"] != "{api_key}" {
		t.Fatalf("api-key header rule = %q", def.HeaderRules.Set["api-key"])
	}
}
