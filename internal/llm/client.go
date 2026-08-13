package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

// Role names a job, not a model. Which model performs it is settings.
type Role string

const (
	RoleClassifier  Role = "classifier"
	RoleSynthesizer Role = "synthesizer"
	// RoleTest is the Settings "test call" button. Logged like any other call so
	// its cost is visible, but exempt from the per-run cap -- a run's budget
	// should not be consumable by clicking a button.
	RoleTest Role = "test"
	// RoleChat answers questions on the chat screen. Logged like any other call,
	// but budgeted separately: see Guard.
	RoleChat Role = "chat"
	// RoleTranscript summarizes video captions. It runs on the classifier's chain
	// -- condensing a transcript is mechanical work, which is what the cheap-first
	// role is for -- but is logged under its own name so the cost is visible rather
	// than blended into classification.
	RoleTranscript Role = "transcript"
	// RoleWeekly writes the weekly round-up. It runs on the synthesizer's chain --
	// turning a pile of stories into prose a person reads is that role's job at a
	// different scale -- with its own line in the cost log.
	RoleWeekly Role = "weekly"
)

// attemptsPerRef bounds retries against a single provider before moving down the
// chain. Two tries catches a transient 429 or blip; more just delays the
// fallback that was configured precisely for this case.
const attemptsPerRef = 2

// retryBackoff is the pause before a retry against the same provider.
var retryBackoff = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

// Chain is a role's primary model plus its ordered fallbacks. Entries may sit on
// different providers -- that is the point (§3.1).
type Chain struct {
	Primary  ModelRef
	Fallback []ModelRef
}

// Refs returns the full attempt order.
func (c Chain) Refs() []ModelRef {
	if c.Primary.IsZero() {
		return c.Fallback
	}
	return append([]ModelRef{c.Primary}, c.Fallback...)
}

// Recorder persists per-call accounting. Satisfied by *store.DB; an interface so
// this package tests without a database.
type Recorder interface {
	RecordLLMCall(rec store.LLMCall) error
	LLMCallsOn(runDate string) (int, error)
	LLMCallsByRoleOn(runDate, role string) (int, error)
}

// Config is the LLM layer's view of settings, resolved fresh per run.
type Config struct {
	Registry       *Registry
	Roles          map[Role]Chain
	MaxCallsPerRun int
	MaxCallsPerDay int
	// MaxChatCallsPerDay budgets the chat screen on its own.
	//
	// Chat shares llm_calls and the global daily cap with the pipeline, and that
	// alone would let a curious evening of questions eat the budget tomorrow
	// morning's run needs. The sky is the product; chat is a lens on it, so chat
	// gets a smaller allowance of its own and cannot starve the runs.
	MaxChatCallsPerDay int
}

// LoadConfig builds Config from the settings map.
//
// Malformed settings are returned as errors here rather than surfacing mid-run:
// discovering a typo'd provider name three minutes into a refresh is strictly
// worse than refusing to start.
func LoadConfig(settings map[string]string) (Config, error) {
	registry, err := ParseRegistry(settings[store.KeyProviders])
	if err != nil {
		return Config{}, err
	}

	classifier, err := ParseModelRef(settings[store.KeyClassifierModel])
	if err != nil {
		return Config{}, fmt.Errorf("classifier: %w", err)
	}
	synthesizer, err := ParseModelRef(settings[store.KeySynthesizerModel])
	if err != nil {
		return Config{}, fmt.Errorf("synthesizer: %w", err)
	}
	classifierFallback, err := ParseModelRefs(settings[store.KeyClassifierFallback])
	if err != nil {
		return Config{}, fmt.Errorf("classifier fallback: %w", err)
	}
	synthFallback, err := ParseModelRefs(settings[store.KeySynthFallback])
	if err != nil {
		return Config{}, fmt.Errorf("synthesizer fallback: %w", err)
	}
	chat, err := ParseModelRef(settings[store.KeyChatModel])
	if err != nil {
		return Config{}, fmt.Errorf("chat: %w", err)
	}
	chatFallback, err := ParseModelRefs(settings[store.KeyChatFallback])
	if err != nil {
		return Config{}, fmt.Errorf("chat fallback: %w", err)
	}

	return Config{
		Registry: registry,
		Roles: map[Role]Chain{
			RoleClassifier:  {Primary: classifier, Fallback: classifierFallback},
			RoleSynthesizer: {Primary: synthesizer, Fallback: synthFallback},
			RoleChat:        {Primary: chat, Fallback: chatFallback},
		},
		MaxCallsPerRun:     atoiDefault(settings[store.KeyMaxCallsPerRun], 200),
		MaxCallsPerDay:     atoiDefault(settings[store.KeyMaxCallsPerDay], 500),
		MaxChatCallsPerDay: atoiDefault(settings[store.KeyMaxChatCallsPerDay], 100),
	}, nil
}

