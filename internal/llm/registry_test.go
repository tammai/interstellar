package llm

import (
	"errors"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/store"
)

func TestParseModelRef(t *testing.T) {
	tests := []struct {
		in       string
		provider string
		model    string
		wantErr  bool
	}{
		{"deepseek/deepseek-chat", "deepseek", "deepseek-chat", false},
		{"ollama/llama3", "ollama", "llama3", false},
		{" deepseek / deepseek-chat ", "deepseek", "deepseek-chat", false},
		// OpenRouter model names contain slashes, so only the first one separates.
		{"openrouter/anthropic/claude-sonnet-4.6", "openrouter", "anthropic/claude-sonnet-4.6", false},

		{"no-slash", "", "", true},
		{"/model", "", "", true},
		{"provider/", "", "", true},
	}

	for _, tc := range tests {
		got, err := ParseModelRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseModelRef(%q) succeeded, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseModelRef(%q): %v", tc.in, err)
			continue
		}
		if got.Provider != tc.provider || got.Model != tc.model {
			t.Errorf("ParseModelRef(%q) = %s/%s, want %s/%s", tc.in, got.Provider, got.Model, tc.provider, tc.model)
		}
	}
}

// An empty ref is an unconfigured role, not a malformed one -- the difference
// between "finish setup" and "you typed it wrong".
func TestEmptyModelRefIsUnconfiguredNotInvalid(t *testing.T) {
	ref, err := ParseModelRef("")
	if err != nil {
		t.Fatalf("empty ref should not error: %v", err)
	}
	if !ref.IsZero() {
		t.Error("empty ref should report IsZero")
	}
}

