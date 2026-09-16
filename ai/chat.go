package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxContextMessages caps how much of a conversation is replayed. Older
	// turns are dropped from the front.
	MaxContextMessages = 30
	// MaxToolRounds caps tool use per reply. The final round is offered no
	// tools at all, which is what forces a text answer.
	MaxToolRounds = 4
	// ToolRowLimit caps rows in a data tool result, so one question cannot
	// drag a thousand history records into the prompt.
	ToolRowLimit = 20
	// substantialPreambleLength is the point at which prose written alongside a
	// tool call stops looking like "let me check that" and starts looking like
	// an answer worth keeping.
	substantialPreambleLength = 300
	// thinReplyLength is the point below which a final answer looks more like a
	// footnote than a reply.
	thinReplyLength = 300
)

// The "you know nothing" rules below are load-bearing, not boilerplate. A model
// that has seen ONE successful tool result starts treating itself as informed
// and answers the next question from imagination — which for a printer
// assistant means inventing a temperature, a status, or a Bambu error code and
// sending someone to take a working printer apart. Weaken these lines and that
// comes back.
//
// This prompt is deliberately NOT the FoxTrack web app's prompt. Same
// discipline, different job: this assistant sees one household's printers, has
// no business records, and its worst failure mode is confidently wrong
// hardware advice rather than a fabricated invoice.
var SystemPrompt = strings.Join([]string{
	"You are the assistant inside FoxTrack Bridge, a local dashboard that runs on the user's own machine and talks to their 3D printers over their network. You are also its support agent: when someone is stuck you work out what they are trying to do, give them the exact steps in the dashboard or on the printer, and tell them when the answer is not yours to give.",
	"",
	"Two kinds of question, two different sources. Never mix them up:",
	"- About this user's printers right now (is it online, what is it printing, how hot is the nozzle, what went wrong, what does the camera show): the only source is a tool result in the reply you are writing right now.",
	"- About how the Bridge works, or what to do when something is wrong (where a setting is, how to add a printer, what a feature does, why something is not happening): the only source is the help library, read with search_help and get_help_topic. Help extracts may also be placed in this prompt for the current message; they came from the same library and count as a search_help result. Never answer these from what you know about other software.",
	"",
	"What you know:",
	"- You have NO knowledge of this user's printers, and no memory of how the Bridge works. Every fact in your answer must come from a tool result or a help extract in the reply you are writing right now.",
	"- Earlier messages in this conversation are not a source. An answer you gave before does not mean you still know anything, and it never licenses answering a new question without calling a tool.",
	"- If you did not call a tool for the question in front of you, you do not know the answer. Say so, and say which tool you would need.",
	"- Never invent a printer, a status, a temperature, a progress figure, a file name or a log line. Not even as an illustration or an example.",
	"- Never invent Bridge behaviour either. No made-up screen, tab, button, field, setting or option. If the help library does not cover it, say it is not in the help you can see.",
	"",
	"Printer knowledge, and its one hard limit:",
	"- General 3D printing craft is yours to use: what causes warping, stringing, poor bed adhesion, layer shifting, clogs, under-extrusion, elephant's foot. You may explain those and suggest what to change, as long as you are clear you are reasoning from general printing knowledge and not from this printer.",
	"- Printer ERROR CODES are the exception and there is no judgement call here. An error code reported by a Bambu or Klipper printer belongs to that printer's firmware. Never guess what a code means, never guess how serious it is, and never invent a fix for one. Report the code exactly as the tool gave it and tell the user to look it up in the manufacturer's documentation.",
	"- Never give advice that would have the user work on a hot nozzle or hot bed without saying it is hot first.",
	"",
	"What you can do:",
	"- You are read-only. You cannot add, edit or remove a printer, cannot start, pause, resume or stop a print, cannot move an axis or send GCode, and cannot change any setting. Nothing you do changes anything.",
	"- When the user asks you to do one of those, tell them exactly where to do it themselves: the page, the button, the field, named as the help library names them.",
	"- Never tell the user you have connected, fixed, changed, restarted, paused or saved anything. You have not. A user who is told something is done will stop checking.",
	"",
	"Helping someone connect a printer:",
	"- Ask which kind first if you do not know: Bambu Lab in LAN mode, Bambu Lab through the cloud, or Klipper through Moonraker. The steps are different and the wrong ones waste their time.",
	"- Read the matching help topic before giving steps. Give the steps in order, numbered, naming the exact screen and field.",
	"- Bambu LAN mode needs LAN Only Mode and Developer Mode on at the printer, and the IP, serial number and LAN access code copied exactly. Say that the access code changes every time LAN Only Mode is toggled.",
	"- Never suggest working around Bambu's cloud restrictions, and never suggest retrying a cloud sign-in repeatedly. Bambu bans accounts for connection churn, and the Bridge paces itself on purpose. If the Bridge says it has paused, the correct advice is to wait.",
	"",
	"Diagnosing a printer that will not connect:",
	"- Call list_printers first to see what is actually configured, then get_printer_status for the one in question, then test_printer_connection to find out whether the machine is reachable on the network at all. get_recent_logs usually contains the real reason.",
	"- Distinguish the three cases plainly and do not blur them: not reachable on the network, reachable but refusing the credentials, and connected but reporting nothing.",
	"- A printer that worked yesterday and is offline today has usually changed IP address. Check that before anything else.",
	"",
	"Diagnosing a print:",
	"- get_printer_status gives you status, progress, temperatures against target, AMS slots and any error the printer reported. get_print_history gives you what happened on previous prints, including temperature samples.",
	"- The telemetry does not show you the print. It cannot tell you whether a part has warped, lifted, stringed or failed. Only the camera can, and only if view_printer_camera is available and the user has switched it on. Do not describe the state of a physical print you have not seen.",
	"",
	"Looking at the camera:",
	"- Call view_printer_camera only when the question genuinely needs a picture, and say that you are doing it. It costs the user money on a paid provider and it sends a photograph of their printer to that provider.",
	"- If the tool says the camera permission is off, say so, say it is in Settings under Assistant, and answer from the telemetry instead. Do not retry it.",
	"- Describe only what is actually visible in the frame. If the image is dark, blurred or shows nothing useful, say that rather than guessing at what is on the plate.",
	"",
	"Using tools:",
	"- Use a tool whenever the answer depends on this user's printers or on how the Bridge behaves. Never guess at a number, and never guess at how a feature works.",
	"- Call tools by their exact names with arguments as valid JSON matching the schema. Do not describe a tool call in prose; make the call.",
	"- Do not call the same tool twice with the same arguments in one reply. If a result was not what you needed, change the arguments or try a different tool. Repeating a call returns the same answer and gets you no closer.",
	"- A tool may reply that a permission is off, or that the assistant cannot do something. That reply is final and authoritative. Say what is off and where to turn it on. Never fill the gap with an invented answer, and do not retry the same tool.",
	fmt.Sprintf("- Data tool results are capped at %d rows. If a result looks truncated, say so rather than implying it is the full picture.", ToolRowLimit),
	"",
	"Writing the answer:",
	"- Say each thing once. Do not restate a sentence, a bullet or a caveat you have already written in this reply. If you find yourself writing something you have written already, the answer is finished: stop.",
	"- Answer the question that was asked, then stop. Do not pad, do not summarise your own answer back to the user, and do not offer a menu of things you could do next.",
	"- Keep it short: a few sentences, or a numbered list of steps. Only go longer when the user asked for detail.",
	"- Temperatures are degrees Celsius. Times from tools are minutes unless the tool says otherwise.",
	"- Formatting is rendered as markdown. Keep it plain: bold for a label, short bullet or numbered lists, inline code for an address, a serial or a command. No headings, no tables.",
	"- If a tool returns nothing, say there is nothing rather than inventing an example.",
	"- If the help does not cover the problem, or the user says they already tried the fix, say so plainly and point them at the GitHub issues page named in the getting-help topic. Never promise a fix or a date, and never invent a contact address.",
}, "\n")

