// Package protocol owns the typed semantic IR and the Chat<->Anthropic
// translation for milestone 2. Protocol code performs no network I/O and
// reads no process environment.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Loss is one diagnosed translation loss. It carries the source field path,
// the reason, and the action — never request values, prompt text, or secrets.
type Loss struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Action string `json:"action"` // "approximated" | "dropped"
}

// RouterError is an ingress-facing failure with a stable machine code.
type RouterError struct {
	Status int
	Code   string
	Msg    string
}

func (e *RouterError) Error() string { return e.Msg }

// Common error codes (mirror the Python router's code strings).
const (
	CodeTranslationLoss   = "translation_loss"
	CodeInvalidGeneration = "invalid_generation_parameter"
	CodeInvalidRequest    = "invalid_request"
	CodeUpstreamBad       = "upstream_response_invalid"
	CodeOverloaded        = "overloaded"
	CodeUnauthorized      = "unauthorized"
)

// --- Typed semantic IR (no map[string]any) ---

// Role is a normalized message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// TextContent is a plain text block.
type TextContent struct{ Text string }

// ImageContent is one bounded inline image for the Gemini->Chat image
// bridge. MIME is lowercased and Data is standard padded base64 for the
// original bytes. Image content is never fetched or decoded to pixels.
type ImageContent struct {
	MIME string
	Data string
}

// AudioContent is one bounded inline audio for the Gemini->Chat audio
// bridge (Chat input_audio only). Format is the Chat format ("wav" | "mp3")
// mapped from the Gemini MIME; Data is standard padded base64 for the
// original bytes. Audio is never fetched or decoded.
type AudioContent struct {
	Format string
	Data   string
}

// FileContent carries a bounded inline PDF. Data is standard padded base64.
// Filename is the safe supplied name, an OpenAI filename default, or empty
// when the destination allows omission.
type FileContent struct {
	MIME     string
	Data     string
	Filename string
}

// ContentKind is the closed set of ordered user content variants. Blocks
// preserve the Gemini part order so the Chat wire array interleaves text,
// image_url, input_audio, and file blocks exactly.
type ContentKind string

const (
	ContentKindText  ContentKind = "text"
	ContentKindImage ContentKind = "image"
	ContentKindAudio ContentKind = "audio"
	ContentKindFile  ContentKind = "file"
)

// ContentBlock is one ordered user content element: plain text, inline
// image, inline audio, or inline file. Kind is closed; Text is set only for
// text blocks, Image only for image blocks, Audio only for audio blocks,
// and File only for file blocks.
type ContentBlock struct {
	Kind  ContentKind
	Text  string       // when Kind == ContentKindText
	Image ImageContent // when Kind == ContentKindImage
	Audio AudioContent // when Kind == ContentKindAudio
	File  FileContent  // when Kind == ContentKindFile
}

// ToolCall is one assistant-requested invocation with a stable identity.
// Function calls carry compact JSON-object Arguments; custom calls carry
// a freeform string CustomInput and never touch CompactArgs.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage // compact JSON object; validated at decode (function only)
	// IsCustom marks a freeform custom invocation. CustomInput is the
	// exact preserved string input; Arguments is nil for custom calls.
	IsCustom    bool
	CustomInput string
}

// ToolResult is the harness-provided result for one ToolCall ID.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Message is one ordered conversation message.
type Message struct {
	Role      Role
	Texts     []TextContent
	ToolCalls []ToolCall
	Results   []ToolResult
	// Blocks carries ordered text/media interleave for the bounded
	// Gemini->Chat media verticals (user messages only): text, image,
	// audio, and PDF file blocks in Gemini part order. Nil when the
	// message carries no bridged media: text-only messages keep the
	// historical Texts-only shape. Other destinations never receive
	// Blocks; their decoders diagnose media as a loss before any I/O.
	Blocks []ContentBlock
}

// Tool kind identities for the closed definition table. Empty Kind reads
// as function for pre-custom literals; decoders always set it explicitly.
const (
	ToolKindFunction = "function"
	ToolKindCustom   = "custom"
)

// CustomFormat is the preserved freeform format for a custom tool.
// Type is "text" (no constraint) or "grammar" (Syntax/Definition preserved
// exactly for native routes; emulated routes carry it as description text).
type CustomFormat struct {
	Type       string // "text" | "grammar"
	Syntax     string // grammar only: "lark" | "regex"
	Definition string // grammar only: preserved grammar source
}

