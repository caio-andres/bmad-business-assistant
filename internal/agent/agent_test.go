package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	bmadassets "github.com/caio-andres/bmad-business-assistant/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

func setup(t *testing.T, p llm.Provider, subagents bool) (*Interpreter, *runtime.TurnContext, PromptInput) {
	t.Helper()
	b, err := bmad.Load(bmadassets.FS())
	if err != nil {
		t.Fatal(err)
	}
	fs := runtime.NewProjectFS(runtime.ProjectInput{Bundle: b, Overlay: bmad.OverlayInput{UserName: "Ana Souza", ProjectName: "pag-cashback"}})
	cat := bmad.NewCatalog(b, nil)
	tc := &runtime.TurnContext{FS: fs, Bundle: b, Catalog: cat, Now: func() time.Time { return time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC) }}
	it := New(Config{Provider: p, Model: "m", SubagentsEnabled: subagents}, runtime.DefaultRegistry(false))
	pi := PromptInput{AppName: "Alicerce", BMadVersion: b.Version, UserName: "Ana Souza", Today: "2026-11-22 (domingo)", Project: "PAG / cashback", Catalog: cat, SubagentsEnabled: subagents}
	return it, tc, pi
}

func TestDemoProductBriefEndToEnd(t *testing.T) {
	it, tc, pi := setup(t, &llm.Demo{}, false)
	var mu sync.Mutex
	var evs []events.Event
	emit := func(e events.Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() }
	tc.Emit = emit
	tc.FS.OnWrite = func(abs string, del bool) { runtime.EmitDocument(tc, abs, del) }

	transcript := []llm.Message{{Role: llm.RoleUser, Content: "Quero criar o product brief"}}
	act, err := ActivationMessages(tc, "bmad-product-brief", "")
	if err != nil {
		t.Fatal(err)
	}
	transcript = append(transcript, act...)
	if tc.ActiveSkill() != "bmad-product-brief" {
		t.Fatal("activation did not set the active skill")
	}

	answers := []string{"Clientes desistem do cashback", "Jovens que usam Pix", "Cashback automático", "Sem resgate manual", "30% de adesão", "3 parceiros"}
	for turn := 0; turn <= len(answers); turn++ {
		pi.ActiveSkill = tc.ActiveSkill()
		res, err := it.RunTurn(context.Background(), transcript, pi, tc, emit, nil)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if strings.Contains(res.Text, "/project") || strings.Contains(res.Text, "_bmad-output") {
			t.Fatalf("turn %d leaked a path: %s", turn, res.Text)
		}
		transcript = res.Messages
		if turn < len(answers) {
			if res.Ask == nil {
				t.Fatalf("turn %d: expected a question, got %q (stopped=%s)", turn, res.Text, res.Stopped)
			}
			transcript = append(transcript, llm.Message{Role: llm.RoleUser, Content: answers[turn]})
		} else if !res.SkillEnded {
			t.Fatalf("last turn should end the skill: %+v", res)
		}
	}
	files, _ := tc.FS.Glob("/project/_bmad-output/**")
	var brief, memlog string
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, "/brief.md"):
			brief = f
		case strings.HasSuffix(f, "/.memlog.md"):
			memlog = f
		}
	}
	if brief == "" || memlog == "" {
		t.Fatalf("missing outputs: %v", files)
	}
	if !strings.Contains(brief, "brief-pag-cashback-2026-11-22") {
		t.Errorf("unexpected workspace: %s", brief)
	}
	data, _ := tc.FS.Read(brief)
	if !strings.Contains(string(data), "status: complete") || !strings.Contains(string(data), "3 parceiros") {
		t.Errorf("brief not finalized:\n%s", data)
	}
	ml, _ := tc.FS.Read(memlog)
	if strings.Count(string(ml), "(decision by user)") != len(answers) {
		t.Errorf("memlog entries:\n%s", ml)
	}
	var acts, docs, opts int
	for _, e := range evs {
		switch e.Type {
		case events.Activity:
			acts++
			if strings.Contains(e.Activity.Label, "/project") {
				t.Errorf("activity leaked path: %s", e.Activity.Label)
			}
		case events.Document:
			docs++
		case events.Options:
			opts++
		}
	}
	if acts == 0 || docs == 0 || opts != len(answers) {
		t.Errorf("events: activities=%d documents=%d options=%d", acts, docs, opts)
	}
}

func TestSanitize(t *testing.T) {
	in := "Gravei em `/project/_bmad-output/planning-artifacts/prds/prd-x/prd.md` e na pasta {project-root}/_bmad-output/planning-artifacts/prds/prd-x/ ok"
	out := Sanitize(in)
	if strings.Contains(out, "/project") || strings.Contains(out, "_bmad-output") {
		t.Fatalf("not sanitized: %s", out)
	}
	if !strings.Contains(out, "**") {
		t.Fatalf("expected friendly name: %s", out)
	}
	var got strings.Builder
	s := newStreamSanitizer(func(d string) { got.WriteString(d) })
	for _, chunk := range []string{"Salvei em /proj", "ect/_bmad-output/brief", "s/b/brief.md agora."} {
		s.Write(chunk)
	}
	full := s.Flush()
	if strings.Contains(full, "/project") || full != got.String() {
		t.Fatalf("stream sanitize: %q / %q", full, got.String())
	}
}

