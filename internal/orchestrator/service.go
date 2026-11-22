package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/agent"
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/guard"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// Friendly errors surfaced by the API.
var (
	ErrBusy     = errors.New("Aguarde a resposta anterior terminar.")
	ErrQuota    = errors.New("Você atingiu o limite diário de uso da IA. O limite reinicia amanhã; fale com o administrador se precisar de mais.")
	ErrBlocked  = errors.New("A mensagem parece conter dados pessoais ou credenciais e não foi enviada. Remova esses dados e tente de novo.")
	ErrNotFound = errors.New("Conversa não encontrada.")
	ErrSkill    = errors.New("Essa etapa não está disponível neste ambiente.")
)

// ContextProvider supplies product context (docs/systems) for a project.
type ContextProvider interface {
	// Docs returns a backend mounted at /project/docs and the system names it covers.
	Docs(ctx context.Context, p *store.Project) (vfs.Backend, []string, error)
}

// Pricing converts tokens to USD (0 = unknown, shown as tokens only).
type Pricing struct{ InputPerMTok, OutputPerMTok float64 }

func (p Pricing) Cost(u llm.Usage) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerMTok + float64(u.OutputTokens)/1e6*p.OutputPerMTok
}

// Service runs turns.
type Service struct {
	Store       *store.Store
	Bundle      *bmad.Bundle
	Catalog     *bmad.Catalog
	Allowed     []string
	Interpreter *agent.Interpreter
	Router      Router
	Guard       *guard.Guard
	Python      *runtime.PythonRunner
	Web         runtime.WebProvider
	Context     ContextProvider
	AppName     string
	Location    *time.Location
	Pricing     Pricing
	DailyQuota  int // tokens per user per day; 0 = unlimited
	LeaseTTL    time.Duration
	Demo        bool
	Logger      *slog.Logger

	// turn cancellation (Stop button)
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// TurnRequest is one user action that triggers a turn.
type TurnRequest struct {
	User           *store.User
	ConversationID string
	Text           string // free text or quick-reply label
	StartSkill     string // explicit catalog start (no LLM routing)
	StartAgent     string // explicit agent start
	IP             string
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Service) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.UTC
}

var weekdays = []string{"domingo", "segunda-feira", "terça-feira", "quarta-feira", "quinta-feira", "sexta-feira", "sábado"}

func (s *Service) today() (string, time.Time) {
	now := time.Now().In(s.loc())
	return fmt.Sprintf("%s (%s)", now.Format("2006-01-02"), weekdays[now.Weekday()]), now
}

// Conversation loads a conversation owned by the user.
func (s *Service) Conversation(ctx context.Context, user *store.User, id string) (*store.Conversation, error) {
	c, err := s.Store.GetConversation(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.UserID != user.ID) {
		return nil, ErrNotFound
	}
	return c, err
}

// Cancel stops a running turn of the conversation (Stop button).
func (s *Service) Cancel(convID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cancels[convID]; ok {
		c()
		return true
	}
	return false
}

// RunTurn executes one turn and streams events. It must be called with a
// context that is NOT tied to the HTTP request, so the turn finishes even if
// the browser disconnects.
func (s *Service) RunTurn(ctx context.Context, req TurnRequest, emit events.Emitter) (err error) {
	if emit == nil {
		emit = events.Discard
	}
	conv, err := s.Conversation(ctx, req.User, req.ConversationID)
	if err != nil {
		return err
	}
	if s.DailyQuota > 0 {
		_, now := s.today()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc())
		if tot, err := s.Store.UserUsageSince(ctx, req.User.ID, start); err == nil && tot.InputTokens+tot.OutputTokens >= s.DailyQuota {
			return ErrQuota
		}
	}
	text := strings.TrimSpace(req.Text)
	if req.StartSkill != "" || req.StartAgent != "" {
		skill := req.StartSkill
		if skill == "" {
			skill = req.StartAgent
		}
		if !s.Catalog.Allowed(skill) {
			return ErrSkill
		}
		if text == "" {
			if it, ok := s.Catalog.Item(skill); ok {
				text = it.Starter
			} else if a, ok := s.Catalog.Agent(skill); ok {
				text = "Quero conversar com " + a.Name + "."
			}
		}
	}
	if text == "" {
		return errors.New("Escreva uma mensagem.")
	}
	if len(text) > 30000 {
		return errors.New("A mensagem é longa demais. Envie em partes ou anexe como documento.")
	}
	scan := s.Guard.Scan(text)
	if scan.Blocked {
		s.audit(ctx, req, "pii_blocked", conv.ID, scan.Summary())
		return ErrBlocked
	}
	text = scan.Text

	turnID := store.NewID()
	ttl := s.LeaseTTL
	if ttl == 0 {
		ttl = 12 * time.Minute
	}
	if err := s.Store.AcquireLease(ctx, conv.ID, turnID, ttl); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ErrBusy
		}
		return err
	}
	tctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]context.CancelFunc{}
	}
	s.cancels[conv.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, conv.ID)
		s.mu.Unlock()
		cancel()
		_ = s.Store.ReleaseLease(context.WithoutCancel(ctx), conv.ID, turnID)
	}()

	if scan.Any() {
		s.audit(ctx, req, "pii_masked", conv.ID, scan.Summary())
		emit(events.Event{Type: events.Notice, Code: "pii_masked", Text: "Para proteger dados pessoais, mascaramos " + scan.Summary() + " antes de enviar à IA."})
	}
	userMsg := &store.Message{ConversationID: conv.ID, Role: "user", Content: text}
	if err := s.Store.AddMessage(ctx, userMsg); err != nil {
		return err
	}
	emit(events.Event{Type: events.Message, ID: userMsg.ID, Text: "user"})

	t := &turn{s: s, ctx: tctx, req: req, conv: conv, emit: emit, text: text}
	return t.run()
}

