package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tammai/interstellar/internal/store"
)

// fakeRecorder stands in for the database so this package tests without one.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []store.LLMCall
	// spentToday is what the daily cap sees.
	spentToday int
	failWrites bool
}

func (f *fakeRecorder) RecordLLMCall(rec store.LLMCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrites {
		return errors.New("disk on fire")
	}
	f.calls = append(f.calls, rec)
	return nil
}

func (f *fakeRecorder) LLMCallsOn(string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spentToday, nil
}

func (f *fakeRecorder) recorded() []store.LLMCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.LLMCall(nil), f.calls...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// modelServer serves an OpenAI-compatible response, recording what it received.
func modelServer(t *testing.T, reply string, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var seenAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
			"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &seenAuth
}

func registryOf(t *testing.T, providers ...Provider) *Registry {
	t.Helper()
	raw, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRegistry(string(raw))
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	return r
}

func TestCompleteSucceedsOnPrimary(t *testing.T) {
	srv, auths := modelServer(t, "hello", http.StatusOK)
	t.Setenv("PRIMARY_KEY", "sk-primary")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "primary", BaseURL: srv.URL, APIKeyEnv: "PRIMARY_KEY"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"primary", "m1"}}},
	}
	rec := &fakeRecorder{}
	c := NewClient(cfg, rec, "2026-08-11", quiet())

	resp, err := c.Complete(context.Background(), RoleClassifier, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("text = %q", resp.Text)
	}
	if resp.Provider != "primary" || resp.Model != "m1" {
		t.Errorf("attribution = %s/%s", resp.Provider, resp.Model)
	}
	if resp.Usage.TokensIn != 11 || resp.Usage.TokensOut != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if got := (*auths)[0]; got != "Bearer sk-primary" {
		t.Errorf("Authorization = %q", got)
	}

	calls := rec.recorded()
	if len(calls) != 1 || !calls[0].OK || calls[0].Provider != "primary" {
		t.Errorf("recorded %+v", calls)
	}
}

// The behavior §3.1 exists for: a chain that crosses vendors, rebuilding base URL
// and auth per hop.
func TestCompleteFallsBackAcrossProviders(t *testing.T) {
	down, _ := modelServer(t, "", http.StatusInternalServerError)
	up, upAuths := modelServer(t, "rescued", http.StatusOK)

	t.Setenv("DOWN_KEY", "sk-down")
	t.Setenv("UP_KEY", "sk-up")

	cfg := Config{
		Registry: registryOf(t,
			Provider{Name: "down", BaseURL: down.URL, APIKeyEnv: "DOWN_KEY"},
			Provider{Name: "up", BaseURL: up.URL, APIKeyEnv: "UP_KEY"},
		),
		Roles: map[Role]Chain{
			RoleSynthesizer: {
				Primary:  ModelRef{"down", "big-model"},
				Fallback: []ModelRef{{"up", "other-model"}},
			},
		},
	}
	rec := &fakeRecorder{}
	c := NewClient(cfg, rec, "2026-08-11", quiet())

	resp, err := c.Complete(context.Background(), RoleSynthesizer, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "rescued" {
		t.Errorf("text = %q, want the fallback's response", resp.Text)
	}
	if resp.Provider != "up" {
		t.Errorf("provider = %q, want up", resp.Provider)
	}
	// Auth must be rebuilt for the new vendor, not carried over.
	if got := (*upAuths)[0]; got != "Bearer sk-up" {
		t.Errorf("fallback Authorization = %q, want the fallback provider's key", got)
	}

	// Every attempt is logged, so a silent cross-vendor fallback is visible after
	// the fact.
	calls := rec.recorded()
	if len(calls) < 3 {
		t.Fatalf("recorded %d attempts, want the failures plus the success: %+v", len(calls), calls)
	}
	var sawFailedDown, sawOKUp bool
	for _, c := range calls {
		if c.Provider == "down" && !c.OK {
			sawFailedDown = true
		}
		if c.Provider == "up" && c.OK {
			sawOKUp = true
		}
	}
	if !sawFailedDown || !sawOKUp {
		t.Errorf("cost log does not show the fallback: %+v", calls)
	}
}

// A 400 will fail identically forever; retrying it just delays the fallback.
func TestCompleteDoesNotRetryNonRetryableStatus(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier, Request{}); err == nil {
		t.Fatal("expected failure")
	}
	if hits != 1 {
		t.Errorf("provider hit %d times, want 1 (400 is not retryable)", hits)
	}
}

func TestCompleteRetriesTransientThenSucceeds(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	resp, err := c.Complete(context.Background(), RoleClassifier, Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" || hits != 2 {
		t.Errorf("text=%q hits=%d, want a retry then success", resp.Text, hits)
	}
}

// A keyless provider (Ollama) must send no Authorization header at all -- an
// empty Bearer is rejected by some servers.
func TestKeylessProviderSendsNoAuthHeader(t *testing.T) {
	srv, auths := modelServer(t, "local", http.StatusOK)

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "ollama", BaseURL: srv.URL, APIKeyEnv: ""}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"ollama", "llama3"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier, Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := (*auths)[0]; got != "" {
		t.Errorf("Authorization = %q, want none for a keyless provider", got)
	}
}

// An unset env var is a config problem: fail fast, don't retry, and say which
// variable is missing.
func TestMissingEnvVarIsNotRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "DEFINITELY_UNSET_KEY_XYZ"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	_, err := c.Complete(context.Background(), RoleClassifier, Request{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_UNSET_KEY_XYZ") {
		t.Errorf("error should name the missing variable, got: %v", err)
	}
	if hits != 0 {
		t.Errorf("provider was contacted %d times with no key", hits)
	}
}

func TestUnconfiguredRoleIsDistinguishable(t *testing.T) {
	cfg := Config{
		Registry: registryOf(t),
		Roles:    map[Role]Chain{RoleClassifier: {}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	_, err := c.Complete(context.Background(), RoleClassifier, Request{})
	if !errors.Is(err, ErrRoleNotConfigured) {
		t.Errorf("err = %v, want ErrRoleNotConfigured so the UI can show a setup hint", err)
	}
}

// A ref pointing at a provider that isn't in the registry must not be fatal when
// a working fallback exists.
func TestUnknownProviderFallsThrough(t *testing.T) {
	up, _ := modelServer(t, "saved", http.StatusOK)
	t.Setenv("UP_KEY", "sk-up")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "up", BaseURL: up.URL, APIKeyEnv: "UP_KEY"}),
		Roles: map[Role]Chain{
			RoleClassifier: {
				Primary:  ModelRef{"typo-provider", "m"},
				Fallback: []ModelRef{{"up", "m"}},
			},
		},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	resp, err := c.Complete(context.Background(), RoleClassifier, Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "saved" {
		t.Errorf("text = %q", resp.Text)
	}
}

// Losing a cost-log row is bad; losing the run because of it is worse.
func TestRecorderFailureDoesNotFailTheCall(t *testing.T) {
	srv, _ := modelServer(t, "fine", http.StatusOK)
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	c := NewClient(cfg, &fakeRecorder{failWrites: true}, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier, Request{}); err != nil {
		t.Errorf("call failed because accounting failed: %v", err)
	}
}

func TestJSONModeSetsResponseFormat(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "[]"}}},
		})
	}))
	defer srv.Close()
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier, Request{JSONMode: true}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v", body["response_format"])
	}
}