// scripted is a provider that replays canned responses per purpose.
type scripted struct {
	mu    sync.Mutex
	main  []llm.Message
	sub   []llm.Message
	calls map[string]int
	seen  []llm.Request
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Chat(ctx context.Context, req llm.Request, onText func(string)) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.seen = append(s.seen, req)
	var m llm.Message
	switch req.Purpose {
	case "subagent":
		m = s.sub[s.calls["subagent"]%len(s.sub)]
	case "summary":
		m = llm.Message{Role: llm.RoleAssistant, Content: "RESUMO"}
	default:
		i := s.calls["interpreter"]
		if i >= len(s.main) {
			m = llm.Message{Role: llm.RoleAssistant, Content: "fim"}
		} else {
			m = s.main[i]
		}
	}
	s.calls[req.Purpose]++
	if onText != nil && m.Content != "" {
		onText(m.Content)
	}
	return &llm.Response{Message: m, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}, Model: "x"}, nil
}

func tcall(id, name string, args any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Arguments: string(b)}
}

func TestParallelSubagentsAndUsage(t *testing.T) {
	p := &scripted{
		main: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				tcall("1", "spawn_subagent", map[string]any{"task": "revise a lente A"}),
				tcall("2", "spawn_subagent", map[string]any{"task": "revise a lente B"}),
				tcall("3", "spawn_subagent", map[string]any{"task": "revise a lente C"}),
			}},
			{Role: llm.RoleAssistant, Content: "Revisões concluídas."},
		},
		sub: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tcall("w", "write_file", map[string]string{"path": "/project/_bmad-output/review.md", "content": "ok"})}},
			{Role: llm.RoleAssistant, Content: "lente revisada"},
		},
	}
	it, tc, pi := setup(t, p, true)
	var usageCalls int
	res, err := it.RunTurn(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "revise"}}, pi, tc, nil, func(string, string, llm.Usage) { usageCalls++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Revisões concluídas." {
		t.Fatalf("text = %q", res.Text)
	}
	var toolMsgs int
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && m.Name == "spawn_subagent" {
			toolMsgs++
			if m.Content != "lente revisada" {
				t.Errorf("subagent result = %q", m.Content)
			}
		}
	}
	if toolMsgs != 3 || p.calls["subagent"] != 6 || usageCalls != 8 {
		t.Fatalf("toolMsgs=%d subCalls=%d usage=%d", toolMsgs, p.calls["subagent"], usageCalls)
	}
	if !tc.FS.Exists("/project/_bmad-output/review.md") {
		t.Fatal("subagent write not visible")
	}
	for _, r := range p.seen {
		if r.Purpose == "subagent" {
			for _, d := range r.Tools {
				if d.Name == "ask_user" || d.Name == "spawn_subagent" || d.Name == "end_skill" {
					t.Fatalf("subagent got main-only tool %s", d.Name)
				}
			}
		}
	}
}

func TestMaxIterationsAndUnknownTool(t *testing.T) {
	loop := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tcall("x", "does_not_exist", map[string]any{})}}
	p := &scripted{main: []llm.Message{loop, loop, loop, loop, loop}}
	it, tc, pi := setup(t, p, false)
	it.cfg.MaxIterations = 3
	res, err := it.RunTurn(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "oi"}}, pi, tc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stopped != "max_iterations" {
		t.Fatalf("stopped = %q", res.Stopped)
	}
	last := res.Messages[len(res.Messages)-1]
	if !strings.Contains(last.Content, "ferramenta desconhecida") {
		t.Fatalf("unknown tool result = %q", last.Content)
	}
}

func TestCompactionReinjectsSkill(t *testing.T) {
	p := &scripted{main: []llm.Message{{Role: llm.RoleAssistant, Content: "seguindo"}}}
	it, tc, pi := setup(t, p, false)
	it.cfg.ContextBudget = 2000
	it.cfg.KeepRecent = 4
	tc.SetActive("bmad-prd", "bmad-agent-pm")
	var transcript []llm.Message
	for i := 0; i < 30; i++ {
		transcript = append(transcript,
			llm.Message{Role: llm.RoleUser, Content: strings.Repeat("resposta longa ", 60)},
			llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("pergunta ", 40)})
	}
	res, err := it.RunTurn(context.Background(), transcript, pi, tc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Compacted || p.calls["summary"] != 1 {
		t.Fatalf("compacted=%v summaries=%d", res.Compacted, p.calls["summary"])
	}
	if !strings.Contains(res.Messages[0].Content, "RESUMO") {
		t.Fatalf("first message = %q", res.Messages[0].Content)
	}
	var reinjected []string
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && strings.HasPrefix(m.ToolCallID, "reinject_") {
			reinjected = append(reinjected, m.ToolCallID)
		}
	}
	if len(reinjected) != 2 {
		t.Fatalf("reinjected = %v", reinjected)
	}
	if len(res.Messages) >= len(transcript) {
		t.Fatalf("transcript not reduced: %d >= %d", len(res.Messages), len(transcript))
	}
}

func TestSystemPromptLayers(t *testing.T) {
	_, _, pi := setup(t, &llm.Demo{}, true)
	pi.Documents = []DocInfo{{Name: "Product Brief", Path: vfs.Root + "/_bmad-output/x/brief.md", Status: "draft"}}
	s := SystemPrompt(pi)
	for _, want := range []string{"BMad Method v6.12.4", "spawn_subagent", "ask_user", "NUNCA mostre caminhos", "Ana Souza", "PAG / cashback", "Product Brief", "bmad-prd"} {
		if !strings.Contains(s, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
