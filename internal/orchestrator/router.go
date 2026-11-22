// Package orchestrator is the deterministic shell around the interpreter:
// intent routing, the conversation state machine, turn locking, quotas and
// persistence of every turn.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Router actions.
const (
	ActionStartSkill = "start_skill"
	ActionAgent      = "agent"
	ActionOutOfScope = "out_of_scope"
	ActionClarify    = "clarify"
)

// Decision is a routing result.
type Decision struct {
	Action     string   `json:"action"`
	Skill      string   `json:"skill,omitempty"`
	Confidence float64  `json:"confidence"`
	Candidates []string `json:"candidates,omitempty"`
	By         string   `json:"by"`
}

// ErrNotConfigured means a router cannot run in this environment.
var ErrNotConfigured = errors.New("roteador não configurado")

// Router classifies a free-text message when no skill is active.
type Router interface {
	Name() string
	Route(ctx context.Context, text string, cat *bmad.Catalog) (Decision, error)
}

// fold lowercases and strips accents.
func fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return strings.ToLower(s)
	}
	return strings.Join(strings.Fields(out), " ")
}

// --- rules -----------------------------------------------------------------------

// RuleRouter scores PT-BR keywords; exact button labels win with confidence 1.
type RuleRouter struct{}

func (RuleRouter) Name() string { return "rules" }

var intentVerbs = []string{"quero", "queria", "preciso", "vamos", "criar", "escrever", "montar", "fazer", "gerar", "elaborar", "construir", "comecar", "iniciar", "rascunhar", "atualizar", "revisar", "me ajuda", "ajudar"}

var outOfScope = []string{"implementar", "implementacao", "codigo", "programar", "codar", "arquitetura tecnica", "arquitetura do sistema", "arquitetura de software", "deploy", "subir em producao", "teste automatizado", "testes automatizados", "pull request", "refatorar", "microsservico", "endpoint", "dockerfile", "kubernetes", "terraform", "sprint planning", "code review"}

var agentAliases = map[string][]string{
	"bmad-agent-analyst":     {"analista", "falar com a mary", "mary"},
	"bmad-agent-pm":          {"gerente de produto", "product manager", "falar com o john", "john"},
	"bmad-agent-ux-designer": {"designer", "falar com a sally", "sally"},
}

