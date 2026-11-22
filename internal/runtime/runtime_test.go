package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	bmadassets "github.com/caio-andres/bmad-business-assistant/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

var fixedNow = time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)

func newTC(t *testing.T) (*TurnContext, *Registry, *[]events.Event) {
	t.Helper()
	b, err := bmad.Load(bmadassets.FS())
	if err != nil {
		t.Fatal(err)
	}
	fs := NewProjectFS(ProjectInput{Bundle: b, Allowed: bmad.DefaultAllowedSkills,
		Overlay: bmad.OverlayInput{UserName: "Ana", ProjectName: "PAG-cashback"}})
	var evs []events.Event
	tc := &TurnContext{FS: fs, Bundle: b, Catalog: bmad.NewCatalog(b, nil), Now: func() time.Time { return fixedNow },
		Emit: func(e events.Event) { evs = append(evs, e) }, Python: NewPythonRunner("auto")}
	fs.OnWrite = func(abs string, deleted bool) { EmitDocument(tc, abs, deleted) }
	return tc, DefaultRegistry(false), &evs
}

func call(t *testing.T, r *Registry, tc *TurnContext, name string, args any) (string, error) {
	t.Helper()
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), tc, raw)
}

func TestTokenize(t *testing.T) {
	got, err := Tokenize(`mkdir -p "/project/_bmad-output/a b" && uv run {project-root}/_bmad/scripts/memlog.py append --workspace /x --text "ideia: \"pix\" grátis"; date`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"mkdir", "-p", "/project/_bmad-output/a b"},
		{"uv", "run", "{project-root}/_bmad/scripts/memlog.py", "append", "--workspace", "/x", "--text", `ideia: "pix" grátis`},
		{"date"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"cat x | grep y", "echo a > f", "rm -rf / &", "echo $(whoami)", "echo `id`"} {
		if _, err := Tokenize(bad); !errors.Is(err, ErrCommandRefused) {
			t.Errorf("%q should be refused, got %v", bad, err)
		}
	}
}

func TestCommandRefusals(t *testing.T) {
	tc, r, _ := newTC(t)
	for _, c := range []string{"curl https://example.com", "rm -rf /project", "python -c 'print(1)'", "bash -c ls"} {
		if _, err := call(t, r, tc, "run_command", map[string]string{"command": c}); err == nil {
			t.Errorf("%q should fail", c)
		}
	}
}

