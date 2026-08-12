// Command skyprobe measures GET /api/sky latency while the write side of a run
// holds the single SQLite connection.
//
// The question it answers: with store.Open capping the pool at one connection,
// does a run's write traffic stall the UI? Reasoning says no -- every
// transaction lives inside internal/store, which imports no network or LLM code,
// so the connection is never held across I/O -- but reasoning is not a
// measurement. This produces the measurement.
//
// Two modes:
//
//	skyprobe                      synthetic: temp DB, real handlers, compose-shaped
//	                              write load. Offline, free, repeatable.
//	skyprobe -url http://host     live: poll a running instance. Add -refresh to
//	                              trigger a real run first (costs real LLM calls).
//
// Output is a baseline percentile table and a loaded one. The number that
// matters is loaded p99 minus baseline p99: that is the queueing delay the
// single connection imposes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing/fstest"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/httpapi"
	"github.com/tammai/interstellar/internal/store"
)

// httpapiRoutes builds the real handler tree over the probe's database.
// Runner and Scheduler are nil on purpose: /api/sky never touches either, and
// leaving them out makes it impossible for the probe to start a pipeline -- and
// therefore impossible for it to make a provider call -- by accident.
func httpapiRoutes(db *store.DB, log *slog.Logger) http.Handler {
	env := config.Env{Port: "0", Location: time.Local}
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("probe")}}
	return httpapi.New(db, env, web, nil, nil, log).Routes()
}

