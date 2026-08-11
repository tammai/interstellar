package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardPerRunCap(t *testing.T) {
	g := NewGuard(2, 0)

	for i := 0; i < 2; i++ {
		if err := g.Reserve(RoleClassifier, 0); err != nil {
			t.Fatalf("reservation %d refused: %v", i+1, err)
		}
	}
	err := g.Reserve(RoleClassifier, 0)
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("third reservation err = %v, want ErrCapReached", err)
	}
	if !strings.Contains(g.StoppedFor(), "per-run cap") {
		t.Errorf("StoppedFor = %q, should be usable as banner text", g.StoppedFor())
	}
}

// The daily cap is derived from the database, so a crash loop cannot spend the
// day's budget several times over.
func TestGuardDailyCapUsesPersistedCount(t *testing.T) {
	g := NewGuard(1000, 50)

	if err := g.Reserve(RoleClassifier, 49); err != nil {
		t.Fatalf("reservation under the daily cap refused: %v", err)
	}
	err := g.Reserve(RoleClassifier, 50)
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("err = %v, want ErrCapReached at the daily cap", err)
	}
	if !strings.Contains(g.StoppedFor(), "daily cap") {
		t.Errorf("StoppedFor = %q", g.StoppedFor())
	}
}

func TestGuardZeroMeansUnlimited(t *testing.T) {
	g := NewGuard(0, 0)
	for i := 0; i < 1000; i++ {
		if err := g.Reserve(RoleClassifier, i); err != nil {
			t.Fatalf("unlimited guard refused at %d: %v", i, err)
		}
	}
}

// Verifying a provider swap must not be blocked by a run's budget, nor consume
// it -- otherwise the §10 "test call verifies in-UI" criterion fails exactly when
// you need it, on an instance that just hit its cap.
func TestGuardExemptsTestCallsFromRunCap(t *testing.T) {
	g := NewGuard(1, 100)

	if err := g.Reserve(RoleClassifier, 0); err != nil {
		t.Fatalf("first run call refused: %v", err)
	}
	if err := g.Reserve(RoleClassifier, 1); !errors.Is(err, ErrCapReached) {
		t.Fatalf("run cap not enforced: %v", err)
	}

	// Still allowed at the run cap.
	if err := g.Reserve(RoleTest, 1); err != nil {
		t.Errorf("test call blocked by the per-run cap: %v", err)
	}
	if g.UsedThisRun() != 1 {
		t.Errorf("UsedThisRun = %d; test calls should not consume the run budget", g.UsedThisRun())
	}

	// But the daily cap still binds, so this is not an unlimited side door.
	if err := g.Reserve(RoleTest, 100); !errors.Is(err, ErrCapReached) {
		t.Errorf("test call ignored the daily cap: %v", err)
	}
}

// §10: "call-cap guard aborts a runaway run".
func TestClientStopsWhenCapReached(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry:       registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:          map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
		MaxCallsPerRun: 2,
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-08-11", quiet())

	for i := 0; i < 2; i++ {
		if _, err := c.Complete(context.Background(), RoleClassifier, Request{}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}

	_, err := c.Complete(context.Background(), RoleClassifier, Request{})
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("err = %v, want ErrCapReached", err)
	}
	if hits != 2 {
		t.Errorf("provider hit %d times, want 2 -- the cap must stop the call, not just log it", hits)
	}
	if c.Guard().StoppedFor() == "" {
		t.Error("no banner reason recorded")
	}
}

// The daily cap must be enforced from persisted state, so it holds across a
// restart mid-run.
func TestClientRespectsPersistedDailyCap(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("K", "sk-x")

	cfg := Config{
		Registry:       registryOf(t, Provider{Name: "p", BaseURL: srv.URL, APIKeyEnv: "K"}),
		Roles:          map[Role]Chain{RoleClassifier: {Primary: ModelRef{"p", "m"}}},
		MaxCallsPerDay: 10,
	}
	// A fresh client, as after a restart, but the day already spent its budget.
	c := NewClient(cfg, &fakeRecorder{spentToday: 10}, "2026-08-11", quiet())

	if _, err := c.Complete(context.Background(), RoleClassifier, Request{}); !errors.Is(err, ErrCapReached) {
		t.Fatalf("err = %v, want ErrCapReached from persisted state", err)
	}
	if hits != 0 {
		t.Errorf("provider was called %d times despite the daily cap", hits)
	}
}
