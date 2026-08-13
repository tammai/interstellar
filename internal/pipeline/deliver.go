package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/notify"
	"github.com/tammai/interstellar/internal/store"
)

// WeeklyRunResult is one end-to-end weekly pass: write, then deliver.
type WeeklyRunResult struct {
	WeeklyResult
	EmailSent   bool   `json:"email_sent"`
	EmailError  string `json:"email_error,omitempty"`
	PingSent    bool   `json:"ping_sent"`
	PingError   string `json:"ping_error,omitempty"`
	Delivered   bool   `json:"delivered"`
	DeliverNote string `json:"deliver_note,omitempty"`
}

// WeeklyRunner composes the weekly round-up and delivers it.
//
// A struct rather than a function because the scheduler needs something it can hold
// without knowing anything about models or SMTP -- see Scheduler.OnWeekly.
type WeeklyRunner struct {
	db  *store.DB
	env config.Env
	log *slog.Logger
	now func() time.Time
}

// NewWeeklyRunner builds a runner. now is injectable so week boundaries are testable.
func NewWeeklyRunner(db *store.DB, env config.Env, log *slog.Logger, now func() time.Time) *WeeklyRunner {
	if now == nil {
		now = time.Now
	}
	return &WeeklyRunner{db: db, env: env, log: log, now: now}
}

// RunScheduled is what the weekly cron entry calls.
//
// It writes LAST week's round-up: fired on a Monday morning, "this week" is four
// hours old and contains nothing.
func (w *WeeklyRunner) RunScheduled(ctx context.Context) {
	when := w.now().In(w.env.Location).AddDate(0, 0, -7)
	if _, err := w.Run(ctx, when, true); err != nil {
		w.log.Error("weekly round-up failed", "err", err)
	}
}

// Run writes the round-up for the week containing `when`, and optionally delivers it.
//
// Config is resolved here, per call, never cached -- §3.1's no-restart promise
// applies to the weekly model and the SMTP settings exactly as it does to a run.
func (w *WeeklyRunner) Run(ctx context.Context, when time.Time, deliver bool) (WeeklyRunResult, error) {
	var out WeeklyRunResult

	settings, err := w.db.Settings()
	if err != nil {
		return out, err
	}
	llmCfg, err := llm.LoadConfig(settings)
	if err != nil {
		return out, fmt.Errorf("model configuration problem: %w", err)
	}
	if st := llmCfg.RoleReadiness(llm.RoleSynthesizer); !st.Ready {
		// The weekly round-up rides the synthesizer's chain, so its readiness is the
		// one that matters. Reported rather than attempted.
		return out, fmt.Errorf("the synthesizer cannot run: %s", st.Problem)
	}

	cfg, err := config.Load(w.db)
	if err != nil {
		return out, err
	}

	runDate := store.RunDate(w.now().In(w.env.Location))
	client := llm.NewClient(llmCfg, w.db, runDate, w.log)

	weekly, err := WriteWeekly(ctx, w.db, client, when, w.log)
	out.WeeklyResult = weekly
	if err != nil {
		return out, err
	}
	if !weekly.Written {
		// Nothing to deliver. A "no news this week" email is worse than silence: it
		// trains the reader to ignore the ones that matter.
		out.DeliverNote = weekly.Skipped
		return out, nil
	}

	if deliver {
		w.deliver(ctx, cfg, &out)
	} else {
		// Always say why nothing was sent. A response with a round-up in it and no
		// word about delivery reads as "it was sent", which is the one thing the
		// owner must not have to guess about.
		out.DeliverNote = "written only; delivery was not requested"
	}
	return out, nil
}

// deliver sends the round-up by whichever channels are switched on.
//
// Both are attempted independently: a broken webhook must not stop the email, and a
// misconfigured relay must not stop the ping. Each failure is recorded against the
// digest so the reason survives the process.
func (w *WeeklyRunner) deliver(ctx context.Context, cfg config.Runtime, out *WeeklyRunResult) {
	digest, err := w.db.WeeklyDigestFor(out.WeekStart)
	if err != nil {
		out.DeliverNote = "could not read back the round-up to send it"
		return
	}

	if !cfg.DigestEnabled && !cfg.PingEnabled {
		out.DeliverNote = "delivery is off; read it in the app"
		return
	}

	if cfg.DigestEnabled {
		smtpCfg := notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, PasswordEnv: cfg.SMTPPasswordEnv,
			From: cfg.DigestFrom, To: cfg.DigestTo,
		}
		subject := fmt.Sprintf("Interstellar · week of %s", digest.WeekStart)
		body := emailBody(digest)

		if err := notify.SendEmail(ctx, smtpCfg, subject, body); err != nil {
			out.EmailError = err.Error()
			w.log.Warn("weekly email failed", "err", err)
		} else {
			out.EmailSent = true
		}
	}

	if cfg.PingEnabled {
		if err := notify.SendPing(ctx, cfg.PingURL, notify.PingPayload{
			Event:     "weekly_digest",
			WeekStart: digest.WeekStart,
			WeekEnd:   digest.WeekEnd,
			Signals:   digest.Signals,
			Topics:    digest.Topics,
			Summary:   firstSentence(digest.Body),
		}); err != nil {
			out.PingError = err.Error()
			w.log.Warn("weekly ping failed", "err", err)
		} else {
			out.PingSent = true
		}
	}

	out.Delivered = out.EmailSent || out.PingSent

	// Recorded even on failure, so "never tried" stays distinguishable from "tried
	// and the relay rejected it".
	via := strings.Join(nonEmpty(
		ifThen(out.EmailSent, "email"), ifThen(out.PingSent, "ping")), "+")
	sendErr := strings.Join(nonEmpty(out.EmailError, out.PingError), "; ")
	if err := w.db.MarkWeeklySent(out.WeekStart, via, sendErr); err != nil {
		w.log.Warn("could not record weekly delivery", "err", err)
	}
}

// emailBody wraps the round-up with the little context an email needs.
//
// The prose is the model's, verbatim. Nothing is reformatted: the round-up was
// written to be read as prose, and a mail client is a worse place to invent
// structure than the screen it was written for.
func emailBody(d store.WeeklyDigest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Week of %s to %s\n", d.WeekStart, d.WeekEnd)
	fmt.Fprintf(&b, "%d signals across %d topics\n\n", d.Signals, d.Topics)
	b.WriteString(strings.Repeat("—", 40))
	b.WriteString("\n\n")
	b.WriteString(d.Body)
	b.WriteString("\n\n")
	b.WriteString(strings.Repeat("—", 40))
	b.WriteString("\n\nThe stars, the sources and the links are in the app.\n")

	return b.String()
}

// firstSentence is the ping's one-line preview.
func firstSentence(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if i := strings.Index(body, ". "); i > 0 && i < 240 {
		return body[:i+1]
	}
	if len(body) > 240 {
		return body[:240] + "…"
	}
	return body
}

func ifThen(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
