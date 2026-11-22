package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// Limits for tool outputs.
const (
	MaxReadBytes   = 120 * 1024
	MaxListEntries = 500
	MaxGrepMatches = 200
)

// Truncate cuts s to max bytes on a rune boundary and flags it.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[... conteúdo truncado: %d de %d bytes exibidos ...]", cut, len(s))
}

func readLabel(tc *TurnContext, args map[string]any) string {
	p, _ := args["path"].(string)
	abs, err := vfs.Normalize(p)
	if err != nil {
		return "Consultando um arquivo"
	}
	switch {
	case strings.Contains(abs, "/.claude/skills/"):
		if strings.Contains(abs, "/assets/") && strings.Contains(strings.ToLower(abs), "template") {
			return "Consultando o modelo do documento"
		}
		if strings.Contains(abs, "/assets/") {
			return "Consultando material de apoio do método"
		}
		return "Consultando o guia da etapa"
	case strings.HasPrefix(abs, vfs.Root+"/_bmad/"):
		return "Lendo a configuração do método"
	case strings.HasPrefix(abs, vfs.Root+"/docs/"):
		return "Lendo o contexto do sistema " + strings.TrimSuffix(FriendlyName(abs), ".md")
	case OutputRel(abs) != "":
		return "Lendo o documento " + FriendlyName(abs)
	}
	return "Consultando um arquivo"
}

func writeLabel(tc *TurnContext, args map[string]any) string {
	p, _ := args["path"].(string)
	if abs, err := vfs.Normalize(p); err == nil && strings.HasSuffix(abs, "/.memlog.md") {
		return "Registrando na memória da sessão"
	}
	return "Atualizando o documento " + FriendlyName(p)
}

