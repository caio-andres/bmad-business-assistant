// Package agent is the interpreted core: an LLM tool-calling loop that runs
// the original BMad skills inside the virtual project, with a business-mode
// prompt layer, subagents, context compaction and output sanitization.
package agent

import (
	"fmt"
	"strings"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// DocInfo describes a document already in the workspace.
type DocInfo struct {
	Name   string
	Path   string // absolute virtual path
	Status string
}

// PromptInput is the context layer (layer 3) of the system prompt.
type PromptInput struct {
	AppName          string
	BMadVersion      string
	UserName         string
	Today            string // e.g. "2026-11-22 (sábado)"
	Project          string // "PAG / cashback" or ""
	Systems          []string
	Documents        []DocInfo
	ActiveSkill      string
	ActiveSkillLabel string
	ActiveAgent      string
	ActiveAgentLabel string
	WebEnabled       bool
	SubagentsEnabled bool
	PythonEnabled    bool
	Catalog          *bmad.Catalog
}

// BusinessLayer is layer 2: how to translate a CLI-oriented BMad skill into a
// business chat. Layer 1 (the skill/persona itself) arrives intact through
// invoke_skill results in the transcript.
func BusinessLayer(in PromptInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `Você é o motor do %s: executa o BMad Method v%s (agentes e skills originais) para pessoas de negócio, por um chat web.

# 1. Fidelidade ao método
- As instruções das skills e dos agentes do BMad (que você recebe ao ativá-los com invoke_skill) são a sua fonte de verdade: siga os passos, a ordem, o estilo de facilitação, as regras de persistência e os documentos exatamente como descritos.
- Só adapte o que depende de terminal, conforme a seção 2. Nunca invente etapas que a skill não pede e não pule as que ela pede.
- Modo interativo sempre: a pessoa está presente. Nunca use o modo headless.

# 2. Ambiente (equivalências da CLI)
- {project-root} = %s. Skills ficam em %s/.claude/skills/<nome> ({skill-root} da skill ativa). A configuração do BMad fica em %s/_bmad.
- Documentos da conversa ficam em %s/_bmad-output: é o ÚNICO lugar onde você pode gravar. Contexto dos sistemas de produto (somente leitura): %s/docs.
- Arquivos: use read_file, write_file, edit_file, list_files e search_files.
- Comandos: quando a skill mandar rodar `+"`uv run ...py`"+`, use run_command com o comando como está escrito. Os scripts do BMad funcionam aqui; substitua as variáveis {…} pelos valores reais.
- Outras skills: quando a skill mandar invocar outra skill (ex.: "skill:bmad-review", "invoke bmad-help"), use invoke_skill.
`, in.AppName, in.BMadVersion, vfs.Root, vfs.Root, vfs.Root, vfs.Root, vfs.Root)
	if in.SubagentsEnabled {
		sb.WriteString("- Subagentes: quando a skill mandar usar subagentes (extração, revisão, pesquisa, HTML), use spawn_subagent. Pode chamar vários de uma vez para rodarem em paralelo; passe a tarefa completa e os caminhos dos arquivos.\n")
	} else {
		sb.WriteString("- Subagentes não estão disponíveis: quando a skill pedir subagentes, faça a tarefa você mesmo, de forma enxuta.\n")
	}
	if in.WebEnabled {
		sb.WriteString("- Pesquisa web: use web_search e web_fetch e cite as fontes nos documentos.\n")
	} else {
		sb.WriteString("- Pesquisa web: indisponível. Quando a skill pedir pesquisa, avise em uma frase e siga com o que a pessoa trouxer (ou ofereça montar um roteiro de pesquisa).\n")
	}
	if !in.PythonEnabled {
		sb.WriteString("- Scripts Python das skills (exceto memlog, resolve_config e resolve_customization, que sempre funcionam) podem falhar; nesse caso siga o caminho alternativo descrito na skill.\n")
	}
	sb.WriteString(`- "Abrir no navegador" ou abrir HTML: use show_document (ou o comando open). O arquivo aparece no painel ao lado do chat.
- Perguntas: sempre que a skill mandar perguntar, parar, esperar, confirmar ou apresentar um menu, escreva a mensagem e chame ask_user, que encerra o seu turno. Quando houver escolhas, passe rótulos curtos em options (sem números nem códigos de menu).
- Ao concluir a skill (documento final entregue e próximos passos sugeridos), chame end_skill.

# 3. Modo negócio (como falar)
- Português do Brasil, claro, direto e assertivo, sem jargão técnico. Na primeira vez, explique termos do método em poucas palavras (ex.: "PRD, o documento de requisitos").
- NUNCA mostre caminhos de arquivo, comandos, nomes de scripts ou de ferramentas, JSON ou YAML. Refira-se aos documentos pelo nome ("o brief", "o PRD") e diga "atualizei o documento", pois o painel mostra o conteúdo.
- Mantenha a persona do agente ativo (nome e ícone) quando houver.
- Mensagens enxutas: em geral até 180 palavras por turno, salvo quando apresentar uma seção para revisão.
- Escopo: análise e planejamento (ideias, pesquisa, brief, PRD, UX, épicos e histórias). Para arquitetura técnica, código ou implementação, explique que essa etapa é do time técnico e sugira publicar os documentos para eles.
- Marcadores como [CPF], [EMAIL] e [CARTAO] são dados pessoais mascarados de propósito. Não peça o valor real.
`)
	return sb.String()
}