// ToolDef is one function or custom tool definition. Kind is closed
// ("function" | "custom"); function tools use Schema/Strict, custom tools
// use Format and never carry a JSON schema.
type ToolDef struct {
	Kind        string // ToolKindFunction | ToolKindCustom ("" reads as function)
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema object, preserved as bounded raw JSON (function only)
	// Strict carries the explicit Responses `strict` flag when present.
	// Nil means omitted/null on the wire (implicit default, diagnosed as loss).
	// Non-nil retains an explicit true/false exactly for Chat encoding.
	Strict *bool
	// Format carries the preserved custom format (custom only, nil = freeform text).
	Format *CustomFormat
}

// IsCustom reports whether this definition is a freeform custom tool.
func (t ToolDef) IsCustom() bool { return t.Kind == ToolKindCustom }

// ToolChoice selects how the model must use tools. Kind distinguishes
// the forced tool family ("function" | "custom", "" reads as function for
// pre-custom literals); Name is set when Mode == "specific".
type ToolChoice struct {
	Mode string // "none" | "auto" | "any" | "specific"
	Name string // set when Mode == "specific"
	Kind string // ToolKindFunction | ToolKindCustom ("" reads as function)
}

// IsCustomChoice reports whether the forced choice targets a custom tool.
func (c *ToolChoice) IsCustomChoice() bool { return c != nil && c.Kind == ToolKindCustom }

// Generation is the decoded, validated source request in IR form.
type Generation struct {
	Model              string
	Messages           []Message
	Tools              []ToolDef
	HasTools           bool
	Choice             *ToolChoice
	ParallelToolCalls  *bool
	Temperature        *float64
	TopP               *float64
	MaxTokens          *int
	Stop               []string
	Stream             bool
	StreamIncludeUsage bool
	// ExtraTopK carries extra_body.top_k (Anthropic native) to the encoder.
	ExtraTopK *int
	// Reasoning carries the closed validated Chat effort control
	// (absent when no reasoning_effort was supplied).
	Reasoning ReasoningIntent
	// ToolPlan carries immutable tool bindings for translated requests.
	// An empty Chat-to-Responses plan still rules out native custom replies.
	ToolPlan *ToolPlan
}

// Limits bounds all translation buffers.
type Limits struct {
	// MaxArgsChars bounds one accumulated tool-argument field.
	MaxArgsChars int
	// MaxStreamArgsChars bounds total accumulated tool arguments per stream.
	MaxStreamArgsChars int
	// MaxToolCalls bounds reconstructed tool calls per response.
	MaxToolCalls int
	// MaxStreamBlocks bounds the total distinct content-block indices opened
	// per SSE stream. Text blocks carry no tool identity but still allocate
	// block state and index records, so the total (not just the tool count)
	// needs an explicit bound.
	MaxStreamBlocks int
	// MaxFrameBytes bounds one SSE frame line.
	MaxFrameBytes int
}

// DefaultLimits mirrors the documented Python translation bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxArgsChars:       1_000_000,
		MaxStreamArgsChars: 8_000_000,
		MaxToolCalls:       1024,
		MaxStreamBlocks:    1024,
		MaxFrameBytes:      8_000_000,
	}
}

// EnforcePolicy applies the strict/lossy policy. Strict refuses any planned
// loss before upstream I/O; lossy keeps the plan and returns the losses for
// the x-model-router-loss header.
func EnforcePolicy(losses []Loss, lossy bool) ([]Loss, *RouterError) {
	if len(losses) > 0 && !lossy {
		fields := ""
		seen := map[string]bool{}
		for _, l := range losses {
			key := l.Field + " (" + l.Reason + ")"
			if !seen[key] {
				seen[key] = true
				if fields != "" {
					fields += "; "
				}
				fields += key
			}
		}
		return nil, &RouterError{
			Status: 400,
			Code:   CodeTranslationLoss,
			Msg: fmt.Sprintf("Translation would lose or approximate: %s. "+
				"Use a same-protocol route, remove these controls, or ask the operator "+
				"to configure compatibility = \"lossy\" (with loss diagnostics).", fields),
		}
	}
	return losses, nil
}

// EncodeLosses serializes losses for the x-model-router-loss header.
func EncodeLosses(losses []Loss) string {
	if len(losses) == 0 {
		return ""
	}
	data, err := json.Marshal(losses)
	if err != nil {
		return "[]"
	}
	return string(data)
}
