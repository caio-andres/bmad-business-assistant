package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// SkillActivation renders the tool result that loads a skill (same shape as
// the coding-agent CLI's Skill tool: base directory + the original SKILL.md).
func SkillActivation(tc *TurnContext, name, args string) (string, error) {
	data, _, err := tc.Bundle.ReadSkill(name)
	if err != nil {
		return "", err
	}
	root := SkillRoot(name)
	var sb strings.Builder
	fmt.Fprintf(&sb, "<skill_activation name=%q skill-root=%q>\n", name, root)
	fmt.Fprintf(&sb, "Base directory for this skill: %s\n", root)
	sb.WriteString("{skill-root} = " + root + " ; {project-root} = " + vfs.Root + " ; {skill-name} = " + name + "\n")
	if strings.TrimSpace(args) != "" {
		sb.WriteString("ARGUMENTS: " + args + "\n")
	}
	sb.WriteString("\n")
	sb.Write(data)
	sb.WriteString("\n</skill_activation>")
	return sb.String(), nil
}

// ApplySkill updates the primary skill/agent after an activation.
func (tc *TurnContext) ApplySkill(name string) {
	tc.mu.Lock()
	changed := false
	isAgent := strings.HasPrefix(name, "bmad-agent-")
	switch {
	case isAgent:
		if tc.activeAgent != name {
			tc.activeAgent, changed = name, true
		}
	case tc.activeSkill == "" || isSupportSkill(tc.activeSkill):
		if tc.activeSkill != name {
			tc.activeSkill, changed = name, true
		}
	}
	skill, agent := tc.activeSkill, tc.activeAgent
	cb := tc.OnSkillChange
	tc.mu.Unlock()
	if changed && cb != nil {
		cb(skill, agent)
	}
}

// Support skills are invoked from inside a workflow and do not replace it.
func isSupportSkill(name string) bool {
	switch name {
	case "bmad-advanced-elicitation", "bmad-party-mode", "bmad-review", "bmad-help":
		return true
	}
	return false
}

