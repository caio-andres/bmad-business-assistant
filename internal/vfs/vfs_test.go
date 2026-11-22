package vfs

import (
	"errors"
	"reflect"
	"testing"
	"testing/fstest"
)

func testFS() *FS {
	bundle := fstest.MapFS{
		"_bmad/scripts/memlog.py":                 {Data: []byte("py")},
		"_bmad/config.toml":                       {Data: []byte("[core]")},
		".claude/skills/bmad-prd/SKILL.md":        {Data: []byte("prd")},
		".claude/skills/bmad-prd/assets/prd-t.md": {Data: []byte("tpl")},
		".claude/skills/bmad-ux/references/x.md":  {Data: []byte("x")},
	}
	overlay := NewMap(map[string][]byte{"config.user.toml": []byte("[core]\nuser_name='a'")})
	return New(
		Mount{Prefix: "_bmad", Backend: &Layered{Layers: []Backend{overlay, &FSBackend{FS: bundle, Base: "_bmad"}}}, ReadOnly: true},
		Mount{Prefix: ".claude/skills", Backend: &FSBackend{FS: bundle, Base: ".claude/skills"}, ReadOnly: true},
		Mount{Prefix: "_bmad-output", Backend: NewMap(nil)},
		Mount{Prefix: "docs", Backend: NewMap(map[string][]byte{"systems/acme-pag-app-pix.md": []byte("pix")}), ReadOnly: true},
	)
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"{project-root}/_bmad/config.toml": "/project/_bmad/config.toml",
		"_bmad-output/a.md":                "/project/_bmad-output/a.md",
		`"/project/docs/x.md"`:             "/project/docs/x.md",
		`_bmad-output\win\path.md`:         "/project/_bmad-output/win/path.md",
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/etc/passwd", "/project/../etc", "_bmad-output/../../x", ""} {
		if _, err := Normalize(bad); !errors.Is(err, ErrOutside) {
			t.Errorf("Normalize(%q) should fail with ErrOutside, got %v", bad, err)
		}
	}
}

func TestReadOnlyAndWritable(t *testing.T) {
	f := testFS()
	if _, err := f.Write("/project/_bmad/scripts/memlog.py", []byte("evil")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected read-only error, got %v", err)
	}
	if d, _ := f.Read("/project/_bmad/scripts/memlog.py"); string(d) != "py" {
		t.Fatal("original file changed")
	}
	if _, err := f.Write("/project/somewhere/else.md", []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected refusal outside mounts, got %v", err)
	}
	var written []string
	f.OnWrite = func(abs string, deleted bool) { written = append(written, abs) }
	abs, err := f.Write("{project-root}/_bmad-output/planning-artifacts/briefs/b/brief.md", []byte("# Brief"))
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0] != abs {
		t.Fatalf("OnWrite not called: %v", written)
	}
	if d, _ := f.Read(abs); string(d) != "# Brief" {
		t.Fatal("read back failed")
	}
}

func TestOverlayShadowsBundle(t *testing.T) {
	f := testFS()
	d, err := f.Read("/project/_bmad/config.user.toml")
	if err != nil || string(d) == "" {
		t.Fatalf("overlay not visible: %v", err)
	}
	d, _ = f.Read("/project/_bmad/config.toml")
	if string(d) != "[core]" {
		t.Fatal("bundle file not visible through layered mount")
	}
}

func TestGlobAndListDir(t *testing.T) {
	f := testFS()
	got, err := f.Glob("/project/.claude/skills/**/*.md")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/project/.claude/skills/bmad-prd/SKILL.md",
		"/project/.claude/skills/bmad-prd/assets/prd-t.md",
		"/project/.claude/skills/bmad-ux/references/x.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("glob = %v", got)
	}
	got, _ = f.Glob("{project-root}/docs/systems/*.md")
	if len(got) != 1 {
		t.Fatalf("systems glob = %v", got)
	}
	got, _ = f.Glob("/project/.claude/skills/{bmad-ux,bmad-prd}/SKILL.md")
	if len(got) != 1 {
		t.Fatalf("brace glob = %v", got)
	}
	entries, err := f.ListDir("/project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entries, []string{".claude/", "_bmad-output/", "_bmad/", "docs/"}) {
		t.Fatalf("listdir root = %v", entries)
	}
}