// Client performs role-based completions.
type Client struct {
	cfg      Config
	recorder Recorder
	http     *http.Client
	log      *slog.Logger
	runDate  string
	guard    *Guard
}

// NewClient builds a client for one run. runDate is the local calendar date the
// run belongs to, used for both accounting and the daily cap.
func NewClient(cfg Config, rec Recorder, runDate string, log *slog.Logger) *Client {
	return &Client{
		cfg:      cfg,
		recorder: rec,
		http:     &http.Client{Timeout: CallTimeout + 15*time.Second},
		log:      log,
		runDate:  runDate,
		guard:    NewGuard(cfg.MaxCallsPerRun, cfg.MaxCallsPerDay, cfg.MaxChatCallsPerDay),
	}
}

// Config exposes the resolved config, for readiness reporting.
func (c *Client) Config() Config { return c.cfg }

// Guard exposes the spend guard so a run can report why it stopped.
func (c *Client) Guard() *Guard { return c.guard }

// ErrRoleNotConfigured means the role has no model assigned -- the first-run
// state, not a failure.
var ErrRoleNotConfigured = errors.New("role has no model configured")

// Complete runs a request through a role's chain.
//
// It walks primary then fallbacks, retrying only transient failures against each
// provider before moving on. Base URL and auth are rebuilt per hop, so a chain
// can cross vendors. Every attempt -- success or failure -- is recorded, so a
// fallback that quietly fires still shows up in the cost log.
func (c *Client) Complete(ctx context.Context, role Role, req Request) (Response, error) {
	return c.CompleteAs(ctx, role, role, req)
}

// CompleteAs runs a request through chainRole's models while accounting for it
// under logRole.
//
// The split exists for the Settings test button: it must exercise the real
// classifier or synthesizer chain -- a test down a different code path proves
// nothing -- while being logged and capped as a test rather than as run work.
func (c *Client) CompleteAs(ctx context.Context, chainRole, logRole Role, req Request) (Response, error) {
	role := chainRole
	chain, ok := c.cfg.Roles[role]
	if !ok {
		return Response{}, fmt.Errorf("unknown role %q", role)
	}
	refs := chain.Refs()
	if len(refs) == 0 {
		return Response{}, fmt.Errorf("%w: %s", ErrRoleNotConfigured, role)
	}

	if err := c.reserve(logRole); err != nil {
		return Response{}, err
	}

	var lastErr error
	for i, ref := range refs {
		provider, err := c.cfg.Registry.Resolve(ref)
		if err != nil {
			// A ref naming a provider that isn't in the registry is a config
			// error, not a transport one. Record it and try the next hop.
			lastErr = err
			c.record(logRole, ref, Usage{}, err)
			continue
		}

		for attempt := 0; attempt < attemptsPerRef; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(retryBackoff[min(attempt-1, len(retryBackoff)-1)]):
				case <-ctx.Done():
					return Response{}, ctx.Err()
				}
			}

			resp, err := call(ctx, c.http, provider, ref.Model, req)
			c.record(logRole, ref, resp.Usage, err)

			if err == nil {
				if i > 0 {
					// Worth a log line: the configured primary is failing and
					// the owner is silently paying a different vendor.
					c.log.Warn("llm fallback used",
						"role", role, "used", ref.String(), "primary", refs[0].String())
				}
				return resp, nil
			}

			lastErr = err
			if !Retryable(err) {
				break // same failure forever; move to the next provider
			}
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
		}
	}

	return Response{}, fmt.Errorf("all %d model(s) for role %s failed: %w", len(refs), role, lastErr)
}

