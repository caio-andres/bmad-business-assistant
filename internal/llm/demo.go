package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Demo is a deterministic, zero-cost provider that drives the real tool loop
// (activate skill, resolve config, memlog, write documents, ask with options,
// finish) so the whole product can be demonstrated and tested without an LLM.
type Demo struct {
	// Delay between streamed words (0 in tests).
	Delay time.Duration
}

func (d *Demo) Name() string { return "demo" }

type demoFlow struct {
	dir, prefix, file, title string
	sections                 []string
	questions                []string
	html                     bool // produce an HTML mock at the end (UX)
}

var demoFlows = map[string]demoFlow{
	"bmad-product-brief": {dir: "planning-artifacts/briefs", prefix: "brief", file: "brief.md", title: "Product Brief",
		sections: []string{"Problema", "Público-alvo", "Solução proposta", "Diferenciais", "Métricas de sucesso", "Escopo do MVP"},
		questions: []string{
			"Para começar, qual problema este produto resolve? Conte com as suas palavras, sem se preocupar com formato.",
			"Quem sente mais esse problema? Descreva o público principal.",
			"Como você imagina a solução, na prática?",
			"O que torna essa solução diferente do que já existe?",
			"Como vamos saber que deu certo? Cite 2 ou 3 métricas.",
			"O que entra na primeira versão e o que fica para depois?",
		}},
	"bmad-prd": {dir: "planning-artifacts/prds", prefix: "prd", file: "prd.md", title: "PRD",
		sections: []string{"Visão e objetivos", "Usuários e personas", "Requisitos funcionais", "Requisitos não funcionais", "Métricas", "Fora de escopo"},
		questions: []string{
			"Qual é a visão do produto e o objetivo principal desta versão?",
			"Quem são os usuários e o que cada um precisa fazer?",
			"Quais funcionalidades são indispensáveis? Liste as principais.",
			"Há exigências de desempenho, segurança, acessibilidade ou regulatórias?",
			"Quais números mostram que o produto está funcionando?",
			"O que fica explicitamente fora desta versão?",
		}},
	"bmad-ux": {dir: "planning-artifacts/ux-designs", prefix: "ux", file: "EXPERIENCE.md", title: "Experiência (UX)", html: true,
		sections: []string{"Contexto de uso", "Jornada principal", "Telas-chave", "Estados e mensagens", "Tom visual"},
		questions: []string{
			"Em que situação a pessoa usa o produto? (lugar, dispositivo, pressa...)",
			"Descreva a jornada principal, do primeiro acesso ao objetivo concluído.",
			"Quais são as telas mais importantes?",
			"O que acontece quando algo dá errado ou está vazio?",
			"Que sensação o visual deve passar? (ex.: confiável, leve, premium)",
		}},
	"bmad-create-epics-and-stories": {dir: "planning-artifacts", prefix: "", file: "epics.md", title: "Épicos e histórias",
		sections: []string{"Épico 1", "Épico 2", "Histórias prioritárias", "Critérios de aceite"},
		questions: []string{
			"Qual é o primeiro grande bloco de valor (épico) a entregar?",
			"E o segundo épico?",
			"Quais histórias de usuário são prioritárias? (\"Como <persona>, quero <ação> para <benefício>\")",
			"Para a história mais importante, quais são os critérios de aceite?",
		}},
	"bmad-brainstorming": {dir: "brainstorming", prefix: "brainstorm", file: "brainstorm.md", title: "Brainstorming",
		sections: []string{"Tema e objetivo", "Ideias", "Ideias ousadas", "Prioridades"},
		questions: []string{
			"O que vamos explorar hoje, e por quê?",
			"Solte ideias à vontade, sem filtro: quantas conseguir.",
			"Agora vamos inverter: e se não houvesse restrição de orçamento ou de tecnologia?",
			"Quais 3 ideias você levaria adiante?",
		}},
}

