package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// One OpenAI-compatible chat-completions adapter covers every provider we
// support: OpenAI natively, Gemini and Anthropic through their
// OpenAI-compatibility endpoints, any custom endpoint, and a local model server
// on the user's own machine.
//
// Unlike the FoxTrack web app, this call is made FROM the user's machine, not
// from a server acting on their behalf. That removes the SSRF problem the web
// app has to guard against, and it is the whole reason the "local" preset can
// exist here: pointing the Bridge at http://localhost:11434/v1 is a feature,
// not an attack.

const (
	PresetOpenAI    = "openai"
	PresetGemini    = "gemini"
	PresetAnthropic = "anthropic"
	PresetCustom    = "custom"
	PresetLocal     = "local"
)

// presetBaseURLs are the fixed endpoints for the hosted presets. "custom" and
// "local" have no entry: the user supplies the URL.
var presetBaseURLs = map[string]string{
	PresetOpenAI:    "https://api.openai.com/v1",
	PresetGemini:    "https://generativelanguage.googleapis.com/v1beta/openai",
	PresetAnthropic: "https://api.anthropic.com/v1",
}

// Presets lists every accepted preset id, for validation and for the UI.
func Presets() []string {
	return []string{PresetOpenAI, PresetGemini, PresetAnthropic, PresetCustom, PresetLocal}
}

// ValidPreset reports whether id is one we know how to call.
func ValidPreset(id string) bool {
	for _, p := range Presets() {
		if p == id {
			return true
		}
	}
	return false
}

// Settings is everything needed to reach a provider. It deliberately mirrors
// config.AI rather than importing it, so this package stays testable without a
// config file on disk.
type Settings struct {
	Preset      string
	BaseURL     string
	Model       string
	APIKey      string
	AllowCamera bool
}

// IsLocal reports whether the configured provider is the user's own machine.
// Error messages name it differently ("Your local model server" rather than
// "The AI provider"), because "check your plan and billing" is nonsense advice
// for llama.cpp.
func (s Settings) IsLocal() bool { return s.Preset == PresetLocal }

// subject is what error messages call this provider.
func (s Settings) subject() string {
	if s.IsLocal() {
		return "Your local model server"
	}
	return "The AI provider"
}

// ResolveBaseURL returns the endpoint root, with any trailing slashes removed.
func (s Settings) ResolveBaseURL() (string, error) {
	if s.Preset == PresetCustom || s.Preset == PresetLocal {
		trimmed := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if trimmed == "" {
			return "", fmt.Errorf("the %s provider needs a base URL", s.Preset)
		}
		return trimmed, nil
	}
	url, ok := presetBaseURLs[s.Preset]
	if !ok {
		return "", fmt.Errorf("unknown AI provider %q", s.Preset)
	}
	return url, nil
}

// Message is one turn on the wire. It is a bare map rather than a struct
// because an assistant turn has to be replayed EXACTLY as the provider sent it:
// Gemini's thinking models attach a thought_signature to tool calls and reject
// the following request with HTTP 400 if it is not echoed back. Reply.Raw is
// already a map[string]any for that reason, so it drops straight in here.
type Message map[string]any

// SystemMessage builds the system turn.
func SystemMessage(text string) Message {
	return Message{"role": "system", "content": text}
}

// UserMessage builds a plain text user turn.
func UserMessage(text string) Message {
	return Message{"role": "user", "content": text}
}

// UserMessageWithImage builds a user turn carrying one image alongside the
// text, in the OpenAI content-parts shape. dataURL is a full
// "data:image/jpeg;base64,..." string. Only reached when the user has switched
// the camera tool on.
func UserMessageWithImage(text, dataURL string) Message {
	return Message{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
		},
	}
}

