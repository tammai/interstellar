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
	// roleSpentToday is what a per-role daily cap sees, keyed by role name.
	roleSpentToday map[string]int
	failWrites     bool
	// quality is what ReportReply attached, keyed by the id handed back at insert.
	quality map[int64]store.ReplyQuality
}

func (f *fakeRecorder) RecordLLMCall(rec store.LLMCall) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrites {
		return 0, errors.New("disk on fire")
	}
	f.calls = append(f.calls, rec)
	// Ids start at 1, so a client that failed to record (and therefore holds 0)
	// can never be mistaken for one that recorded the first call.
	return int64(len(f.calls)), nil
}

func (f *fakeRecorder) RecordReplyQuality(callID int64, q store.ReplyQuality) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrites {
		return errors.New("disk on fire")
	}
	if f.quality == nil {
		f.quality = map[int64]store.ReplyQuality{}
	}
	f.quality[callID] = q
	return nil
}

func (f *fakeRecorder) LLMCallsOn(string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spentToday, nil
}

func (f *fakeRecorder) LLMCallsByRoleOn(_, role string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roleSpentToday[role], nil
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

// Switching a provider off takes it out of the chain -- the whole point of the
// switch -- and does so without inventing a failed call in the cost log.
func TestSwitchedOffProviderIsSkippedNotCharged(t *testing.T) {
	off, offAuths := modelServer(t, "should never answer", http.StatusOK)
	on, _ := modelServer(t, "answered", http.StatusOK)

	t.Setenv("OFF_KEY", "sk-off")
	t.Setenv("ON_KEY", "sk-on")

	cfg := Config{
		Registry: registryOf(t,
			Provider{Name: "off", BaseURL: off.URL, APIKeyEnv: "OFF_KEY", Disabled: true},
			Provider{Name: "on", BaseURL: on.URL, APIKeyEnv: "ON_KEY"},
		),
		Roles: map[Role]Chain{
			RoleSynthesizer: {
				Primary:  ModelRef{"off", "parked-model"},
				Fallback: []ModelRef{{"on", "working-model"}},
			},
		},
	}
	rec := &fakeRecorder{}
	c := NewClient(cfg, rec, "2026-08-18", quiet())

	resp, err := c.Complete(context.Background(), RoleSynthesizer, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "answered" || resp.Provider != "on" {
		t.Errorf("answered by %q with %q, want the provider that is still on", resp.Provider, resp.Text)
	}
	// The switch has to actually stop the traffic, not merely relabel it.
	if len(*offAuths) != 0 {
		t.Errorf("the switched-off provider was called %d times", len(*offAuths))
	}

	// A skipped hop is not an attempt. Recording one would bill a call that never
	// happened and make a parked vendor look like a failing one.
	for _, call := range rec.recorded() {
		if call.Provider == "off" {
			t.Errorf("cost log has a row for a provider that was never called: %+v", call)
		}
	}
}

// With nowhere left to go the call fails, rather than falling through to a
// provider the owner had switched off.
func TestChainOfSwitchedOffProvidersFails(t *testing.T) {
	srv, auths := modelServer(t, "should never answer", http.StatusOK)
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t,
			Provider{Name: "parked", BaseURL: srv.URL, APIKeyEnv: "K", Disabled: true},
		),
		Roles: map[Role]Chain{
			RoleSynthesizer: {Primary: ModelRef{"parked", "m"}},
		},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-18", quiet())

	if _, err := c.Complete(context.Background(), RoleSynthesizer, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Complete succeeded with every provider switched off")
	}
	if len(*auths) != 0 {
		t.Errorf("a switched-off provider was called %d times", len(*auths))
	}
}

// A provider's error body is an envelope around one useful sentence. The
// envelope is what got persisted and then truncated, so the sentence -- the only
// part naming what to do -- was cut off before it was reached.
func TestProviderErrorKeepsTheReadablePart(t *testing.T) {
	body := `{"error":{"message":"this model requires a subscription, upgrade for access: https://ollama.com/upgrade (ref: f011eb64)","type":"api_error","param":null,"code":null}}`
	got := snippet([]byte(body))

	if !strings.HasPrefix(got, "this model requires a subscription") {
		t.Errorf("snippet = %q, want it to lead with the provider's message", got)
	}
	for _, noise := range []string{`"type"`, `"param"`, `{"error"`} {
		if strings.Contains(got, noise) {
			t.Errorf("snippet = %q, still carries the envelope %s", got, noise)
		}
	}
}

// A wrong base_url answers with an HTML page and no envelope. Its first line is
// the clue, so the raw body is still the right fallback.
func TestProviderErrorFallsBackToTheRawBody(t *testing.T) {
	got := snippet([]byte("<html><title>404 Not Found</title>"))
	if !strings.Contains(got, "404 Not Found") {
		t.Errorf("snippet = %q, want the raw body when there is no error envelope", got)
	}

	// Valid JSON of an unfamiliar shape is not guessed at: printing a field we
	// hoped was the message would be confidently wrong.
	got = snippet([]byte(`{"detail":"nope"}`))
	if !strings.Contains(got, "detail") {
		t.Errorf("snippet = %q, want the raw body for an unrecognised shape", got)
	}
}

// The reply's quality must land on the attempt that produced it, which after a
// fallback is not the attempt the owner configured.
//
// This is what keeps the per-model view honest: a primary that 500s and a
// fallback that answers badly are two different diagnoses, and attributing the
// bad answer to the primary would send the owner to change the wrong setting.
func TestReportReplyAttachesToTheHopThatAnswered(t *testing.T) {
	down, _ := modelServer(t, "", http.StatusInternalServerError)
	up, _ := modelServer(t, `{"verdicts":[]}`, http.StatusOK)
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t,
			Provider{Name: "down", BaseURL: down.URL, APIKeyEnv: "K"},
			Provider{Name: "up", BaseURL: up.URL, APIKeyEnv: "K"}),
		Roles: map[Role]Chain{RoleClassifier: {
			Primary:  ModelRef{"down", "a"},
			Fallback: []ModelRef{{"up", "b"}},
		}},
	}
	rec := &fakeRecorder{}
	c := NewClient(cfg, rec, "2026-08-11", quiet())

	resp, err := c.Complete(context.Background(), RoleClassifier, Request{Expect: 30})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	c.ReportReply(resp.CallID, 4, true)

	// Every attempt still got a row, including the two retries against `down`.
	if len(rec.calls) < 2 {
		t.Fatalf("cost log has %d rows, want the failed attempts and the success", len(rec.calls))
	}
	if resp.CallID != int64(len(rec.calls)) {
		t.Fatalf("response carries call id %d, want the last row (%d)", resp.CallID, len(rec.calls))
	}
	if got := rec.calls[resp.CallID-1]; got.Provider != "up" {
		t.Errorf("quality attached to provider %q, want the hop that answered (up)", got.Provider)
	}

	q, ok := rec.quality[resp.CallID]
	if !ok {
		t.Fatalf("no quality recorded against %d; have %v", resp.CallID, rec.quality)
	}
	if q.ItemsOut != 4 || !q.ParseOK {
		t.Errorf("quality = %+v, want 4 items and a clean parse", q)
	}
	// The failing hops must carry no quality at all: they never replied, and a
	// zero there would read as a model that answered nothing.
	for id := range rec.quality {
		if id != resp.CallID {
			t.Errorf("quality recorded against attempt %d, which produced no reply", id)
		}
	}
}