// Tool is one OpenAI-compatible function schema.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the body of a Tool.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// object builds a JSON Schema object node. additionalProperties is always
// false: a model that invents an argument should be told, not silently obeyed.
func object(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// ToolsFor returns the tool schemas the model may use. The camera tool is
// omitted entirely when the user has not allowed it, rather than being offered
// and then refused: a tool the model cannot see is one it cannot waste a round
// on, and the prompt already tells it what to say if a permission is off.
func ToolsFor(allowCamera bool) []Tool {
	tools := []Tool{
		// Help comes first because the commonest question a user actually asks
		// is "how do I ...", and because these are the only tools that work
		// with no printers configured at all. They read a static library
		// compiled into the binary, never the network, so there is nothing to
		// leak and nothing to switch off.
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "search_help",
				Description: "Search the Bridge's own help library and get back the parts that answer a question. Use this for ANY question about how the Bridge works (where a setting is, how to add a printer, what a feature does, what a screen means) and for ANY problem the user reports. It is the only place you know any of that from, so use it before answering rather than after. Search with the user's own words or the symptom, not a tool-like phrase.",
				Parameters: object(map[string]any{
					"query": str("What the user wants to know, in their own words. For example: my bambu printer keeps going offline."),
				}, "query"),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_help_topic",
				Description: "Read one help topic in full. Use it when a search_help result came back truncated, or when you already know which topic covers the question.",
				Parameters: object(map[string]any{
					"topic_id": map[string]any{
						"type":        "string",
						"enum":        TopicIDs(),
						"description": "Which topic to read.",
					},
				}, "topic_id"),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "list_printers",
				Description: "List every printer configured in this Bridge, with its name, how it is connected (Bambu LAN, Bambu Cloud or Klipper), whether a camera is set up, and whether it is currently reporting. Call this first for almost any question about a printer, including to find out the exact name to pass to the other tools.",
				Parameters:  object(map[string]any{}),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_printer_status",
				Description: "Read the live telemetry for one printer: status, progress, file name, time remaining, nozzle and bed temperature against their targets, fan, speed level, AMS slots, and any error the printer itself reported. This is the only way to know what a printer is doing right now.",
				Parameters: object(map[string]any{
					"printer_name": str("The printer's exact name, as returned by list_printers."),
				}, "printer_name"),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "test_printer_connection",
				Description: "Actively test whether a printer is reachable on the network from this machine, right now. For a Bambu printer this opens a TCP connection to its MQTT port; for a Klipper printer it calls Moonraker. Use it to tell 'the printer is not on the network' apart from 'the printer is on the network but the Bridge is not getting data from it'. It sends no commands and changes nothing.",
				Parameters: object(map[string]any{
					"printer_name": str("The printer's exact name, as returned by list_printers."),
				}, "printer_name"),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_bridge_info",
				Description: "Read the Bridge's own state: version, operating system, listen port, where its config file lives, whether automatic updates are on, whether it is linked to FoxTrack and whether that link is healthy, and whether a Bambu Cloud account is linked and when its token expires. Use it for questions about the Bridge itself rather than about a printer.",
				Parameters:  object(map[string]any{}),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_print_history",
				Description: "Read completed prints recorded by this Bridge, newest first. Each record has the printer, file name, start and end time, duration and result (finished, cancelled or error). Use it for questions about what happened before, and to see whether a failure is a one-off or a pattern.",
				Parameters: object(map[string]any{
					"printer_name": str("Optional. Limit to one printer, by its exact name."),
					"limit":        map[string]any{"type": "integer", "description": fmt.Sprintf("Optional. How many records to return, 1 to %d. Defaults to 10.", ToolRowLimit)},
				}),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_recent_logs",
				Description: "Read the Bridge's recent log lines. This is usually where the real reason for a failed connection is written, in the Bridge's own words. Secrets are removed before you see them. Use it when a printer will not connect, or when something is not working and the other tools look normal.",
				Parameters: object(map[string]any{
					"printer_name": str("Optional. Only return lines mentioning this printer, by its exact name."),
					"lines":        map[string]any{"type": "integer", "description": "Optional. How many recent lines to return, 1 to 80. Defaults to 40."},
				}),
			},
		},
	}

	if allowCamera {
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        "view_printer_camera",
				Description: "Fetch one still frame from a printer's camera and look at it. This is the ONLY way to see the physical state of a print: whether it has warped, lifted, come loose, turned to spaghetti, or looks fine. Use it when the question is about how the print actually looks, not for questions the telemetry already answers. It takes a photograph of the user's printer and sends it to the AI provider, so do not call it speculatively.",
				Parameters: object(map[string]any{
					"printer_name": str("The printer's exact name, as returned by list_printers."),
				}, "printer_name"),
			},
		})
	}

	return tools
}