// ToolMessage builds the result turn for one tool call. result is marshalled to
// JSON; if that fails the model is told so rather than being sent nothing,
// because a missing tool turn breaks the protocol for the next round.
func ToolMessage(callID string, result any) Message {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"error":"failed","message":"The tool result could not be encoded."}`)
	}
	return Message{"role": "tool", "tool_call_id": callID, "content": string(body)}
}

// Client calls one configured provider.
type Client struct {
	Settings Settings
	HTTP     *http.Client
}

// NewClient returns a client with the timeouts this package expects. A tool
// round trip plus generation can outlast 60s on a loaded provider, and a local
// model on CPU is slower still, so chat gets 120s.
func NewClient(s Settings) *Client {
	return &Client{Settings: s, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// Sampling. Left at the provider default this would be 1.0, which is the direct
// cause of degenerate repetition loops on smaller and local models. 0.3 is low
// enough to stop a loop and high enough that the prose is not wooden.
//
// Skipped for the openai preset alone: its newer reasoning models reject any
// temperature but the default with a 400, and a broken request is worse than a
// hot one. Same "newer model" branch as the token cap below.
const chatTemperature = 0.3

// Repetition penalties, sent to every provider except the openai preset (whose
// newer reasoning models reject sampling parameters outright, the same reason
// temperature is skipped there).
//
// These are not tuning for its own sake. A small local model asked a "how do I"
// question will answer it correctly and then keep writing sign-offs — "That's
// it, let me know if you need anything else" — forty times, until it hits the
// token cap. Temperature alone does not stop that, because every individual
// line is a plausible next line. A frequency penalty does, because the tokens
// those lines are made of have all been used already.
//
// Both are deliberately mild. Pushed higher they start distorting legitimate
// repetition, and a printer answer repeats words like "printer", "nozzle" and
// "LAN access code" honestly and often.
const (
	chatFrequencyPenalty = 0.4
	chatPresencePenalty  = 0.1
)

const maxCompletionTokens = 2048

// Chat runs one round: messages in, one model reply out. tools may be empty, in
// which case no tools and no tool_choice are sent, which is what forces a text
// answer on the final round. toolChoice is "required", "auto", or "" for absent.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool, toolChoice string) (Reply, error) {
	base, err := c.Settings.ResolveBaseURL()
	if err != nil {
		return Reply{}, err
	}

	body := map[string]any{
		"model":    c.Settings.Model,
		"messages": messages,
	}
	if len(tools) > 0 {
		body["tools"] = tools
		// Servers that do not implement tool_choice generally ignore the field
		// rather than rejecting it, so behaviour degrades to "auto" rather than
		// breaking.
		if toolChoice != "" {
			body["tool_choice"] = toolChoice
		}
	}
	// Newer OpenAI models reject max_tokens in favour of max_completion_tokens;
	// everyone else, including the compatibility endpoints, expects max_tokens.
	if c.Settings.Preset == PresetOpenAI {
		body["max_completion_tokens"] = maxCompletionTokens
	} else {
		body["max_tokens"] = maxCompletionTokens
		body["temperature"] = chatTemperature
		body["frequency_penalty"] = chatFrequencyPenalty
		body["presence_penalty"] = chatPresencePenalty
	}

	raw, err := c.post(ctx, base+"/chat/completions", body)
	if err != nil {
		return Reply{}, err
	}
	return ParseReply(raw)
}

// ListModels calls GET {base}/models, the OpenAI-compatible listing endpoint,
// so the settings screen can offer a picker instead of a free-text box. Short
// timeout: a user is waiting on a dropdown.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	base, err := c.Settings.ResolveBaseURL()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	c.authorize(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.statusError(resp, 200)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}

	seen := make(map[string]bool, len(parsed.Data))
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// post sends one JSON body and returns the raw response bytes.
func (c *Client) post(ctx context.Context, url string, body map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.statusError(resp, 300)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// authorize sets the bearer header. A local server usually wants no key at all,
// and some reject the header outright, so an empty key sends nothing.
func (c *Client) authorize(req *http.Request) {
	if key := strings.TrimSpace(c.Settings.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// statusError turns a non-2xx response into a message a user can act on. The
// provider's body is surfaced, but only a short slice of it, and never the
// request: the request holds the key and the whole transcript. Providers echo
// back the key they rejected — OpenAI replies to a bad key with a partly-masked
// copy of it — so the slice goes through RedactKey first.
func (c *Client) statusError(resp *http.Response, _ int) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	body := strings.TrimSpace(string(raw))
	if len(body) > 300 {
		body = body[:300]
	}
	body = RedactKey(body, c.Settings.APIKey)
	return fmt.Errorf("%s", ProviderErrorMessage(resp.StatusCode, body, c.Settings.subject()))
}

// transportError explains the failures a user can actually fix. A local server
// that is not running is by far the commonest one, and "connection refused" on
// its own sends people to the wrong place.
func (c *Client) transportError(err error) error {
	msg := RedactKey(err.Error(), c.Settings.APIKey)
	if c.Settings.IsLocal() {
		return fmt.Errorf("could not reach your local model server at %s. Check it is running and that the base URL is right: %s", c.Settings.BaseURL, msg)
	}
	return fmt.Errorf("could not reach the AI provider: %s", msg)
}