// Expect and Repair describe the QUESTION, so they belong on every attempt of it
// -- including the ones that never came back. Without that, a chain that fails
// twice and succeeds once looks like a model asked about a third as many items as
// it really was.
func TestRecordedAttemptsCarryTheRequestsShape(t *testing.T) {
	srv, _ := modelServer(t, "ok", http.StatusServiceUnavailable)
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry: registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:    map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
	}
	rec := &fakeRecorder{}
	c := NewClient(cfg, rec, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier,
		Request{Expect: 30, Repair: true}); err == nil {
		t.Fatal("expected the call to fail")
	}

	if len(rec.calls) == 0 {
		t.Fatal("no attempts recorded")
	}
	for i, got := range rec.calls {
		if got.ItemsIn != 30 {
			t.Errorf("attempt %d recorded items_in=%d, want 30", i, got.ItemsIn)
		}
		if !got.Repair {
			t.Errorf("attempt %d is not marked a repair; the cost of asking twice is attributable or it is invisible", i)
		}
		if got.OK {
			t.Errorf("attempt %d recorded as OK against a 503", i)
		}
	}
}

// A row that could not be written has no id, and reporting against it must be a
// no-op rather than a write against row 0 or an error into the run.
func TestReportReplyIsANoOpWithoutARow(t *testing.T) {
	rec := &fakeRecorder{failWrites: true}
	c := NewClient(Config{}, rec, "2026-08-11", quiet())

	c.ReportReply(0, 12, true)

	if len(rec.quality) != 0 {
		t.Errorf("quality written without a row: %v", rec.quality)
	}
}
