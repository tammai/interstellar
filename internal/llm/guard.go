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
	mu          sync.Mutex
	maxPerRun   int
	maxPerDay   int
	usedThisRun int
	stoppedFor  string
}

// NewGuard builds a guard. A cap of zero or less means unlimited, which is what
// an owner who clears the field is asking for.
func NewGuard(maxPerRun, maxPerDay int) *Guard {
	return &Guard{maxPerRun: maxPerRun, maxPerDay: maxPerDay}
}

// Reserve accounts for one upcoming call, or refuses it.
//
// spentToday is the count already recorded for this run date, read fresh from
// the database by the caller.
func (g *Guard) Reserve(role Role, spentToday int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.maxPerDay > 0 && spentToday >= g.maxPerDay {
		g.stoppedFor = fmt.Sprintf("daily cap of %d LLM calls reached", g.maxPerDay)
		return fmt.Errorf("%w: %s", ErrCapReached, g.stoppedFor)
	}

	// The Settings test-call button is exempt from the per-run cap: verifying a
	// provider swap must not be blocked by, or able to consume, a run's budget.
	// The daily cap above still applies, so it cannot be used to spend without
	// limit.
	if role != RoleTest && g.maxPerRun > 0 && g.usedThisRun >= g.maxPerRun {
		g.stoppedFor = fmt.Sprintf("per-run cap of %d LLM calls reached", g.maxPerRun)
		return fmt.Errorf("%w: %s", ErrCapReached, g.stoppedFor)
	}

	if role != RoleTest {
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
