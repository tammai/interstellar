// Package llm talks to language models.
//
// §3.1 is a hard requirement: either role must be able to run on any model from
// any provider, switched in settings without touching code or restarting. That
// shapes everything here.
//
//   - There is exactly ONE transport: OpenAI-compatible chat completions. That
//     single contract covers DeepSeek, Google, Anthropic's compat layer,
//     OpenRouter, LiteLLM, Groq, Together, and Ollama. Native cloud SDKs are
//     explicitly out of scope -- if Bedrock/Vertex/Foundry ever matter, the
//     answer is a registry entry pointing at a LiteLLM gateway, never a vendor
//     dependency in this package.
//   - Providers are DATA, not code. Adding one is a settings edit.
//   - API keys are referenced by env var NAME. The value is read at call time
//     and never stored, returned, or logged.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Provider is one registry entry.
type Provider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// APIKeyEnv is the NAME of the environment variable holding the key -- never
	// the key. Empty means keyless (Ollama), and no Authorization header is sent.
	APIKeyEnv string `json:"api_key_env"`
	// Disabled takes a provider out of service without deleting it. Deleting is
	// the lossy option: it drops the base URL and the env var name, so switching
	// a vendor off for an afternoon and back on again means retyping both.
	//
	// Stored as "disabled" rather than "enabled" so that ABSENT means ON. The
	// registry is a JSON blob inside `settings` with no migration path, and every
	// entry already written by every instance predates this field -- with an
	// "enabled" flag, the zero value would read as off and a running instance
	// would wake up with its whole registry switched off. The API inverts it back
	// to `active`, matching topics and sources; see providerDTO.
	Disabled bool `json:"disabled,omitempty"`
}

// Active reports whether this provider may be routed to.
func (p Provider) Active() bool { return !p.Disabled }

// Keyless reports whether this provider needs no credential.
func (p Provider) Keyless() bool { return strings.TrimSpace(p.APIKeyEnv) == "" }

// APIKey reads the referenced environment variable. The second return is false
// when the variable is unset or empty, which is a readiness problem to surface
// in the UI rather than a call to attempt and fail.
func (p Provider) APIKey() (string, bool) {
	if p.Keyless() {
		return "", true
	}
	v := strings.TrimSpace(os.Getenv(p.APIKeyEnv))
	return v, v != ""
}

// Endpoint returns the chat completions URL for this provider.
func (p Provider) Endpoint() string {
	return strings.TrimSuffix(p.BaseURL, "/") + "/chat/completions"
}

// Registry is the set of configured providers, keyed by name.
type Registry struct {
	providers []Provider
	byName    map[string]Provider
}

// ParseRegistry reads the providers setting.
func ParseRegistry(raw string) (*Registry, error) {
	var list []Provider
	if strings.TrimSpace(raw) == "" {
		list = nil
	} else if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("providers setting is not valid JSON: %w", err)
	}

	r := &Registry{byName: make(map[string]Provider, len(list))}
	for _, p := range list {
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		if p.Name == "" {
			return nil, fmt.Errorf("provider entry with no name")
		}
		if p.BaseURL == "" {
			return nil, fmt.Errorf("provider %q has no base_url", p.Name)
		}
		if _, dup := r.byName[p.Name]; dup {
			return nil, fmt.Errorf("duplicate provider %q", p.Name)
		}
		r.byName[p.Name] = p
		r.providers = append(r.providers, p)
	}
	return r, nil
}

// Providers returns the entries in configured order.
func (r *Registry) Providers() []Provider { return r.providers }

// Lookup finds a provider by name.
func (r *Registry) Lookup(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// ModelRef is a "provider/model" pair, the form roles and fallback chains use.
type ModelRef struct {
	Provider string
	Model    string
}

func (m ModelRef) String() string { return m.Provider + "/" + m.Model }

// IsZero reports whether the ref is unset -- an unconfigured role rather than a
// malformed one.
func (m ModelRef) IsZero() bool { return m.Provider == "" && m.Model == "" }

// ParseModelRef splits "provider/model". The model half may itself contain
// slashes, which OpenRouter model names routinely do ("openrouter/anthropic/
// claude-sonnet-4.6"), so only the first slash is a separator.
func ParseModelRef(s string) (ModelRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ModelRef{}, nil // unconfigured, not invalid
	}
	provider, model, found := strings.Cut(s, "/")
	if !found {
		return ModelRef{}, fmt.Errorf("model %q must be written provider/model", s)
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return ModelRef{}, fmt.Errorf("model %q must be written provider/model", s)
	}
	return ModelRef{Provider: provider, Model: model}, nil
}

// ParseModelRefs reads a JSON array of "provider/model" strings, used for
// fallback chains.
func ParseModelRefs(raw string) ([]ModelRef, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("fallback chain is not a JSON array of strings: %w", err)
	}
	out := make([]ModelRef, 0, len(list))
	for _, s := range list {
		ref, err := ParseModelRef(s)
		if err != nil {
			return nil, err
		}
		if ref.IsZero() {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}

// ErrProviderDisabled is returned by Resolve for a provider the owner has
// switched off.
//
// A distinct error because the two failures deserve different treatment: a ref
// naming a provider that is not in the registry is a mistake worth recording as
// a failed attempt, while a ref naming one that is deliberately off is not a
// failure at all and must not be written to llm_calls. See Client.CompleteAs.
var ErrProviderDisabled = errors.New("provider is switched off")

// Resolve pairs a ref with its provider, refusing one that is switched off.
//
// Lookup deliberately still returns disabled entries -- the settings screen has
// to list them to switch them back on. Resolve is the routing path, and that is
// where "off" has to mean something.
func (r *Registry) Resolve(ref ModelRef) (Provider, error) {
	p, ok := r.Lookup(ref.Provider)
	if !ok {
		return Provider{}, fmt.Errorf("no provider named %q in the registry", ref.Provider)
	}
	if p.Disabled {
		return Provider{}, fmt.Errorf("%w: %s", ErrProviderDisabled, p.Name)
	}
	return p, nil
}
