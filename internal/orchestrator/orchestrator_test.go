package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	bmadassets "github.com/caio-andres/bmad-business-assistant/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/agent"
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/guard"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
)

func catalog(t *testing.T) (*bmad.Bundle, *bmad.Catalog) {
	t.Helper()
	b, err := bmad.Load(bmadassets.FS())
	if err != nil {
		t.Fatal(err)
	}
	return b, bmad.NewCatalog(b, nil)
}

func TestRuleRouter(t *testing.T) {
	_, cat := catalog(t)
	r := RuleRouter{}
	cases := []struct {
		text, action, skill string
		confident           bool
	}{
		{"quero escrever o PRD do novo app de cashback", ActionStartSkill, "bmad-prd", true},
		{"Product Brief", ActionStartSkill, "bmad-product-brief", true},
		{"Quero fazer um brainstorming.", ActionStartSkill, "bmad-brainstorming", true},
		{"preciso montar as histórias de usuário com critério de aceite", ActionStartSkill, "bmad-create-epics-and-stories", true},
		{"quero desenhar as telas do app", ActionStartSkill, "bmad-ux", true},
		{"pesquisa sobre concorrentes de carteira digital", ActionStartSkill, "bmad-deep-recon", true},
		{"prd ou brief?", ActionStartSkill, "bmad-product-brief", false},
		{"queria falar com a Mary", ActionAgent, "bmad-agent-analyst", true},
		{"implementar o código do backend", ActionOutOfScope, "", true},
		{"bom dia", ActionClarify, "", false},
	}
	for _, c := range cases {
		d, err := r.Route(context.Background(), c.text, cat)
		if err != nil {
			t.Fatal(err)
		}
		if d.Action != c.action || d.Skill != c.skill || (d.Confidence >= 0.7) != c.confident {
			t.Errorf("%q -> %+v", c.text, d)
		}
	}
}

type fakeLLMRouter struct{ d Decision }

func (f fakeLLMRouter) Name() string { return "fake" }
func (f fakeLLMRouter) Route(context.Context, string, *bmad.Catalog) (Decision, error) {
	return f.d, nil
}

func TestChainRouter(t *testing.T) {
	_, cat := catalog(t)
	c := &ChainRouter{Threshold: 0.7, Routers: []Router{RuleRouter{}, JevRouter{}, fakeLLMRouter{Decision{Action: ActionStartSkill, Skill: "bmad-prfaq", Confidence: 0.9}}}}
	d, _ := c.Route(context.Background(), "tenho uma ideia de produto", cat)
	if d.Skill != "bmad-prfaq" {
		t.Fatalf("llm fallback not used: %+v", d)
	}
	c.Routers[2] = fakeLLMRouter{Decision{Action: ActionClarify, Confidence: 0.4, Candidates: []string{"bmad-prd", "nao-existe"}}}
	d, _ = c.Route(context.Background(), "prd ou brief?", cat)
	if d.Action != ActionClarify || len(d.Candidates) == 0 || len(d.Candidates) > 3 {
		t.Fatalf("clarify: %+v", d)
	}
	for _, s := range d.Candidates {
		if !cat.Allowed(s) {
			t.Fatalf("candidate outside catalog: %s", s)
		}
	}
	if _, err := (&LLMRouter{Provider: &llm.Demo{}}).Route(context.Background(), "x", cat); err != nil {
		t.Fatalf("demo llm router: %v", err)
	}
	if d, err := parseDecision("```json\n{\"action\":\"start_skill\",\"skill\":\"bmad-prd\",\"confidence\":1.7}\n```"); err != nil || d.Confidence != 1 {
		t.Fatalf("parse: %+v %v", d, err)
	}
}

type env struct {
	svc  *Service
	st   *store.Store
	user *store.User
}

func newEnv(t *testing.T, p llm.Provider) *env {
	t.Helper()
	b, cat := catalog(t)
	st, err := store.Open(context.Background(), "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u := &store.User{Email: "ana@acme.example", Name: "Ana Souza"}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	it := agent.New(agent.Config{Provider: p, Model: "m"}, runtime.DefaultRegistry(false))
	svc := &Service{Store: st, Bundle: b, Catalog: cat, Interpreter: it, Guard: &guard.Guard{Policy: guard.PolicyMask},
		Router: &ChainRouter{Threshold: 0.7, Routers: []Router{RuleRouter{}, &LLMRouter{Provider: p}}}, AppName: "Alicerce", Demo: true,
		Python: runtime.NewPythonRunner("")}
	return &env{svc: svc, st: st, user: u}
}

func (e *env) conv(t *testing.T) *store.Conversation {
	c := &store.Conversation{UserID: e.user.ID, Title: "Nova conversa"}
	if err := e.st.CreateConversation(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

type recorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *recorder) emit(e events.Event) { r.mu.Lock(); r.evs = append(r.evs, e); r.mu.Unlock() }
func (r *recorder) last(typ string) *events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.evs) - 1; i >= 0; i-- {
		if r.evs[i].Type == typ {
			return &r.evs[i]
		}
	}
	return nil
}