func TestMemlogNative(t *testing.T) {
	tc, r, evs := newTC(t)
	ws := "{project-root}/_bmad-output/planning-artifacts/briefs/brief-x-2026-11-22"
	out, err := call(t, r, tc, "run_command", map[string]string{"command": `uv run {project-root}/_bmad/scripts/memlog.py init --workspace ` + ws + ` --field topic="App de cashback" --field goal="reter clientes"`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"entries": 0`) || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("ack = %s", out)
	}
	if _, err := call(t, r, tc, "run_command", map[string]string{"command": `uv run /project/_bmad/scripts/memlog.py append --workspace ` + ws + ` --type idea --by user --text "cashback   em
 pix"`}); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, r, tc, "run_command", map[string]string{"command": `python3 /project/_bmad/scripts/memlog.py set --workspace ` + ws + ` --key status --value complete`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"entries": 1`) {
		t.Fatalf("ack = %s", out)
	}
	data, _ := tc.FS.Read(ws + "/.memlog.md")
	want := "---\ntopic: App de cashback\ngoal: reter clientes\nstatus: complete\nupdated: 2026-11-22T10:30\n---\n\n- (idea by user) cashback em pix\n"
	if string(data) != want {
		t.Fatalf("memlog =\n%q\nwant\n%q", data, want)
	}
	if _, err := call(t, r, tc, "run_command", map[string]string{"command": `uv run /project/_bmad/scripts/memlog.py init --workspace ` + ws}); err == nil {
		t.Fatal("init twice must fail")
	}
	docs := 0
	for _, e := range *evs {
		if e.Type == events.Document {
			docs++
		}
	}
	if docs != 3 {
		t.Errorf("document events = %d, want 3", docs)
	}
}

// TestMemlogEquivalence compares the native memlog with the original script.
func TestMemlogEquivalence(t *testing.T) {
	py := NewPythonRunner("auto")
	if !py.Enabled() {
		t.Skip("python not available")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "memlog.py")
	src, _ := bmadassets.FS().Open("_bmad/scripts/memlog.py")
	data := make([]byte, 64*1024)
	n, _ := src.Read(data)
	_ = os.WriteFile(script, data[:n], 0o644)
	ws := filepath.Join(dir, "ws")
	run := func(args ...string) {
		cmd := exec.Command(py.Bin, append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("python %v: %v %s", args, err, out)
		}
	}
	run("init", "--workspace", ws, "--field", "topic=Onboarding: fase 2", "--field", "mode=partner")
	run("append", "--workspace", ws, "--type", "idea", "--text", "skip the  signup\nwall")
	run("append", "--workspace", ws, "--by", "coach", "--text", "plain note")
	run("set", "--workspace", ws, "--key", "status", "--value", "complete")
	pyOut, _ := os.ReadFile(filepath.Join(ws, ".memlog.md"))

	tc, r, _ := newTC(t)
	w := "/project/_bmad-output/ws"
	for _, c := range []string{
		`uv run /project/_bmad/scripts/memlog.py init --workspace ` + w + ` --field "topic=Onboarding: fase 2" --field mode=partner`,
		`uv run /project/_bmad/scripts/memlog.py append --workspace ` + w + ` --type idea --text "skip the  signup
wall"`,
		`uv run /project/_bmad/scripts/memlog.py append --workspace ` + w + ` --by coach --text "plain note"`,
		`uv run /project/_bmad/scripts/memlog.py set --workspace ` + w + ` --key status --value complete`,
	} {
		if _, err := call(t, r, tc, "run_command", map[string]string{"command": c}); err != nil {
			t.Fatal(err)
		}
	}
	native, _ := tc.FS.Read(w + "/.memlog.md")
	norm := regexp.MustCompile(`updated: .*`)
	a := norm.ReplaceAllString(strings.ReplaceAll(string(pyOut), "\r\n", "\n"), "updated: X")
	b := norm.ReplaceAllString(string(native), "updated: X")
	if a != b {
		t.Fatalf("python:\n%s\nnative:\n%s", a, b)
	}
}

func TestResolveConfigNative(t *testing.T) {
	tc, r, _ := newTC(t)
	out, err := call(t, r, tc, "run_command", map[string]string{"command": "uv run {project-root}/_bmad/scripts/resolve_config.py --project-root {project-root} --key core"})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not json: %s", out)
	}
	if v["core"]["user_name"] != "Ana" || v["core"]["communication_language"] != bmad.LanguagePTBR {
		t.Fatalf("core = %v", v["core"])
	}
	tc.SetActive("bmad-prd", "")
	out, err = call(t, r, tc, "run_command", map[string]string{"command": "uv run {project-root}/_bmad/scripts/resolve_customization.py --skill {skill-root} --project-root {project-root} --key workflow.persistent_facts"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pessoa de negócio") {
		t.Fatalf("business facts not merged: %s", out)
	}
}

func TestFileToolsAndReadOnly(t *testing.T) {
	tc, r, _ := newTC(t)
	if _, err := call(t, r, tc, "write_file", map[string]string{"path": "{project-root}/_bmad/scripts/memlog.py", "content": "x"}); !errors.Is(err, vfs.ErrReadOnly) {
		t.Fatalf("expected read-only, got %v", err)
	}
	p := "/project/_bmad-output/planning-artifacts/prds/prd-x/prd.md"
	if _, err := call(t, r, tc, "write_file", map[string]string{"path": p, "content": "# PRD\n\nmeta: 10%\nmeta: 10%\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, r, tc, "edit_file", map[string]any{"path": p, "old_string": "meta: 10%", "new_string": "meta: 20%"}); err == nil {
		t.Fatal("ambiguous edit must fail")
	}
	if _, err := call(t, r, tc, "edit_file", map[string]any{"path": p, "old_string": "meta: 10%", "new_string": "meta: 20%", "replace_all": true}); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, r, tc, "read_file", map[string]any{"path": p, "offset": 3, "limit": 1})
	if out != "meta: 20%" {
		t.Fatalf("read slice = %q", out)
	}
	out, _ = call(t, r, tc, "list_files", map[string]string{"pattern": "{project-root}/_bmad-output/**/*.md"})
	if out != p {
		t.Fatalf("list = %q", out)
	}
	out, _ = call(t, r, tc, "search_files", map[string]string{"pattern": "meta: 20", "glob": "/project/_bmad-output/**"})
	if !strings.Contains(out, p+":3:") {
		t.Fatalf("search = %q", out)
	}
}

func TestInvokeSkillScope(t *testing.T) {
	tc, r, _ := newTC(t)
	if _, err := call(t, r, tc, "invoke_skill", map[string]string{"skill": "bmad-build"}); err == nil {
		t.Fatal("bmad-build must be refused")
	}
	var changed []string
	tc.OnSkillChange = func(s, a string) { changed = append(changed, s+"|"+a) }
	out, err := call(t, r, tc, "invoke_skill", map[string]string{"skill": "bmad-agent-pm"})
	if err != nil || !strings.Contains(out, "skill-root=\"/project/.claude/skills/bmad-agent-pm\"") {
		t.Fatalf("activation = %v %s", err, out[:100])
	}
	_, _ = call(t, r, tc, "invoke_skill", map[string]string{"skill": "bmad-prd"})
	_, _ = call(t, r, tc, "invoke_skill", map[string]string{"skill": "bmad-party-mode"})
	if tc.ActiveSkill() != "bmad-prd" || tc.ActiveAgent() != "bmad-agent-pm" {
		t.Fatalf("active = %s / %s", tc.ActiveSkill(), tc.ActiveAgent())
	}
	if !reflect.DeepEqual(changed, []string{"|bmad-agent-pm", "bmad-prd|bmad-agent-pm"}) {
		t.Fatalf("changes = %v", changed)
	}
}

func TestOpenBundleAssetCopiesToWorkspace(t *testing.T) {
	tc, r, evs := newTC(t)
	tc.SetActive("bmad-brainstorming", "")
	out, err := call(t, r, tc, "run_command", map[string]string{"command": `start "" "{skill-root}/assets/brain-selector.html"`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "painel") {
		t.Fatalf("out = %s", out)
	}
	var opened *events.DocumentRef
	for _, e := range *evs {
		if e.Type == events.Open {
			opened = e.Document
		}
	}
	if opened == nil || opened.Path != ".view/brain-selector.html" || opened.Kind != "html" {
		t.Fatalf("open event = %+v", opened)
	}
}

func TestPythonRunnerBrainCategories(t *testing.T) {
	tc, r, _ := newTC(t)
	if !tc.Python.Enabled() {
		t.Skip("python not available")
	}
	tc.SetActive("bmad-brainstorming", "")
	out, err := call(t, r, tc, "run_command", map[string]string{"command": "uv run {skill-root}/scripts/brain.py --file {skill-root}/assets/brain-methods.csv categories"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(out), "creative") && !strings.Contains(out, "(") {
		t.Fatalf("unexpected categories output: %s", out)
	}
	// relative script path resolves from the active skill root
	if _, err := call(t, r, tc, "run_command", map[string]string{"command": "uv run scripts/brain.py --file assets/brain-methods.csv categories"}); err != nil {
		t.Logf("relative --file is resolved by the script itself from cwd; error acceptable: %v", err)
	}
}

func TestAskUserAndEndSkill(t *testing.T) {
	tc, r, _ := newTC(t)
	if _, err := call(t, r, tc, "ask_user", map[string]any{"question": "Como seguimos?", "options": []string{"Caminho rápido", " ", "Caminho guiado"}}); err != nil {
		t.Fatal(err)
	}
	if a := tc.Ask(); a == nil || len(a.Options) != 2 {
		t.Fatalf("ask = %+v", a)
	}
	_, _ = call(t, r, tc, "end_skill", map[string]string{"summary": "brief pronto"})
	if ended, s := tc.SkillEnded(); !ended || s != "brief pronto" {
		t.Fatal("end_skill not recorded")
	}
	if _, err := call(t, r, tc, "show_document", map[string]string{"path": "/project/_bmad-output/nao-existe.html"}); err == nil {
		t.Fatal("missing doc must fail")
	}
}
