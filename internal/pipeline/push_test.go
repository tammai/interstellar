package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/notify"
	"github.com/tammai/interstellar/internal/store"
)

// fakeSlack records what was posted to it.
type fakeSlack struct {
	*httptest.Server
	mu       sync.Mutex
	messages []string
	status   int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{status: http.StatusOK}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.messages = append(f.messages, body.Text)
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	// The fake speaks TLS because SendSlack refuses plain http -- the webhook URL
	// is a bearer credential and sending it in clear would hand the workspace to
	// anyone on the path. Swapping the client is what lets the test reach a
	// self-signed server without weakening the rule being tested.
	prev := notify.Client
	notify.Client = f.Server.Client()
	t.Cleanup(func() { notify.Client = prev; f.Close() })
	return f
}

func (f *fakeSlack) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messages...)
}

// pushCfg points Slack at the fake through an env var, which is how a real
// webhook is resolved: the settings table holds the NAME, never the URL.
func pushCfg(t *testing.T, url string) config.Runtime {
	t.Helper()
	t.Setenv("TEST_SLACK_WEBHOOK", url)
	return config.Runtime{SlackEnabled: true, SlackWebhookEnv: "TEST_SLACK_WEBHOOK"}
}

func seedPushClaim(t *testing.T, db *store.DB, entityID, itemID int64, changeType, sourceType, url string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO claims (entity_id, item_id, change_type, source_type, summary, url, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entityID, itemID, changeType, sourceType, changeType+" happened", url, changeType+url,
	); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
}

func seedPushEntity(t *testing.T, db *store.DB, name, layer string) int64 {
	t.Helper()
	res, err := db.ImportWatchlist([]store.EntityImport{{Name: name, Layer: layer}})
	if err != nil || res.Added != 1 {
		t.Fatalf("seed entity %q: %v (%+v)", name, err, res)
	}
	all, _ := db.Entities()
	for _, e := range all {
		if e.Name == name {
			return e.ID
		}
	}
	t.Fatalf("entity %q not found after import", name)
	return 0
}

// A quiet day posts NOTHING. A daily "nothing happened" trains the reader to
// stop looking, and the first genuinely quiet week would undo whatever attention
// the channel had earned.
func TestPushSendsNothingWhenNothingChanged(t *testing.T) {
	db := testDB(t)
	slack := newFakeSlack(t)

	res, err := Push(context.Background(), db, pushCfg(t, slack.URL), quietLogger())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Sent {
		t.Error("an empty run must not post")
	}
	if len(slack.sent()) != 0 {
		t.Errorf("posted %d messages on a quiet day", len(slack.sent()))
	}
	if res.Skipped != "nothing new" {
		t.Errorf("skipped = %q", res.Skipped)
	}
}

// Incidents and acquisitions go immediately and separately; everything else
// waits for the batch. An incident buried at position nine of a layer nobody
// scrolls to is an incident nobody reads.
func TestPushSeparatesUrgentFromDigest(t *testing.T) {
	db := testDB(t)
	slack := newFakeSlack(t)

	cursor := seedPushEntity(t, db, "Cursor", "coding_agents")
	snyk := seedPushEntity(t, db, "Snyk", "security")
	src := seedSource(t, db, "a feed")
	a := seedItem(t, db, src, "https://x.test/1", "one")
	b := seedItem(t, db, src, "https://x.test/2", "two")

	seedPushClaim(t, db, cursor, a, "launch", "independent", "https://x.test/1")
	seedPushClaim(t, db, snyk, b, "incident", "first_party", "https://x.test/2")

	res, err := Push(context.Background(), db, pushCfg(t, slack.URL), quietLogger())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Urgent != 1 {
		t.Errorf("urgent = %d, want 1", res.Urgent)
	}

	msgs := slack.sent()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (one urgent, one digest)", len(msgs))
	}
	if !strings.Contains(msgs[0], "INCIDENT") || !strings.Contains(msgs[0], "Snyk") {
		t.Errorf("first message is not the incident: %q", msgs[0])
	}
	if !strings.Contains(msgs[1], "Cursor") {
		t.Errorf("digest missing the launch: %q", msgs[1])
	}
	if strings.Contains(msgs[1], "Snyk") {
		t.Errorf("urgent item repeated in the digest: %q", msgs[1])
	}
	// The digest groups by layer, which is how the owner reads the field.
	if !strings.Contains(msgs[1], "coding agents") {
		t.Errorf("digest is not grouped by layer: %q", msgs[1])
	}
}

// Everything sent is stamped, so tomorrow does not repeat it.
func TestPushMarksWhatItSent(t *testing.T) {
	db := testDB(t)
	slack := newFakeSlack(t)

	cursor := seedPushEntity(t, db, "Cursor", "coding_agents")
	src := seedSource(t, db, "a feed")
	item := seedItem(t, db, src, "https://x.test/1", "one")
	seedPushClaim(t, db, cursor, item, "launch", "independent", "https://x.test/1")

	cfg := pushCfg(t, slack.URL)
	if _, err := Push(context.Background(), db, cfg, quietLogger()); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	res, err := Push(context.Background(), db, cfg, quietLogger())
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if res.Sent {
		t.Error("the same change was pushed twice")
	}
	if len(slack.sent()) != 1 {
		t.Errorf("messages = %d, want 1", len(slack.sent()))
	}
}

// A Slack outage costs a day's notification, not a day's changes: nothing is
// stamped, so the next push picks them up.
func TestPushFailureLeavesClaimsUnpushed(t *testing.T) {
	db := testDB(t)
	slack := newFakeSlack(t)
	slack.status = http.StatusInternalServerError

	cursor := seedPushEntity(t, db, "Cursor", "coding_agents")
	src := seedSource(t, db, "a feed")
	item := seedItem(t, db, src, "https://x.test/1", "one")
	seedPushClaim(t, db, cursor, item, "launch", "independent", "https://x.test/1")

	cfg := pushCfg(t, slack.URL)
	if _, err := Push(context.Background(), db, cfg, quietLogger()); err == nil {
		t.Fatal("a 500 from Slack should be reported")
	}

	pending, err := db.PendingDeltas(14, 0)
	if err != nil {
		t.Fatalf("PendingDeltas: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending after a failed push = %d, want 1", len(pending))
	}
}

// An unset variable is reported BY NAME -- the owner needs to know which one to
// set, and the value must never reach a log or a status payload.
func TestPushReportsUnsetWebhookByName(t *testing.T) {
	db := testDB(t)
	t.Setenv("TEST_SLACK_WEBHOOK", "")

	cfg := config.Runtime{SlackEnabled: true, SlackWebhookEnv: "TEST_SLACK_WEBHOOK"}
	res, err := Push(context.Background(), db, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !strings.Contains(res.Skipped, "TEST_SLACK_WEBHOOK") {
		t.Errorf("skipped = %q, want the variable named", res.Skipped)
	}
}

func TestPushDisabled(t *testing.T) {
	db := testDB(t)
	slack := newFakeSlack(t)
	cfg := pushCfg(t, slack.URL)
	cfg.SlackEnabled = false

	res, err := Push(context.Background(), db, cfg, quietLogger())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.Sent || len(slack.sent()) != 0 {
		t.Error("disabled push still posted")
	}
}