type turn struct {
	s    *Service
	ctx  context.Context
	req  TurnRequest
	conv *store.Conversation
	emit events.Emitter
	text string

	transcript []llm.Message
	tc         *runtime.TurnContext
	prompt     agent.PromptInput
	usage      llm.Usage
	cost       float64
}

func (t *turn) state(state string) {
	skill, agentName := "", ""
	if t.tc != nil {
		skill, agentName = t.tc.ActiveSkill(), t.tc.ActiveAgent()
	}
	label := ""
	if skill != "" {
		label = t.s.Catalog.Label(skill)
	} else if agentName != "" {
		label = t.s.Catalog.Label(agentName)
	}
	_ = t.s.Store.SetState(t.ctx, t.conv.ID, state, skill, agentName)
	t.emit(events.Event{Type: events.State, State: &events.StateInfo{State: state, ActiveSkill: skill, ActiveAgent: agentName, SkillLabel: label}})
}

func (t *turn) onUsage(purpose, model string, u llm.Usage) {
	c := t.s.Pricing.Cost(u)
	t.usage.Add(u)
	t.cost += c
	if err := t.s.Store.AddUsage(context.WithoutCancel(t.ctx), store.UsageRecord{UserID: t.req.User.ID, ConversationID: t.conv.ID, Purpose: purpose, Model: model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: c}); err != nil {
		t.s.logger().Warn("usage not recorded", "err", err)
	}
}

func (t *turn) setup() error {
	var err error
	t.transcript, err = t.s.Store.LoadTranscript(t.ctx, t.conv.ID)
	if err != nil {
		return err
	}
	ws, err := t.s.Store.NewWorkspaceBackend(t.ctx, t.conv.ID, "assistant")
	if err != nil {
		return err
	}
	var project *store.Project
	projectName, projectLabel := "", ""
	var docs vfs.Backend
	var systems []string
	if t.conv.ProjectID != "" {
		if p, err := t.s.Store.GetProject(t.ctx, t.conv.ProjectID); err == nil {
			project = p
			projectName = strings.ToLower(p.Sigla) + "-" + p.Slug
			projectLabel = p.Sigla + " / " + p.Name
			if t.s.Context != nil {
				if d, sys, err := t.s.Context.Docs(t.ctx, p); err == nil {
					docs, systems = d, sys
				} else {
					t.s.logger().Warn("product context unavailable", "project", p.ID, "err", err)
				}
			}
		}
	}
	_ = project
	fs := runtime.NewProjectFS(runtime.ProjectInput{
		Bundle: t.s.Bundle, Allowed: t.s.Allowed, Workspace: ws, Docs: docs,
		Overlay: bmad.OverlayInput{UserName: t.req.User.Name, ProjectName: projectName, Language: bmad.LanguagePTBR, HasSystems: len(systems) > 0},
	})
	t.tc = &runtime.TurnContext{FS: fs, Bundle: t.s.Bundle, Catalog: t.s.Catalog, Emit: t.emit, Python: t.s.Python, Web: t.s.Web}
	t.tc.SetActive(t.conv.ActiveSkill, t.conv.ActiveAgent)
	fs.OnWrite = func(abs string, deleted bool) { runtime.EmitDocument(t.tc, abs, deleted) }
	t.tc.OnSkillChange = func(skill, agentName string) { t.state(store.StateExecuting) }

	today, _ := t.s.today()
	files, _ := t.s.Store.ListFiles(t.ctx, t.conv.ID)
	var docsInfo []agent.DocInfo
	for _, f := range files {
		if strings.HasPrefix(f.Path, ".view/") || strings.HasSuffix(f.Path, ".memlog.md") {
			continue
		}
		status := ""
		if data, err := ws.Read(f.Path); err == nil {
			if fm, _ := bmad.Frontmatter(data); fm != nil {
				if v, ok := fm["status"].(string); ok {
					status = v
				}
			}
		}
		if f.UpdatedBy == "user" {
			status = strings.TrimSpace(status + " · editado manualmente pela pessoa (releia antes de alterar)")
		}
		docsInfo = append(docsInfo, agent.DocInfo{Name: runtime.FriendlyName(f.Path), Path: vfs.Root + "/" + runtime.OutputDir + "/" + f.Path, Status: status})
	}
	t.prompt = agent.PromptInput{
		AppName: t.s.AppName, BMadVersion: t.s.Bundle.Version, UserName: t.req.User.Name, Today: today,
		Project: projectLabel, Systems: systems, Documents: docsInfo, Catalog: t.s.Catalog,
		WebEnabled: t.s.Web != nil, SubagentsEnabled: t.s.Interpreter.Tools().Has("spawn_subagent"),
		PythonEnabled: t.s.Python.Enabled(),
	}
	return nil
}

