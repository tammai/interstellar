// Command verify-classifier is the §10 gate: "classifier JSON parses 100% on a
// 200-item sample (gate before any model swap)".
//
// It is deliberately a separate command rather than part of `go test`. It makes
// real, billable calls against whatever provider the instance is configured to
// use, and a test suite that spends money when someone types `make test` is a
// trap. Run it on purpose, before trusting a model you just switched to.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
)

func main() {
	sample := flag.Int("n", 200, "how many items to sample (§10 specifies 200)")
	verbose := flag.Bool("v", false, "log each model call")
	flag.Parse()

	if err := run(*sample, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "\nverify-classifier: %v\n", err)
		os.Exit(1)
	}
}

func run(sample int, verbose bool) error {
	env, err := config.FromEnv()
	if err != nil {
		return err
	}

	logLevel := slog.LevelWarn
	if verbose {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	if !verbose {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	db, err := store.Open(env.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	settings, err := db.Settings()
	if err != nil {
		return err
	}
	llmCfg, err := llm.LoadConfig(settings)
	if err != nil {
		return err
	}

	// Fail before spending anything if the role cannot run.
	for _, st := range llmCfg.Readiness() {
		if st.Role == string(llm.RoleClassifier) && !st.Ready {
			return fmt.Errorf("classifier is not ready: %s", st.Problem)
		}
	}

	cfg, err := config.Load(db)
	if err != nil {
		return err
	}

	// The gate is allowed to exceed the per-run cap: it is a deliberate,
	// operator-initiated check, not a runaway pipeline.
	llmCfg.MaxCallsPerRun = 0

	runDate := store.RunDate(time.Now().In(env.Location))
	client := llm.NewClient(llmCfg, db, runDate, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	chain := llmCfg.Roles[llm.RoleClassifier]
	fmt.Printf("Gate: classifier JSON must parse on every batch of a %d-item sample.\n", sample)
	fmt.Printf("Model: %s  (database: %s)\n\n", chain.Primary.String(), env.DBPath)

	report, err := pipeline.VerifyClassifier(ctx, db, client, cfg, sample, log)
	if err != nil {
		return err
	}

	printReport(report)

	if !report.Passed() {
		return fmt.Errorf("GATE FAILED — %d of %d batches did not parse",
			len(report.Batches)-report.ParsedOK, len(report.Batches))
	}
	return nil
}

func printReport(r pipeline.VerifyReport) {
	fmt.Printf("Topic used:  %s\n", r.Topic)
	fmt.Printf("Items:       %d in %d batches\n\n", r.Items, len(r.Batches))

	for _, b := range r.Batches {
		status := "ok"
		switch {
		case b.Err != nil:
			status = "FAILED"
		case b.Repaired:
			status = "ok (repaired)"
		}
		fmt.Printf("  batch %2d  %3d items → %3d verdicts  %6dms  %s\n",
			b.Index, b.Items, b.Verdicts, b.Latency.Milliseconds(), status)
		if b.Err != nil {
			fmt.Printf("            %v\n", b.Err)
		}
	}

	fmt.Printf("\nParsed:      %d/%d batches\n", r.ParsedOK, len(r.Batches))
	fmt.Printf("Repairs:     %d\n", r.Repairs)
	// Coverage is not part of the §10 gate, but a model that parses perfectly
	// while silently dropping a third of the items it was given is not a model
	// worth shipping, and nothing else would surface that.
	fmt.Printf("Unscored:    %d item(s) the model never returned a verdict for\n", r.MissingItems)
	fmt.Printf("Elapsed:     %s\n\n", r.Elapsed.Round(time.Millisecond))

	if r.Passed() {
		fmt.Println("GATE PASSED")
		if r.Repairs > 0 {
			fmt.Printf("Note: %d batch(es) needed a repair round-trip. That works, but it is a\n"+
				"round trip you pay for on every run — a model needing it often is worth replacing.\n", r.Repairs)
		}
		if r.MissingItems > r.Items/10 {
			fmt.Printf("Note: %d%% of items went unscored. The JSON is valid but the model is not\n"+
				"doing the job; consider stepping up a tier.\n", r.MissingItems*100/r.Items)
		}
	}
}