var defaultFlow = demoFlow{dir: "planning-artifacts/notes", prefix: "notes", file: "notes.md", title: "Anotações",
	sections:  []string{"Contexto", "Objetivo", "Detalhes", "Próximos passos"},
	questions: []string{"Qual é o contexto?", "Qual é o objetivo?", "Que detalhes não podem ficar de fora?", "Quais são os próximos passos?"}}

// demoExamples are fictitious sample answers offered as quick replies, so a
// demo can be clicked through end to end.
var demoExamples = map[string][]string{
	"bmad-product-brief": {
		"Clientes abandonam a adesão ao cashback porque o processo é longo e confuso.",
		"Pessoas de 25 a 45 anos que usam Pix toda semana.",
		"Cashback automático em compras com Pix, sem cadastro extra, direto no app.",
		"Crédito na hora, sem pontos nem resgate manual.",
		"Adesão de 30% da base ativa em 6 meses e NPS acima de 60.",
		"MVP com 3 parceiros em Pix; cartão e marketplace depois.",
	},
	"bmad-prd": {
		"Ser o jeito mais simples de ganhar cashback no dia a dia, lançando com 3 parceiros.",
		"Cliente final, parceiro lojista e time de operações.",
		"Ativar em 1 toque, extrato de cashback, campanhas por parceiro e aviso de crédito.",
		"Crédito em até 5 segundos, LGPD e acessibilidade WCAG AA.",
		"Adesão, valor médio creditado e transações Pix por cliente ativo.",
		"Cashback em cartão de crédito e programa de pontos.",
	},
	"bmad-ux": {
		"No celular, na fila do caixa, com pressa e uma mão só.",
		"Vê o banner, ativa com um toque, paga com Pix e recebe o aviso do crédito.",
		"Início com saldo, tela de ativação e extrato.",
		"Sem conexão: aviso e nova tentativa; sem cashback: sugestão de parceiros.",
		"Confiável e leve, com cores calmas e números grandes.",
	},
	"bmad-create-epics-and-stories": {
		"Ativação e crédito de cashback em Pix.",
		"Painel de campanhas para parceiros.",
		"Como cliente, quero ativar o cashback com um toque para não perder benefícios.",
		"Dado um cliente elegível, quando tocar em Ativar, então o cashback aparece no extrato.",
	},
	"bmad-brainstorming": {
		"Como aumentar o uso do Pix entre clientes jovens.",
		"Cashback em Pix, desafios mensais, metas compartilhadas com amigos.",
		"Pix por voz e Pix offline por aproximação.",
		"Cashback em Pix, desafios mensais e metas com amigos.",
	},
}

func demoOptions(workflow string, i int) []string {
	if ex := demoExamples[workflow]; i < len(ex) {
		return []string{ex[i]}
	}
	return nil
}

var demoAgentMenu = map[string][]string{
	"bmad-agent-analyst":     {"Brainstorming de ideias", "Pesquisa de mercado e concorrência", "Product Brief"},
	"bmad-agent-pm":          {"PRD: requisitos do produto", "Épicos e histórias"},
	"bmad-agent-ux-designer": {"Experiência e design (UX)"},
}

var demoLabelSkill = []struct{ key, skill string }{
	{"brainstorm", "bmad-brainstorming"}, {"pesquisa", "bmad-deep-recon"}, {"brief", "bmad-product-brief"},
	{"prd", "bmad-prd"}, {"épico", "bmad-create-epics-and-stories"}, {"epico", "bmad-create-epics-and-stories"},
	{"ux", "bmad-ux"}, {"experiência", "bmad-ux"},
}

var agentNames = map[string]string{"bmad-agent-analyst": "📊 Mary", "bmad-agent-pm": "📋 John", "bmad-agent-ux-designer": "🎨 Sally"}

var (
	reUser    = regexp.MustCompile(`(?m)^- Pessoa: (.+)$`)
	reProject = regexp.MustCompile(`(?m)^- Projeto vinculado: (.+)$`)
	reToday   = regexp.MustCompile(`(?m)^- Data de hoje: (\d{4}-\d{2}-\d{2})`)
)

