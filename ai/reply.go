package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SECTION 1 — types.

// ToolCall is one tool invocation the model asked for.
type ToolCall struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

// Reply is the parsed assistant turn.
type Reply struct {
	Content   string
	ToolCalls []ToolCall
	// Raw is the provider's own assistant message, kept verbatim so it can be
	// replayed as conversation history. Some providers (Gemini thinking models)
	// hide required state in fields we do not model, and rebuilding the turn
	// from Content and ToolCalls drops it and breaks the next request with
	// HTTP 400. Never normalize or rebuild Raw.
	Raw map[string]any
}

// ErrEmptyReply is returned when the provider gives back no usable message.
var ErrEmptyReply = errors.New("the AI provider returned an empty response")

// SECTION 2 — ParseReply.

// ParseReply decodes an OpenAI-compatible chat-completion response body into a
// Reply.
func ParseReply(body []byte) (Reply, error) {
	var envelope struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Reply{}, fmt.Errorf("decode reply: %w", err)
	}
	if len(envelope.Choices) == 0 || len(envelope.Choices[0].Message) == 0 || string(envelope.Choices[0].Message) == "null" {
		return Reply{}, ErrEmptyReply
	}
	msg := envelope.Choices[0].Message

	var raw map[string]any
	if err := json.Unmarshal(msg, &raw); err != nil {
		return Reply{}, fmt.Errorf("decode reply: %w", err)
	}
	if _, ok := raw["role"]; !ok {
		raw["role"] = "assistant"
	}

	var typed struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(msg, &typed); err != nil {
		return Reply{}, fmt.Errorf("decode reply: %w", err)
	}

	content := ""
	if typed.Content != nil {
		content = *typed.Content
	}

	var calls []ToolCall
	for _, tc := range typed.ToolCalls {
		if tc.ID == "" || tc.Function.Name == "" {
			continue
		}
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		calls = append(calls, ToolCall{ID: tc.ID, Name: tc.Function.Name, ArgumentsJSON: args})
	}

	if len(calls) > 0 {
		return Reply{Content: content, ToolCalls: calls, Raw: raw}, nil
	}

	embedded := ParseEmbeddedToolCalls(content)
	if len(embedded) == 0 {
		return Reply{Content: content, ToolCalls: nil, Raw: raw}, nil
	}

	// The model wrote its tool call into the text instead of the tool_calls
	// field (small local models behind OpenAI-compatible servers do this). The
	// synthesized turn is used because those same servers accept a proper
	// tool_calls array in history but choke on the prose that produced it.
	synthesized := make([]map[string]any, 0, len(embedded))
	for _, c := range embedded {
		synthesized = append(synthesized, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.ArgumentsJSON,
			},
		})
	}
	newRaw := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": synthesized,
	}
	return Reply{Content: "", ToolCalls: embedded, Raw: newRaw}, nil
}

// SECTION 3 — ParseEmbeddedToolCalls.

var (
	toolCallTagRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	fencedJSONRe  = regexp.MustCompile(`(?s)^` + "```" + `(?:json)?\s*(.*?)\s*` + "```" + `$`)
)

// ParseEmbeddedToolCalls recovers tool calls a model wrote into the reply text
// instead of the tool_calls field.
func ParseEmbeddedToolCalls(content string) []ToolCall {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}

	var candidates []string
	if matches := toolCallTagRe.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
		for _, m := range matches {
			candidates = append(candidates, m[1])
		}
	} else {
		// Only if the tagged form found nothing: try a fenced JSON block, else
		// the whole text. A sentence that merely contains braces is an answer,
		// not a tool call, so it is only a candidate when it is a bare object.
		body := trimmed
		if m := fencedJSONRe.FindStringSubmatch(trimmed); m != nil {
			body = m[1]
		}
		if strings.HasPrefix(body, "{") && strings.HasSuffix(body, "}") {
			candidates = append(candidates, body)
		}
	}

	var calls []ToolCall
	for i, cand := range candidates {
		var obj map[string]any
		if err := json.Unmarshal([]byte(cand), &obj); err != nil {
			continue
		}
		fn := obj
		if f, ok := obj["function"].(map[string]any); ok {
			fn = f
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		var args any
		if a, ok := fn["arguments"]; ok {
			args = a
		} else if a, ok := fn["parameters"]; ok {
			args = a
		}
		var argsJSON string
		if s, ok := args.(string); ok {
			argsJSON = s
		} else if args == nil {
			argsJSON = "{}"
		} else {
			b, err := json.Marshal(args)
			if err != nil {
				argsJSON = "{}"
			} else {
				argsJSON = string(b)
			}
		}
		calls = append(calls, ToolCall{
			ID:            fmt.Sprintf("call_text_%d", i),
			Name:          name,
			ArgumentsJSON: argsJSON,
		})
	}
	return calls
}

