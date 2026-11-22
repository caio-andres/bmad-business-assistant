package bmad

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	bmadassets "github.com/caio-andres/bmad-business-assistant/bmad"
)

func loadBundle(t *testing.T) *Bundle {
	t.Helper()
	b, err := Load(bmadassets.FS())
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return b
}

func TestBundleVersionAndSkills(t *testing.T) {
	b := loadBundle(t)
	if b.Version != "6.12.4" {
		t.Fatalf("version = %q, want 6.12.4", b.Version)
	}
	for _, s := range []string{"bmad-prd", "bmad-product-brief", "bmad-agent-pm", "bmad-brainstorming"} {
		if _, ok := b.Skills[s]; !ok {
			t.Errorf("skill %s not loaded", s)
		}
	}
	if !b.Skills["bmad-agent-pm"].IsAgent || b.Skills["bmad-prd"].IsAgent {
		t.Error("agent detection wrong")
	}
	if b.Agents["bmad-agent-analyst"].Name != "Mary" {
		t.Errorf("analyst name = %q", b.Agents["bmad-agent-analyst"].Name)
	}
	if len(b.Help) == 0 {
		t.Error("help catalog empty")
	}
}

func TestCatalogScope(t *testing.T) {
	c := NewCatalog(loadBundle(t), nil)
	if c.Allowed("bmad-build") || c.Allowed("bmad-architecture") {
		t.Fatal("implementation skills must not be allowed by default")
	}
	if !c.Allowed("bmad-prd") {
		t.Fatal("prd must be allowed")
	}
	names := map[string]bool{}
	for _, a := range c.Agents {
		names[a.Name] = true
	}
	for _, n := range []string{"Mary", "John", "Sally"} {
		if !names[n] {
			t.Errorf("agent %s missing", n)
		}
	}
	if _, ok := c.Agent("bmad-agent-dev"); ok {
		t.Error("dev agent must not be listed")
	}
}

func TestStructuralMergeRules(t *testing.T) {
	base := map[string]any{
		"agent": map[string]any{
			"icon":       "a",
			"principles": []any{"p1"},
			"menu":       []any{map[string]any{"code": "BP", "description": "x"}, map[string]any{"code": "MR", "description": "y"}},
		},
	}
	override := map[string]any{
		"agent": map[string]any{
			"icon":       "b",
			"principles": []any{"p2"},
			"menu":       []any{map[string]any{"code": "MR", "description": "changed"}, map[string]any{"code": "NEW", "description": "z"}},
		},
	}
	got, err := MergeLayers(base, override)
	if err != nil {
		t.Fatal(err)
	}
	agent := got["agent"].(map[string]any)
	if agent["icon"] != "b" {
		t.Errorf("scalar override failed: %v", agent["icon"])
	}
	if !reflect.DeepEqual(agent["principles"], []any{"p1", "p2"}) {
		t.Errorf("array append failed: %v", agent["principles"])
	}
	menu := agent["menu"].([]any)
	if len(menu) != 3 || menu[1].(map[string]any)["description"] != "changed" || menu[2].(map[string]any)["code"] != "NEW" {
		t.Errorf("keyed merge failed: %v", menu)
	}
}

func TestKeyedMergeRejectsNonStringID(t *testing.T) {
	_, err := MergeLayers(map[string]any{"a": []any{map[string]any{"id": 1}}}, map[string]any{"a": []any{map[string]any{"id": 2}}})
	if err == nil {
		t.Fatal("expected error for non-string id")
	}
}

func TestOverlayConfigIsPTBR(t *testing.T) {
	b := loadBundle(t)
	files := fstest.MapFS{}
	// copy the central config files from the bundle
	for _, p := range []string{"_bmad/config.toml", "_bmad/custom/config.toml"} {
		data, err := readBundle(b, p)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = &fstest.MapFile{Data: data}
	}
	files["_bmad/config.user.toml"] = &fstest.MapFile{Data: []byte(UserConfigTOML(OverlayInput{UserName: "Ana Souza", ProjectName: "PAG-cashback"}))}
	cfg, err := ResolveCentralConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	core := cfg["core"].(map[string]any)
	if core["communication_language"] != LanguagePTBR || core["document_output_language"] != LanguagePTBR {
		t.Errorf("language not PT-BR: %v", core)
	}
	if core["user_name"] != "Ana Souza" || core["project_name"] != "PAG-cashback" {
		t.Errorf("user/project not applied: %v", core)
	}
}

