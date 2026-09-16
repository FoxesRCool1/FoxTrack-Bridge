package ai

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReply_PlainContent(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"Hello there."}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply.Content != "Hello there." {
		t.Fatalf("content = %q, want %q", reply.Content, "Hello there.")
	}
	if len(reply.ToolCalls) != 0 {
		t.Fatalf("expected no tool calls, got %d", len(reply.ToolCalls))
	}
}

func TestParseReply_ToolCalls(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","function":{"name":"list_printers","arguments":"{}"}},{"id":"call_2","function":{"name":"get_status","arguments":"{\"printer\":\"p1\"}"}}]}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reply.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(reply.ToolCalls))
	}
	if reply.ToolCalls[0].ID != "call_1" || reply.ToolCalls[0].Name != "list_printers" {
		t.Fatalf("first call = %+v", reply.ToolCalls[0])
	}
	if reply.ToolCalls[1].ID != "call_2" || reply.ToolCalls[1].Name != "get_status" {
		t.Fatalf("second call = %+v", reply.ToolCalls[1])
	}
}

func TestParseReply_DropsToolCallWithoutID(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"","function":{"name":"no_id","arguments":"{}"}},{"id":"call_ok","function":{"name":"ok","arguments":"{}"}}]}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(reply.ToolCalls))
	}
	if reply.ToolCalls[0].ID != "call_ok" {
		t.Fatalf("kept call = %+v", reply.ToolCalls[0])
	}
}

func TestParseReply_EmptyArgumentsBecomesEmptyObject(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","function":{"name":"noop","arguments":""}}]}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(reply.ToolCalls))
	}
	if reply.ToolCalls[0].ArgumentsJSON != "{}" {
		t.Fatalf("arguments = %q, want %q", reply.ToolCalls[0].ArgumentsJSON, "{}")
	}
}

func TestParseReply_NoChoicesIsError(t *testing.T) {
	body := `{"choices":[]}`
	_, err := ParseReply([]byte(body))
	if !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("expected ErrEmptyReply, got %v", err)
	}
}

func TestParseReply_RawPreservesUnknownFields(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"hi","thought_signature":"abc123"}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := reply.Raw["thought_signature"]; !ok {
		t.Fatalf("Raw missing thought_signature: %v", reply.Raw)
	}
	if reply.Raw["thought_signature"] != "abc123" {
		t.Fatalf("thought_signature = %v", reply.Raw["thought_signature"])
	}
}

func TestParseReply_EmbeddedTaggedToolCall(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"<tool_call>{\"name\":\"list_printers\",\"arguments\":{}}</tool_call>"}}]}`
	reply, err := ParseReply([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply.Content != "" {
		t.Fatalf("content = %q, want empty", reply.Content)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(reply.ToolCalls))
	}
	if reply.ToolCalls[0].Name != "list_printers" {
		t.Fatalf("name = %q", reply.ToolCalls[0].Name)
	}
	tcs, ok := reply.Raw["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("Raw tool_calls = %v", reply.Raw["tool_calls"])
	}
}

func TestParseEmbeddedToolCalls_BareJSONObject(t *testing.T) {
	calls := ParseEmbeddedToolCalls(`{"name":"list_printers","arguments":{}}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "list_printers" {
		t.Fatalf("name = %q", calls[0].Name)
	}
}

func TestParseEmbeddedToolCalls_FencedJSON(t *testing.T) {
	calls := ParseEmbeddedToolCalls("```json\n{\"name\":\"get_status\",\"arguments\":{\"printer\":\"p1\"}}\n```")
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_status" {
		t.Fatalf("name = %q", calls[0].Name)
	}
}

func TestParseEmbeddedToolCalls_ProseWithBracesIsNotACall(t *testing.T) {
	calls := ParseEmbeddedToolCalls("The printer uses {braces} in its config file sometimes.")
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %d", len(calls))
	}
}