// FileTools registers read/write/edit/list/search.
func FileTools(r *Registry) {
	r.Add(&Tool{
		Def: defOf("read_file",
			"Lê um arquivo do projeto. Caminhos podem usar {project-root} ou ser absolutos em /project. Use offset/limit (linhas, base 1) para arquivos grandes.",
			schema(map[string]any{
				"path":   str("Caminho do arquivo, ex.: {project-root}/_bmad/config.toml"),
				"offset": integer("Linha inicial (1 = primeira). Opcional."),
				"limit":  integer("Quantidade de linhas. Opcional."),
			}, "path")),
		Label:    readLabel,
		Parallel: true,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			data, err := tc.FS.Read(a.Path)
			if err != nil {
				return "", err
			}
			s := string(data)
			if a.Offset > 0 || a.Limit > 0 {
				lines := strings.Split(s, "\n")
				start := a.Offset - 1
				if start < 0 {
					start = 0
				}
				if start > len(lines) {
					start = len(lines)
				}
				end := len(lines)
				if a.Limit > 0 && start+a.Limit < end {
					end = start + a.Limit
				}
				s = strings.Join(lines[start:end], "\n")
			}
			if s == "" {
				return "(arquivo vazio)", nil
			}
			return Truncate(s, MaxReadBytes), nil
		},
	})

	r.Add(&Tool{
		Def: defOf("write_file",
			"Cria ou substitui um arquivo. Só é possível gravar dentro de {project-root}/_bmad-output (os documentos da conversa). A pessoa vê o documento atualizado no painel ao lado do chat.",
			schema(map[string]any{
				"path":    str("Caminho do arquivo a gravar, dentro de {project-root}/_bmad-output"),
				"content": str("Conteúdo completo do arquivo"),
			}, "path", "content")),
		Label: writeLabel,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			abs, err := tc.FS.Write(a.Path, []byte(a.Content))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Arquivo gravado: %s (%d bytes).", abs, len(a.Content)), nil
		},
	})

	r.Add(&Tool{
		Def: defOf("edit_file",
			"Substitui um trecho exato de um arquivo existente em {project-root}/_bmad-output. old_string deve ser único no arquivo, a menos que replace_all seja true.",
			schema(map[string]any{
				"path":        str("Caminho do arquivo"),
				"old_string":  str("Trecho exato a substituir"),
				"new_string":  str("Novo trecho"),
				"replace_all": boolean("Substituir todas as ocorrências"),
			}, "path", "old_string", "new_string")),
		Label: writeLabel,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			data, err := tc.FS.Read(a.Path)
			if err != nil {
				return "", err
			}
			s := string(data)
			if a.Old == "" {
				return "", fmt.Errorf("old_string vazio; use write_file para reescrever o arquivo")
			}
			n := strings.Count(s, a.Old)
			switch {
			case n == 0:
				return "", fmt.Errorf("trecho não encontrado no arquivo")
			case n > 1 && !a.ReplaceAll:
				return "", fmt.Errorf("trecho aparece %d vezes; inclua mais contexto ou use replace_all", n)
			}
			if a.ReplaceAll {
				s = strings.ReplaceAll(s, a.Old, a.New)
			} else {
				s = strings.Replace(s, a.Old, a.New, 1)
			}
			abs, err := tc.FS.Write(a.Path, []byte(s))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Arquivo editado: %s (%d substituição(ões)).", abs, max(1, boolInt(a.ReplaceAll)*n)), nil
		},
	})

	r.Add(&Tool{
		Def: defOf("list_files",
			"Lista arquivos do projeto por padrão glob (*, ?, **, {a,b}) ou o conteúdo de uma pasta. Ex.: {project-root}/_bmad-output/**/*.md",
			schema(map[string]any{"pattern": str("Padrão glob ou pasta")}, "pattern")),
		Label:    func(*TurnContext, map[string]any) string { return "Procurando documentos" },
		Parallel: true,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			var items []string
			var err error
			if strings.ContainsAny(a.Pattern, "*?{") {
				items, err = tc.FS.Glob(a.Pattern)
			} else {
				items, err = tc.FS.ListDir(a.Pattern)
			}
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				return "Nenhum arquivo encontrado.", nil
			}
			more := ""
			if len(items) > MaxListEntries {
				more = fmt.Sprintf("\n[... %d itens omitidos ...]", len(items)-MaxListEntries)
				items = items[:MaxListEntries]
			}
			return strings.Join(items, "\n") + more, nil
		},
	})

	r.Add(&Tool{
		Def: defOf("search_files",
			"Busca uma expressão regular no conteúdo dos arquivos (como grep). Retorna caminho:linha: texto.",
			schema(map[string]any{
				"pattern": str("Expressão regular (sintaxe RE2)"),
				"glob":    str("Restringe a arquivos que casem com este glob. Padrão: {project-root}/**"),
			}, "pattern")),
		Label:    func(*TurnContext, map[string]any) string { return "Procurando nos documentos" },
		Parallel: true,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Glob    string `json:"glob"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return "", fmt.Errorf("expressão regular inválida: %v", err)
			}
			if a.Glob == "" {
				a.Glob = vfs.Root + "/**"
			}
			files, err := tc.FS.Glob(a.Glob)
			if err != nil {
				return "", err
			}
			var out []string
			for _, f := range files {
				data, err := tc.FS.Read(f)
				if err != nil || len(data) > 1<<20 {
					continue
				}
				for i, line := range strings.Split(string(data), "\n") {
					if re.MatchString(line) {
						if len(line) > 300 {
							line = line[:300] + "…"
						}
						out = append(out, fmt.Sprintf("%s:%d: %s", f, i+1, line))
						if len(out) >= MaxGrepMatches {
							return strings.Join(out, "\n") + "\n[... limite de resultados atingido ...]", nil
						}
					}
				}
			}
			if len(out) == 0 {
				return "Nenhuma ocorrência.", nil
			}
			return strings.Join(out, "\n"), nil
		},
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defOf(name, desc string, params json.RawMessage) llmToolDef {
	return llmToolDef{Name: name, Description: desc, Parameters: params}
}
