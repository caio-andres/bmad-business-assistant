// Package repoctx anchors business documents in the real product systems:
// it discovers repositories by convention (<org>-<sigla>-<app|infra>-<name>)
// and serves business-oriented summaries to the skills at {project-root}/docs/systems.
package repoctx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

// System is a product repository.
type System struct {
	Name        string `json:"name"` // full repo name
	Sigla       string `json:"sigla"`
	Kind        string `json:"kind"` // app | infra
	Short       string `json:"short"`
	Description string `json:"description,omitempty"`
	Head        string `json:"head,omitempty"` // version marker (commit SHA or content hash)
}

// Source lists repositories and reads their files.
type Source interface {
	Name() string
	List(ctx context.Context, sigla string) ([]System, error)
	File(ctx context.Context, sys System, path string) ([]byte, error)
	Tree(ctx context.Context, sys System) ([]string, error)
}

// ParseName splits <org>-<sigla>-<kind>-<name>.
func ParseName(org, repo string) (System, bool) {
	pre := strings.ToLower(org) + "-"
	if !strings.HasPrefix(strings.ToLower(repo), pre) {
		return System{}, false
	}
	parts := strings.SplitN(strings.TrimPrefix(strings.ToLower(repo), pre), "-", 3)
	if len(parts) != 3 || len(parts[0]) != 3 || (parts[1] != "app" && parts[1] != "infra") || parts[2] == "" {
		return System{}, false
	}
	return System{Name: repo, Sigla: strings.ToUpper(parts[0]), Kind: parts[1], Short: parts[2]}, true
}

// --- local source (demo/repos) ----------------------------------------------------------

// LocalSource reads fictitious repositories from a folder.
type LocalSource struct {
	Dir string
	Org string
}

func (l *LocalSource) Name() string { return "local" }

func (l *LocalSource) List(_ context.Context, sigla string) ([]System, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []System
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, ok := ParseName(l.Org, e.Name())
		if !ok || (sigla != "" && s.Sigla != strings.ToUpper(sigla)) {
			continue
		}
		s.Head = l.hash(e.Name())
		if readme, err := os.ReadFile(filepath.Join(l.Dir, e.Name(), "README.md")); err == nil {
			s.Description = firstParagraph(string(readme))
		}
		out = append(out, s)
	}
	return out, nil
}

