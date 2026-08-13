// Package notify delivers the weekly round-up off-instance: email over SMTP, or a
// webhook ping.
//
// Both are OFF by default, and that is a design position rather than caution. Spec
// decision #10 dropped email from Phase 0 on purpose -- the web UI is the front
// door, and the pull model is what the habit is being tested on. Phase 2 opens only
// when "I forgot to open it" actually happens, so this exists to be switched on if
// that day comes, not to start mailing anyone.
//
// Credentials follow the same rule as provider keys (§3.1): the settings row holds
// the NAME of an environment variable, never a secret. Nothing here writes a
// password to the database or returns one from an endpoint.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Timeout bounds a delivery attempt. Generous for a slow relay, finite because a
// scheduled send must not wedge the scheduler goroutine.
const Timeout = 30 * time.Second

// SMTPConfig is everything needed to send one message.
//
// The defaults in bootstrap point at Resend's relay, which is why PasswordEnv
// exists rather than a password: Resend authenticates with the literal username
// "resend" and an API key as the password, and that key belongs in the environment
// exactly like a provider key.
type SMTPConfig struct {
	Host        string
	Port        int
	Username    string
	PasswordEnv string
	From        string
	To          string
}

// Configured reports whether enough is set to attempt a send.
func (c SMTPConfig) Configured() bool {
	return c.Host != "" && c.Port != 0 && c.From != "" && c.To != ""
}

// Password reads the secret from the environment at send time.
func (c SMTPConfig) Password() (string, bool) {
	if c.PasswordEnv == "" {
		return "", true // an open relay; unusual but legal
	}
	v := os.Getenv(c.PasswordEnv)
	return v, v != ""
}

// Problem describes why a send cannot be attempted, in words the owner can act on.
func (c SMTPConfig) Problem() string {
	switch {
	case c.Host == "":
		return "no SMTP host set"
	case c.Port == 0:
		return "no SMTP port set"
	case c.From == "":
		return "no From address set"
	case c.To == "":
		return "no To address set"
	}
	if _, ok := c.Password(); !ok {
		return fmt.Sprintf("environment variable %s is not set", c.PasswordEnv)
	}
	return ""
}

// SendEmail delivers one plain-text message.
//
// Implicit TLS on 465, STARTTLS everywhere else. Both paths verify the certificate:
// the message carries a week of someone's reading interests and the credential is an
// API key, so InsecureSkipVerify is not on the table even for a personal tool.
func SendEmail(ctx context.Context, cfg SMTPConfig, subject, body string) error {
	if problem := cfg.Problem(); problem != "" {
		return fmt.Errorf("%s", problem)
	}
	password, _ := cfg.Password()

	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	dialer := &net.Dialer{Timeout: Timeout}

	var (
		conn net.Conn
		err  error
	)
	if cfg.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()

	if cfg.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	if password != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp from %q: %w", cfg.From, err)
	}
	// One recipient per address, so a typo in the second does not silently drop the
	// first.
	for _, to := range recipients(cfg.To) {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp to %q: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(message(cfg, subject, body)); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}

	return client.Quit()
}

func recipients(to string) []string {
	var out []string
	for _, part := range strings.Split(to, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// message builds the RFC 5322 bytes.
//
// Plain text, not HTML. The round-up is prose a person reads once; an HTML version
// would be two bodies to keep in step for no gain, and plain text renders correctly
// in every client including the ones that strip markup.
func message(cfg SMTPConfig, subject, body string) []byte {
	var b bytes.Buffer

	// Encoded so a subject carrying a topic title in Vietnamese (§9) does not arrive
	// as mojibake.
	fmt.Fprintf(&b, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(recipients(cfg.To), ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")

	// Dot-stuffing: a line that is exactly "." would otherwise end the message
	// early, truncating everything after it.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}

	return b.Bytes()
}

// PingPayload is what a webhook receives.
//
// Deliberately small and structured rather than the round-up itself. A ping's job is
// "there is something to read", and the thing to read lives in the instance -- a
// webhook that carried the whole digest would be publishing a week of someone's
// reading interests to a third-party URL.
type PingPayload struct {
	Event     string `json:"event"`
	WeekStart string `json:"week_start"`
	WeekEnd   string `json:"week_end"`
	Signals   int    `json:"signals"`
	Topics    int    `json:"topics"`
	Summary   string `json:"summary"`
}

// SendPing posts a small JSON payload to a webhook.
func SendPing(ctx context.Context, url string, payload PingPayload) error {
	if url == "" {
		return fmt.Errorf("no webhook URL set")
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("webhook URL must start with http:// or https://")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Interstellar/0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