// SECTION 4 — repetition collapsing.

const (
	repeatMinLength       = 24
	repeatAllowance       = 2
	replyMaxLength        = 8000
	collapseLineMinLength = 240
	collapseMinSentences  = 4
)

var (
	whitespaceRe   = regexp.MustCompile(`\s+`)
	newlineRunRe   = regexp.MustCompile(`\n{3,}`)
	trailingWordRe = regexp.MustCompile(`\s+\S*$`)
)

// repeatKey normalizes a fragment for repeat detection.
func repeatKey(s string) string {
	return whitespaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), " ")
}

// dropRepeats removes fragments that repeat beyond the allowance.
func dropRepeats(fragments []string) (kept []string, dropped bool) {
	counts := make(map[string]int)
	for _, frag := range fragments {
		k := repeatKey(frag)
		if len(k) < repeatMinLength {
			kept = append(kept, frag)
			continue
		}
		counts[k]++
		if counts[k] > repeatAllowance {
			dropped = true
			continue
		}
		kept = append(kept, frag)
	}
	return kept, dropped
}

// collapseSentences drops repeated sentences within a single long line.
func collapseSentences(line string) (string, bool) {
	if len(line) < collapseLineMinLength {
		return line, false
	}
	var parts []string
	var current strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '.' || r == '!' || r == '?' {
			j := i + 1
			for j < len(runes) && isWhitespace(runes[j]) {
				j++
			}
			if j < len(runes) {
				current.WriteRune(r)
				parts = append(parts, current.String())
				current.Reset()
				i = j - 1
				continue
			}
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	if len(parts) < collapseMinSentences {
		return line, false
	}
	kept, dropped := dropRepeats(parts)
	return strings.Join(kept, " "), dropped
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// CollapseRepetition trims degenerate repetition from a reply. This exists
// because degenerate repetition is a sampling failure that prompt wording
// reduces but never fixes, and it is deliberately conservative: a good answer
// does repeat a short line, so only substantial lines count and a line must
// appear a THIRD time before anything is dropped.
func CollapseRepetition(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	keptLines, dropped := dropRepeats(lines)

	for i, line := range keptLines {
		collapsed, lineDropped := collapseSentences(line)
		keptLines[i] = collapsed
		dropped = dropped || lineDropped
	}

	out := newlineRunRe.ReplaceAllString(strings.Join(keptLines, "\n"), "\n\n")
	out = strings.TrimSpace(out)

	truncated := false
	if len(out) > replyMaxLength {
		out = out[:replyMaxLength]
		out = trailingWordRe.ReplaceAllString(out, "")
		truncated = true
	}

	if truncated {
		return out + "\n\n_(This reply was cut short: the assistant kept going without adding anything new.)_"
	}
	return out
}

// SECTION 5 — error text helpers.

const rateLimitMessage = "Your AI provider says you have hit its rate limit. Wait a moment and try again, or check your plan and billing with your provider. Free tiers allow only a few requests per minute."

// ProviderErrorMessage builds a user-facing error string for a failed request.
func ProviderErrorMessage(status int, body, subject string) string {
	detail := body
	if status == 429 {
		detail = rateLimitMessage
	}
	return strings.TrimSpace(fmt.Sprintf("%s returned an error (%d). %s", subject, status, detail))
}

// RedactKey removes an API key (and any masked fragment of it) from text.
// Providers echo back a partly-masked copy of the key they rejected, which is
// why the 8-character windows are needed and why 8 is short enough to catch a
// mask but long enough that ordinary prose does not collide.
func RedactKey(text, apiKey string) string {
	key := strings.TrimSpace(apiKey)
	if len(key) < 8 {
		return text
	}
	text = strings.ReplaceAll(text, key, "[redacted]")

	seen := make(map[string]bool)
	for i := 0; i+8 <= len(key); i++ {
		window := key[i : i+8]
		seen[window] = true
	}
	windows := make([]string, 0, len(seen))
	for w := range seen {
		windows = append(windows, w)
	}
	sort.Strings(windows)
	for _, w := range windows {
		if strings.Contains(text, w) {
			text = strings.ReplaceAll(text, w, "[redacted]")
		}
	}
	return text
}
