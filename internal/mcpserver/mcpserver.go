// Package mcpserver exposes the business documents to developers' coding
// agents (Claude Code, Codex, Devin...) over MCP: projects, conversations
// linked to them and their BMad documents (read-only).
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps are the services the tools read from.
type Deps struct {
	Store     *store.Store
	Catalog   *bmad.Catalog
	Publisher workspace.Publisher
	Version   string
}

type projectArgs struct {
	Sigla string `json:"sigla,omitempty" jsonschema:"filtra pela sigla (3 caracteres), opcional"`
}

type idArgs struct {
	ProjectID string `json:"projectId" jsonschema:"id do projeto"`
}

type docArgs struct {
	ConversationID string `json:"conversationId" jsonschema:"id da conversa"`
	Path           string `json:"path" jsonschema:"caminho do documento relativo a _bmad-output"`
}

type centralArgs struct {
	ProjectID string `json:"projectId" jsonschema:"id do projeto"`
	Path      string `json:"path,omitempty" jsonschema:"documento a ler; vazio lista os documentos"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// New builds the MCP server.
func New(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "alicerce", Version: d.Version}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "list_projects", Description: "Lista os projetos de negócio (sigla/projeto) e o status de revisão."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a projectArgs) (*mcp.CallToolResult, any, error) {
			list, err := d.Store.ListProjects(ctx)
			if err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			for _, p := range list {
				if a.Sigla != "" && !strings.EqualFold(a.Sigla, p.Sigla) {
					continue
				}
				status := "-"
				if d.Publisher != nil {
					if st, err := d.Publisher.Status(ctx, p.Sigla, p.Slug); err == nil {
						status = st.Status
						if st.PRURL != "" {
							status += " " + st.PRURL
						}
					}
				}
				fmt.Fprintf(&b, "- %s | %s/%s | %s | sistemas: %s | revisão: %s\n", p.ID, p.Sigla, p.Slug, p.Name, strings.Join(p.Systems, ", "), status)
			}
			if b.Len() == 0 {
				return text("Nenhum projeto."), nil, nil
			}
			return text(b.String()), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_project_documents", Description: "Lista as conversas de um projeto e os documentos BMad de cada uma (brief, PRD, UX, épicos)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
			convs, err := d.Store.ListProjectConversations(ctx, a.ProjectID)
			if err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			for _, c := range convs {
				fmt.Fprintf(&b, "## %s (%s) — conversa %s, atualizada %s\n", c.Title, c.State, c.ID, c.UpdatedAt.Format("2006-01-02 15:04"))
				files, _ := d.Store.ListFiles(ctx, c.ID)
				for _, f := range files {
					if strings.HasPrefix(f.Path, ".view/") || strings.HasPrefix(f.Path, ".conflicts/") {
						continue
					}
					fmt.Fprintf(&b, "- %s (%s, %d bytes)\n", f.Path, runtime.FriendlyName(f.Path), f.Size)
				}
			}
			if b.Len() == 0 {
				return text("Nenhuma conversa vinculada a este projeto."), nil, nil
			}
			return text(b.String()), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_document", Description: "Lê um documento de uma conversa (Markdown/HTML), como escrito pelo time de negócio com o BMad."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a docArgs) (*mcp.CallToolResult, any, error) {
			p, err := workspace.CleanPath(a.Path)
			if err != nil {
				return nil, nil, err
			}
			data, err := d.Store.ReadFile(ctx, a.ConversationID, p)
			if err != nil {
				return nil, nil, fmt.Errorf("documento não encontrado")
			}
			return text(string(data)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_published", Description: "Lista ou lê os documentos publicados do projeto no repositório central (branch do PR em revisão ou a padrão)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a centralArgs) (*mcp.CallToolResult, any, error) {
			if d.Publisher == nil {
				return nil, nil, fmt.Errorf("publicação não configurada")
			}
			p, err := d.Store.GetProject(ctx, a.ProjectID)
			if err != nil {
				return nil, nil, fmt.Errorf("projeto não encontrado")
			}
			snap, err := d.Publisher.Load(ctx, p.Sigla, p.Slug)
			if err != nil {
				return nil, nil, err
			}
			if a.Path == "" {
				var b strings.Builder
				fmt.Fprintf(&b, "Fonte: %s (status %s %s)\n", snap.Ref, snap.Status, snap.PRURL)
				for _, f := range snap.Files {
					fmt.Fprintf(&b, "- %s\n", f.Path)
				}
				return text(b.String()), nil, nil
			}
			for _, f := range snap.Files {
				if f.Path == a.Path {
					return text(string(f.Content)), nil, nil
				}
			}
			return nil, nil, fmt.Errorf("documento não publicado")
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_catalog", Description: "Mostra as etapas do BMad disponíveis para o time de negócio."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			var b strings.Builder
			fmt.Fprintf(&b, "BMad Method v%s\n", d.Catalog.Version)
			for _, it := range d.Catalog.Items {
				fmt.Fprintf(&b, "- %s: %s (%s)\n", it.Skill, it.Label, it.Phase)
			}
			return text(b.String()), nil, nil
		})
	return s
}
