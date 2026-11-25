package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// webVendorServer answers one vendor call and records what the adaptor sent.
type webVendorCall struct {
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
}

func webVendorServer(t *testing.T, status int, header map[string]string, response string, call *webVendorCall) *openaiProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.method, call.path, call.query, call.header = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.body)
		}
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return &openaiProvider{id: uuid.New(), baseURL: srv.URL, apiKey: "vendor-key", hc: srv.Client()}
}

// forwardWeb runs one call through the vendor adaptor for format. kind is
// "search" or "fetch"; the engine is the vendor's own.
func forwardWeb(t *testing.T, p *openaiProvider, format, kind, body string) (*webResult, map[string]any) {
	t.Helper()
	p.format = format
	var in map[string]any
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	in["engine"] = format
	req, err := parseWebToolRequest(kind, in)
	if err != nil {
		t.Fatalf("parseWebToolRequest: %v", err)
	}
	resp, err := forwardWebTool(context.Background(), p, req)
	if err != nil {
		t.Fatalf("forwardWebTool: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("response is not JSON: %s", resp.Body)
	}
	return resp, out
}

func TestParseWebToolRequestResolvesEnginesAndValidates(t *testing.T) {
	cases := []struct {
		kind    string
		body    string
		engine  string
		wantErr string
	}{
		{"search", `{"query":"go generics"}`, "search.auto", ""},
		{"search", `{"query":"x","engine":"Brave"}`, "search.brave", ""},
		{"search", `{"query":"x","engine":"parallel","max_results":3,"freshness":"week"}`, "search.parallel", ""},
		{"search", `{"query":"  "}`, "", "query is required"},
		{"search", `{"query":"x","engine":"jina"}`, "", "unknown engine"},
		{"search", `{"query":"x","engine":7}`, "", "engine must be a string"},
		{"search", `{"query":"x","max_results":11}`, "", "max_results"},
		{"search", `{"query":"x","max_results":2.5}`, "", "max_results"},
		{"search", `{"query":"x","freshness":"hour"}`, "", "freshness"},
		{"fetch", `{"url":"https://example.com/a"}`, "fetch.jina", ""},
		{"fetch", `{"url":"https://example.com","engine":"firecrawl"}`, "fetch.firecrawl", ""},
		{"fetch", `{"url":"https://example.com","engine":"auto"}`, "", "unknown engine"},
		{"fetch", `{"url":"file:///etc/passwd"}`, "", "http or https"},
		{"fetch", `{"url":"https://user:pw@example.com"}`, "", "credentials"},
		{"fetch", `{"url":"/relative"}`, "", "http or https"},
		{"fetch", `{"url":"http://127.0.0.1:8080/"}`, "", "public address"},
		{"fetch", `{"url":"http://192.168.1.1"}`, "", "public address"},
		{"fetch", `{"url":"http://[::1]/"}`, "", "public address"},
		{"fetch", `{"url":"http://LocalHost./admin"}`, "", "public address"},
		{"fetch", `{"url":"http://1.1.1.1/"}`, "fetch.jina", ""},
		{"search", `{"query":123}`, "", "query must be a string"},
	}
	for _, tc := range cases {
		var body map[string]any
		if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
			t.Fatal(err)
		}
		req, err := parseWebToolRequest(tc.kind, body)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.body, err, tc.wantErr)
			}
			continue
		}
		if err != nil || req.engine.ID != tc.engine {
			t.Errorf("%s: engine = %q, err = %v, want %q", tc.body, req.engine.ID, err, tc.engine)
		}
	}
}

