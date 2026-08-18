package notify

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRelay is a minimal SMTP server: enough of the protocol to accept one message
// and hand back what it received.
//
// No TLS, so it exercises the plaintext path. That is the honest scope of a unit
// test here -- verifying real STARTTLS against Resend needs Resend, which is what
// POST /api/delivery/test is for.
type fakeRelay struct {
	addr string
	mu   sync.Mutex
	// received holds message bodies, commands the protocol lines that carried them.
	// The envelope is only visible in the second: a display name that is perfectly
	// fine in a From: header is a 501 in MAIL FROM, and a fake that recorded only
	// bodies could not tell the two apart -- which is how that bug got past this
	// file once already.
	received []string
	commands []string
	rejectAt string // command to answer with 550, or empty
	ln       net.Listener
}

func newFakeRelay(t *testing.T, rejectAt string) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &fakeRelay{addr: ln.Addr().String(), ln: ln, rejectAt: rejectAt}
	go r.serve()
	t.Cleanup(func() { ln.Close() })
	return r
}

func (r *fakeRelay) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		go r.handle(conn)
	}
}

func (r *fakeRelay) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }

	w("220 fake ESMTP")
	inData := false
	var body strings.Builder

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")

		if inData {
			if trimmed == "." {
				inData = false
				r.mu.Lock()
				r.received = append(r.received, body.String())
				r.mu.Unlock()
				body.Reset()
				w("250 queued")
				continue
			}
			body.WriteString(trimmed)
			body.WriteString("\n")
			continue
		}

		upper := strings.ToUpper(trimmed)
		r.mu.Lock()
		r.commands = append(r.commands, trimmed)
		r.mu.Unlock()

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			// AUTH advertised so smtp.PlainAuth is willing to run over plaintext…
			// which it is not, on a non-localhost host. 127.0.0.1 counts as local.
			w("250-fake")
			w("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(upper, "AUTH"):
			w("235 ok")
		case r.rejectAt != "" && strings.HasPrefix(upper, r.rejectAt):
			w("550 rejected")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			// Strict on purpose, and this is the whole point of the fake. A real
			// relay answers an envelope path carrying a display name with 501 "Bad
			// sender address syntax" -- Resend does, correctly -- so a fake that
			// answered 250 to anything made the send look fine in tests while every
			// send from a fresh instance failed.
			if !bareEnvelopePath(trimmed) {
				w(`501 Error: Bad sender address syntax`)
				continue
			}
			w("250 ok")
		case strings.HasPrefix(upper, "DATA"):
			inData = true
			w("354 go ahead")
		case strings.HasPrefix(upper, "QUIT"):
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// bareEnvelopePath reports whether an SMTP command's argument is an RFC 5321 path:
// one bracketed addr-spec, no display name, no nested brackets, no spaces.
func bareEnvelopePath(line string) bool {
	_, arg, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	arg = strings.TrimSpace(arg)
	// ESMTP parameters (SIZE=, BODY=8BITMIME) follow the path, space-separated.
	if i := strings.IndexByte(arg, ' '); i > 0 {
		arg = arg[:i]
	}
	if !strings.HasPrefix(arg, "<") || !strings.HasSuffix(arg, ">") {
		return false
	}
	inner := arg[1 : len(arg)-1]
	if strings.ContainsAny(inner, "<> \"") {
		return false
	}
	return strings.Count(inner, "@") == 1
}

func (r *fakeRelay) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.received...)
}

func (r *fakeRelay) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

func relayConfig(t *testing.T, relay *fakeRelay) SMTPConfig {
	t.Helper()
	host, port, _ := net.SplitHostPort(relay.addr)
	var p int
	fmt.Sscanf(port, "%d", &p)
	t.Setenv("TEST_SMTP_KEY", "re_testkey")
	return SMTPConfig{
		Host: host, Port: p, Username: "resend", PasswordEnv: "TEST_SMTP_KEY",
		From: "Interstellar <onboarding@resend.dev>", To: "owner@example.test",
	}
}

func TestSendEmailDeliversAWellFormedMessage(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)

	err := SendEmail(context.Background(), cfg, "Week of 2026-08-10", "First line.\nSecond line.")
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}

	msgs := relay.messages()
	if len(msgs) != 1 {
		t.Fatalf("relay received %d messages, want 1", len(msgs))
	}
	msg := msgs[0]

	for _, want := range []string{
		// Quoted and bracketed because mail.Address.String() renders it, which is
		// what makes a non-ASCII display name arrive encoded rather than raw. A
		// client shows "Interstellar" either way.
		`From: "Interstellar" <onboarding@resend.dev>`,
		"To: <owner@example.test>",
		"Content-Type: text/plain; charset=utf-8",
		"MIME-Version: 1.0",
		"First line.",
		"Second line.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
	// A Date header, or spam filters treat it as suspicious.
	if !strings.Contains(msg, "Date: ") {
		t.Error("message has no Date header")
	}
	// The secret must never appear in the message itself.
	if strings.Contains(msg, "re_testkey") {
		t.Error("the API key leaked into the message body")
	}
}

