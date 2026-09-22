package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// SlackMessage is what goes to an incoming webhook.
//
// mrkdwn text rather than Block Kit, for the same reason the weekly round-up's
// body is plain text with hard newlines: it has to work in more than one place,
// and a block layout that renders beautifully in a channel is a wall of JSON in a
// log or a test. Slack accepts a single text field on every surface it has.
type SlackMessage struct {
	Text string `json:"text"`
}

// Client is the HTTP client outbound notifications use.
//
// A package variable for the same reason sources.DefaultClient is one: a test
// server speaking TLS presents a self-signed certificate, and http.DefaultClient
// correctly refuses it. Swapping the client is how a test exercises the real
// https path -- which matters here, because refusing plain http IS the behaviour
// under test. A test that reached the fake over http would be testing a code path
// this function does not have.
var Client = http.DefaultClient

// slackTextLimit is Slack's cap on a message's text field. Exceeding it is a 400
// and the whole digest is lost, so a long day is truncated with a line saying so
// rather than silently dropped.
const slackTextLimit = 38000

// SendSlack posts a message to an incoming webhook.
//
// Separate from SendPing, deliberately. A ping is content-free on purpose -- its
// doc comment says a webhook carrying the digest would publish a week of the
// owner's reading interests to a third-party URL -- and that rule is correct for
// an endpoint the owner did not choose. This one carries content because the
// owner chose Slack, which means accepting that the deltas travel there. The two
// should not be merged: bending PingPayload to carry a digest would quietly
// repeal a rule that still applies to the round-up.
//
// The URL is a CREDENTIAL, not an address. Anyone holding a Slack incoming
// webhook URL can post into the workspace, so it follows the provider-key
// contract exactly: settings store the env var NAME and the value is read at send
// time. Nothing here ever logs it.
func SendSlack(ctx context.Context, webhookURL string, msg SlackMessage) error {
	if webhookURL == "" {
		return fmt.Errorf("no Slack webhook URL set")
	}
	if !strings.HasPrefix(webhookURL, "https://") {
		// Slack's own webhooks are always https. Refusing http here is not
		// pedantry: the URL is a bearer credential and sending it in clear would
		// hand the workspace to anyone on the path.
		return fmt.Errorf("Slack webhook URL must start with https://")
	}

	if len(msg.Text) > slackTextLimit {
		msg.Text = msg.Text[:slackTextLimit] + "\n… truncated."
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Interstellar/0.1")

	resp, err := Client.Do(req)
	if err != nil {
		// The error from a failed request embeds the URL, which is the credential.
		// Report the failure without it.
		return fmt.Errorf("could not reach the Slack webhook")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Slack answers with a short reason in the body ("invalid_token",
		// "channel_not_found"), which is the only part naming what to do -- the
		// same reasoning llm.snippet applies to provider errors.
		buf := make([]byte, 200)
		n, _ := resp.Body.Read(buf)
		reason := strings.TrimSpace(string(buf[:n]))
		if reason == "" {
			return fmt.Errorf("Slack returned %s", resp.Status)
		}
		return fmt.Errorf("Slack returned %s: %s", resp.Status, reason)
	}
	return nil
}
