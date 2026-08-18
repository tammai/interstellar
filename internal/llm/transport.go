package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CallTimeout bounds a single model call. Generous, because a synthesizer pass
// over a day of matches is not fast, but finite: §10 budgets the whole refresh
// at three minutes.
const CallTimeout = 120 * time.Second

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is a provider-agnostic completion request.
type Request struct {
	Messages []Message
	// JSONMode asks the provider for guaranteed-parseable JSON via
	// response_format. Support varies -- DeepSeek and OpenAI honor it, others
	// ignore it -- so it is an optimization, never the thing correctness rests
	// on. Tolerant parsing in jsonx.go is what actually holds the JSON gate.
	JSONMode bool
	// MaxTokens of 0 lets the provider decide.
	MaxTokens int
}

// Usage is the token accounting a call reports.
type Usage struct {
	TokensIn  int
	TokensOut int
}

// Response is a completed call.
type Response struct {
	Text     string
	Usage    Usage
	Provider string
	Model    string
}

// chatRequest is the OpenAI-compatible wire format -- the one contract this
// package speaks.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// callError carries whether a failure is worth retrying. Timeouts, 429s and 5xx
// are transient; a 400 or 401 will fail identically forever, so burning the
// fallback chain on one is pure latency.
type callError struct {
	err       error
	status    int
	retryable bool
}

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// Retryable reports whether err is worth another attempt.
func Retryable(err error) bool {
	var ce *callError
	if ok := asCallError(err, &ce); ok {
		return ce.retryable
	}
	// An unclassified error is most likely a transport hiccup.
	return true
}

func asCallError(err error, target **callError) bool {
	for err != nil {
		if ce, ok := err.(*callError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// call performs one OpenAI-compatible chat completion against one provider.
// This is the only function in the codebase that talks to a model.
func call(ctx context.Context, client *http.Client, p Provider, model string, req Request) (Response, error) {
	key, ok := p.APIKey()
	if !ok {
		// Non-retryable: no amount of backoff conjures a missing env var.
		return Response{}, &callError{
			err:       fmt.Errorf("provider %q: environment variable %s is not set", p.Name, p.APIKeyEnv),
			retryable: false,
		}
	}

	body := chatRequest{
		Model:     model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
	}
	if req.JSONMode {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, &callError{err: fmt.Errorf("encode request: %w", err), retryable: false}
	}

	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint(), bytes.NewReader(payload))
	if err != nil {
		return Response{}, &callError{err: fmt.Errorf("build request: %w", err), retryable: false}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if !p.Keyless() {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		// Transport failures (including context deadline) are worth a retry.
		return Response{}, &callError{err: fmt.Errorf("call %s: %w", p.Name, err), retryable: true}
	}
	defer resp.Body.Close()

	// Cap the read: a misconfigured base_url can point at something that streams
	// forever.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, &callError{err: fmt.Errorf("read %s response: %w", p.Name, err), retryable: true}
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode >= 500
		return Response{}, &callError{
			err:       fmt.Errorf("%s returned %d: %s", p.Name, resp.StatusCode, snippet(raw)),
			status:    resp.StatusCode,
			retryable: retryable,
		}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, &callError{
			err:       fmt.Errorf("%s returned unparseable JSON: %w", p.Name, err),
			retryable: true,
		}
	}
	if parsed.Error != nil {
		return Response{}, &callError{
			err:       fmt.Errorf("%s error: %s", p.Name, parsed.Error.Message),
			retryable: false,
		}
	}
	if len(parsed.Choices) == 0 {
		return Response{}, &callError{
			err:       fmt.Errorf("%s returned no choices", p.Name),
			retryable: true,
		}
	}

	return Response{
		Text:     parsed.Choices[0].Message.Content,
		Usage:    Usage{TokensIn: parsed.Usage.PromptTokens, TokensOut: parsed.Usage.CompletionTokens},
		Provider: p.Name,
		Model:    model,
	}, nil
}

// snippet trims a provider error body to something loggable, preferring the
// sentence a person can act on over the envelope it arrived in.
//
// OpenAI-compatible providers wrap their errors as {"error":{"message":"..."}},
// and the message is the whole of the useful part: "this model requires a
// subscription, upgrade for access" tells you what to do, while the JSON around
// it tells you nothing and is long enough to push the message out of a truncated
// banner entirely. That is not hypothetical -- a lapsed Ollama subscription
// produced a 63-failure run whose banner was cut off mid-request-id.
//
// Falls back to the raw body, because a wrong base_url returns an HTML page with
// no envelope at all, and that page's first line is the clue.
func snippet(b []byte) string {
	if msg := errorMessage(b); msg != "" {
		return clip(msg)
	}
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	return clip(s)
}

// errorMessage digs the human-readable message out of an OpenAI-shaped error
// body. Returns "" for anything else, including valid JSON of another shape --
// guessing at an unknown schema would print something misleading with more
// confidence than the raw body deserves.
func errorMessage(b []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Error.Message)
}

func clip(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	// ToValidUTF8 drops a rune the cut may have split. These strings are stored
	// in llm_calls.error, so a broken one is persisted mojibake in the one place
	// someone looks when diagnosing a provider.
	return strings.ToValidUTF8(s[:max], "") + "..."
}