func containsWord(text, kw string) bool {
	kw = fold(kw)
	i := strings.Index(text, kw)
	for i >= 0 {
		before := i == 0 || !isLetter(rune(text[i-1]))
		end := i + len(kw)
		// keywords ending with a stem (e.g. "concorr") match prefixes
		after := end >= len(text) || !isLetter(rune(text[end])) || len(kw) >= 6 ||
			(text[end] == 's' && (end+1 >= len(text) || !isLetter(rune(text[end+1])))) // plural
		if before && after {
			return true
		}
		next := strings.Index(text[i+1:], kw)
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

func isLetter(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func (RuleRouter) Route(_ context.Context, text string, cat *bmad.Catalog) (Decision, error) {
	t := fold(text)
	if t == "" {
		return Decision{Action: ActionClarify, By: "rules"}, nil
	}
	// exact label / starter (buttons and shortcuts)
	for _, it := range cat.Items {
		if t == fold(it.Label) || t == fold(it.Starter) || t == fold(strings.TrimPrefix(it.Skill, "bmad-")) || t == it.Skill {
			return Decision{Action: ActionStartSkill, Skill: it.Skill, Confidence: 1, By: "rules"}, nil
		}
	}
	for _, a := range cat.Agents {
		if t == fold(a.Name) || t == fold(a.Title) || t == a.Skill {
			return Decision{Action: ActionAgent, Skill: a.Skill, Confidence: 1, By: "rules"}, nil
		}
	}
	verb := false
	for _, v := range intentVerbs {
		if containsWord(t, v) {
			verb = true
			break
		}
	}
	type scored struct {
		skill string
		score float64
	}
	var scores []scored
	for _, it := range cat.Items {
		hits := 0
		for _, kw := range it.Keywords {
			if containsWord(t, kw) {
				hits++
			}
		}
		if hits > 0 {
			s := 0.6 + 0.15*float64(hits-1)
			if verb {
				s += 0.15
			}
			scores = append(scores, scored{it.Skill, min(s, 0.95)})
		}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	if len(scores) > 0 {
		d := Decision{Action: ActionStartSkill, Skill: scores[0].skill, Confidence: scores[0].score, By: "rules"}
		for i, s := range scores {
			if i < 3 {
				d.Candidates = append(d.Candidates, s.skill)
			}
		}
		if len(scores) > 1 && scores[1].score >= scores[0].score {
			d.Confidence -= 0.25
		}
		return d, nil
	}
	for skill, aliases := range agentAliases {
		if !cat.Allowed(skill) {
			continue
		}
		for _, al := range aliases {
			if containsWord(t, al) {
				return Decision{Action: ActionAgent, Skill: skill, Confidence: 0.8, By: "rules"}, nil
			}
		}
	}
	for _, kw := range outOfScope {
		if containsWord(t, kw) {
			return Decision{Action: ActionOutOfScope, Confidence: 0.85, By: "rules"}, nil
		}
	}
	return Decision{Action: ActionClarify, Confidence: 0, By: "rules"}, nil
}

// --- LLM ---------------------------------------------------------------------------

// LLMRouter asks a (cheap) model for a JSON classification.
type LLMRouter struct {
	Provider llm.Provider
	Model    string
	OnUsage  func(model string, u llm.Usage)
}

func (r *LLMRouter) Name() string { return "llm" }

func (r *LLMRouter) Route(ctx context.Context, text string, cat *bmad.Catalog) (Decision, error) {
	var sb strings.Builder
	sb.WriteString(`Você classifica o pedido de uma pessoa de negócio para um assistente que conduz o BMad Method (análise e planejamento de produto).
Responda SOMENTE um JSON: {"action": "start_skill" | "agent" | "out_of_scope" | "clarify", "skill": "<nome da skill ou agente>", "confidence": <0 a 1>, "candidates": ["<até 3 skills plausíveis>"]}
- start_skill: o pedido corresponde claramente a uma etapa da lista.
- agent: a pessoa quer conversar com um dos agentes (persona) sem etapa definida.
- out_of_scope: pede implementação, código, arquitetura técnica, testes, deploy ou infraestrutura.
- clarify: não dá para saber; liste candidatos.
Etapas:
`)
	for _, it := range cat.Items {
		fmt.Fprintf(&sb, "- %s: %s. %s\n", it.Skill, it.Label, it.Summary)
	}
	sb.WriteString("Agentes:\n")
	for _, a := range cat.Agents {
		fmt.Fprintf(&sb, "- %s: %s, %s. %s\n", a.Skill, a.Name, a.Title, a.Description)
	}
	resp, err := r.Provider.Chat(ctx, llm.Request{
		Model: r.Model, Purpose: "router", JSONMode: true, MaxTokens: 200,
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: sb.String()}, {Role: llm.RoleUser, Content: text}},
	}, nil)
	if err != nil {
		return Decision{}, err
	}
	if r.OnUsage != nil {
		r.OnUsage(resp.Model, resp.Usage)
	}
	d, err := parseDecision(resp.Message.Content)
	if err != nil {
		return Decision{}, err
	}
	d.By = "llm"
	// never trust names outside the catalog
	if d.Skill != "" && !cat.Allowed(d.Skill) {
		d.Skill, d.Action, d.Confidence = "", ActionClarify, 0
	}
	var cands []string
	for _, c := range d.Candidates {
		if cat.Allowed(c) && len(cands) < 3 {
			cands = append(cands, c)
		}
	}
	d.Candidates = cands
	return d, nil
}

func parseDecision(s string) (Decision, error) {
	var d Decision
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return d, fmt.Errorf("resposta do roteador sem JSON")
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &d); err != nil {
		return d, err
	}
	switch d.Action {
	case ActionStartSkill, ActionAgent, ActionOutOfScope, ActionClarify:
	default:
		d.Action = ActionClarify
	}
	d.Confidence = max(0, min(1, d.Confidence))
	return d, nil
}

// --- Jev (enterprise router, behind a feature flag) ---------------------------------

// JevRouter is the adapter slot for the corporate intent router (D9). Without
// credentials it reports ErrNotConfigured and the chain falls through.
type JevRouter struct{ APIKey string }

func (JevRouter) Name() string { return "jev" }

func (j JevRouter) Route(context.Context, string, *bmad.Catalog) (Decision, error) {
	if j.APIKey == "" {
		return Decision{}, ErrNotConfigured
	}
	return Decision{}, fmt.Errorf("integração com o roteador corporativo ainda não implementada (veja A-DECIDIR.md)")
}

// --- chain ---------------------------------------------------------------------------

// ChainRouter tries routers in order; the first confident decision wins.
type ChainRouter struct {
	Routers   []Router
	Threshold float64
}

func (c *ChainRouter) Name() string { return "chain" }

func (c *ChainRouter) Route(ctx context.Context, text string, cat *bmad.Catalog) (Decision, error) {
	th := c.Threshold
	if th <= 0 {
		th = 0.7
	}
	var best Decision
	var cands []string
	addCands := func(list ...string) {
		for _, s := range list {
			if s == "" {
				continue
			}
			dup := false
			for _, x := range cands {
				dup = dup || x == s
			}
			if !dup && len(cands) < 3 {
				cands = append(cands, s)
			}
		}
	}
	for _, r := range c.Routers {
		d, err := r.Route(ctx, text, cat)
		if err != nil {
			continue
		}
		if d.Action != ActionClarify && d.Confidence >= th {
			return d, nil
		}
		if d.Confidence > best.Confidence {
			best = d
		}
		if d.Action == ActionStartSkill {
			addCands(d.Skill)
		}
		addCands(d.Candidates...)
	}
	if len(cands) == 0 {
		addCands("bmad-product-brief", "bmad-brainstorming", "bmad-help")
	}
	var allowed []string
	for _, s := range cands {
		if cat.Allowed(s) {
			allowed = append(allowed, s)
		}
	}
	return Decision{Action: ActionClarify, Confidence: best.Confidence, Candidates: allowed, By: "chain"}, nil
}
