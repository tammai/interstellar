package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file is where the §10 acceptance criterion lives: "classifier JSON parses
// 100% on a 200-item sample." That bar cannot be met by asking nicely.
//
// response_format is an optimization, not a guarantee -- support varies across
// the providers the registry is designed to accept, and Ollama-hosted models
// vary model to model. So parsing assumes the output is *almost* JSON and works
// to recover it: fenced blocks, a sentence of preamble, a wrapper object, a
// trailing comma. Anything still unusable is reported so the caller can spend
// one repair round-trip on it.

// ExtractArray recovers a JSON array from model output.
//
// It handles, in order: markdown fences, prose around the payload, an object
// wrapping the array under a single key, and trailing commas.
func ExtractArray(raw string) ([]json.RawMessage, error) {
	text := stripFences(raw)

	candidate, ok := firstBalanced(text, '[', ']')
	if !ok {
		// Some providers in JSON-object mode cannot return a bare array, so they
		// wrap it: {"verdicts": [...]}. Unwrap a single array-valued key.
		if obj, objOK := firstBalanced(text, '{', '}'); objOK {
			if inner, innerOK := unwrapSingleArray(obj); innerOK {
				candidate = inner
			} else {
				return nil, fmt.Errorf("no JSON array in model output: %s", snippetOf(raw))
			}
		} else {
			return nil, fmt.Errorf("no JSON array in model output: %s", snippetOf(raw))
		}
	}

	candidate = dropTrailingCommas(candidate)

	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &elems); err != nil {
		return nil, fmt.Errorf("model output is not a JSON array: %w", err)
	}
	return elems, nil
}

// DecodeElements decodes each element independently, dropping the ones that
// don't survive.
//
// Per-element decoding is the point: one malformed entry in a batch of thirty
// must cost one verdict, not the whole batch and a retry. The dropped count is
// returned so the caller can log it rather than lose it silently.
func DecodeElements[T any](elems []json.RawMessage, validate func(T) error) (out []T, dropped int) {
	out = make([]T, 0, len(elems))
	for _, e := range elems {
		var v T
		if err := json.Unmarshal(e, &v); err != nil {
			dropped++
			continue
		}
		if validate != nil {
			if err := validate(v); err != nil {
				dropped++
				continue
			}
		}
		out = append(out, v)
	}
	return out, dropped
}

// stripFences removes markdown code fences, which models add even when told not
// to. Handles ```json, bare ```, and an unclosed opening fence.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "```") {
		return s
	}

	start := strings.Index(s, "```")
	rest := s[start+3:]
	// Drop an optional language tag on the opening fence.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		firstLine := strings.TrimSpace(rest[:nl])
		if firstLine == "" || isWord(firstLine) {
			rest = rest[nl+1:]
		}
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

func isWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return len(s) > 0
}

// firstBalanced returns the first balanced open..close span, ignoring delimiters
// inside string literals. A naive LastIndex would break on any payload whose
// content contains a bracket, which article titles regularly do.
func firstBalanced(s string, open, close byte) (string, bool) {
	start := -1
	depth := 0
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case open:
			if depth == 0 {
				start = i
			}
			depth++
		case close:
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					return s[start : i+1], true
				}
			}
		}
	}
	return "", false
}

// unwrapSingleArray pulls the array out of a wrapper object that has exactly one
// array-valued key.
func unwrapSingleArray(obj string) (string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dropTrailingCommas(obj)), &m); err != nil {
		return "", false
	}
	var found string
	count := 0
	for _, v := range m {
		trimmed := strings.TrimSpace(string(v))
		if strings.HasPrefix(trimmed, "[") {
			found = trimmed
			count++
		}
	}
	if count != 1 {
		return "", false
	}
	return found, true
}

// dropTrailingCommas removes commas that sit immediately before a closing
// bracket or brace. It tracks string state, so a comma inside a title is safe.
func dropTrailingCommas(s string) string {
	out := make([]byte, 0, len(s))
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if escaped {
			escaped = false
			out = append(out, ch)
			continue
		}
		if inString {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			out = append(out, ch)
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == ',' {
			// Look ahead past whitespace for a closer.
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == ']' || s[j] == '}') {
				continue // drop it
			}
		}
		out = append(out, ch)
	}
	return string(out)
}

func snippetOf(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 160
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