func TestCustomOverridesParse(t *testing.T) {
	b := loadBundle(t)
	files := CustomOverrides(b, DefaultAllowedSkills, OverlayInput{HasSystems: true})
	brief, ok := files["custom/bmad-product-brief.toml"]
	if !ok {
		t.Fatal("brief override missing")
	}
	parsed, err := ParseTOML([]byte(brief), "brief")
	if err != nil {
		t.Fatal(err)
	}
	facts := parsed["workflow"].(map[string]any)["persistent_facts"].([]any)
	if len(facts) != len(BusinessFacts)+1 {
		t.Errorf("facts = %d", len(facts))
	}
	agent := files["custom/bmad-agent-pm.toml"]
	if !strings.Contains(agent, "[agent]") {
		t.Error("agent override must use [agent] block")
	}
}

func readBundle(b *Bundle, p string) ([]byte, error) {
	f, err := b.FS.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, _ := f.Stat()
	buf := make([]byte, st.Size())
	_, err = f.Read(buf)
	return buf, err
}

// TestEquivalenceWithPythonScripts runs the original BMad scripts (when Python
// 3.11+ is available) against a materialized copy of the bundle and compares
// the JSON with our native implementation.
func TestEquivalenceWithPythonScripts(t *testing.T) {
	py := findPython()
	if py == "" {
		t.Skip("python 3.11+ not available")
	}
	b := loadBundle(t)
	root := t.TempDir()
	if err := materialize(b, root); err != nil {
		t.Fatal(err)
	}
	userCfg := UserConfigTOML(OverlayInput{UserName: "Ana", ProjectName: "XPT-demo"})
	if err := os.WriteFile(filepath.Join(root, "_bmad", "config.user.toml"), []byte(userCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range CustomOverrides(b, []string{"bmad-prd"}, OverlayInput{}) {
		if err := os.WriteFile(filepath.Join(root, "_bmad", filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projectFS := os.DirFS(root)

	// resolve_config
	out := runPy(t, py, root, "_bmad/scripts/resolve_config.py", "--project-root", root)
	native, err := ResolveCentralConfig(projectFS)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "resolve_config", out, native)

	// resolve_customization with keys
	skillDir := filepath.Join(root, ".claude", "skills", "bmad-prd")
	out = runPy(t, py, root, "_bmad/scripts/resolve_customization.py", "--skill", skillDir, "--project-root", root, "--key", "workflow")
	merged, err := ResolveCustomization(projectFS, ".claude/skills/bmad-prd")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "resolve_customization", out, SelectKeys(merged, []string{"workflow"}))

	// agent menu merge
	agentDir := filepath.Join(root, ".claude", "skills", "bmad-agent-analyst")
	out = runPy(t, py, root, "_bmad/scripts/resolve_customization.py", "--skill", agentDir, "--project-root", root, "--key", "agent.menu")
	merged, err = ResolveCustomization(projectFS, ".claude/skills/bmad-agent-analyst")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "agent.menu", out, SelectKeys(merged, []string{"agent.menu"}))
}

func findPython() string {
	for _, c := range []string{"python3", "python"} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		v, err := exec.Command(p, "-c", "import sys,tomllib;print(sys.version_info[1])").Output()
		if err == nil && len(strings.TrimSpace(string(v))) > 0 {
			return p
		}
	}
	return ""
}

func materialize(b *Bundle, root string) error {
	return WalkFiles(b.FS, ".", func(p string, data []byte) error {
		dst := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

func runPy(t *testing.T, py, root, script string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(py, append([]string{filepath.Join(root, filepath.FromSlash(script))}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("%s failed: %v: %s", script, err, ee.Stderr)
		}
		t.Fatalf("%s failed: %v", script, err)
	}
	return out
}

func assertJSONEqual(t *testing.T, label string, pyOut []byte, native any) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(pyOut, &a); err != nil {
		t.Fatalf("%s: python output not JSON: %v", label, err)
	}
	nb, _ := json.Marshal(native)
	_ = json.Unmarshal(nb, &b)
	if !reflect.DeepEqual(a, b) {
		pa, _ := json.MarshalIndent(a, "", " ")
		pb, _ := json.MarshalIndent(b, "", " ")
		t.Fatalf("%s mismatch\npython: %s\nnative: %s", label, pa, pb)
	}
}