// reserve asks the guard for permission before spending anything.
func (c *Client) reserve(role Role) error {
	spentToday, err := c.recorder.LLMCallsOn(c.runDate)
	if err != nil {
		return fmt.Errorf("spend guard: %w", err)
	}

	// Only chat has a per-role cap, so only chat pays for the extra read.
	var roleSpentToday int
	if role == RoleChat {
		roleSpentToday, err = c.recorder.LLMCallsByRoleOn(c.runDate, string(role))
		if err != nil {
			return fmt.Errorf("spend guard: %w", err)
		}
	}
	return c.guard.Reserve(role, spentToday, roleSpentToday)
}

// record writes one attempt to llm_calls. A failure to record is logged, never
// fatal: losing a cost log row is bad, losing the run is worse.
func (c *Client) record(role Role, ref ModelRef, usage Usage, callErr error) {
	rec := store.LLMCall{
		RunDate:   c.runDate,
		Role:      string(role),
		Provider:  ref.Provider,
		Model:     ref.Model,
		TokensIn:  usage.TokensIn,
		TokensOut: usage.TokensOut,
		OK:        callErr == nil,
	}
	if callErr != nil {
		rec.Err = callErr.Error()
	}
	if err := c.recorder.RecordLLMCall(rec); err != nil {
		c.log.Warn("record llm call", "err", err)
	}
}

// RoleStatus reports whether a role can actually run.
type RoleStatus struct {
	Role     string `json:"role"`
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Ready    bool   `json:"ready"`
	Problem  string `json:"problem,omitempty"`
}

// Readiness checks each role end to end: model assigned, provider known, key
// present. This is what turns "the run failed" into "DEEPSEEK_API_KEY is not
// set", which is the difference between a usable setup screen and a mystery.
//
// It covers the two roles a RUN needs, and deliberately not chat. run.go refuses
// to start when Ready() is false, so folding chat in here would mean an owner who
// cleared chat_model -- a screen they may never open -- silently stopped their
// morning pipeline. Chat readiness is asked for separately, by the screen that
// actually needs it.
func (cfg Config) Readiness() []RoleStatus {
	out := make([]RoleStatus, 0, 2)
	for _, role := range []Role{RoleClassifier, RoleSynthesizer} {
		out = append(out, cfg.RoleReadiness(role))
	}
	return out
}

// RoleReadiness checks one role end to end.
func (cfg Config) RoleReadiness(role Role) RoleStatus {
	chain := cfg.Roles[role]
	st := RoleStatus{Role: string(role)}

	if chain.Primary.IsZero() {
		st.Problem = "no model configured"
		return st
	}

	st.Ref = chain.Primary.String()
	st.Provider = chain.Primary.Provider

	provider, err := cfg.Registry.Resolve(chain.Primary)
	if err != nil {
		st.Problem = err.Error()
		return st
	}
	if _, ok := provider.APIKey(); !ok {
		st.Problem = fmt.Sprintf("environment variable %s is not set", provider.APIKeyEnv)
		return st
	}

	st.Ready = true
	return st
}

// Ready reports whether every role can run.
func (cfg Config) Ready() bool {
	for _, st := range cfg.Readiness() {
		if !st.Ready {
			return false
		}
	}
	return true
}

func atoiDefault(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