func TestForwardWebToolRejectsVendorOutsideItsKind(t *testing.T) {
	p := &openaiProvider{format: "brave"}
	req, err := parseWebToolRequest("fetch", map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forwardWebTool(context.Background(), p, req); err == nil {
		t.Fatal("a Brave account must not serve page fetch")
	}
}

func TestParallelSearchNormalizesResults(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"search_id":"s1","results":[
		{"url":"https://a.dev","title":"A","publish_date":"2026-09-01","excerpts":["one","two"]},
		{"url":"https://b.dev","title":null,"publish_date":null,"excerpts":[]}
	],"usage":[{"name":"sku_search","count":1}]}`, &call)
	resp, out := forwardWeb(t, p, "parallel", "search", `{"query":"rust async","max_results":5,"freshness":"month"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if call.method != http.MethodPost || call.path != "/v1/search" || call.header.Get("X-Api-Key") != "vendor-key" {
		t.Fatalf("call = %s %s key=%q", call.method, call.path, call.header.Get("X-Api-Key"))
	}
	settings := call.body["advanced_settings"].(map[string]any)
	if settings["max_results"].(float64) != 5 {
		t.Fatalf("max_results = %v", settings["max_results"])
	}
	if _, ok := settings["source_policy"].(map[string]any)["after_date"].(string); !ok {
		t.Fatalf("freshness did not become after_date: %v", settings)
	}
	if _, ok := call.body["model"]; ok {
		t.Fatal("the gateway model name must not reach the vendor")
	}
	if out["engine"] != "parallel" || out["object"] != "web.search" {
		t.Fatalf("envelope = %v", out)
	}
	results := out["results"].([]any)
	first := results[0].(map[string]any)
	if first["title"] != "A" || first["snippet"] != "one\n…\ntwo" || first["published_at"] != "2026-09-01" {
		t.Fatalf("first result = %v", first)
	}
	if second := results[1].(map[string]any); second["title"] != "" || second["url"] != "https://b.dev" {
		t.Fatalf("second result = %v", second)
	}
}

func TestBraveSearchNormalizesAndKeepsQuotaHeaders(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, map[string]string{
		"X-RateLimit-Policy":    "1;w=1, 2000;w=2592000",
		"X-RateLimit-Remaining": "0, 1500",
	}, `{"web":{"results":[{"title":"Tom &amp; Jerry","url":"https://c.dev","description":"desc","page_age":"2026-09-27T10:00:00","extra_snippets":["more"]}]}}`, &call)
	resp, out := forwardWeb(t, p, "brave", "search", `{"query":"cartoons","freshness":"day"}`)
	if call.method != http.MethodGet || call.path != "/res/v1/web/search" || call.header.Get("X-Subscription-Token") != "vendor-key" {
		t.Fatalf("call = %s %s token=%q", call.method, call.path, call.header.Get("X-Subscription-Token"))
	}
	for _, want := range []string{"q=cartoons", "count=10", "freshness=pd", "text_decorations=false"} {
		if !strings.Contains(call.query, want) {
			t.Fatalf("query %q lacks %q", call.query, want)
		}
	}
	first := out["results"].([]any)[0].(map[string]any)
	if first["title"] != "Tom & Jerry" || first["snippet"] != "desc\nmore" || first["published_at"] != "2026-09-27" {
		t.Fatalf("result = %v", first)
	}
	if resp.Header.Get("X-Ratelimit-Policy") == "" {
		t.Fatal("quota headers must reach the passive quota sampler")
	}
}

func TestBraveInvalidKeyFailsOverLikeUnauthorized(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 422, nil, `{"type":"ErrorResponse","error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"The provided subscription token is invalid.","status":422}}`, &call)
	resp, out := forwardWeb(t, p, "brave", "search", `{"query":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized || !generationShouldFailover(resp.StatusCode) {
		t.Fatalf("status = %d, want 401 so the next account is tried", resp.StatusCode)
	}
	if msg := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "subscription token is invalid") {
		t.Fatalf("message = %q", msg)
	}
}

func TestJinaFetchNormalizes(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"code":200,"status":20000,"data":{"title":"Example","url":"https://example.com/","content":"# Hi","httpStatus":200,"usage":{"tokens":12}}}`, &call)
	_, out := forwardWeb(t, p, "jina", "fetch", `{"url":"https://example.com"}`)
	if call.method != http.MethodPost || call.path != "/" || call.body["url"] != "https://example.com" {
		t.Fatalf("call = %s %s body=%v", call.method, call.path, call.body)
	}
	if call.header.Get("Authorization") != "Bearer vendor-key" || call.header.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", call.header)
	}
	if out["content"] != "# Hi" || out["title"] != "Example" || out["url"] != "https://example.com/" || out["status"].(float64) != 200 {
		t.Fatalf("out = %v", out)
	}
}

func TestJinaInsufficientBalanceFailsOver(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 402, nil, `{"code":402,"status":40203,"message":"InsufficientBalanceError","readableMessage":"Account balance not enough"}`, &call)
	resp, out := forwardWeb(t, p, "jina", "fetch", `{"url":"https://example.com"}`)
	if resp.StatusCode != 402 || !generationShouldFailover(resp.StatusCode) {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if msg := out["error"].(map[string]any)["message"]; msg != "jina: Account balance not enough" {
		t.Fatalf("message = %v", msg)
	}
}