func (t *turn) run() error {
	if err := t.setup(); err != nil {
		return err
	}
	t.transcript = append(t.transcript, llm.Message{Role: llm.RoleUser, Content: t.text})
	start := t.req.StartSkill
	startAgent := t.req.StartAgent
	inSkill := t.conv.ActiveSkill != "" || t.conv.ActiveAgent != ""

	if start == "" && startAgent == "" && !inSkill {
		t.state(store.StateRouting)
		d, err := t.s.Router.Route(t.ctx, t.text, t.s.Catalog)
		if err != nil {
			d = Decision{Action: ActionClarify}
		}
		t.s.logger().Info("routed", "conversation", t.conv.ID, "action", d.Action, "skill", d.Skill, "confidence", d.Confidence, "by", d.By)
		switch d.Action {
		case ActionStartSkill:
			start = d.Skill
		case ActionAgent:
			startAgent = d.Skill
		case ActionOutOfScope:
			return t.deterministicReply(outOfScopeText, s2labels(t.s.Catalog, []string{"bmad-create-epics-and-stories", "bmad-prd"}))
		default:
			return t.deterministicReply(clarifyText, s2labels(t.s.Catalog, d.Candidates))
		}
	}
	if t.req.StartSkill != "" || t.req.StartAgent != "" {
		// an explicit start from the UI replaces whatever was active
		keepAgent := t.tc.ActiveAgent()
		if t.req.StartAgent != "" {
			keepAgent = ""
		}
		t.tc.SetActive("", keepAgent)
	}
	if startAgent != "" {
		act, err := agent.ActivationMessages(t.tc, startAgent, "")
		if err != nil {
			return err
		}
		t.transcript = append(t.transcript, act...)
	}
	if start != "" {
		if start != t.tc.ActiveSkill() {
			act, err := agent.ActivationMessages(t.tc, start, t.text)
			if err != nil {
				return err
			}
			t.transcript = append(t.transcript, act...)
		}
	}
	if start != "" || startAgent != "" {
		t.maybeTitle(start, startAgent)
	}
	t.state(store.StateExecuting)
	if t.s.Demo && len(t.transcript) <= 4 {
		t.emit(events.Event{Type: events.Notice, Code: "demo", Text: "Modo demonstração: as respostas são simuladas, sem IA real. Configure um provedor para usar o BMad de verdade."})
	}

	res, err := t.s.Interpreter.RunTurn(t.ctx, t.transcript, t.prompt, t.tc, t.emit, t.onUsage)
	if res != nil && len(res.Messages) > 0 {
		t.transcript = res.Messages
	}
	if err != nil {
		cancelled := errors.Is(t.ctx.Err(), context.Canceled)
		msg := "A IA não respondeu agora. Seus documentos estão salvos; tente de novo em alguns instantes."
		code := "llm_unavailable"
		if cancelled {
			msg, code = "Resposta interrompida.", "cancelled"
		}
		t.s.logger().Warn("turn failed", "conversation", t.conv.ID, "err", err)
		partial := ""
		if res != nil {
			partial = res.Text
		}
		t.finish(partial, nil, res, store.StateError)
		t.emit(events.Event{Type: events.Error, Code: code, Text: msg})
		return nil
	}
	state := store.StateAwaitingUser
	if res.SkillEnded {
		state = store.StateIdle
		t.tc.SetActive("", "")
	} else if t.tc.ActiveSkill() == "" && t.tc.ActiveAgent() == "" {
		state = store.StateIdle
	}
	if res.Stopped != "" {
		t.emit(events.Event{Type: events.Notice, Code: res.Stopped, Text: "Pausamos esta resposta para não demorar demais. Envie \"continuar\" para seguir de onde parou."})
	}
	var opts []string
	if res.Ask != nil {
		opts = res.Ask.Options
	}
	t.finish(res.Text, opts, res, state)
	return nil
}