func main() {
	var (
		url      = flag.String("url", "", "probe a live instance at this base URL instead of the synthetic harness")
		refresh  = flag.Bool("refresh", false, "live mode: POST /api/refresh after the baseline (spends real LLM calls)")
		baseline = flag.Duration("baseline", 5*time.Second, "quiet sampling window before load starts")
		load     = flag.Duration("load", 20*time.Second, "sampling window with the write load running")
		interval = flag.Duration("interval", 50*time.Millisecond, "polling interval")
		slow     = flag.Duration("slow", 100*time.Millisecond, "log any sample slower than this as it happens")
		topics   = flag.Int("topics", 7, "synthetic: topics to seed")
		items    = flag.Int("items", 400, "synthetic: items to seed")
		members  = flag.Int("members", 5, "synthetic: items per composed signal")
	)
	flag.Parse()

	if *url != "" {
		if err := live(*url, *refresh, *baseline, *load, *interval, *slow); err != nil {
			fmt.Fprintln(os.Stderr, "skyprobe:", err)
			os.Exit(1)
		}
		return
	}
	if err := synthetic(*topics, *items, *members, *baseline, *load, *interval, *slow); err != nil {
		fmt.Fprintln(os.Stderr, "skyprobe:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- sampling

type sample struct {
	at  time.Time
	dur time.Duration
	err error
}

type poller struct {
	get     func() (time.Duration, error)
	every   time.Duration
	slowLog time.Duration

	mu      sync.Mutex
	samples []sample
	stop    chan struct{}
	done    chan struct{}
}

func newPoller(get func() (time.Duration, error), every, slowLog time.Duration) *poller {
	return &poller{get: get, every: every, slowLog: slowLog,
		stop: make(chan struct{}), done: make(chan struct{})}
}

func (p *poller) run() {
	go func() {
		defer close(p.done)
		tick := time.NewTicker(p.every)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
			}
			d, err := p.get()
			s := sample{at: time.Now(), dur: d, err: err}
			if err != nil {
				fmt.Printf("  %s  ERROR  %v\n", s.at.Format("15:04:05.000"), err)
			} else if d >= p.slowLog {
				fmt.Printf("  %s  slow   %6.1fms\n", s.at.Format("15:04:05.000"), ms(d))
			}
			p.mu.Lock()
			p.samples = append(p.samples, s)
			p.mu.Unlock()
		}
	}()
}

func (p *poller) close() {
	close(p.stop)
	<-p.done
}

// since returns the samples taken at or after t.
func (p *poller) since(t time.Time) []sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sample
	for _, s := range p.samples {
		if !s.at.Before(t) {
			out = append(out, s)
		}
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---------------------------------------------------------------- reporting

type stats struct {
	n                      int
	errs                   int
	p50, p90, p99, max, mn time.Duration
}

func summarize(samples []sample) stats {
	var st stats
	durs := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		if s.err != nil {
			st.errs++
			continue
		}
		durs = append(durs, s.dur)
	}
	st.n = len(durs)
	if st.n == 0 {
		return st
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	st.mn = durs[0]
	st.p50 = pct(durs, 0.50)
	st.p90 = pct(durs, 0.90)
	st.p99 = pct(durs, 0.99)
	st.max = durs[len(durs)-1]
	return st
}

// pct picks the nearest-rank percentile. With a few hundred samples that is
// honest in a way interpolation is not: p99 of 200 samples is the second-worst
// observation, and it should be reported as such rather than smoothed.
func pct(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func report(label string, st stats) {
	if st.n == 0 {
		fmt.Printf("  %-10s no successful samples (%d errors)\n", label, st.errs)
		return
	}
	fmt.Printf("  %-10s n=%-4d min %6.1f  p50 %6.1f  p90 %6.1f  p99 %6.1f  max %6.1f  (ms)",
		label, st.n, ms(st.mn), ms(st.p50), ms(st.p90), ms(st.p99), ms(st.max))
	if st.errs > 0 {
		fmt.Printf("  errors=%d", st.errs)
	}
	fmt.Println()
}

func verdict(base, loaded stats) {
	if base.n == 0 || loaded.n == 0 {
		return
	}
	delta := loaded.p99 - base.p99
	fmt.Printf("\n  p99 under load is %+.1fms vs baseline; worst sample %.1fms.\n", ms(delta), ms(loaded.max))
	switch {
	case loaded.max > 500*time.Millisecond:
		fmt.Println("  STALL: a request waited over half a second. Something holds the connection across I/O.")
	case loaded.max > 100*time.Millisecond:
		fmt.Println("  Visible: the tail is into perceptible territory. Worth finding which write blocks.")
	default:
		fmt.Println("  No stall: writes release the connection fast enough that reads never queue perceptibly.")
	}
}

// ---------------------------------------------------------------- live mode

func live(base string, doRefresh bool, baselineFor, loadFor, every, slowLog time.Duration) error {
	client := &http.Client{Timeout: 30 * time.Second}
	probe := func() (time.Duration, error) {
		start := time.Now()
		resp, err := client.Get(base + "/api/sky")
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		// Read the body: the handler streams JSON, so time-to-first-byte would
		// hide exactly the connection contention we are looking for.
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return time.Since(start), fmt.Errorf("status %d", resp.StatusCode)
		}
		return time.Since(start), nil
	}

	if _, err := probe(); err != nil {
		return fmt.Errorf("cannot reach %s/api/sky: %w", base, err)
	}

	fmt.Printf("probing %s every %s\n\n", base, every)
	p := newPoller(probe, every, slowLog)
	p.run()
	defer p.close()

	baseStart := time.Now()
	fmt.Printf("baseline: %s quiet...\n", baselineFor)
	time.Sleep(baselineFor)
	baseStats := summarize(p.since(baseStart))

	loadStart := time.Now()
	if doRefresh {
		resp, err := client.Post(base+"/api/refresh", "application/json", nil)
		if err != nil {
			return fmt.Errorf("start run: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("start run: status %d", resp.StatusCode)
		}
		fmt.Println("run started; sampling until it goes idle or the load window ends...")
		waitForIdle(client, base, loadFor)
	} else {
		fmt.Printf("sampling %s -- trigger a run now (UI refresh, or the schedule)...\n", loadFor)
		time.Sleep(loadFor)
	}
	loadStats := summarize(p.since(loadStart))

	fmt.Println()
	report("baseline", baseStats)
	report("loaded", loadStats)
	verdict(baseStats, loadStats)
	return nil
}

// waitForIdle polls /api/status until the run finishes or the window expires.
// It gives the run a moment to leave idle first, so a slow start is not read as
// an instant finish.
func waitForIdle(client *http.Client, base string, max time.Duration) {
	deadline := time.Now().Add(max)
	var sawRunning bool
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		resp, err := client.Get(base + "/api/status")
		if err != nil {
			continue
		}
		var st struct {
			Run struct {
				Stage string `json:"stage"`
			} `json:"run"`
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if st.Run.Stage != "idle" {
			sawRunning = true
			continue
		}
		if sawRunning {
			fmt.Println("run finished.")
			return
		}
	}
}

// ------------------------------------------------------------ synthetic mode

func synthetic(nTopics, nItems, perSignal int, baselineFor, loadFor, every, slowLog time.Duration) error {
	dir, err := os.MkdirTemp("", "skyprobe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	db, err := store.Open(filepath.Join(dir, "interstellar.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Bootstrap(); err != nil {
		return err
	}

	itemIDs, err := seed(db, nTopics, nItems)
	if err != nil {
		return err
	}
	topics, err := db.ActiveTopics()
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d topics, %d items in %s\n\n", len(topics), len(itemIDs), dir)

	// The real server, real handlers, real store. Only the HTTP transport is
	// local, and the pipeline is replaced by the write loop below -- Runner and
	// Scheduler stay nil because /api/sky never touches them.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapiRoutes(db, log))
	defer srv.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	probe := func() (time.Duration, error) {
		start := time.Now()
		resp, err := client.Get(srv.URL + "/api/sky")
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return time.Since(start), fmt.Errorf("status %d", resp.StatusCode)
		}
		return time.Since(start), nil
	}

	p := newPoller(probe, every, slowLog)
	p.run()
	defer p.close()

	baseStart := time.Now()
	fmt.Printf("baseline: %s with no writes...\n", baselineFor)
	time.Sleep(baselineFor)
	baseStats := summarize(p.since(baseStart))

	fmt.Printf("load: %s of compose-shaped writes...\n", loadFor)
	loadStart := time.Now()
	w := writeLoad(db, topics, itemIDs, perSignal, loadFor)
	loadStats := summarize(p.since(loadStart))

	fmt.Println()
	report("baseline", baseStats)
	report("loaded", loadStats)
	fmt.Printf("\n  writes: %d ReplaceSignals + %d UpsertItem in %s; slowest single write %.1fms (%s)\n",
		w.composes, w.upserts, loadFor.Round(time.Millisecond), ms(w.worst), w.worstOp)
	verdict(baseStats, loadStats)
	return nil
}

type writeStats struct {
	composes, upserts int
	worst             time.Duration
	worstOp           string
}

// writeLoad mimics the shape of a run's write side: the compose stage replacing
// each topic's signal group, interleaved with ingest-style item upserts. It runs
// on one goroutine because the real pipeline does too -- every stage funnels its
// writes through a single drain loop.
func writeLoad(db *store.DB, topics []store.Topic, itemIDs []int64, perSignal int, for_ time.Duration) writeStats {
	var w writeStats
	deadline := time.Now().Add(for_)
	runDate := store.RunDate(time.Now())

	track := func(op string, start time.Time) {
		if d := time.Since(start); d > w.worst {
			w.worst, w.worstOp = d, op
		}
	}

	for round := 0; time.Now().Before(deadline); round++ {
		for _, t := range topics {
			if time.Now().After(deadline) {
				break
			}

			var signals []store.NewSignal
			for start := 0; start+perSignal <= len(itemIDs); start += perSignal {
				signals = append(signals, store.NewSignal{
					RunDate: runDate,
					TopicID: t.ID,
					Title:   fmt.Sprintf("%s signal %d", t.Title, len(signals)),
					Reason:  "synthetic load",
					ItemIDs: itemIDs[start : start+perSignal],
				})
			}

			began := time.Now()
			if err := db.ReplaceSignals(runDate, t.ID, signals); err != nil {
				fmt.Fprintln(os.Stderr, "  write error:", err)
				continue
			}
			track("ReplaceSignals", began)
			w.composes++

			// Ingest-shaped traffic between composes: many small transactions
			// rather than one fat one.
			for i := 0; i < 20 && time.Now().Before(deadline); i++ {
				began := time.Now()
				_, _, err := db.UpsertItem(store.NewItem{
					SourceID:     1,
					CanonicalURL: fmt.Sprintf("https://example.test/load/%d/%d/%d", round, t.ID, i),
					Title:        "synthetic item",
					Summary:      "written to contend for the single connection",
					PublishedAt:  time.Now(),
				})
				if err != nil {
					continue
				}
				track("UpsertItem", began)
				w.upserts++
			}
		}
	}
	return w
}

// seed fills the database with enough topics, items and matches that /api/sky
// and ReplaceSignals both do real work. Bootstrap already seeds topics; this
// tops them up to the requested count and gives every one a definition so none
// render as needs-setup.
func seed(db *store.DB, nTopics, nItems int) ([]int64, error) {
	existing, err := db.AllTopics()
	if err != nil {
		return nil, err
	}
	for i := len(existing); i < nTopics; i++ {
		x, y := store.NextTopicPosition(existing)
		id, err := db.CreateTopic(store.Topic{
			Title: fmt.Sprintf("Probe topic %d", i), Definition: "synthetic",
			Priority: 1, Active: true, PosX: x, PosY: y,
		})
		if err != nil {
			return nil, err
		}
		existing = append(existing, store.Topic{ID: id, PosX: x, PosY: y})
	}

	sources, err := db.ActiveSources()
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources seeded")
	}

	itemIDs := make([]int64, 0, nItems)
	for i := 0; i < nItems; i++ {
		src := sources[i%len(sources)]
		id, _, err := db.UpsertItem(store.NewItem{
			SourceID:     src.ID,
			CanonicalURL: fmt.Sprintf("https://example.test/seed/%d", i),
			Title:        fmt.Sprintf("Seed item %d", i),
			Summary:      "seeded so the read path has rows to return",
			PublishedAt:  time.Now().Add(-time.Duration(i) * time.Minute),
		})
		if err != nil {
			return nil, err
		}
		itemIDs = append(itemIDs, id)
	}

	// Matches, because ReplaceSignals computes score from them and skips a
	// signal whose members have none -- without these the write load would be
	// measuring a no-op.
	topics, err := db.ActiveTopics()
	if err != nil {
		return nil, err
	}
	for _, t := range topics {
		matches := make([]store.Match, 0, len(itemIDs))
		for i, id := range itemIDs {
			matches = append(matches, store.Match{
				ItemID: id, TopicID: t.ID,
				Score: float64(4 + i%2), Reason: "seeded",
			})
		}
		if err := db.SaveMatches(matches); err != nil {
			return nil, err
		}
	}
	return itemIDs, nil
}