// §9 keeps topic titles in Vietnamese, so a subject carrying one must not arrive as
// mojibake.
func TestSendEmailEncodesNonASCIISubjects(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)

	if err := SendEmail(context.Background(), cfg, "Tuần của Tiếng Việt", "Body."); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	msg := relay.messages()[0]
	if strings.Contains(msg, "Subject: Tuần") {
		t.Error("the subject was sent as raw UTF-8 rather than encoded")
	}
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Errorf("the subject is not RFC 2047 encoded:\n%s", msg)
	}
}

// A body line that is exactly "." would end the message early, truncating
// everything after it.
func TestSendEmailDotStuffsTheBody(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)

	body := "Before the dot.\n.\nAfter the dot."
	if err := SendEmail(context.Background(), cfg, "Test", body); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}

	msgs := relay.messages()
	if len(msgs) != 1 {
		t.Fatalf("relay received %d messages", len(msgs))
	}
	if !strings.Contains(msgs[0], "After the dot.") {
		t.Errorf("the message was truncated at the bare dot:\n%s", msgs[0])
	}
}

func TestSendEmailSplitsMultipleRecipients(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)
	cfg.To = "one@example.test, two@example.test"

	if err := SendEmail(context.Background(), cfg, "Test", "Body."); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	msg := relay.messages()[0]
	if !strings.Contains(msg, "<one@example.test>, <two@example.test>") {
		t.Errorf("To header = wrong:\n%s", msg)
	}
}

// The envelope takes the bare address and the header takes the display name. This is
// the bug the strict relay above exists for: digest_from seeds to
// `Interstellar <onboarding@resend.dev>`, and sending that string down MAIL FROM
// meant every send from a fresh instance came back 501 "Bad sender address syntax".
func TestSendEmailSendsBareAddressesInTheEnvelope(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)
	// A comma inside a display name, which is legal and is why the recipient list is
	// parsed rather than split on commas. This is ONE recipient.
	cfg.To = `"Mai, Tam" <owner@example.test>`

	if err := SendEmail(context.Background(), cfg, "Test", "Body."); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}

	var envelope []string
	for _, line := range relay.sent() {
		if u := strings.ToUpper(line); strings.HasPrefix(u, "MAIL FROM") || strings.HasPrefix(u, "RCPT TO") {
			envelope = append(envelope, line)
		}
	}
	joined := strings.Join(envelope, "\n")

	if !strings.Contains(joined, "MAIL FROM:<onboarding@resend.dev>") {
		t.Errorf("envelope sender is not a bare address:\n%s", joined)
	}
	if !strings.Contains(joined, "RCPT TO:<owner@example.test>") {
		t.Errorf("envelope recipient is not a bare address:\n%s", joined)
	}
	if n := strings.Count(joined, "RCPT TO"); n != 1 {
		t.Errorf("%d recipients from a display name containing a comma:\n%s", n, joined)
	}
	// The display name must not reach the envelope at all, under any quoting.
	for _, line := range envelope {
		if strings.Contains(line, "Interstellar") || strings.Contains(line, "Mai") {
			t.Errorf("a display name reached the envelope: %q", line)
		}
	}
	// And it must still reach the header, which is where it belongs.
	if msg := relay.messages()[0]; !strings.Contains(msg, "Interstellar") {
		t.Errorf("the display name was lost from the From header:\n%s", msg)
	}
}

// §9 keeps topic definitions in Vietnamese, and an owner naming their instance in it
// gets a display name that is illegal in a header as raw bytes.
func TestSendEmailEncodesANonASCIIFromName(t *testing.T) {
	relay := newFakeRelay(t, "")
	cfg := relayConfig(t, relay)
	cfg.From = "Tin tức <onboarding@resend.dev>"

	if err := SendEmail(context.Background(), cfg, "Test", "Body."); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	msg := relay.messages()[0]
	if strings.Contains(msg, "From: Tin tức") {
		t.Errorf("the From name was sent as raw UTF-8:\n%s", msg)
	}
	if !strings.Contains(msg, "From: =?utf-8?") {
		t.Errorf("the From name is not RFC 2047 encoded:\n%s", msg)
	}
	// Encoding the name must not touch the address.
	if !strings.Contains(strings.Join(relay.sent(), "\n"), "MAIL FROM:<onboarding@resend.dev>") {
		t.Error("the envelope sender changed when the display name was encoded")
	}
}

