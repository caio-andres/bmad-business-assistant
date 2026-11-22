// Package runtime provides the tools the interpreter offers to the model: the
// same capabilities a coding-agent CLI gives BMad skills (files, commands,
// skills, subagents, questions), restricted to a virtual project.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// OutputDir is the writable workspace mount ({output_folder}).
const OutputDir = "_bmad-output"

// AskRequest is a question the model wants to put to the user (ends the turn).
type AskRequest struct {
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// WebProvider is an optional web research backend.
type WebProvider interface {
	Search(ctx context.Context, query string, max int) ([]WebResult, error)
	Fetch(ctx context.Context, url string) (string, error)
}

// WebResult is one search hit.
type WebResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// SubagentFunc runs a nested interpreter loop and returns its final answer.
type SubagentFunc func(ctx context.Context, task string, files []string) (string, error)

// TurnContext is the per-turn state shared by tools.
type TurnContext struct {
	FS      *vfs.FS
	Bundle  *bmad.Bundle
	Catalog *bmad.Catalog
	Emit    events.Emitter
	Now     func() time.Time
	Python  *PythonRunner
	Web     WebProvider
	// Subagent is set by the interpreter when subagents are enabled.
	Subagent SubagentFunc
	// Depth is 0 for the main loop, 1 for subagents.
	Depth int

	mu          sync.Mutex
	activeSkill string
	activeAgent string
	ask         *AskRequest
	skillEnded  bool
	endSummary  string
	// OnSkillChange is called when invoke_skill changes the primary skill or agent.
	OnSkillChange func(skill, agent string)
}

// ActiveSkill returns the primary workflow skill of the conversation.
func (tc *TurnContext) ActiveSkill() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.activeSkill
}

// ActiveAgent returns the agent persona in use.
func (tc *TurnContext) ActiveAgent() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.activeAgent
}

// SetActive restores state at the beginning of a turn.
func (tc *TurnContext) SetActive(skill, agent string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.activeSkill, tc.activeAgent = skill, agent
}

// Ask returns the pending question, if any.
func (tc *TurnContext) Ask() *AskRequest {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.ask
}

// SkillEnded reports whether end_skill was called this turn.
func (tc *TurnContext) SkillEnded() (bool, string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.skillEnded, tc.endSummary
}

// ResetTurnFlags clears ask/end markers (called by the loop at turn start).
func (tc *TurnContext) ResetTurnFlags() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.ask, tc.skillEnded, tc.endSummary = nil, false, ""
}

// SkillRoot returns the virtual skill directory for a skill name.
func SkillRoot(name string) string { return vfs.Root + "/" + bmad.SkillsDir + "/" + name }

func (tc *TurnContext) now() time.Time {
	if tc.Now != nil {
		return tc.Now()
	}
	return time.Now()
}

// Tool is one capability offered to the model.
type Tool struct {
	Def      llm.ToolDef
	Run      func(ctx context.Context, tc *TurnContext, args json.RawMessage) (string, error)
	Label    func(tc *TurnContext, args map[string]any) string // business-language activity; "" hides it
	Parallel bool                                              // may run concurrently with other parallel calls
	MainOnly bool                                              // not offered to subagents
}

// Registry is an ordered set of tools.
type Registry struct {
	tools map[string]*Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]*Tool{}} }

// Add registers a tool (replacing one with the same name).
func (r *Registry) Add(t *Tool) {
	if _, ok := r.tools[t.Def.Name]; !ok {
		r.order = append(r.order, t.Def.Name)
	}
	r.tools[t.Def.Name] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Defs returns tool definitions for the given depth.
func (r *Registry) Defs(depth int) []llm.ToolDef {
	var defs []llm.ToolDef
	for _, n := range r.order {
		t := r.tools[n]
		if depth > 0 && t.MainOnly {
			continue
		}
		defs = append(defs, t.Def)
	}
	return defs
}

// Names returns registered tool names in order.
func (r *Registry) Names() []string { return append([]string{}, r.order...) }

func schema(props map[string]any, required ...string) json.RawMessage {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	b, _ := json.Marshal(s)
	return b
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func strArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// decode parses tool arguments leniently (models sometimes send "" or invalid JSON).
func decode(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		s = "{}"
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("argumentos inválidos (JSON esperado): %v", err)
	}
	return nil
}

// ArgsMap decodes arguments into a generic map (for labels).
func ArgsMap(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = decode(raw, &m)
	return m
}

// --- friendly names ---------------------------------------------------------

var friendlyNames = map[string]string{
	"brief.md":               "Brief",
	"addendum.md":            "Adendo",
	"prd.md":                 "PRD",
	".memlog.md":             "Memória da sessão",
	"DESIGN.md":              "Design (UX)",
	"EXPERIENCE.md":          "Experiência (UX)",
	"epics.md":               "Épicos e histórias",
	"research.md":            "Pesquisa",
	"SPEC.md":                "Spec",
	"prfaq.md":               "PRFAQ",
	"validation-report.html": "Relatório de validação",
	"validation-report.md":   "Relatório de validação",
	"brain-selector.html":    "Seletor de técnicas",
}

// FriendlyName returns a business-friendly document name for a virtual path.
func FriendlyName(p string) string {
	base := path.Base(p)
	if n, ok := friendlyNames[base]; ok {
		return n
	}
	if strings.HasPrefix(base, "brainstorm") && strings.HasSuffix(base, ".md") {
		return "Brainstorming"
	}
	return base
}

// DocKind classifies a file for the panel.
func DocKind(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown", ".txt":
		return "markdown"
	case ".html", ".htm":
		return "html"
	default:
		return "other"
	}
}

// OutputRel returns the path relative to the workspace mount, or "" if outside.
func OutputRel(abs string) string {
	prefix := vfs.Root + "/" + OutputDir + "/"
	if strings.HasPrefix(abs, prefix) {
		return strings.TrimPrefix(abs, prefix)
	}
	return ""
}

// DocRef builds a document reference for an absolute workspace path.
func DocRef(abs string, deleted bool) *events.DocumentRef {
	return &events.DocumentRef{Path: OutputRel(abs), Name: FriendlyName(abs), Kind: DocKind(abs), Deleted: deleted}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type llmToolDef = llm.ToolDef