func TestFirecrawlScrapeNormalizes(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"success":true,"data":{"markdown":"body","metadata":{"title":["T1","T2"],"sourceURL":"https://x.dev","url":"https://x.dev/final","statusCode":404}}}`, &call)
	_, out := forwardWeb(t, p, "firecrawl", "fetch", `{"url":"https://x.dev"}`)
	if call.path != "/v2/scrape" || call.header.Get("Authorization") != "Bearer vendor-key" {
		t.Fatalf("call = %s auth=%q", call.path, call.header.Get("Authorization"))
	}
	if formats := call.body["formats"].([]any); len(formats) != 1 || formats[0] != "markdown" {
		t.Fatalf("formats = %v", call.body["formats"])
	}
	if out["title"] != "T1" || out["url"] != "https://x.dev/final" || out["content"] != "body" || out["status"].(float64) != 404 {
		t.Fatalf("out = %v", out)
	}
}

func TestParallelExtractFailureIsNotRetried(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"extract_id":"e1","results":[],"errors":[{"url":"https://gone.dev","error_type":"fetch_error","http_status_code":404,"content":"not found"}]}`, &call)
	resp, out := forwardWeb(t, p, "parallel", "fetch", `{"url":"https://gone.dev"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity || generationShouldFailover(resp.StatusCode) {
		t.Fatalf("status = %d, want a 422 that goes back to the caller", resp.StatusCode)
	}
	if full := call.body["advanced_settings"].(map[string]any)["full_content"]; full != true {
		t.Fatalf("full_content = %v", full)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "fetch_failed" || !strings.Contains(e["message"].(string), "HTTP 404") {
		t.Fatalf("error = %v", e)
	}
}

func TestWebProbeSampleSummarizes(t *testing.T) {
	search := webProbeSample("search", []byte(`{"object":"web.search","results":[{"title":"A","url":"https://a.dev","snippet":"s"}]}`))
	if search != "1 条结果\nA — https://a.dev" {
		t.Fatalf("search sample = %q", search)
	}
	fetch := webProbeSample("fetch", []byte(`{"object":"web.fetch","title":"Example","content":"  hello  "}`))
	if fetch != "Example\nhello" {
		t.Fatalf("fetch sample = %q", fetch)
	}
}

func TestBraveSnippetDropsRepeatedParts(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"web":{"results":[{"title":"T","url":"https://c.dev","description":"Set a timeout per request...","extra_snippets":["Set a timeout per request, in seconds.","Other &#x27;part&#x27;.","Other 'part'."]}]}}`, &call)
	_, out := forwardWeb(t, p, "brave", "search", `{"query":"x"}`)
	if got := out["results"].([]any)[0].(map[string]any)["snippet"]; got != "Set a timeout per request, in seconds.\nOther 'part'." {
		t.Fatalf("snippet = %q", got)
	}
}

func TestJinaPageErrorsGoBackToCaller(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{403, `{"data":null,"path":"url","code":403,"name":"AbuseAlleviationError","status":40305,"readableMessage":"AbuseAlleviationError: Request to localhost or non-public IP: 10.0.0.1"}`},
		{422, `{"data":null,"path":"url","code":422,"name":"SubmittedDataMalformedError","status":42203,"readableMessage":"SubmittedDataMalformedError: Domain 'x.invalid' could not be resolved"}`},
	} {
		var call webVendorCall
		p := webVendorServer(t, tc.status, nil, tc.body, &call)
		resp, out := forwardWeb(t, p, "jina", "fetch", `{"url":"https://x.dev"}`)
		if resp.StatusCode != http.StatusUnprocessableEntity || generationShouldFailover(resp.StatusCode) {
			t.Fatalf("vendor %d: status = %d, want 422 without failover", tc.status, resp.StatusCode)
		}
		if e := out["error"].(map[string]any); e["code"] != "fetch_failed" || !strings.HasPrefix(e["message"].(string), "jina: ") {
			t.Fatalf("vendor %d: error = %v", tc.status, e)
		}
	}
}

func TestJinaPublishedTimeIsADate(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"data":{"title":"P","url":"https://x.dev","content":"c","publishedTime":"Fri, 12 Apr 2024 23:47:34 GMT"}}`, &call)
	if _, out := forwardWeb(t, p, "jina", "fetch", `{"url":"https://x.dev"}`); out["published_at"] != "2024-04-12" {
		t.Fatalf("published_at = %v", out["published_at"])
	}
	if got := datePart("2026-09-28T22:27:58+0000"); got != "2026-09-28" {
		t.Fatalf("datePart = %q", got)
	}
}

func TestFirecrawlUnsuccessfulScrapeIsFetchFailed(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"success":false,"code":"SCRAPE_DNS_RESOLUTION_ERROR","error":"DNS resolution failed for hostname"}`, &call)
	resp, out := forwardWeb(t, p, "firecrawl", "fetch", `{"url":"https://x.dev"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", resp.StatusCode, resp.Body)
	}
	if e := out["error"].(map[string]any); e["code"] != "fetch_failed" || e["message"] != "firecrawl: DNS resolution failed for hostname" {
		t.Fatalf("error = %v", e)
	}
}

func TestParallelExtractFallsBackToExcerpts(t *testing.T) {
	var call webVendorCall
	p := webVendorServer(t, 200, nil, `{"results":[{"url":"https://x.dev","title":"T","publish_date":"2026-09-28","excerpts":["one","two"],"full_content":""}],"errors":[]}`, &call)
	if _, out := forwardWeb(t, p, "parallel", "fetch", `{"url":"https://x.dev"}`); out["content"] != "one\n\ntwo" {
		t.Fatalf("out = %v", out)
	}
	p = webVendorServer(t, 200, nil, `{"results":[{"url":"https://x.dev","title":"T","excerpts":[],"full_content":""}],"errors":[]}`, &call)
	if resp, out := forwardWeb(t, p, "parallel", "fetch", `{"url":"https://x.dev"}`); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty page: status = %d out = %v", resp.StatusCode, out)
	}
}
