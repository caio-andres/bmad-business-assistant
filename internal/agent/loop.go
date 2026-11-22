package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
)

// Config tunes the interpreter.
type Config struct {
	Provider            llm.Provider
	Model               string
	SubagentModel       string
	SummaryModel        string
	MaxIterations       int           // tool iterations per turn (main loop)
	MaxSubIterations    int           // per subagent
	TurnTimeout         time.Duration // whole turn
	ToolTimeout         time.Duration // per tool call (subagents get 3x)
	MaxToolResult       int           // bytes per tool result kept in the transcript
	ContextBudget       int           // estimated tokens before compaction
	KeepRecent          int           // messages kept verbatim when compacting
	SubagentsEnabled    bool
	SubagentConcurrency int
	Temperature         *float64
}

func (c *Config) defaults() {
	if c.MaxIterations == 0 {
		c.MaxIterations = 40
	}
	if c.MaxSubIterations == 0 {
		c.MaxSubIterations = 15
	}
	if c.TurnTimeout == 0 {
		c.TurnTimeout = 10 * time.Minute
	}
	if c.ToolTimeout == 0 {
		c.ToolTimeout = 90 * time.Second
	}
	if c.MaxToolResult == 0 {
		c.MaxToolResult = 150 * 1024
	}
	if c.ContextBudget == 0 {
		c.ContextBudget = 60000
	}
	if c.KeepRecent == 0 {
		c.KeepRecent = 16
	}
	if c.SubagentConcurrency == 0 {
		c.SubagentConcurrency = 3
	}
	if c.SubagentModel == "" {
		c.SubagentModel = c.Model
	}
	if c.SummaryModel == "" {
		c.SummaryModel = c.SubagentModel
	}
}

// Interpreter runs turns.
type Interpreter struct {
	cfg   Config
	tools *runtime.Registry
}

// New builds an interpreter; it adds spawn_subagent to a copy of tools when enabled.
func New(cfg Config, tools *runtime.Registry) *Interpreter {
	cfg.defaults()
	it := &Interpreter{cfg: cfg, tools: tools}
	if cfg.SubagentsEnabled {
		it.tools.Add(&runtime.Tool{
			Def: llm.ToolDef{
				Name: "spawn_subagent",
				Description: "Delega uma tarefa a um subagente com contexto novo (extrair trechos de documentos, revisar com uma lente, pesquisar, gerar um HTML). " +
					"Ele lê e grava arquivos, mas não conversa com a pessoa. Várias chamadas na mesma resposta rodam em paralelo. Devolve só o resumo final.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string","description":"Tarefa completa, autossuficiente (o subagente não vê esta conversa)"},"files":{"type":"array","items":{"type":"string"},"description":"Caminhos de arquivos cujo conteúdo deve ser entregue ao subagente"}},"required":["task"]}`),
			},
			Parallel: true,
			MainOnly: true,
			Label: func(tc *runtime.TurnContext, a map[string]any) string {
				t, _ := a["task"].(string)
				t = strings.Join(strings.Fields(t), " ")
				if r := []rune(t); len(r) > 70 {
					t = string(r[:70]) + "…"
				}
				return "Pedindo apoio a um especialista: " + Sanitize(t)
			},
			Run: func(ctx context.Context, tc *runtime.TurnContext, raw json.RawMessage) (string, error) {
				if tc.Subagent == nil {
					return "", errors.New("subagentes indisponíveis; faça a tarefa você mesmo")
				}
				var a struct {
					Task  string   `json:"task"`
					Files []string `json:"files"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", fmt.Errorf("argumentos inválidos: %v", err)
				}
				return tc.Subagent(ctx, a.Task, a.Files)
			},
		})
	}
	return it
}

// Tools exposes the registry (for MCP exposure and tests).
func (it *Interpreter) Tools() *runtime.Registry { return it.tools }

// TurnResult is what a turn produced.
type TurnResult struct {
	Messages   []llm.Message // full transcript after the turn (system excluded)
	Text       string        // visible assistant text (sanitized)
	Ask        *runtime.AskRequest
	SkillEnded bool
	EndSummary string
	Usage      llm.Usage
	Calls      int
	Activities []events.ActivityInfo
	Stopped    string // "", "max_iterations", "timeout"
	Compacted  bool
}

// UsageHook is called after every provider call (for accounting).
type UsageHook func(purpose, model string, u llm.Usage)

