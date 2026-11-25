package biz

import "github.com/jucode/jucode/internal/data/ent/schema"

// Built-in provider definitions: the vendors worth shipping a working
// configuration for. Each one is a plain payload — the same shape an operator
// authors in the console — so seeding it into the definition table is the only
// difference between "shipped" and "written by hand".

// BuiltinDefinition is a definition that ships with the build.
type BuiltinDefinition struct {
	DefinitionID    string
	Name            string
	Vendor          string
	BaseURL         string
	APIFormat       string
	NativeProtocols []string
	DefaultProtocol string
	EndpointPaths   map[string]any
	HeaderRules     schema.HeaderRules
	CredentialLabel string
	CredentialHint  string
	ModelsJSON      string
	// ModelsPath overrides the upstream model-list endpoint for account
	// model sync ("" = the /v1/models convention, ModelsPathNone = sync
	// binds ModelsJSON).
	ModelsPath string
	Note       string
}

// ModelsPathNone declares a vendor without a model-list endpoint: account
// model sync binds the definition's own model list instead of fetching one.
const ModelsPathNone = "none"

// AzureFoundryClaudeDefinitionID marks Claude-on-Foundry accounts; the gateway
// strips the request fields Foundry rejects for them.
const AzureFoundryClaudeDefinitionID = "azure-foundry-claude"

// AzureOpenAIDefinitionID marks Azure OpenAI accounts, reached directly or
// through a relay; the gateway strips the Responses input fields Azure rejects
// for them.
const AzureOpenAIDefinitionID = "azure-openai"

// azureFoundryModels are Foundry's default deployment names (they equal the
// Claude model ids), per the Claude in Microsoft Foundry model table. A
// deployment renamed in the portal is bound by hand on the account.
const azureFoundryModels = `[
  {"name": "claude-fable-5-1"},
  {"name": "claude-fable-5"},
  {"name": "claude-opus-5-5"},
  {"name": "claude-opus-5"},
  {"name": "claude-opus-4-8"},
  {"name": "claude-opus-4-7"},
  {"name": "claude-opus-4-6"},
  {"name": "claude-opus-4-5"},
  {"name": "claude-sonnet-5"},
  {"name": "claude-sonnet-4-6"},
  {"name": "claude-sonnet-4-5"},
  {"name": "claude-haiku-4-5"}
]`

// Web tool vendors (llm/web_tools.go, biz/web_tools.go). Their accounts speak
// only the "web" protocol and bind no models: an engine is served by every
// account whose api_format names one of its vendors.

