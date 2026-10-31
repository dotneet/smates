package agent

import (
	"os"
	"strings"

	"github.com/dotneet/smates/internal/config"
)

// Providers of the built-in backend (agent.provider).
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderOpenRouter = "openrouter"
)

// provider describes one API the built-in backend can talk to.
type provider struct {
	name    string
	model   string // default model
	baseURL string // default base URL
	keyEnv  string
	// tokenEnv holds a bearer token used when keyEnv is empty ("" when unsupported).
	tokenEnv   string
	baseURLEnv string
	dialect    dialect
}

// providers in auto-selection order: the first one with credentials is used.
var providers = []provider{
	{name: ProviderAnthropic, model: DefaultModel, baseURL: defaultBaseURL, keyEnv: envAPIKey, tokenEnv: envAuthToken, baseURLEnv: envBaseURL, dialect: anthropicDialect{}},
	{name: ProviderOpenAI, model: "gpt-6-luna", baseURL: "https://api.openai.com/v1", keyEnv: "OPENAI_API_KEY", baseURLEnv: "OPENAI_BASE_URL", dialect: responsesDialect{}},
	{name: ProviderOpenRouter, model: "anthropic/claude-haiku-5.5", baseURL: "https://openrouter.ai/api/v1", keyEnv: "OPENROUTER_API_KEY", baseURLEnv: "OPENROUTER_BASE_URL", dialect: chatDialect{}},
}

func lookupProvider(name string) (provider, bool) {
	for _, p := range providers {
		if p.name == name {
			return p, true
		}
	}
	return provider{}, false
}

func (p provider) configured() bool {
	return os.Getenv(p.keyEnv) != "" || (p.tokenEnv != "" && os.Getenv(p.tokenEnv) != "")
}

// credentialHint names the variables that configure p, for error messages.
func (p provider) credentialHint() string {
	if p.tokenEnv != "" {
		return p.keyEnv + " (or " + p.tokenEnv + ")"
	}
	return p.keyEnv
}

// explicitProvider returns the provider agent.provider names, if any.
func explicitProvider(cfg config.AgentConfig) (provider, bool) {
	name := strings.TrimSpace(cfg.Provider)
	if name == "" || name == "auto" {
		return provider{}, false
	}
	return lookupProvider(name)
}

// providerOf returns the provider the built-in backend uses with cfg: agent.provider
// when set, otherwise the first provider whose credentials are in the environment,
// otherwise Anthropic.
func providerOf(cfg config.AgentConfig) provider {
	if p, ok := explicitProvider(cfg); ok {
		return p
	}
	for _, p := range providers {
		if p.configured() {
			return p
		}
	}
	return providers[0]
}

// ProviderOf returns the name of the provider the built-in backend uses with cfg.
func ProviderOf(cfg config.AgentConfig) string { return providerOf(cfg).name }

// ModelOf returns the model the built-in backend uses with cfg: agent.model, or the
// default model of the selected provider.
func ModelOf(cfg config.AgentConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	return providerOf(cfg).model
}

// CredentialEnv lists the environment variables holding the credentials of the
// provider selected by cfg. They are smates' own secrets: test processes do not get
// them. Other providers' variables are left alone, since tests may use them.
func CredentialEnv(cfg config.AgentConfig) []string {
	p := providerOf(cfg)
	if p.tokenEnv != "" {
		return []string{p.keyEnv, p.tokenEnv}
	}
	return []string{p.keyEnv}
}

// missingCredentials explains why the built-in backend cannot run with cfg.
func missingCredentials(cfg config.AgentConfig) string {
	if p, ok := explicitProvider(cfg); ok {
		return "no credentials: set " + p.credentialHint() + " for agent.provider " + p.name + ", or configure agent.command"
	}
	hints := make([]string, len(providers))
	for i, p := range providers {
		hints[i] = p.credentialHint()
	}
	return "no credentials: set " + strings.Join(hints[:len(hints)-1], ", ") + " or " + hints[len(hints)-1] + ", or configure agent.command"
}