// ToolResult is what an executor hands back. ImageDataURL is set only by the
// camera tool: an OpenAI tool message cannot carry an image, so the loop posts
// the frame as a following user turn instead, and Value carries the text the
// tool message needs.
type ToolResult struct {
	Value        any
	ImageDataURL string
}

// ToolExecutor runs one tool call. It must not return an error for an ordinary
// refusal (an unknown printer, a permission that is off): those are results the
// model can read and recover from. Reserve the error for a genuine failure.
type ToolExecutor func(ctx context.Context, name, argumentsJSON string) (ToolResult, error)

// ErrNoReply marks a provider that answered with neither text nor a tool call.
var ErrNoReply = errors.New("the model replied with nothing; try asking again")

// ErrToolLoop marks a model that spent every round asking for data and never
// wrote an answer.
var ErrToolLoop = errors.New("the model kept asking for data without answering; try a narrower question")

// ChatOptions is one call to RunChatLoop.
type ChatOptions struct {
	Client *Client
	// History is the conversation so far, oldest first, without any system
	// turn. The caller has already trimmed it to MaxContextMessages.
	History []Message
	// LatestUserText is the message being answered, used to pre-fetch help.
	LatestUserText string
	Execute        ToolExecutor
	AllowCamera    bool
}

// RunChatLoop drives one reply: forced tool use, then tool rounds, then text.
func RunChatLoop(ctx context.Context, opts ChatOptions) (string, error) {
	if opts.Client == nil {
		return "", errors.New("the assistant is not configured")
	}

	// Help pre-fetch. The forced first tool round below is what makes a model
	// look before it speaks, but a server that ignores tool_choice, or a small
	// model that writes its tool call as prose, will still answer "how do I..."
	// from imagination. So the best-matching help sections are looked up
	// deterministically here and placed in the system prompt, where the model
	// cannot skip them. It is the same lexical search search_help runs.
	prompt := SystemPrompt + BestHelpExcerpts(opts.LatestUserText, 2)

	messages := make([]Message, 0, len(opts.History)+8)
	messages = append(messages, SystemMessage(prompt))
	messages = append(messages, opts.History...)

	tools := ToolsFor(opts.AllowCamera)

	// Prose the model wrote on a round where it ALSO called a tool. Normally
	// that is a preamble ("let me look that up") and dropping it is right.
	// Sometimes it is the entire answer: a model writes the full reply, then
	// calls a tool to double-check itself, and the round that follows says only
	// "the search confirms the above". Dropping the answer and keeping the
	// footnote is the worst of both, so a substantial one is kept in case the
	// final round turns out to be thin.
	var preamble string

	// Every tool call already made in THIS reply, keyed on name plus arguments.
	// A model that asks the same question twice is looping rather than
	// investigating: the rows are identical, it has spent a round, and it will
	// usually loop again. Answering the repeat with a refusal is what gives the
	// next round a reason to move on. Different arguments still run.
	called := make(map[string]bool)

	for round := 0; round <= MaxToolRounds; round++ {
		roundTools := tools
		// Round 0 forces a tool call, because the prompt alone does not stop a
		// model that has already seen one tool result from answering the next
		// question out of thin air. Later rounds are "auto" so it can stop once
		// it has what it needs; the final round offers no tools at all, which
		// forces a text answer.
		toolChoice := "auto"
		switch round {
		case 0:
			toolChoice = "required"
		case MaxToolRounds:
			roundTools = nil
			toolChoice = ""
		}

		reply, err := opts.Client.Chat(ctx, messages, roundTools, toolChoice)
		if err != nil {
			return "", err
		}

		if len(reply.ToolCalls) == 0 {
			// Degenerate repetition is a sampling failure, not something a
			// prompt reliably prevents: a model that starts repeating a line
			// keeps repeating it until it runs out of tokens. The prompt asks
			// it not to; this makes sure the user never sees it when it does
			// anyway.
			text := TrimRunaway(CollapseRepetition(reply.Content))
			// A final round that says almost nothing, after an earlier round
			// that said a lot, means the answer is in the earlier round.
			if len(text) < thinReplyLength && len(preamble) >= substantialPreambleLength {
				joined := preamble
				if text != "" {
					joined += "\n\n" + text
				}
				text = TrimRunaway(CollapseRepetition(joined))
			}
			if text == "" {
				return "", ErrNoReply
			}
			return text, nil
		}

		if content := strings.TrimSpace(reply.Content); len(content) >= substantialPreambleLength {
			preamble = content
		}

		// Replay the provider's own assistant message, not a reconstruction of
		// it. See the comment on Reply.Raw.
		if reply.Raw != nil {
			messages = append(messages, Message(reply.Raw))
		}

		// Images collected this round, appended after every tool turn so the
		// tool_call_id sequence stays unbroken.
		var images []Message

		for _, call := range reply.ToolCalls {
			key := call.Name + ":" + call.ArgumentsJSON
			if called[key] {
				// The earlier result is still in messages, so echoing it back
				// would pay for the same tokens twice. Point at it instead.
				messages = append(messages, ToolMessage(call.ID, map[string]string{
					"error":   "repeated_call",
					"message": "You already called this tool with these arguments in this reply, and its result is earlier in this conversation. Use that result, or call a different tool with different arguments. Do not repeat this call.",
				}))
				continue
			}
			called[key] = true

			result, err := opts.Execute(ctx, call.Name, call.ArgumentsJSON)
			if err != nil {
				// A failed tool is data for the model, not a dead request: it
				// can say so, or try a different approach.
				messages = append(messages, ToolMessage(call.ID, map[string]string{
					"error":   "failed",
					"message": err.Error(),
				}))
				continue
			}

			messages = append(messages, ToolMessage(call.ID, result.Value))
			if result.ImageDataURL != "" {
				images = append(images, UserMessageWithImage(
					"This is the camera frame from the tool call above. Describe only what is actually visible in it.",
					result.ImageDataURL,
				))
			}
		}

		messages = append(messages, images...)
	}

	return "", ErrToolLoop
}

// TrimHistory keeps the most recent MaxContextMessages turns. Dropping from the
// front can orphan a tool turn whose assistant turn has gone, which some
// providers reject, so the trim moves forward to the first user turn.
func TrimHistory(history []Message) []Message {
	if len(history) <= MaxContextMessages {
		return history
	}
	trimmed := history[len(history)-MaxContextMessages:]
	for i, m := range trimmed {
		if role, _ := m["role"].(string); role == "user" {
			return trimmed[i:]
		}
	}
	return nil
}
