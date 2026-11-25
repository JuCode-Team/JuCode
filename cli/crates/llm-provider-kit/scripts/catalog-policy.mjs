// Which parts of the upstream catalog this crate ships.
//
// Upstream also covers subscription logins (Claude Pro/Max, ChatGPT/Codex,
// Kimi Code, SuperGrok, ...) and logins that reuse another vendor's OAuth
// client. This crate only signs in to providers with their official API keys,
// so those providers are dropped (auth rule, models, default model, api
// routes), and every remaining auth rule keeps an api-key login at most.

/** Providers removed outright. */
export const REMOVED_PROVIDERS = new Set([
	"openai-codex",
	"openai-codex-device",
	"zai-coding-plan",
	"kimi-code",
	"xai-oauth",
	"muse-code",
	"gitlab-duo",
	"gitlab-duo-agent",
	"github-copilot",
	"google-gemini-cli",
	"google-antigravity",
	"cursor",
	"devin",
	"qwen-portal",
	"charm-hyper",
]);

/** Api-key logins for kept providers whose upstream login is OAuth. */
const API_KEY_LOGINS = {
	anthropic: {
		name: "Anthropic",
		login: {
			kind: "api-key",
			authUrl: "https://console.anthropic.com/settings/keys",
			instructions: "Create or copy your API key from the Anthropic Console",
			prompt: "Paste your Anthropic API key",
			placeholder: "sk-ant-...",
		},
	},
	openrouter: {
		login: {
			kind: "api-key",
			authUrl: "https://openrouter.ai/keys",
			instructions: "Create or copy your API key from OpenRouter",
			prompt: "Paste your OpenRouter API key",
			placeholder: "sk-or-...",
		},
	},
};

/** Login kinds kept as they are; anything else is removed from the rule. */
const KEPT_LOGIN_KINDS = new Set(["api-key", "custom"]);

/** Applies the policy to a generated artifact (mutates and returns it). */
export function cutDown(artifact) {
	artifact.authProviders = artifact.authProviders
		.filter((p) => !REMOVED_PROVIDERS.has(p.id))
		.map((p) => {
			const { refresh, callbackPort, pasteCode, result, storeAs, ...rule } = p;
			Object.assign(rule, API_KEY_LOGINS[p.id] ?? {});
			if (rule.login && !KEPT_LOGIN_KINDS.has(rule.login.kind)) delete rule.login;
			return rule;
		});
	artifact.apiRoutes = artifact.apiRoutes.filter((r) => !REMOVED_PROVIDERS.has(r.provider));
	for (const table of [artifact.models, artifact.defaultModels]) {
		for (const id of Object.keys(table)) {
			if (REMOVED_PROVIDERS.has(id)) delete table[id];
		}
	}
	return artifact;
}