func (l *LocalSource) hash(repo string) string {
	h := sha1.New()
	_ = filepath.WalkDir(filepath.Join(l.Dir, repo), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if data, err := os.ReadFile(p); err == nil {
				h.Write([]byte(p))
				h.Write(data)
			}
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (l *LocalSource) File(_ context.Context, sys System, p string) ([]byte, error) {
	c, err := workspace.CleanPath(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(l.Dir, sys.Name, filepath.FromSlash(c)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, workspace.ErrNotFound
	}
	return data, err
}

func (l *LocalSource) Tree(_ context.Context, sys System) ([]string, error) {
	root := filepath.Join(l.Dir, sys.Name)
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func firstParagraph(md string) string {
	for _, para := range strings.Split(md, "\n\n") {
		p := strings.TrimSpace(para)
		if p != "" && !strings.HasPrefix(p, "#") && !strings.HasPrefix(p, "!") && !strings.HasPrefix(p, "[") {
			p = strings.Join(strings.Fields(p), " ")
			if len([]rune(p)) > 220 {
				p = string([]rune(p)[:220]) + "…"
			}
			return p
		}
	}
	return ""
}

// --- provider ---------------------------------------------------------------------------

// Provider implements orchestrator.ContextProvider.
type Provider struct {
	Source    Source
	Publisher workspace.Publisher // where _repos/<repo>.md summaries live (may be nil)
	Store     *store.Store        // cache
	LLM       llm.Provider
	Model     string
	MaxAge    time.Duration
	Logger    *slog.Logger

	mu       sync.Mutex
	inflight map[string]bool
}

// Systems lists systems of a sigla (used by the project screen).
func (p *Provider) Systems(ctx context.Context, sigla string) ([]System, error) {
	if p.Source == nil {
		return nil, nil
	}
	return p.Source.List(ctx, sigla)
}

// Docs returns docs/systems/*.md for the project's systems.
func (p *Provider) Docs(ctx context.Context, proj *store.Project) (vfs.Backend, []string, error) {
	all, err := p.Systems(ctx, proj.Sigla)
	if err != nil {
		return nil, nil, err
	}
	want := map[string]bool{}
	for _, s := range proj.Systems {
		want[strings.ToLower(s)] = true
	}
	files := map[string][]byte{}
	var names []string
	var index strings.Builder
	index.WriteString("# Sistemas de produto da sigla " + proj.Sigla + "\n\n")
	for _, s := range all {
		if len(want) > 0 && !want[strings.ToLower(s.Name)] {
			continue
		}
		sum, ok := p.summary(ctx, s)
		fmt.Fprintf(&index, "- **%s** (%s): %s\n", s.Name, kindLabel(s.Kind), s.Description)
		if !ok {
			fmt.Fprintf(&index, "  - resumo em preparação\n")
			continue
		}
		files["systems/"+s.Name+".md"] = []byte(sum)
		names = append(names, s.Name)
	}
	if len(names) == 0 && len(files) == 0 {
		return vfs.NewMap(nil), nil, nil
	}
	files["systems/index.md"] = []byte(index.String())
	return vfs.NewMap(files), names, nil
}

func kindLabel(k string) string {
	if k == "infra" {
		return "infraestrutura"
	}
	return "aplicação"
}

func (p *Provider) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// summary resolves the summary by priority: bmad-context.md in the repo,
// then the central _repos/<repo>.md (cached in the DB), else lazy generation.
func (p *Provider) summary(ctx context.Context, s System) (string, bool) {
	if data, err := p.Source.File(ctx, s, "bmad-context.md"); err == nil && len(data) > 0 {
		return string(data), true
	}
	var cached string
	var cachedRef string
	var cachedAt time.Time
	if p.Store != nil {
		if r, err := p.Store.GetRepoSummary(ctx, s.Name); err == nil {
			cached, cachedRef, cachedAt = r.Summary, r.SourceRef, r.GeneratedAt
		}
	}
	if cached == "" && p.Publisher != nil {
		if data, err := p.Publisher.ReadShared(ctx, "_repos/"+s.Name+".md"); err == nil {
			cached = string(data)
			fm, _ := bmad.Frontmatter(data)
			if v, ok := fm["source_sha"].(string); ok {
				cachedRef = v
			}
			if v, ok := fm["generated_at"].(string); ok {
				cachedAt, _ = time.Parse(time.RFC3339, v)
			}
			if p.Store != nil {
				_ = p.Store.PutRepoSummary(ctx, &store.RepoSummary{System: s.Name, Summary: cached, SourceRef: cachedRef})
			}
		}
	}
	maxAge := p.MaxAge
	if maxAge == 0 {
		maxAge = 7 * 24 * time.Hour
	}
	stale := cached != "" && s.Head != "" && cachedRef != s.Head && time.Since(cachedAt) > maxAge
	if cached == "" || stale {
		p.generateAsync(s)
	}
	return cached, cached != ""
}

func (p *Provider) generateAsync(s System) {
	if p.LLM == nil {
		return
	}
	p.mu.Lock()
	if p.inflight == nil {
		p.inflight = map[string]bool{}
	}
	if p.inflight[s.Name] {
		p.mu.Unlock()
		return
	}
	p.inflight[s.Name] = true
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inflight, s.Name)
			p.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, err := p.Generate(ctx, s); err != nil {
			p.log().Warn("repo summary failed", "system", s.Name, "err", err)
		}
	}()
}

var manifests = []string{"package.json", "go.mod", "pom.xml", "build.gradle", "requirements.txt", "pyproject.toml", "Cargo.toml", "openapi.yaml", "openapi.yml", "docker-compose.yml", "serverless.yml", "template.yaml"}

// Generate writes a fresh summary (synchronously).
func (p *Provider) Generate(ctx context.Context, s System) (string, error) {
	var in strings.Builder
	fmt.Fprintf(&in, "Repositório: %s (sigla %s, tipo %s)\n\n", s.Name, s.Sigla, kindLabel(s.Kind))
	if readme, err := p.Source.File(ctx, s, "README.md"); err == nil {
		in.WriteString("## README\n" + truncate(string(readme), 20000) + "\n\n")
	}
	tree, _ := p.Source.Tree(ctx, s)
	if len(tree) > 400 {
		tree = tree[:400]
	}
	in.WriteString("## Arquivos\n" + strings.Join(tree, "\n") + "\n\n")
	for _, m := range manifests {
		if data, err := p.Source.File(ctx, s, m); err == nil {
			in.WriteString("## " + m + "\n" + truncate(string(data), 4000) + "\n\n")
		}
	}
	for _, f := range tree {
		if strings.HasSuffix(f, ".tf") && strings.Count(f, "/") <= 1 {
			if data, err := p.Source.File(ctx, s, f); err == nil {
				in.WriteString("## " + f + "\n" + truncate(string(data), 3000) + "\n\n")
			}
		}
	}
	resp, err := p.LLM.Chat(ctx, llm.Request{Model: p.Model, Purpose: "repo-summary", Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: `Você escreve, para pessoas de negócio, o resumo de um sistema de produto a partir do repositório. Em português do Brasil, sem jargão desnecessário e sem inventar. Use exatamente estas seções em Markdown:
# <nome amigável do sistema>
## O que faz
## Para quem
## Principais fluxos
## Integrações
## Recursos de infraestrutura
## Limitações conhecidas
Quando não houver informação, escreva "Não identificado no repositório".`},
		{Role: llm.RoleUser, Content: in.String()},
	}}, nil)
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(resp.Message.Content)
	doc := fmt.Sprintf("---\nsystem: %s\nkind: %s\nsource_sha: %s\ngenerated_at: %s\n---\n\n%s\n", s.Name, s.Kind, s.Head, time.Now().UTC().Format(time.RFC3339), body)
	if p.Publisher != nil {
		if err := p.Publisher.WriteShared(ctx, "_repos/"+s.Name+".md", []byte(doc), "docs(_repos): resumo de "+s.Name); err != nil {
			p.log().Warn("could not save summary centrally", "system", s.Name, "err", err)
		}
	}
	if p.Store != nil {
		_ = p.Store.PutRepoSummary(ctx, &store.RepoSummary{System: s.Name, Summary: doc, SourceRef: s.Head})
	}
	return doc, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[...]"
}