// RunTurn executes the loop until the model stops, asks the user, or limits hit.
func (it *Interpreter) RunTurn(ctx context.Context, transcript []llm.Message, prompt PromptInput, tc *runtime.TurnContext, emit events.Emitter, onUsage UsageHook) (*TurnResult, error) {
	ctx, cancel := context.WithTimeout(ctx, it.cfg.TurnTimeout)
	defer cancel()
	if emit == nil {
		emit = events.Discard
	}
	res := &TurnResult{}
	tc.ResetTurnFlags()
	if it.cfg.SubagentsEnabled {
		tc.Subagent = func(sctx context.Context, task string, files []string) (string, error) {
			return it.runSubagent(sctx, task, files, prompt, tc, res, onUsage)
		}
	}

	msgs := append([]llm.Message{}, transcript...)
	if compacted, ok, err := it.maybeCompact(ctx, msgs, tc, res, onUsage); err == nil && ok {
		msgs = compacted
		res.Compacted = true
		emit(events.Event{Type: events.Notice, Text: "A conversa ficou longa: resumimos as partes antigas para continuar com qualidade."})
	}

	system := llm.Message{Role: llm.RoleSystem, Content: SystemPrompt(prompt)}
	var visible []string
	var actSeq int64
	for i := 0; ; i++ {
		if i >= it.cfg.MaxIterations {
			res.Stopped = "max_iterations"
			break
		}
		san := newStreamSanitizer(func(d string) { emit(events.Event{Type: events.Text, Text: d}) })
		resp, err := it.cfg.Provider.Chat(ctx, llm.Request{
			Model: it.cfg.Model, Messages: append([]llm.Message{system}, msgs...),
			Tools: it.tools.Defs(0), Temperature: it.cfg.Temperature, Purpose: "interpreter",
		}, san.Write)
		text := san.Flush()
		if err != nil {
			res.Messages = msgs
			res.Text = strings.TrimSpace(strings.Join(visible, "\n\n"))
			if ctx.Err() == context.DeadlineExceeded {
				res.Stopped = "timeout"
				return res, nil
			}
			return res, err
		}
		res.Usage.Add(resp.Usage)
		if onUsage != nil {
			onUsage("interpreter", resp.Model, resp.Usage)
		}
		msgs = append(msgs, resp.Message)
		if strings.TrimSpace(text) != "" {
			visible = append(visible, strings.TrimSpace(text))
			emit(events.Event{Type: events.Text, Text: "\n\n"})
		}
		if len(resp.Message.ToolCalls) == 0 {
			break
		}
		results := it.execTools(ctx, tc, resp.Message.ToolCalls, emit, &actSeq, res)
		msgs = append(msgs, results...)
		if tc.Ask() != nil {
			break
		}
		if ended, _ := tc.SkillEnded(); ended {
			// one more call lets the model write its closing message
			if i+1 < it.cfg.MaxIterations {
				san := newStreamSanitizer(func(d string) { emit(events.Event{Type: events.Text, Text: d}) })
				resp, err := it.cfg.Provider.Chat(ctx, llm.Request{Model: it.cfg.Model, Messages: append([]llm.Message{system}, msgs...), Temperature: it.cfg.Temperature, Purpose: "interpreter"}, san.Write)
				if err == nil {
					res.Usage.Add(resp.Usage)
					if onUsage != nil {
						onUsage("interpreter", resp.Model, resp.Usage)
					}
					resp.Message.ToolCalls = nil
					msgs = append(msgs, resp.Message)
					if t := strings.TrimSpace(san.Flush()); t != "" {
						visible = append(visible, t)
					}
				}
			}
			break
		}
	}
	res.Messages = msgs
	res.Text = strings.TrimSpace(strings.Join(visible, "\n\n"))
	res.Ask = tc.Ask()
	res.SkillEnded, res.EndSummary = tc.SkillEnded()
	if res.Ask != nil && res.Ask.Question != "" && !strings.Contains(res.Text, strings.TrimSpace(res.Ask.Question)) {
		q := Sanitize(res.Ask.Question)
		res.Text = strings.TrimSpace(res.Text + "\n\n" + q)
		emit(events.Event{Type: events.Text, Text: q})
	}
	if res.Ask != nil && len(res.Ask.Options) > 0 {
		emit(events.Event{Type: events.Options, Options: res.Ask.Options})
	}
	return res, nil
}

