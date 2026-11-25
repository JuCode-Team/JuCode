//! JuCode's own provider template, layered over the shared kit's built-ins.
//!
//! The gateway endpoint is JuCode's, so it lives here rather than in
//! `llm-provider-kit`; everything else comes from the kit's template list.

use llm_provider_kit::{Protocol, ProviderTemplate};

/// The JuCode gateway: the OpenAI Responses API of a self-hosted JuCode
/// server. It has no default address (`/login <web-url>` or
/// `jucode_api_url` sets one) and no built-in models: they come from the
/// server's `/v1/models` at login.
pub const JUCODE_TEMPLATE: ProviderTemplate = ProviderTemplate {
    id: "jucode",
    base_url: "",
    protocol: Protocol::OpenAiResponses,
    models: &[],
};

/// Template lookup: the JuCode gateway first, then the kit's built-ins.
pub fn template(id: &str) -> Option<&'static ProviderTemplate> {
    if id == JUCODE_TEMPLATE.id {
        return Some(&JUCODE_TEMPLATE);
    }
    llm_provider_kit::providers::template(id)
}

/// Every template in picker order — JuCode's gateway, then the kit's.
pub fn templates() -> impl Iterator<Item = &'static ProviderTemplate> {
    std::iter::once(&JUCODE_TEMPLATE).chain(llm_provider_kit::templates())
}