func builtinDefinitions() []BuiltinDefinition {
	return []BuiltinDefinition{
		{
			// Claude in Microsoft Foundry: the native Messages API under
			// {resource}.services.ai.azure.com/anthropic, keyed by `api-key` or
			// `x-api-key` (both accepted, so the gateway's default X-API-Key
			// works as is). It has no Models API, hence ModelsPathNone, and it
			// rejects the whole request when anthropic-beta carries a flag it
			// does not support: the advisor tool is documented as unsupported
			// and Claude Code sends advisor-tool-* by default.
			DefinitionID:    AzureFoundryClaudeDefinitionID,
			Name:            "Azure AI Foundry（Claude）",
			Vendor:          "azure",
			BaseURL:         "https://{resource}.services.ai.azure.com/anthropic",
			NativeProtocols: []string{"messages"},
			DefaultProtocol: "messages",
			HeaderRules: schema.HeaderRules{
				DropValues: map[string][]string{"Anthropic-Beta": {"advisor-tool-*"}},
			},
			ModelsJSON:      azureFoundryModels,
			ModelsPath:      ModelsPathNone,
			CredentialLabel: "API Key",
			CredentialHint:  "Foundry 门户 → Build → Models → 部署的 Details 页里的 Key。",
			Note:            "base_url 里的 {resource} 换成 Foundry 资源名。Foundry 不提供模型列表接口，同步只绑定内置清单；自定义部署名在「支持模型」里手动添加。上游仍报 Unexpected value(s) 时，把报错里的 beta 名加进 header 规则的 drop_values。",
		},
		{
			// Azure OpenAI v1 API: {resource}.openai.azure.com/openai/v1/...,
			// keyed by `api-key`. Its Responses endpoint rejects any input
			// field it does not know, including the internal metadata Codex
			// 0.164 started sending, which the gateway strips for this type.
			DefinitionID:    AzureOpenAIDefinitionID,
			Name:            "Azure OpenAI",
			Vendor:          "azure",
			BaseURL:         "https://{resource}.openai.azure.com/openai",
			APIFormat:       "openai",
			NativeProtocols: []string{"responses", "chat_completions"},
			DefaultProtocol: "responses",
			HeaderRules: schema.HeaderRules{
				Remove: []string{"Authorization"},
				Set:    map[string]string{"api-key": "{api_key}"},
			},
			CredentialLabel: "API Key",
			CredentialHint:  "Azure 门户 → Azure OpenAI 资源 → Keys and Endpoint 里的 KEY 1 或 KEY 2。",
			Note:            "base_url 里的 {resource} 换成 Azure OpenAI 资源名。模型名填部署名。经中转接入 Azure 时也选此类型，并把 base_url 改成中转地址、header 规则清空。",
		},
		{
			DefinitionID:    "parallel",
			Name:            "Parallel（网页搜索与抓取）",
			Vendor:          "parallel",
			BaseURL:         "https://api.parallel.ai",
			APIFormat:       "parallel",
			NativeProtocols: []string{"web"},
			DefaultProtocol: "web",
			ModelsPath:      ModelsPathNone,
			CredentialLabel: "API Key",
			CredentialHint:  "platform.parallel.ai → Settings → API Keys。",
			Note:            "服务 /tools/v1/search（Search API）和 /tools/v1/fetch（Extract API）。Parallel 没有余额查询接口，余额在 platform.parallel.ai 控制台查看。",
		},
		{
			DefinitionID:    "brave-search",
			Name:            "Brave Search（网页搜索）",
			Vendor:          "brave",
			BaseURL:         "https://api.search.brave.com",
			APIFormat:       "brave",
			NativeProtocols: []string{"web"},
			DefaultProtocol: "web",
			ModelsPath:      ModelsPathNone,
			CredentialLabel: "Subscription Token",
			CredentialHint:  "api-dashboard.search.brave.com → API Keys。",
			Note:            "服务 /tools/v1/search。Brave 没有余额查询接口，每次响应的 X-RateLimit-* 头会带上月度额度，网关据此被动更新账号额度。",
		},
		{
			DefinitionID:    "jina-reader",
			Name:            "Jina Reader（网页抓取）",
			Vendor:          "jina",
			BaseURL:         "https://r.jina.ai",
			APIFormat:       "jina",
			NativeProtocols: []string{"web"},
			DefaultProtocol: "web",
			ModelsPath:      ModelsPathNone,
			CredentialLabel: "API Key",
			CredentialHint:  "jina.ai/api-dashboard 里的 API Key。",
			Note:            "服务 /tools/v1/fetch。Jina 没有公开的余额查询接口，余额在 jina.ai 控制台查看。",
		},
		{
			DefinitionID:    "firecrawl",
			Name:            "Firecrawl（网页抓取）",
			Vendor:          "firecrawl",
			BaseURL:         "https://api.firecrawl.dev",
			APIFormat:       "firecrawl",
			NativeProtocols: []string{"web"},
			DefaultProtocol: "web",
			ModelsPath:      ModelsPathNone,
			CredentialLabel: "API Key",
			CredentialHint:  "firecrawl.dev/app/api-keys 里的 fc- 开头的 Key。",
			Note:            "服务 /tools/v1/fetch（v2 scrape）。剩余额度从 /v2/team/credit-usage 读取。",
		},
	}
}

// BuiltinDefinitionByID returns a shipped definition.
func BuiltinDefinitionByID(definitionID string) (BuiltinDefinition, bool) {
	for _, def := range builtinDefinitions() {
		if def.DefinitionID == definitionID {
			return def, true
		}
	}
	return BuiltinDefinition{}, false
}
