package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortTimeout swaps CallTimeout for the length of a test, so a slow model can be
// simulated without waiting two minutes.
func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := CallTimeout
	CallTimeout = d
	t.Cleanup(func() { CallTimeout = prev })
}

// slowServer never answers inside the timeout. headersFirst sends the status line
// at once and stalls on the body, which is what OpenRouter does while a slow model
// thinks -- the "read openrouter response: context deadline exceeded" case.
func slowServer(t *testing.T, headersFirst bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if headersFirst {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// A model that does not answer in time is reported as such -- not as
// context.DeadlineExceeded, which the pipeline reads as the run being cancelled --
// and the chain moves straight to its next model instead of spending a second
// full timeout on the same one.
func TestTimeoutGoesToTheFallbackWithoutRetrying(t *testing.T) {
	for _, headersFirst := range []bool{false, true} {
		name := "no response"
		if headersFirst {
			name = "headers then nothing"
		}
		t.Run(name, func(t *testing.T) {
			shortTimeout(t, 150*time.Millisecond)
			slow, hits := slowServer(t, headersFirst)
			fast, _ := modelServer(t, "rescued", http.StatusOK)

			cfg := Config{
				Registry: registryOf(t,
					Provider{Name: "slow", BaseURL: slow.URL},
					Provider{Name: "fast", BaseURL: fast.URL},
				),
				Roles: map[Role]Chain{RoleSynthesizer: {
					Primary:  ModelRef{"slow", "thinker"},
					Fallback: []ModelRef{{"fast", "quick"}},
				}},
			}
			rec := &fakeRecorder{}
			c := NewClient(cfg, rec, "2026-10-05", quiet())

			resp, err := c.Complete(context.Background(), RoleSynthesizer, Request{
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Provider != "fast" {
				t.Errorf("answered by %q, want the fallback", resp.Provider)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("slow model called %d times, want 1: a timeout is not retried on the same model", n)
			}
			calls := rec.recorded()
			if len(calls) != 2 || calls[0].OK || !strings.Contains(calls[0].Err, "did not answer within") {
				t.Errorf("cost log = %+v, want the timeout recorded in words, then the fallback", calls)
			}
		})
	}
}

// With no fallback the call fails -- as ErrCallTimeout, and never as a cancelled
// context, which is the distinction the stages depend on.
func TestTimeoutIsNotAContextError(t *testing.T) {
	shortTimeout(t, 100*time.Millisecond)
	slow, _ := slowServer(t, true)
	cfg := Config{
		Registry: registryOf(t, Provider{Name: "slow", BaseURL: slow.URL}),
		Roles:    map[Role]Chain{RoleSynthesizer: {Primary: ModelRef{"slow", "thinker"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-10-05", quiet())

	_, err := c.Complete(context.Background(), RoleSynthesizer, Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v, want ErrCallTimeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("err = %v reads as a context error; the pipeline would stop the whole run", err)
	}
}

// The run's own cancellation is still a cancellation: Stop and shutdown must stop.
func TestCancelledRunIsStillReportedAsCancelled(t *testing.T) {
	shortTimeout(t, 3*time.Second)
	slow, _ := slowServer(t, false)
	cfg := Config{
		Registry: registryOf(t, Provider{Name: "slow", BaseURL: slow.URL}),
		Roles:    map[Role]Chain{RoleSynthesizer: {Primary: ModelRef{"slow", "thinker"}}},
	}
	c := NewClient(cfg, &fakeRecorder{}, "2026-10-05", quiet())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, RoleSynthesizer, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if errors.Is(err, ErrCallTimeout) {
		t.Errorf("err = %v; a cancelled run was reported as a slow model", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test context did not expire")
	}
}