func (d *Demo) Chat(ctx context.Context, req Request, onText func(string)) (*Response, error) {
	var msg Message
	switch req.Purpose {
	case "router":
		msg = Message{Role: RoleAssistant, Content: `{"action":"clarify","confidence":0.3}`}
	case "summary":
		msg = Message{Role: RoleAssistant, Content: "Resumo (modo demonstração): a pessoa está construindo um documento do BMad com a assistente; as respostas anteriores já estão registradas no documento e na memória da sessão."}
	case "subagent":
		msg = Message{Role: RoleAssistant, Content: "Resumo do especialista (modo demonstração): tarefa analisada; nenhum ponto crítico encontrado."}
	case "deck":
		msg = Message{Role: RoleAssistant, Content: demoDeck(req)}
	case "repo-summary":
		msg = Message{Role: RoleAssistant, Content: "# Resumo do sistema (modo demonstração)\n\n## O que faz\nSistema fictício usado na demonstração.\n\n## Principais fluxos\n- Fluxo principal\n\n## Integrações\n- Nenhuma conhecida\n\n## Limitações conhecidas\n- Resumo gerado sem LLM real.\n"}
	default:
		msg = d.interpret(req)
	}
	if onText != nil && msg.Content != "" && req.Purpose == "interpreter" {
		for i, w := range strings.SplitAfter(msg.Content, " ") {
			if d.Delay > 0 && i > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(d.Delay):
				}
			}
			onText(w)
		}
	}
	in := EstimateTokens(req.Messages)
	return &Response{Message: msg, Model: "demo", FinishReason: "stop", Usage: Usage{InputTokens: in, OutputTokens: len(msg.Content)/4 + 10}}, nil
}

type demoCtx struct {
	user, project, today string
}

