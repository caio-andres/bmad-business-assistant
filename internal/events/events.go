// Package events defines the typed stream a turn emits to the UI (SSE).
package events

// Types of events.
const (
	Text     = "text"     // assistant text delta
	Activity = "activity" // tool activity in business language
	Document = "document" // a document was created/updated/deleted
	Open     = "open"     // focus a document in the panel
	Options  = "options"  // quick-reply buttons
	State    = "state"    // conversation state / active skill changed
	Usage    = "usage"    // token usage for this turn
	Notice   = "notice"   // informational banner (masking, demo mode, compaction)
	Error    = "error"    // friendly error
	Message  = "message"  // a persisted message (id) — sent at the end of the turn
	Done     = "done"     // turn finished
)

// ActivityInfo describes a tool call in progress or finished.
type ActivityInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Tool   string `json:"tool"`
	Status string `json:"status"` // running | done | error
}

// DocumentRef points to a document in the conversation workspace.
type DocumentRef struct {
	Path    string `json:"path"` // relative to _bmad-output (e.g. planning-artifacts/briefs/x/brief.md)
	Name    string `json:"name"` // friendly name
	Kind    string `json:"kind"` // markdown | html | other
	Deleted bool   `json:"deleted,omitempty"`
}

// StateInfo carries the conversation state after a transition.
type StateInfo struct {
	State       string `json:"state"`
	ActiveSkill string `json:"activeSkill,omitempty"`
	ActiveAgent string `json:"activeAgent,omitempty"`
	SkillLabel  string `json:"skillLabel,omitempty"`
}

// UsageInfo is accumulated token usage.
type UsageInfo struct {
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd,omitempty"`
}

// Event is one item of the turn stream.
type Event struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	Activity *ActivityInfo `json:"activity,omitempty"`
	Document *DocumentRef  `json:"document,omitempty"`
	Options  []string      `json:"options,omitempty"`
	State    *StateInfo    `json:"state,omitempty"`
	Usage    *UsageInfo    `json:"usage,omitempty"`
	ID       string        `json:"id,omitempty"`
	Code     string        `json:"code,omitempty"`
}

// Emitter receives events; implementations must be safe for concurrent use.
type Emitter func(Event)

// Discard drops events.
func Discard(Event) {}