func (t *turn) finish(text string, options []string, res *agent.TurnResult, state string) {
	ctx := context.WithoutCancel(t.ctx)
	skill, agentName := t.tc.ActiveSkill(), t.tc.ActiveAgent()
	if err := t.s.Store.SaveTurn(ctx, t.conv.ID, t.transcript, state, skill, agentName); err != nil {
		t.s.logger().Error("save turn", "err", err)
	}
	if strings.TrimSpace(text) != "" || len(options) > 0 {
		m := &store.Message{ConversationID: t.conv.ID, Role: "assistant", Content: text, Options: options}
		if res != nil {
			m.Activities = res.Activities
		}
		if err := t.s.Store.AddMessage(ctx, m); err == nil {
			t.emit(events.Event{Type: events.Message, ID: m.ID, Text: "assistant"})
		}
	}
	t.state(state)
	t.emit(events.Event{Type: events.Usage, Usage: &events.UsageInfo{InputTokens: t.usage.InputTokens, OutputTokens: t.usage.OutputTokens, CostUSD: t.cost}})
}

const clarifyText = "Posso conduzir algumas etapas do método com você. Qual delas combina mais com o que você precisa agora? Se preferir, conte com mais detalhes o que tem em mãos."

const outOfScopeText = "Essa parte (implementação, código ou arquitetura técnica) é feita pelo time técnico, com as ferramentas deles. Aqui eu ajudo a preparar o que eles vão usar: requisitos, experiência, épicos e histórias. Quando estiver pronto, é só publicar os documentos no projeto para o time técnico pegar. Quer começar por uma destas etapas?"

func s2labels(cat *bmad.Catalog, skills []string) []string {
	var out []string
	for _, s := range skills {
		if it, ok := cat.Item(s); ok && len(out) < 3 {
			out = append(out, it.Label)
		}
	}
	return out
}

// deterministicReply answers without calling the LLM (clarify / out of scope).
func (t *turn) deterministicReply(text string, options []string) error {
	t.emit(events.Event{Type: events.Text, Text: text})
	if len(options) > 0 {
		t.emit(events.Event{Type: events.Options, Options: options})
	}
	t.transcript = append(t.transcript, llm.Message{Role: llm.RoleAssistant, Content: text})
	t.finish(text, options, nil, store.StateIdle)
	return nil
}

func (t *turn) maybeTitle(skill, agentName string) {
	if t.conv.Title != "" && t.conv.Title != "Nova conversa" {
		return
	}
	label := t.s.Catalog.Label(skill)
	if skill == "" {
		label = t.s.Catalog.Label(agentName)
	}
	t.conv.Title = label
	_ = t.s.Store.UpdateConversationMeta(t.ctx, t.conv)
}

// EndSkill deactivates the current skill without calling the LLM.
func (s *Service) EndSkill(ctx context.Context, user *store.User, convID string) error {
	conv, err := s.Conversation(ctx, user, convID)
	if err != nil {
		return err
	}
	turnID := store.NewID()
	if err := s.Store.AcquireLease(ctx, conv.ID, turnID, time.Minute); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ErrBusy
		}
		return err
	}
	defer s.Store.ReleaseLease(context.WithoutCancel(ctx), conv.ID, turnID)
	tr, err := s.Store.LoadTranscript(ctx, conv.ID)
	if err != nil {
		return err
	}
	label := s.Catalog.Label(conv.ActiveSkill)
	if conv.ActiveSkill == "" {
		label = s.Catalog.Label(conv.ActiveAgent)
	}
	tr = append(tr,
		llm.Message{Role: llm.RoleUser, Content: "[A pessoa encerrou a etapa \"" + label + "\" pela interface. Não continue essa skill; aguarde um novo pedido.]"},
		llm.Message{Role: llm.RoleAssistant, Content: "Etapa encerrada."})
	if err := s.Store.SaveTurn(ctx, conv.ID, tr, store.StateIdle, "", ""); err != nil {
		return err
	}
	return s.Store.AddMessage(ctx, &store.Message{ConversationID: conv.ID, Role: "notice", Content: "Etapa \"" + label + "\" encerrada. Os documentos continuam disponíveis no painel."})
}

func (s *Service) audit(ctx context.Context, req TurnRequest, action, target, detail string) {
	_ = s.Store.Audit(context.WithoutCancel(ctx), store.AuditEntry{UserID: req.User.ID, Action: action, Target: target, Detail: detail, IP: req.IP})
}