// SkillTools registers invoke_skill, ask_user, show_document and end_skill.
func SkillTools(r *Registry) {
	r.Add(&Tool{
		Def: defOf("invoke_skill",
			"Ativa uma skill do BMad instalada (como o comando de skill de uma CLI de agente): devolve as instruções originais da skill para você seguir. "+
				"Use quando a skill/agente atual mandar invocar outra skill (ex.: bmad-party-mode, bmad-advanced-elicitation, bmad-help, bmad-review) ou quando a pessoa pedir outra etapa.",
			schema(map[string]any{
				"skill": str("Nome da skill, ex.: bmad-prd"),
				"args":  str("Argumentos/instruções repassados à skill. Opcional."),
			}, "skill")),
		MainOnly: false,
		Label: func(tc *TurnContext, a map[string]any) string {
			s, _ := a["skill"].(string)
			if tc.Catalog != nil {
				return "Abrindo a etapa: " + tc.Catalog.Label(s)
			}
			return "Abrindo a etapa " + s
		},
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Skill string `json:"skill"`
				Args  string `json:"args"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			name := strings.TrimSpace(strings.TrimPrefix(a.Skill, "skill:"))
			name = strings.Fields(name + " ")[0]
			if tc.Catalog != nil && !tc.Catalog.Allowed(name) {
				return "", fmt.Errorf("a skill %q não está disponível neste ambiente (escopo do negócio: análise e planejamento). Explique à pessoa, sem jargão, que essa etapa é feita pelo time técnico e siga com o que for possível", name)
			}
			out, err := SkillActivation(tc, name, a.Args)
			if err != nil {
				return "", err
			}
			if tc.Depth == 0 {
				tc.ApplySkill(name)
			}
			return out, nil
		},
	})

	r.Add(&Tool{
		Def: defOf("ask_user",
			"Mostra à pessoa uma pergunta com opções em botões e ENCERRA o seu turno (a resposta chega na próxima mensagem). "+
				"Use sempre que a skill mandar perguntar algo, parar e esperar, ou apresentar um menu de escolhas. Escreva a pergunta no texto da sua resposta ou em `question`; "+
				"em `options`, coloque de 2 a 6 rótulos curtos (sem numeração). A pessoa também pode responder livremente.",
			schema(map[string]any{
				"question": str("Pergunta curta (opcional se já estiver no seu texto)"),
				"options":  strArray("Rótulos curtos das respostas sugeridas, ex.: [\"Caminho rápido\", \"Caminho guiado\"]"),
			})),
		MainOnly: true,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a AskRequest
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			var opts []string
			for _, o := range a.Options {
				o = strings.TrimSpace(o)
				if o != "" && len(opts) < 8 {
					opts = append(opts, o)
				}
			}
			a.Options = opts
			tc.mu.Lock()
			tc.ask = &a
			tc.mu.Unlock()
			return "Pergunta exibida para a pessoa. Termine sua resposta agora, sem chamar outras ferramentas; a resposta dela chegará na próxima mensagem.", nil
		},
	})

	r.Add(&Tool{
		Def: defOf("show_document",
			"Abre um documento no painel ao lado do chat para a pessoa ver (equivale a abrir no navegador). Use para HTMLs gerados (protótipos, relatórios) ou para destacar um documento.",
			schema(map[string]any{"path": str("Caminho do arquivo")}, "path")),
		Label: func(tc *TurnContext, a map[string]any) string {
			p, _ := a["path"].(string)
			return "Abrindo " + FriendlyName(p) + " no painel"
		},
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			return tc.openCmd([]string{a.Path})
		},
	})

	r.Add(&Tool{
		Def: defOf("end_skill",
			"Encerra a etapa (skill) atual quando o trabalho dela terminou (documento final entregue e próximos passos sugeridos). A conversa volta ao início e a pessoa pode começar outra etapa.",
			schema(map[string]any{
				"summary": str("Resumo de uma ou duas frases do que foi produzido"),
			})),
		MainOnly: true,
		Label:    func(*TurnContext, map[string]any) string { return "Concluindo a etapa" },
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Summary string `json:"summary"`
			}
			_ = decode(raw, &a)
			tc.mu.Lock()
			tc.skillEnded, tc.endSummary = true, a.Summary
			tc.mu.Unlock()
			return "Etapa encerrada. Termine com uma mensagem curta de fechamento.", nil
		},
	})
}

// WebTools registers web_search and web_fetch when a provider is configured.
func WebTools(r *Registry) {
	r.Add(&Tool{
		Def: defOf("web_search",
			"Pesquisa na web e devolve títulos, links e trechos. Cite as fontes no documento.",
			schema(map[string]any{
				"query":       str("Consulta"),
				"max_results": integer("Máximo de resultados (padrão 8)"),
			}, "query")),
		Parallel: true,
		Label: func(tc *TurnContext, a map[string]any) string {
			q, _ := a["query"].(string)
			if len(q) > 60 {
				q = q[:60] + "…"
			}
			return "Pesquisando na web: " + q
		},
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			if tc.Web == nil {
				return "", errWebDisabled
			}
			var a struct {
				Query string `json:"query"`
				Max   int    `json:"max_results"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			if a.Max <= 0 || a.Max > 15 {
				a.Max = 8
			}
			res, err := tc.Web.Search(ctx, a.Query, a.Max)
			if err != nil {
				return "", err
			}
			if len(res) == 0 {
				return "Nenhum resultado.", nil
			}
			var sb strings.Builder
			for i, r := range res {
				fmt.Fprintf(&sb, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
			}
			return sb.String(), nil
		},
	})
	r.Add(&Tool{
		Def:      defOf("web_fetch", "Baixa uma página pública e devolve o texto principal.", schema(map[string]any{"url": str("URL http(s)")}, "url")),
		Parallel: true,
		Label:    func(*TurnContext, map[string]any) string { return "Lendo uma página da web" },
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			if tc.Web == nil {
				return "", errWebDisabled
			}
			var a struct {
				URL string `json:"url"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			text, err := tc.Web.Fetch(ctx, a.URL)
			if err != nil {
				return "", err
			}
			return Truncate(text, 40*1024), nil
		},
	})
}

var errWebDisabled = errors.New("a pesquisa na web não está habilitada neste ambiente. Avise a pessoa em uma frase e siga com o que ela trouxer (ou ofereça montar um roteiro de pesquisa para ela rodar em outra ferramenta)")

// EmitDocument notifies the UI about a workspace write.
func EmitDocument(tc *TurnContext, abs string, deleted bool) {
	if tc.Emit == nil || OutputRel(abs) == "" || strings.Contains(abs, "/.view/") && deleted {
		return
	}
	tc.Emit(events.Event{Type: events.Document, Document: DocRef(abs, deleted)})
}

// DefaultRegistry returns the standard tool set (web tools only when enabled).
func DefaultRegistry(web bool) *Registry {
	r := NewRegistry()
	FileTools(r)
	CommandTool(r)
	SkillTools(r)
	if web {
		WebTools(r)
	}
	return r
}