func (it *Interpreter) execTools(ctx context.Context, tc *runtime.TurnContext, calls []llm.ToolCall, emit events.Emitter, seq *int64, res *TurnResult) []llm.Message {
	out := make([]llm.Message, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, it.cfg.SubagentConcurrency)
	var mu sync.Mutex
	runOne := func(i int, call llm.ToolCall) {
		tool, ok := it.tools.Get(call.Name)
		result := ""
		status := "done"
		var act *events.ActivityInfo
		if !ok || (tc.Depth > 0 && tool.MainOnly) {
			result = fmt.Sprintf("ERRO: ferramenta desconhecida %q. Ferramentas disponíveis: %s", call.Name, strings.Join(it.tools.Names(), ", "))
		} else {
			if tool.Label != nil {
				if label := tool.Label(tc, runtime.ArgsMap(json.RawMessage(call.Arguments))); label != "" {
					act = &events.ActivityInfo{ID: fmt.Sprintf("a%d", atomic.AddInt64(seq, 1)), Label: label, Tool: call.Name, Status: "running"}
					emit(events.Event{Type: events.Activity, Activity: act})
				}
			}
			timeout := it.cfg.ToolTimeout
			if call.Name == "spawn_subagent" {
				timeout *= 3
			}
			tctx, cancel := context.WithTimeout(ctx, timeout)
			r, err := safeRun(tctx, tool, tc, json.RawMessage(call.Arguments))
			cancel()
			if err != nil {
				result = "ERRO: " + err.Error()
				status = "error"
			} else {
				result = r
			}
		}
		result = runtime.Truncate(result, it.cfg.MaxToolResult)
		out[i] = llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: result}
		if act != nil {
			done := *act
			done.Status = status
			emit(events.Event{Type: events.Activity, Activity: &done})
			mu.Lock()
			res.Activities = append(res.Activities, done)
			mu.Unlock()
		}
		mu.Lock()
		res.Calls++
		mu.Unlock()
	}
	for i := 0; i < len(calls); {
		tool, ok := it.tools.Get(calls[i].Name)
		if !ok || !tool.Parallel {
			runOne(i, calls[i])
			i++
			continue
		}
		// run a batch of consecutive parallel-safe calls concurrently
		j := i
		for j < len(calls) {
			t, ok := it.tools.Get(calls[j].Name)
			if !ok || !t.Parallel {
				break
			}
			j++
		}
		for k := i; k < j; k++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(k int) {
				defer wg.Done()
				defer func() { <-sem }()
				runOne(k, calls[k])
			}(k)
		}
		wg.Wait()
		i = j
	}
	return out
}

func safeRun(ctx context.Context, tool *runtime.Tool, tc *runtime.TurnContext, raw json.RawMessage) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("falha interna na ferramenta: %v", r)
		}
	}()
	return tool.Run(ctx, tc, raw)
}

func (it *Interpreter) runSubagent(ctx context.Context, task string, files []string, prompt PromptInput, parent *runtime.TurnContext, res *TurnResult, onUsage UsageHook) (string, error) {
	child := &runtime.TurnContext{
		FS: parent.FS, Bundle: parent.Bundle, Catalog: parent.Catalog, Now: parent.Now,
		Python: parent.Python, Web: parent.Web, Depth: 1, Emit: events.Discard,
	}
	child.SetActive(parent.ActiveSkill(), parent.ActiveAgent())
	var sb strings.Builder
	sb.WriteString(task)
	budget := 80 * 1024
	for _, f := range files {
		data, err := parent.FS.Read(f)
		if err != nil {
			fmt.Fprintf(&sb, "\n\n[arquivo %s indisponível: %v]", f, err)
			continue
		}
		content := string(data)
		if len(content) > budget {
			content = runtime.Truncate(content, budget)
		}
		budget -= len(content)
		fmt.Fprintf(&sb, "\n\n<arquivo caminho=%q>\n%s\n</arquivo>", f, content)
		if budget <= 0 {
			break
		}
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: SubagentPrompt(prompt)},
		{Role: llm.RoleUser, Content: sb.String()},
	}
	var last string
	var seq int64
	for i := 0; i < it.cfg.MaxSubIterations; i++ {
		resp, err := it.cfg.Provider.Chat(ctx, llm.Request{Model: it.cfg.SubagentModel, Messages: msgs, Tools: it.tools.Defs(1), Temperature: it.cfg.Temperature, Purpose: "subagent"}, nil)
		if err != nil {
			return "", err
		}
		res.Usage.Add(resp.Usage)
		if onUsage != nil {
			onUsage("subagent", resp.Model, resp.Usage)
		}
		msgs = append(msgs, resp.Message)
		if strings.TrimSpace(resp.Message.Content) != "" {
			last = resp.Message.Content
		}
		if len(resp.Message.ToolCalls) == 0 {
			break
		}
		sub := &TurnResult{}
		msgs = append(msgs, it.execTools(ctx, child, resp.Message.ToolCalls, events.Discard, &seq, sub)...)
	}
	if strings.TrimSpace(last) == "" {
		last = "(o subagente terminou sem resumo)"
	}
	return last, nil
}