func TestDemoFlowPersisted(t *testing.T) {
	e := newEnv(t, &llm.Demo{})
	ctx := context.Background()
	c := e.conv(t)
	rec := &recorder{}
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "quero escrever o PRD do app de cashback"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetConversation(ctx, c.ID)
	if got.State != store.StateAwaitingUser || got.ActiveSkill != "bmad-prd" || got.Title == "Nova conversa" {
		t.Fatalf("after first turn: %+v", got)
	}
	if o := rec.last(events.Options); o == nil || len(o.Options) != 1 {
		t.Fatalf("options event: %+v", o)
	}
	for i := 0; i < 6; i++ {
		if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "resposta " + string(rune('A'+i))}, rec.emit); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = e.st.GetConversation(ctx, c.ID)
	if got.State != store.StateIdle || got.ActiveSkill != "" {
		t.Fatalf("skill should have ended: %+v", got)
	}
	files, _ := e.st.ListFiles(ctx, c.ID)
	var prd string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/prd.md") {
			prd = f.Path
		}
	}
	if prd == "" {
		t.Fatalf("prd not persisted: %+v", files)
	}
	data, _ := e.st.ReadFile(ctx, c.ID, prd)
	if !strings.Contains(string(data), "resposta F") || !strings.Contains(string(data), "status: complete") {
		t.Fatalf("prd content:\n%s", data)
	}
	msgs, _ := e.st.ListMessages(ctx, c.ID)
	if len(msgs) != 14 {
		t.Fatalf("messages = %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "/project") {
			t.Fatalf("leak in persisted message: %s", m.Content)
		}
	}
	tot, _ := e.st.UserUsageSince(ctx, e.user.ID, time.Now().Add(-time.Hour))
	if tot.Calls == 0 {
		t.Fatal("usage not recorded")
	}
}

func TestClarifyOutOfScopeAndButtons(t *testing.T) {
	e := newEnv(t, &llm.Demo{})
	ctx := context.Background()
	c := e.conv(t)
	rec := &recorder{}
	_ = e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "bom dia"}, rec.emit)
	if o := rec.last(events.Options); o == nil || len(o.Options) == 0 || len(o.Options) > 3 {
		t.Fatalf("clarify options: %+v", o)
	}
	got, _ := e.st.GetConversation(ctx, c.ID)
	if got.State != store.StateIdle {
		t.Fatalf("state = %s", got.State)
	}
	rec = &recorder{}
	_ = e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "quero implementar o código do backend"}, rec.emit)
	if tx := rec.last(events.Text); tx == nil || !strings.Contains(tx.Text, "time técnico") {
		t.Fatalf("out of scope reply: %+v", tx)
	}
	// explicit button start: no routing
	rec = &recorder{}
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, StartSkill: "bmad-ux"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	got, _ = e.st.GetConversation(ctx, c.ID)
	if got.ActiveSkill != "bmad-ux" {
		t.Fatalf("button start: %+v", got)
	}
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, StartSkill: "bmad-build"}, rec.emit); !errors.Is(err, ErrSkill) {
		t.Fatalf("out-of-catalog start: %v", err)
	}
	if err := e.svc.EndSkill(ctx, e.user, c.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = e.st.GetConversation(ctx, c.ID)
	if got.State != store.StateIdle || got.ActiveSkill != "" {
		t.Fatalf("end skill: %+v", got)
	}
	files, _ := e.st.ListFiles(ctx, c.ID)
	if len(files) == 0 {
		t.Fatal("documents must be kept after ending the skill")
	}
}

// slow blocks until released, to test the turn lock.
type slow struct {
	llm.Demo
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (s *slow) Chat(ctx context.Context, req llm.Request, on func(string)) (*llm.Response, error) {
	if req.Purpose == "interpreter" {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}
	return s.Demo.Chat(ctx, req, on)
}

func TestTurnLockIsolationQuotaPII(t *testing.T) {
	p := &slow{release: make(chan struct{}), started: make(chan struct{})}
	e := newEnv(t, p)
	ctx := context.Background()
	c := e.conv(t)
	done := make(chan error)
	go func() {
		done <- e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, StartSkill: "bmad-product-brief"}, nil)
	}()
	<-p.started
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "oi de novo"}, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second turn: %v", err)
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	other := &store.User{Email: "bia@acme.example", Name: "Bia"}
	_ = e.st.CreateUser(ctx, other)
	if err := e.svc.RunTurn(ctx, TurnRequest{User: other, ConversationID: c.ID, Text: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("isolation: %v", err)
	}

	rec := &recorder{}
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "o CPF do cliente é 529.982.247-25"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	if n := rec.last(events.Notice); n == nil || n.Code != "pii_masked" {
		t.Fatalf("pii notice: %+v", rec.evs)
	}
	msgs, _ := e.st.ListMessages(ctx, c.ID)
	for _, m := range msgs {
		if strings.Contains(m.Content, "529.982.247-25") {
			t.Fatal("CPF persisted")
		}
	}
	tr, _ := e.st.LoadTranscript(ctx, c.ID)
	for _, m := range tr {
		if strings.Contains(m.Content, "529.982.247-25") {
			t.Fatal("CPF sent to the model")
		}
	}

	e.svc.Guard.Policy = guard.PolicyBlock
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "cpf 529.982.247-25"}, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("block: %v", err)
	}
	e.svc.Guard.Policy = guard.PolicyMask
	e.svc.DailyQuota = 1
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, Text: "mais uma"}, nil); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
}

// failing provider: turn ends in error state with a friendly event.
type failing struct{}

func (failing) Name() string { return "failing" }
func (failing) Chat(context.Context, llm.Request, func(string)) (*llm.Response, error) {
	return nil, llm.ErrUnavailable
}

func TestProviderFailure(t *testing.T) {
	e := newEnv(t, failing{})
	ctx := context.Background()
	c := e.conv(t)
	rec := &recorder{}
	if err := e.svc.RunTurn(ctx, TurnRequest{User: e.user, ConversationID: c.ID, StartSkill: "bmad-prd"}, rec.emit); err != nil {
		t.Fatal(err)
	}
	if ev := rec.last(events.Error); ev == nil || ev.Code != "llm_unavailable" {
		t.Fatalf("error event: %+v", rec.evs)
	}
	got, _ := e.st.GetConversation(ctx, c.ID)
	if got.State != store.StateError || got.Busy {
		t.Fatalf("state after failure: %+v", got)
	}
}