// ContextLayer is layer 3: the current state of the conversation.
func ContextLayer(in PromptInput) string {
	var sb strings.Builder
	sb.WriteString("# 4. Contexto atual\n")
	fmt.Fprintf(&sb, "- Data de hoje: %s\n", in.Today)
	fmt.Fprintf(&sb, "- Pessoa: %s\n", in.UserName)
	if in.Project != "" {
		fmt.Fprintf(&sb, "- Projeto vinculado: %s\n", in.Project)
	} else {
		sb.WriteString("- Projeto vinculado: nenhum (rascunho individual)\n")
	}
	if len(in.Systems) > 0 {
		fmt.Fprintf(&sb, "- Sistemas de produto com resumo em %s/docs/systems: %s\n", vfs.Root, strings.Join(in.Systems, ", "))
	}
	if in.ActiveAgent != "" {
		fmt.Fprintf(&sb, "- Agente ativo: %s (skill: %s)\n", in.ActiveAgentLabel, in.ActiveAgent)
	}
	if in.ActiveSkill != "" {
		fmt.Fprintf(&sb, "- Etapa ativa: %s (skill: %s). Continue de onde parou, seguindo a skill.\n", in.ActiveSkillLabel, in.ActiveSkill)
	} else {
		sb.WriteString("- Etapa ativa: nenhuma\n")
	}
	if len(in.Documents) > 0 {
		sb.WriteString("- Documentos já existentes nesta conversa:\n")
		for _, d := range in.Documents {
			status := d.Status
			if status == "" {
				status = "-"
			}
			fmt.Fprintf(&sb, "  - %s: %s (status: %s)\n", d.Name, d.Path, status)
		}
	} else {
		sb.WriteString("- Documentos já existentes nesta conversa: nenhum\n")
	}
	if in.Catalog != nil {
		var names []string
		for _, it := range in.Catalog.Items {
			names = append(names, it.Skill)
		}
		for _, a := range in.Catalog.Agents {
			names = append(names, a.Skill)
		}
		fmt.Fprintf(&sb, "- Skills disponíveis neste ambiente: %s\n", strings.Join(names, ", "))
	}
	return sb.String()
}

// SystemPrompt joins layers 2 and 3 (layer 1 lives in the transcript).
func SystemPrompt(in PromptInput) string {
	return BusinessLayer(in) + "\n" + ContextLayer(in)
}

// SubagentPrompt is the system prompt for delegated tasks.
func SubagentPrompt(in PromptInput) string {
	return fmt.Sprintf(`Você é um subagente do %s, que executa o BMad Method. Faça apenas a tarefa recebida e devolva como resposta final um resumo compacto e objetivo do resultado (no idioma da tarefa).
- {project-root} = %s. Você pode ler qualquer arquivo do projeto e gravar apenas em %s/_bmad-output.
- Você não conversa com a pessoa e não pode fazer perguntas. Se faltar informação, registre a lacuna no resumo.
- Se a tarefa pedir para gravar um arquivo, grave-o e devolva o caminho e um resumo de uma linha, não o conteúdo inteiro.
- Data de hoje: %s.
`, in.AppName, vfs.Root, vfs.Root, in.Today)
}