// --- compaction -----------------------------------------------------------------

const summaryInstruction = `Resuma a conversa abaixo para que outro assistente continue exatamente de onde ela parou. Preserve:
- decisões, respostas e preferências da pessoa (com as palavras dela quando importarem);
- a etapa/skill do BMad em andamento, o passo atual e o que falta;
- documentos criados/alterados (caminhos) e o estado de cada um;
- perguntas em aberto.
Seja denso e factual, em português, em até 600 palavras.`

func (it *Interpreter) maybeCompact(ctx context.Context, msgs []llm.Message, tc *runtime.TurnContext, res *TurnResult, onUsage UsageHook) ([]llm.Message, bool, error) {
	if llm.EstimateTokens(msgs) <= it.cfg.ContextBudget || len(msgs) <= it.cfg.KeepRecent+2 {
		return msgs, false, nil
	}
	cut := len(msgs) - it.cfg.KeepRecent
	for cut < len(msgs) && msgs[cut].Role != llm.RoleUser {
		cut++
	}
	if cut >= len(msgs)-1 || cut < 2 {
		return msgs, false, nil
	}
	var sb strings.Builder
	for _, m := range msgs[:cut] {
		content := m.Content
		if m.Role == llm.RoleTool && len(content) > 1500 {
			content = content[:1500] + " […]"
		}
		switch m.Role {
		case llm.RoleTool:
			fmt.Fprintf(&sb, "[resultado de %s]: %s\n", m.Name, content)
		case llm.RoleAssistant:
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if len(args) > 800 {
					args = args[:800] + "…"
				}
				fmt.Fprintf(&sb, "[assistente chamou %s %s]\n", tc.Name, args)
			}
			if content != "" {
				fmt.Fprintf(&sb, "ASSISTENTE: %s\n", content)
			}
		default:
			fmt.Fprintf(&sb, "%s: %s\n", strings.ToUpper(m.Role), content)
		}
	}
	resp, err := it.cfg.Provider.Chat(ctx, llm.Request{Model: it.cfg.SummaryModel, Purpose: "summary", Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: summaryInstruction},
		{Role: llm.RoleUser, Content: runtime.Truncate(sb.String(), 200*1024)},
	}}, nil)
	if err != nil {
		return msgs, false, err
	}
	res.Usage.Add(resp.Usage)
	if onUsage != nil {
		onUsage("summary", resp.Model, resp.Usage)
	}
	out := []llm.Message{
		{Role: llm.RoleUser, Content: "[Resumo automático da conversa até aqui]\n" + resp.Message.Content},
		{Role: llm.RoleAssistant, Content: "Entendido. Vou continuar a partir desse resumo, seguindo a skill ativa."},
	}
	// Re-inject the active skill instructions so layer 1 is never lost.
	for _, name := range []string{tc.ActiveAgent(), tc.ActiveSkill()} {
		if name == "" {
			continue
		}
		if act, err := runtime.SkillActivation(tc, name, ""); err == nil {
			id := "reinject_" + name
			out = append(out,
				llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "invoke_skill", Arguments: fmt.Sprintf(`{"skill":%q}`, name)}}},
				llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: "invoke_skill", Content: act},
			)
		}
	}
	return append(out, msgs[cut:]...), true, nil
}

// ActivationMessages builds the synthetic invoke_skill call/result the shell
// injects when it starts a skill deterministically (no LLM round-trip).
func ActivationMessages(tc *runtime.TurnContext, skill, args string) ([]llm.Message, error) {
	act, err := runtime.SkillActivation(tc, skill, args)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("start_%s_%d", skill, time.Now().UnixNano())
	argJSON, _ := json.Marshal(map[string]string{"skill": skill})
	tc.ApplySkill(skill)
	return []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "invoke_skill", Arguments: string(argJSON)}}},
		{Role: llm.RoleTool, ToolCallID: id, Name: "invoke_skill", Content: act},
	}, nil
}