func TestParseEmbeddedToolCalls_FunctionWrapper(t *testing.T) {
	calls := ParseEmbeddedToolCalls(`{"function":{"name":"set_speed","arguments":{"level":"sport"}}}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "set_speed" {
		t.Fatalf("name = %q", calls[0].Name)
	}
}

func TestParseEmbeddedToolCalls_StringArgumentsPassThrough(t *testing.T) {
	calls := ParseEmbeddedToolCalls(`{"name":"noop","arguments":"{\"a\":1}"}`)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].ArgumentsJSON != `{"a":1}` {
		t.Fatalf("arguments = %q", calls[0].ArgumentsJSON)
	}
}

func TestCollapseRepetition_KeepsShortRepeatedLines(t *testing.T) {
	line := "short line"
	text := strings.Repeat(line+"\n", 5)
	out := CollapseRepetition(text)
	if got := strings.Count(out, line); got != 5 {
		t.Fatalf("expected 5 occurrences, got %d", got)
	}
}

func TestCollapseRepetition_DropsLongLineAfterTwo(t *testing.T) {
	line := strings.Repeat("a", 40)
	text := strings.Repeat(line+"\n", 5)
	out := CollapseRepetition(text)
	if got := strings.Count(out, line); got != 2 {
		t.Fatalf("expected 2 occurrences, got %d", got)
	}
}

func TestCollapseRepetition_AddsNoticeWhenTruncated(t *testing.T) {
	text := strings.Repeat("word ", 2000)
	out := CollapseRepetition(text)
	if !strings.HasSuffix(out, "_(This reply was cut short: the assistant kept going without adding anything new.)_") {
		t.Fatalf("output missing truncation notice: %q", out[len(out)-80:])
	}
}

func TestCollapseRepetition_EmptyInput(t *testing.T) {
	if out := CollapseRepetition(""); out != "" {
		t.Fatalf("expected empty, got %q", out)
	}
	if out := CollapseRepetition("   \n  "); out != "" {
		t.Fatalf("expected empty, got %q", out)
	}
}

func TestProviderErrorMessage_RateLimitReplacesBody(t *testing.T) {
	out := ProviderErrorMessage(429, "raw body text", "OpenAI")
	if !strings.Contains(out, rateLimitMessage) {
		t.Fatalf("output missing rate limit message: %q", out)
	}
	if strings.Contains(out, "raw body text") {
		t.Fatalf("output should not contain raw body: %q", out)
	}
}

func TestProviderErrorMessage_OtherStatusKeepsBody(t *testing.T) {
	out := ProviderErrorMessage(500, "internal boom", "OpenAI")
	if !strings.Contains(out, "internal boom") {
		t.Fatalf("output missing body: %q", out)
	}
	if !strings.Contains(out, "500") {
		t.Fatalf("output missing status: %q", out)
	}
}

func TestRedactKey_ShortKeyUnchanged(t *testing.T) {
	text := "the key is abc here"
	if out := RedactKey(text, "abc"); out != text {
		t.Fatalf("short key should be unchanged, got %q", out)
	}
}

func TestRedactKey_WholeKeyReplaced(t *testing.T) {
	key := "sk-abcdef123456"
	text := "use " + key + " to connect"
	out := RedactKey(text, key)
	if strings.Contains(out, key) {
		t.Fatalf("key still present: %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("expected [redacted]: %q", out)
	}
}

func TestRedactKey_MaskedFragmentReplaced(t *testing.T) {
	// A key whose 8-character windows are contiguous, so a provider mask that
	// keeps a run of 8 characters still exposes a window RedactKey can catch.
	key := "abcdefgh12345678"
	// The mask keeps the contiguous window "cdefgh12" (chars 2..9) intact.
	masked := "ab********5678"
	text := "provider echoed " + masked
	out := RedactKey(text, key)
	if strings.Contains(out, "cdefgh12") {
		t.Fatalf("masked fragment still present: %q", out)
	}
}