func parseDemoCtx(req Request) demoCtx {
	c := demoCtx{user: "você", project: "rascunho", today: time.Now().Format("2006-01-02")}
	if len(req.Messages) == 0 || req.Messages[0].Role != RoleSystem {
		return c
	}
	sys := req.Messages[0].Content
	if m := reUser.FindStringSubmatch(sys); m != nil {
		c.user = strings.Fields(m[1])[0]
	}
	if m := reProject.FindStringSubmatch(sys); m != nil && !strings.HasPrefix(m[1], "nenhum") {
		c.project = slug(m[1])
	}
	if m := reToday.FindStringSubmatch(sys); m != nil {
		c.today = m[1]
	}
	return c
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteRune('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func call(id, name string, args any) ToolCall {
	b, _ := json.Marshal(args)
	return ToolCall{ID: "demo_" + id, Name: name, Arguments: string(b)}
}

func (d *Demo) interpret(req Request) Message {
	c := parseDemoCtx(req)
	msgs := req.Messages
	last := msgs[len(msgs)-1]

	// After end_skill: closing message only.
	if last.Role == RoleTool && last.Name == "end_skill" {
		return Message{Role: RoleAssistant, Content: "Etapa concluída! O documento final está no painel. Você pode exportá-lo (Word, PowerPoint, PDF) ou publicá-lo no projeto para o time técnico revisar."}
	}

	workflow, agent, actIdx := "", "", -1
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Name != "invoke_skill" {
				continue
			}
			var a struct {
				Skill string `json:"skill"`
			}
			_ = json.Unmarshal([]byte(tc.Arguments), &a)
			if strings.HasPrefix(a.Skill, "bmad-agent-") {
				agent = a.Skill
			} else if a.Skill != "" {
				workflow, actIdx = a.Skill, i
			}
		}
	}
	lastUser := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			lastUser = msgs[i].Content
			break
		}
	}

	if workflow == "" {
		if agent == "" {
			return Message{Role: RoleAssistant, Content: "Olá! Escolha uma etapa no catálogo para começarmos."}
		}
		low := strings.ToLower(lastUser)
		for _, ls := range demoLabelSkill {
			if strings.Contains(low, ls.key) {
				return Message{Role: RoleAssistant, ToolCalls: []ToolCall{call("invoke", "invoke_skill", map[string]string{"skill": ls.skill})}}
			}
		}
		name := agentNames[agent]
		return Message{Role: RoleAssistant,
			Content:   fmt.Sprintf("%s aqui. Olá, %s! Posso conduzir uma destas etapas com você. Por qual começamos?", name, c.user),
			ToolCalls: []ToolCall{call("menu", "ask_user", map[string]any{"options": demoAgentMenu[agent]})}}
	}

	flow, ok := demoFlows[workflow]
	if !ok {
		flow = defaultFlow
		flow.title = workflow
	}
	// user answers after the activation
	var answers []string
	for _, m := range msgs[actIdx+1:] {
		if m.Role == RoleUser {
			answers = append(answers, m.Content)
		}
	}
	// If the demo already acted in this turn (tool results after the last user message), stop.
	if last.Role == RoleTool && strings.HasPrefix(last.ToolCallID, "demo_") && last.Name != "invoke_skill" {
		return Message{Role: RoleAssistant, Content: "Pronto."}
	}

	ws := "{project-root}/_bmad-output/" + flow.dir
	if flow.prefix != "" {
		ws += "/" + flow.prefix + "-" + c.project + "-" + c.today
	}
	docPath := ws + "/" + flow.file
	memlog := "uv run {project-root}/_bmad/scripts/memlog.py"
	persona := agentNames[agent]
	if persona == "" {
		persona = "📊 Mary"
	}
	k := len(answers)
	if k == 0 {
		text := fmt.Sprintf("%s aqui. Olá, %s! Vamos construir juntos o documento **%s**. Ele vai aparecer no painel ao lado e ser atualizado a cada resposta.\n\n*(Modo demonstração: as respostas da IA são simuladas.)*\n\n%s",
			persona, c.user, flow.title, flow.questions[0])
		return Message{Role: RoleAssistant, Content: text, ToolCalls: []ToolCall{
			call("cfg", "run_command", map[string]string{"command": "uv run {project-root}/_bmad/scripts/resolve_customization.py --skill {skill-root} --project-root {project-root} --key workflow"}),
			call("init", "run_command", map[string]string{"command": fmt.Sprintf(`%s init --workspace %s --field topic="%s" --field mode=demo`, memlog, ws, flow.title)}),
			call("write", "write_file", map[string]string{"path": docPath, "content": renderDemoDoc(flow, c, answers, false)}),
			call("ask", "ask_user", map[string]any{"options": demoOptions(workflow, 0)}),
		}}
	}
	answer := strings.Join(strings.Fields(answers[k-1]), " ")
	calls := []ToolCall{
		call(fmt.Sprintf("log%d", k), "run_command", map[string]string{"command": fmt.Sprintf(`%s append --workspace %s --type decision --by user --text %q`, memlog, ws, flow.sections[min(k, len(flow.sections))-1]+": "+answer)}),
	}
	if k < len(flow.sections) {
		calls = append(calls,
			call(fmt.Sprintf("write%d", k), "write_file", map[string]string{"path": docPath, "content": renderDemoDoc(flow, c, answers, false)}),
			call(fmt.Sprintf("ask%d", k), "ask_user", map[string]any{"options": demoOptions(workflow, k)}),
		)
		text := fmt.Sprintf("Anotado na seção **%s**. ✅\n\n%s", flow.sections[k-1], flow.questions[k])
		return Message{Role: RoleAssistant, Content: text, ToolCalls: calls}
	}
	calls = append(calls,
		call("final", "write_file", map[string]string{"path": docPath, "content": renderDemoDoc(flow, c, answers, true)}),
		call("status", "run_command", map[string]string{"command": fmt.Sprintf(`%s append --workspace %s --type event --text "documento concluído"`, memlog, ws)}),
	)
	if flow.html {
		calls = append(calls,
			call("mock", "write_file", map[string]string{"path": ws + "/mockups/key-screen.html", "content": demoMockHTML(c, answers)}),
			call("show", "show_document", map[string]string{"path": ws + "/mockups/key-screen.html"}),
		)
	}
	calls = append(calls, call("end", "end_skill", map[string]string{"summary": flow.title + " concluído"}))
	return Message{Role: RoleAssistant, Content: fmt.Sprintf("Perfeito, registrei a última seção. O documento **%s** está completo.", flow.title), ToolCalls: calls}
}