// A malformed address is caught before a connection is made, so the Weekly screen can
// say what is wrong instead of relaying a numeric SMTP code.
func TestSMTPConfigProblemCatchesMalformedAddresses(t *testing.T) {
	t.Setenv("SET_KEY", "value")
	base := SMTPConfig{
		Host: "smtp.resend.com", Port: 587, Username: "resend", PasswordEnv: "SET_KEY",
		From: "Interstellar <onboarding@resend.dev>", To: "owner@example.test",
	}
	if p := base.Problem(); p != "" {
		t.Fatalf("a display-name From is legal and reported %q", p)
	}

	for name, mutate := range map[string]func(*SMTPConfig){
		"from is not an address": func(c *SMTPConfig) { c.From = "Interstellar" },
		"from has two addresses": func(c *SMTPConfig) { c.From = "a@b.test, c@d.test" },
		"to is not an address":   func(c *SMTPConfig) { c.To = "owner at example" },
	} {
		c := base
		mutate(&c)
		if c.Problem() == "" {
			t.Errorf("%s: no problem reported", name)
		}
	}

	// A trailing comma is a typo, not a reason to refuse to send.
	ok := base
	ok.To = "one@example.test, two@example.test, "
	if p := ok.Problem(); p != "" {
		t.Errorf("a trailing comma reported %q", p)
	}
}

func TestSendEmailReportsRelayRejections(t *testing.T) {
	relay := newFakeRelay(t, "RCPT TO")
	cfg := relayConfig(t, relay)

	err := SendEmail(context.Background(), cfg, "Test", "Body.")
	if err == nil {
		t.Fatal("a rejected recipient should be an error")
	}
	// The address has to be in the message, or the owner cannot tell which failed.
	if !strings.Contains(err.Error(), "owner@example.test") {
		t.Errorf("error does not name the recipient: %v", err)
	}
}

// The configuration contract: the settings row holds an env var NAME, and a missing
// value is a clear problem rather than a silent unauthenticated send.
func TestSMTPConfigProblemNamesWhatIsMissing(t *testing.T) {
	base := SMTPConfig{
		Host: "smtp.resend.com", Port: 587, Username: "resend",
		PasswordEnv: "DEFINITELY_UNSET_KEY",
		From:        "a@b.test", To: "c@d.test",
	}
	if p := base.Problem(); !strings.Contains(p, "DEFINITELY_UNSET_KEY") {
		t.Errorf("problem = %q; it should name the unset variable", p)
	}

	t.Setenv("SET_KEY", "value")
	ok := base
	ok.PasswordEnv = "SET_KEY"
	if p := ok.Problem(); p != "" {
		t.Errorf("a fully configured relay reports %q", p)
	}
	if !ok.Configured() {
		t.Error("Configured() is false for a complete config")
	}

	for name, mutate := range map[string]func(*SMTPConfig){
		"no host": func(c *SMTPConfig) { c.Host = "" },
		"no port": func(c *SMTPConfig) { c.Port = 0 },
		"no from": func(c *SMTPConfig) { c.From = "" },
		"no to":   func(c *SMTPConfig) { c.To = "" },
	} {
		c := ok
		mutate(&c)
		if c.Problem() == "" {
			t.Errorf("%s: no problem reported", name)
		}
		if c.Configured() {
			t.Errorf("%s: Configured() is true", name)
		}
	}
}

func TestSendPingPostsASmallPayload(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		got = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := SendPing(context.Background(), srv.URL, PingPayload{
		Event: "weekly_digest", WeekStart: "2026-08-10", Signals: 12,
		Summary: "Harness convergence.",
	})
	if err != nil {
		t.Fatalf("SendPing: %v", err)
	}
	for _, want := range []string{`"event":"weekly_digest"`, `"week_start":"2026-08-10"`, `"signals":12`} {
		if !strings.Contains(got, want) {
			t.Errorf("payload missing %q: %s", want, got)
		}
	}
}

func TestSendPingValidatesAndReportsFailures(t *testing.T) {
	if err := SendPing(context.Background(), "", PingPayload{}); err == nil {
		t.Error("an empty URL should be an error")
	}
	if err := SendPing(context.Background(), "not-a-url", PingPayload{}); err == nil {
		t.Error("a URL with no scheme should be an error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	err := SendPing(context.Background(), bad.URL, PingPayload{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 should be reported with its status: %v", err)
	}
}