func TestProviderEndpoint(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1/", "https://api.deepseek.com/v1/chat/completions"},
	} {
		p := Provider{Name: "x", BaseURL: tc.base}
		if got := p.Endpoint(); got != tc.want {
			t.Errorf("Endpoint() for %q = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestKeylessProvider(t *testing.T) {
	p := Provider{Name: "ollama", BaseURL: "http://x", APIKeyEnv: ""}
	if !p.Keyless() {
		t.Error("empty api_key_env should be keyless")
	}
	if _, ok := p.APIKey(); !ok {
		t.Error("keyless provider should report a usable key state")
	}
}

func TestProviderAPIKeyReadsEnvAtCallTime(t *testing.T) {
	p := Provider{Name: "p", BaseURL: "http://x", APIKeyEnv: "TEST_KEY_LATE"}

	if _, ok := p.APIKey(); ok {
		t.Error("unset variable reported as present")
	}

	// The value is read on demand, so setting it later takes effect with no
	// reconstruction -- the mechanism behind "no restart".
	t.Setenv("TEST_KEY_LATE", "sk-late")
	got, ok := p.APIKey()
	if !ok || got != "sk-late" {
		t.Errorf("APIKey() = %q, %v", got, ok)
	}
}

func TestParseRegistryRejectsBadEntries(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"","base_url":"http://x"}]`,
		`[{"name":"p"}]`,
		`[{"name":"p","base_url":"http://x"},{"name":"p","base_url":"http://y"}]`,
		`not json`,
	} {
		if _, err := ParseRegistry(raw); err == nil {
			t.Errorf("ParseRegistry(%q) succeeded, want error", raw)
		}
	}
}

func TestParseRegistryEmptyIsUsable(t *testing.T) {
	r, err := ParseRegistry("")
	if err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	if len(r.Providers()) != 0 {
		t.Error("empty registry should have no providers")
	}
}

// The shipped defaults must parse and be internally consistent: every role must
// name a provider that actually exists in the registry.
func TestShippedDefaultsAreCoherent(t *testing.T) {
	db := openTempStore(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	settings, err := db.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}

	cfg, err := LoadConfig(settings)
	if err != nil {
		t.Fatalf("LoadConfig on shipped defaults: %v", err)
	}

	for _, name := range []string{"deepseek", "anthropic", "google", "openrouter", "litellm", "ollama"} {
		if _, ok := cfg.Registry.Lookup(name); !ok {
			t.Errorf("preset provider %q missing from the registry", name)
		}
	}

	for role, chain := range cfg.Roles {
		if chain.Primary.IsZero() {
			t.Errorf("role %s ships unconfigured", role)
			continue
		}
		if _, err := cfg.Registry.Resolve(chain.Primary); err != nil {
			t.Errorf("role %s points at a provider not in the registry: %v", role, err)
		}
	}

	// §3.1's cheap-first decision.
	if got := cfg.Roles[RoleClassifier].Primary.String(); got != "deepseek/deepseek-chat" {
		t.Errorf("classifier default = %q, want deepseek/deepseek-chat", got)
	}
	if got := cfg.Roles[RoleSynthesizer].Primary.String(); got != "deepseek/deepseek-chat" {
		t.Errorf("synthesizer default = %q, want deepseek/deepseek-chat", got)
	}
}

// Readiness must name the missing variable, not just say "not ready".
func TestReadinessExplainsWhatIsMissing(t *testing.T) {
	cfg, err := LoadConfig(map[string]string{
		store.KeyProviders:        `[{"name":"deepseek","base_url":"https://x","api_key_env":"UNSET_FOR_TEST_ABC"}]`,
		store.KeyClassifierModel:  "deepseek/deepseek-chat",
		store.KeySynthesizerModel: "nonexistent/model",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Ready() {
		t.Error("config with an unset key reported ready")
	}

	statuses := cfg.Readiness()
	byRole := map[string]RoleStatus{}
	for _, s := range statuses {
		byRole[s.Role] = s
	}

	if got := byRole["classifier"].Problem; !strings.Contains(got, "UNSET_FOR_TEST_ABC") {
		t.Errorf("classifier problem = %q, should name the missing variable", got)
	}
	if got := byRole["synthesizer"].Problem; !strings.Contains(got, "nonexistent") {
		t.Errorf("synthesizer problem = %q, should name the unknown provider", got)
	}
}

func TestReadinessPassesWhenConfigured(t *testing.T) {
	t.Setenv("PRESENT_KEY", "sk-here")
	cfg, err := LoadConfig(map[string]string{
		store.KeyProviders:        `[{"name":"p","base_url":"https://x","api_key_env":"PRESENT_KEY"}]`,
		store.KeyClassifierModel:  "p/a",
		store.KeySynthesizerModel: "p/b",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Ready() {
		t.Errorf("fully configured setup reported not ready: %+v", cfg.Readiness())
	}
}

// A typo in settings must surface before a run starts, not three minutes in.
func TestLoadConfigRejectsMalformedSettings(t *testing.T) {
	for name, settings := range map[string]map[string]string{
		"bad provider json": {store.KeyProviders: `[[[`},
		"bad model ref":     {store.KeyProviders: `[]`, store.KeyClassifierModel: "no-slash"},
		"bad fallback json": {store.KeyProviders: `[]`, store.KeyClassifierFallback: `{"not":"an array"}`},
		"bad fallback ref":  {store.KeyProviders: `[]`, store.KeyClassifierFallback: `["no-slash"]`},
	} {
		if _, err := LoadConfig(settings); err == nil {
			t.Errorf("%s: LoadConfig succeeded, want error", name)
		}
	}
}

func TestParseModelRefsSkipsBlanks(t *testing.T) {
	refs, err := ParseModelRefs(`["a/b", "", "c/d"]`)
	if err != nil {
		t.Fatalf("ParseModelRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("got %d refs, want 2", len(refs))
	}
}

func openTempStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Absent means ON. The registry is an unmigrated JSON blob, so every entry
// written before the flag existed has no "disabled" key -- and reading those as
// off would switch a running instance's whole registry off on upgrade.
func TestProviderWithoutTheFlagIsActive(t *testing.T) {
	r, err := ParseRegistry(`[{"name":"p","base_url":"https://x","api_key_env":"K"}]`)
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	p, _ := r.Lookup("p")
	if !p.Active() {
		t.Error("a provider entry with no disabled flag came back switched off")
	}
	if _, err := r.Resolve(ModelRef{"p", "m"}); err != nil {
		t.Errorf("Resolve refused an entry with no disabled flag: %v", err)
	}
}

// Lookup still sees a disabled provider -- the settings screen has to list it to
// switch it back on -- while Resolve, the routing path, refuses it.
func TestDisabledProviderListsButDoesNotResolve(t *testing.T) {
	r, err := ParseRegistry(`[{"name":"p","base_url":"https://x","api_key_env":"K","disabled":true}]`)
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if _, ok := r.Lookup("p"); !ok {
		t.Fatal("a switched-off provider vanished from Lookup, so nothing could switch it back on")
	}
	if len(r.Providers()) != 1 {
		t.Fatalf("Providers() returned %d entries, want the switched-off one listed", len(r.Providers()))
	}
	_, err = r.Resolve(ModelRef{"p", "m"})
	if !errors.Is(err, ErrProviderDisabled) {
		t.Errorf("Resolve error = %v, want ErrProviderDisabled", err)
	}
}

// A role standing on a switched-off provider is not ready, and says what to do.
func TestReadinessExplainsASwitchedOffProvider(t *testing.T) {
	t.Setenv("PRESENT_KEY", "sk-here")
	cfg, err := LoadConfig(map[string]string{
		store.KeyProviders:        `[{"name":"p","base_url":"https://x","api_key_env":"PRESENT_KEY","disabled":true}]`,
		store.KeyClassifierModel:  "p/a",
		store.KeySynthesizerModel: "p/b",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Ready() {
		t.Error("a role on a switched-off provider reported ready")
	}
	got := cfg.RoleReadiness(RoleClassifier).Problem
	if !strings.Contains(got, "switched off") || !strings.Contains(got, "p") {
		t.Errorf("problem = %q, should name the provider and say it is switched off", got)
	}
}

// Only the primary counts. A provider that appears solely in a fallback chain is
// free to switch off -- that is the ordinary use of the switch.
func TestRolesUsingCountsPrimariesOnly(t *testing.T) {
	cfg, err := LoadConfig(map[string]string{
		store.KeyProviders: `[{"name":"a","base_url":"https://x","api_key_env":"K"},
		                      {"name":"b","base_url":"https://y","api_key_env":"K"}]`,
		store.KeyClassifierModel:    "a/one",
		store.KeyClassifierFallback: `["b/two"]`,
		store.KeySynthesizerModel:   "a/three",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.RolesUsing("b"); len(got) != 0 {
		t.Errorf("RolesUsing(fallback-only provider) = %v, want none", got)
	}
	got := cfg.RolesUsing("a")
	if len(got) != 2 || got[0] != RoleClassifier || got[1] != RoleSynthesizer {
		t.Errorf("RolesUsing = %v, want classifier and synthesizer in that order", got)
	}
}
