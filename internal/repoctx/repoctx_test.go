package repoctx

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

func TestParseName(t *testing.T) {
	if s, ok := ParseName("acme", "acme-pag-app-pix"); !ok || s.Sigla != "PAG" || s.Kind != "app" || s.Short != "pix" {
		t.Fatalf("%+v", s)
	}
	for _, bad := range []string{"other-pag-app-pix", "acme-pagamentos-app-x", "acme-pag-lib-x", "acme-pag-app-"} {
		if _, ok := ParseName("acme", bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestProviderDocs(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pub := &workspace.Local{Root: t.TempDir()}
	p := &Provider{Source: &LocalSource{Dir: filepath.Join("..", "..", "demo", "repos"), Org: "acme"}, Publisher: pub, Store: st, LLM: &llm.Demo{}}

	sys, _ := p.Systems(ctx, "PAG")
	if len(sys) != 2 {
		t.Fatalf("PAG systems = %+v", sys)
	}
	// bmad-context.md is used as is, synchronously
	be, names, err := p.Docs(ctx, &store.Project{Sigla: "ONB"})
	if err != nil || len(names) != 1 {
		t.Fatalf("ONB docs: %v %v", err, names)
	}
	data, _ := be.Read("systems/acme-onb-app-cadastro.md")
	if !strings.Contains(string(data), "prova de vida") {
		t.Fatalf("bmad-context not used: %s", data)
	}
	// PAG has no summary yet: generated in background, saved centrally
	_, names, _ = p.Docs(ctx, &store.Project{Sigla: "PAG", Systems: []string{"acme-pag-app-pix"}})
	if len(names) != 0 {
		t.Fatalf("summary should be pending, got %v", names)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := pub.ReadShared(ctx, "_repos/acme-pag-app-pix.md"); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	be, names, _ = p.Docs(ctx, &store.Project{Sigla: "PAG", Systems: []string{"acme-pag-app-pix"}})
	if len(names) != 1 {
		t.Fatalf("summary not available after generation: %v", names)
	}
	data, _ = be.Read("systems/acme-pag-app-pix.md")
	if !strings.Contains(string(data), "source_sha:") || !strings.Contains(string(data), "generated_at:") {
		t.Fatalf("frontmatter missing: %s", data)
	}
	if idx, _ := be.Read("systems/index.md"); !strings.Contains(string(idx), "acme-pag-app-pix") {
		t.Fatal("index missing")
	}
}
