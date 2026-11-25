package schema

// HeaderRules rewrites the headers of requests sent to an upstream account,
// for gateways that are Anthropic/OpenAI-compatible in body but not in
// headers: a different credential header (Azure `api-key`, APIM
// `Ocp-Apim-Subscription-Key`), a header the gateway rejects, or individual
// values of a comma-separated list header it rejects (an `anthropic-beta`
// flag the vendor has not enabled).
//
// Applied after the gateway has built the request headers, in the order
// remove → drop_values → set. Set values may reference the account
// credential as {api_key}.
type HeaderRules struct {
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
	// DropValues removes matching entries from a comma-separated header; a
	// pattern ending in "*" matches by prefix. The header is removed when no
	// entry is left.
	DropValues map[string][]string `json:"drop_values,omitempty"`
}
