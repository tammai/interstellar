package llm

import (
	"errors"
	"fmt"
	"sync"
)

// ErrCapReached is returned when the spend guard stops a run. It is deliberately
// distinguishable from a provider failure: the run aborted on purpose, and the
// UI banner says so (§10).
var ErrCapReached = errors.New("llm call cap reached")

// Guard enforces the §3.1 spend caps -- the replacement for LiteLLM's budget
// ceiling. It counts CALLS, not dollars: a call count is knowable before a
// request is sent and identical across providers, whereas per-token pricing
// varies by vendor and changes without notice.
//
// The per-run cap is in memory, which is correct: a run is a process-lifetime
// concept, and a restart legitimately starts a fresh run. The per-day cap is
// derived from llm_calls on every reservation, so it survives a restart -- a
// crash loop cannot spend the day's budget several times over.
type Guard struct {
	mu            sync.Mutex
	maxPerRun     int
	maxPerDay     int
	maxChatPerDay int
	usedThisRun   int
	stoppedFor    string
}

// NewGuard builds a guard. A cap of zero or less means unlimited, which is what
// an owner who clears the field is asking for.
func NewGuard(maxPerRun, maxPerDay, maxChatPerDay int) *Guard {
	return &Guard{maxPerRun: maxPerRun, maxPerDay: maxPerDay, maxChatPerDay: maxChatPerDay}
}

// offRunBudget reports whether a role spends from something other than a run's
// budget. All of these are things a person does while sitting in the UI, and none
// is a run: blocking them on a run's allowance would mean a refresh that hit its
// cap also broke the Settings test button, the chat screen, and the button that
// suggests sources for a topic.
//
// The daily cap still applies to every one of them, so "not part of a run" never
// means "unlimited".
func offRunBudget(role Role) bool {
	return role == RoleTest || role == RoleChat || role == RoleSuggest
}

// Reserve accounts for one upcoming call, or refuses it.
//
// spentToday is the total count already recorded for this run date, and
// roleSpentToday the count for this role alone. Both are read fresh from the
// database by the caller rather than tracked in memory, so a crash loop cannot
// spend the same budget several times over.
func (g *Guard) Reserve(role Role, spentToday, roleSpentToday int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.maxPerDay > 0 && spentToday >= g.maxPerDay {
		g.stoppedFor = fmt.Sprintf("daily cap of %d LLM calls reached", g.maxPerDay)
		return fmt.Errorf("%w: %s", ErrCapReached, g.stoppedFor)
	}

	// Chat's own daily allowance, checked on top of the global one. Without it,
	// chat and the pipeline share a single budget and an evening of questions
	// leaves tomorrow morning's run with nothing to spend -- the sky is the
	// product, and chat is a lens on it.
	if role == RoleChat && g.maxChatPerDay > 0 && roleSpentToday >= g.maxChatPerDay {
		g.stoppedFor = fmt.Sprintf("daily chat cap of %d calls reached", g.maxChatPerDay)
		return fmt.Errorf("%w: %s", ErrCapReached, g.stoppedFor)
	}

	// The Settings test-call button and the chat screen are exempt from the
	// per-run cap: verifying a provider swap, or asking a question, must not be
	// blocked by -- or able to consume -- a run's budget. The daily caps above
	// still apply, so neither can spend without limit.
	if !offRunBudget(role) && g.maxPerRun > 0 && g.usedThisRun >= g.maxPerRun {
		g.stoppedFor = fmt.Sprintf("per-run cap of %d LLM calls reached", g.maxPerRun)
		return fmt.Errorf("%w: %s", ErrCapReached, g.stoppedFor)
	}

	if !offRunBudget(role) {
		g.usedThisRun++
	}
	return nil
}

// UsedThisRun reports calls reserved so far.
func (g *Guard) UsedThisRun() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.usedThisRun
}

// StoppedFor returns the human-readable reason the guard halted a run, or "" if
// it hasn't. This is the banner text.
func (g *Guard) StoppedFor() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stoppedFor
}
