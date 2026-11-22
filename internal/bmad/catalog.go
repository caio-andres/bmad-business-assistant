package bmad

import (
	"sort"
	"strings"
)

// Phase groups catalog items the way a business person thinks about the work.
type Phase string

const (
	PhaseIdeas    Phase = "ideias"
	PhaseResearch Phase = "pesquisa"
	PhasePlanning Phase = "planejamento"
	PhaseSupport  Phase = "apoio"
)

// CatalogItem is a business-friendly entry point to a BMad skill.
type CatalogItem struct {
	Skill    string   `json:"skill"`
	Label    string   `json:"label"`
	Summary  string   `json:"summary"`
	Phase    Phase    `json:"phase"`
	Agent    string   `json:"agent,omitempty"` // agent skill that "hosts" this item in the UI
	Icon     string   `json:"icon"`
	Outputs  string   `json:"outputs,omitempty"`
	Order    int      `json:"order"`
	Keywords []string `json:"-"`
	// Starter is sent as the user's first message when the item is started from a button.
	Starter string `json:"starter"`
}

// CatalogAgent is a persona the business person can talk to directly.
type CatalogAgent struct {
	Skill       string `json:"skill"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
}

// Catalog is the set of skills and agents exposed to the business team.
type Catalog struct {
	Version string         `json:"bmadVersion"`
	Items   []CatalogItem  `json:"items"`
	Agents  []CatalogAgent `json:"agents"`
	allowed map[string]bool
}

// DefaultAllowedSkills is the analysis + planning scope (decision D1).
var DefaultAllowedSkills = []string{
	"bmad-brainstorming", "bmad-forge-idea", "bmad-deep-recon", "bmad-prfaq",
	"bmad-product-brief", "bmad-prd", "bmad-ux", "bmad-create-epics-and-stories",
	"bmad-spec", "bmad-advanced-elicitation", "bmad-party-mode", "bmad-review",
	"bmad-help",
	"bmad-agent-analyst", "bmad-agent-pm", "bmad-agent-ux-designer",
}

var ptBRItems = []CatalogItem{
	{Skill: "bmad-brainstorming", Label: "Brainstorming de ideias", Phase: PhaseIdeas, Agent: "bmad-agent-analyst", Icon: "lightbulb", Order: 10,
		Summary:  "Gere muito mais ideias do que sozinho, com técnicas criativas e a IA como facilitadora ou parceira.",
		Keywords: []string{"brainstorm", "ideia", "ideias", "ideação", "ideacao", "criativ", "debate de ideias"},
		Starter:  "Quero fazer um brainstorming."},
	{Skill: "bmad-forge-idea", Label: "Testar uma ideia", Phase: PhaseIdeas, Agent: "bmad-agent-analyst", Icon: "flame", Order: 20,
		Summary:  "Coloque uma ideia ainda crua à prova com perguntas de várias perspectivas, até decidir seguir ou largar.",
		Keywords: []string{"testar ideia", "validar ideia", "testar minha ideia", "pressão", "estressar", "pôr à prova"},
		Starter:  "Quero testar uma ideia."},
	{Skill: "bmad-prfaq", Label: "PRFAQ (Working Backwards)", Phase: PhaseIdeas, Agent: "bmad-agent-analyst", Icon: "newspaper", Order: 30,
		Summary:  "Escreva o comunicado de imprensa do produto pronto e responda às perguntas difíceis de clientes e executivos.",
		Keywords: []string{"prfaq", "press release", "working backwards", "comunicado"},
		Starter:  "Quero fazer o desafio PRFAQ."},
	{Skill: "bmad-deep-recon", Label: "Pesquisa de mercado e concorrência", Phase: PhaseResearch, Agent: "bmad-agent-analyst", Icon: "search", Order: 40,
		Summary:  "Pesquise mercado, domínio, concorrentes ou a voz do cliente, com fontes citadas, ou monte o roteiro para pesquisar fora.",
		Keywords: []string{"pesquisa", "pesquisar", "mercado", "concorr", "benchmark", "domínio", "dominio", "tendência", "tendencia", "voz do cliente"},
		Starter:  "Quero fazer uma pesquisa."},
	{Skill: "bmad-product-brief", Label: "Product Brief", Phase: PhasePlanning, Agent: "bmad-agent-analyst", Icon: "file-text", Order: 50, Outputs: "brief",
		Summary:  "Transforme a ideia em um resumo de produto claro: problema, público, solução, diferenciais e métricas.",
		Keywords: []string{"brief", "resumo do produto", "visão do produto", "visao do produto", "one pager", "one-pager"},
		Starter:  "Quero criar um product brief."},
	{Skill: "bmad-prd", Label: "PRD: requisitos do produto", Phase: PhasePlanning, Agent: "bmad-agent-pm", Icon: "clipboard", Order: 60, Outputs: "prd",
		Summary:  "Escreva, atualize ou valide o documento de requisitos que o time técnico vai usar para construir.",
		Keywords: []string{"prd", "requisito", "requirements", "funcionalidade", "especificação de produto", "especificacao de produto"},
		Starter:  "Quero criar um PRD."},
	{Skill: "bmad-ux", Label: "Experiência e design (UX)", Phase: PhasePlanning, Agent: "bmad-agent-ux-designer", Icon: "palette", Order: 70, Outputs: "ux",
		Summary:  "Registre como o produto se parece e como se comporta: telas, jornadas, estados e protótipos visuais.",
		Keywords: []string{"ux", "ui", "tela", "design", "experiência", "experiencia", "jornada", "protótipo", "prototipo", "wireframe", "layout"},
		Starter:  "Quero planejar a experiência (UX) do produto."},
	{Skill: "bmad-create-epics-and-stories", Label: "Épicos e histórias", Phase: PhasePlanning, Agent: "bmad-agent-pm", Icon: "layers", Order: 80, Outputs: "epics",
		Summary:  "Quebre os requisitos em épicos e histórias de usuário com critérios de aceite, prontos para o time técnico.",
		Keywords: []string{"épico", "epico", "história", "historia", "stories", "backlog", "critério de aceite", "criterio de aceite", "user story"},
		Starter:  "Quero criar os épicos e histórias."},
	{Skill: "bmad-spec", Label: "Spec resumida", Phase: PhasePlanning, Agent: "bmad-agent-pm", Icon: "scroll", Order: 90,
		Summary:  "Condense ideias, briefs, PRDs ou anotações numa especificação curta que guia o que construir.",
		Keywords: []string{"spec", "especificação curta", "especificacao curta", "condensar"},
		Starter:  "Quero criar uma spec resumida."},
	{Skill: "bmad-review", Label: "Revisar um documento", Phase: PhaseSupport, Icon: "check-circle", Order: 100,
		Summary:  "Peça uma revisão crítica de um documento: estrutura, clareza, lacunas e riscos.",
		Keywords: []string{"revisar", "revisão", "revisao", "review", "criticar"},
		Starter:  "Quero revisar um documento."},
	{Skill: "bmad-advanced-elicitation", Label: "Aprofundar o que já existe", Phase: PhaseSupport, Icon: "zoom-in", Order: 110,
		Summary:  "Leve um rascunho além da primeira versão com métodos como pré-mortem, primeiros princípios e red team.",
		Keywords: []string{"aprofundar", "pré-mortem", "pre-mortem", "red team", "primeiros princípios", "socrático"},
		Starter:  "Quero aprofundar um rascunho."},
	{Skill: "bmad-party-mode", Label: "Mesa redonda com especialistas", Phase: PhaseSupport, Icon: "users", Order: 120,
		Summary:  "Coloque analista, PM e UX discutindo o seu tema ao mesmo tempo, cada um com sua visão.",
		Keywords: []string{"mesa redonda", "party", "vários especialistas", "varios especialistas", "discussão", "discussao"},
		Starter:  "Quero uma mesa redonda com os especialistas."},
	{Skill: "bmad-help", Label: "Não sei por onde começar", Phase: PhaseSupport, Icon: "help-circle", Order: 130,
		Summary:  "Conte o que você tem em mãos e receba a recomendação do próximo passo no método.",
		Keywords: []string{"ajuda", "help", "por onde começo", "por onde comeco", "o que faço", "o que faco", "próximo passo", "proximo passo"},
		Starter:  "Não sei por onde começar. Pode me ajudar?"},
}

var ptBRAgentTitles = map[string]string{
	"bmad-agent-analyst":     "Analista de Negócios",
	"bmad-agent-pm":          "Gerente de Produto",
	"bmad-agent-ux-designer": "Designer de UX",
}

var ptBRAgentDescriptions = map[string]string{
	"bmad-agent-analyst":     "Pesquisa, ideação, análise de concorrência e product brief. Rigor de estrategista, tudo com evidência.",
	"bmad-agent-pm":          "PRD, épicos e histórias. Pergunta \"por quê?\" até chegar no valor real para o usuário.",
	"bmad-agent-ux-designer": "Experiência, telas e jornadas. Empatia com rigor nos casos de borda.",
}

// NewCatalog builds the business catalog restricted to allowed skills that are installed.
func NewCatalog(b *Bundle, allowed []string) *Catalog {
	if len(allowed) == 0 {
		allowed = DefaultAllowedSkills
	}
	c := &Catalog{Version: b.Version, allowed: map[string]bool{}}
	for _, s := range allowed {
		if _, ok := b.Skills[s]; ok {
			c.allowed[s] = true
		}
	}
	for _, it := range ptBRItems {
		if c.allowed[it.Skill] {
			c.Items = append(c.Items, it)
		}
	}
	sort.SliceStable(c.Items, func(i, j int) bool { return c.Items[i].Order < c.Items[j].Order })
	for _, skill := range []string{"bmad-agent-analyst", "bmad-agent-pm", "bmad-agent-ux-designer"} {
		if !c.allowed[skill] {
			continue
		}
		info := b.Agents[skill]
		c.Agents = append(c.Agents, CatalogAgent{
			Skill: skill, Name: info.Name, Icon: info.Icon,
			Title: ptBRAgentTitles[skill], Description: ptBRAgentDescriptions[skill],
		})
	}
	return c
}

// Allowed reports whether a skill may be invoked in this environment.
func (c *Catalog) Allowed(skill string) bool { return c.allowed[skill] }

// Item returns the catalog item for a skill, if listed.
func (c *Catalog) Item(skill string) (CatalogItem, bool) {
	for _, it := range c.Items {
		if it.Skill == skill {
			return it, true
		}
	}
	return CatalogItem{}, false
}

// Agent returns the catalog agent for a skill, if listed.
func (c *Catalog) Agent(skill string) (CatalogAgent, bool) {
	for _, a := range c.Agents {
		if a.Skill == skill {
			return a, true
		}
	}
	return CatalogAgent{}, false
}

// Label returns a friendly name for any skill (catalog label, agent name or raw name).
func (c *Catalog) Label(skill string) string {
	if it, ok := c.Item(skill); ok {
		return it.Label
	}
	if a, ok := c.Agent(skill); ok {
		return a.Name + " (" + a.Title + ")"
	}
	return strings.TrimPrefix(skill, "bmad-")
}