func renderDemoDoc(f demoFlow, c demoCtx, answers []string, final bool) string {
	status := "draft"
	if final {
		status = "complete"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "---\ntitle: %q\nstatus: %s\ncreated: %s\nupdated: %s\n---\n\n", f.title+" — "+c.project, status, c.today, c.today)
	fmt.Fprintf(&sb, "# %s: %s\n\n> Documento gerado no **modo demonstração** (respostas da IA simuladas).\n\n", f.title, c.project)
	for i, s := range f.sections {
		fmt.Fprintf(&sb, "## %s\n\n", s)
		if i < len(answers) {
			sb.WriteString(strings.TrimSpace(answers[i]) + "\n\n")
		} else {
			sb.WriteString("_A preencher._\n\n")
		}
	}
	if final {
		sb.WriteString("## Próximos passos\n\n- Revisar com as pessoas envolvidas\n- Publicar no projeto para o time técnico\n")
	}
	return sb.String()
}

func demoMockHTML(c demoCtx, answers []string) string {
	title := "Tela principal"
	if len(answers) > 2 {
		title = answers[2]
		if r := []rune(title); len(r) > 60 {
			title = string(r[:60])
		}
	}
	return `<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><title>Protótipo</title>
<style>body{font-family:system-ui,sans-serif;background:#f4f1ea;margin:0;display:grid;place-items:center;min-height:100vh}
.phone{width:320px;border-radius:28px;background:#fff;box-shadow:0 20px 60px rgba(0,0,0,.15);padding:24px}
h1{font-size:20px;margin:8px 0 16px}.card{background:#14532d;color:#fff;border-radius:16px;padding:16px;margin-bottom:12px}
.btn{display:block;width:100%;padding:12px;border:0;border-radius:12px;background:#0f766e;color:#fff;font-weight:600;margin-top:12px}
small{color:#666}</style></head><body><div class="phone"><small>Protótipo de demonstração · ` + htmlEscape(c.project) + `</small>
<h1>` + htmlEscape(title) + `</h1><div class="card">Saldo disponível<br><strong style="font-size:28px">R$ 1.250,00</strong></div>
<p>Jornada principal simulada a partir das respostas da sessão.</p><button class="btn" onclick="this.textContent='Feito! ✅'">Continuar</button></div></body></html>`
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func demoDeck(req Request) string {
	// Build slides from markdown headings in the last user message.
	content := ""
	if n := len(req.Messages); n > 0 {
		content = req.Messages[n-1].Content
	}
	type slide struct {
		Title   string   `json:"title"`
		Bullets []string `json:"bullets"`
	}
	var slides []slide
	var cur *slide
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "## "):
			slides = append(slides, slide{Title: strings.TrimPrefix(t, "## ")})
			cur = &slides[len(slides)-1]
		case cur != nil && t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ">") && len(cur.Bullets) < 4:
			cur.Bullets = append(cur.Bullets, strings.TrimLeft(t, "-* "))
		}
	}
	if len(slides) > 10 {
		slides = slides[:10]
	}
	out, _ := json.Marshal(map[string]any{"title": "Resumo executivo", "subtitle": "Modo demonstração", "slides": slides,
		"nextSteps": []string{"Validar com as áreas envolvidas", "Publicar para o time técnico"}})
	return string(out)
}
