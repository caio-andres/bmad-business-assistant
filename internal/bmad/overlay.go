package bmad

import (
	"fmt"
	"sort"
	"strings"
)

// Language used for both conversation and documents (business team is PT-BR).
const LanguagePTBR = "Portuguese (Brazil)"

// OverlayInput describes who is talking and about which project.
type OverlayInput struct {
	UserName    string
	ProjectName string // e.g. "PAG-cashback" or "rascunho"
	Language    string
	// SystemsGlob is loaded as persistent facts when the conversation has product context.
	HasSystems bool
}

// BusinessFacts are injected through BMad's native customization layer
// (_bmad/custom/<skill>.toml persistent_facts), so every skill and agent carries them.
var BusinessFacts = []string{
	"Quem conversa com você é uma pessoa de negócio usando um chat web: ela não tem terminal, não vê arquivos e não conhece termos técnicos. Fale de forma simples, direta e assertiva.",
	"Nunca mostre caminhos de arquivo, comandos ou nomes de scripts para a pessoa; refira-se aos documentos pelo nome (por exemplo, \"o brief\" ou \"o PRD\").",
	"Os documentos aparecem para a pessoa num painel ao lado do chat assim que você os grava; diga \"atualizei o documento\" em vez de citar onde salvou.",
}

func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", "", "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// UserConfigTOML renders the per-conversation _bmad/config.user.toml.
func UserConfigTOML(in OverlayInput) string {
	lang := in.Language
	if lang == "" {
		lang = LanguagePTBR
	}
	name := strings.TrimSpace(in.UserName)
	if name == "" {
		name = "Time de Negócio"
	}
	project := strings.TrimSpace(in.ProjectName)
	if project == "" {
		project = "rascunho"
	}
	var sb strings.Builder
	sb.WriteString("# Gerado pela aplicação para esta conversa (equivalente ao config.user.toml pessoal).\n\n")
	sb.WriteString("[core]\n")
	fmt.Fprintf(&sb, "user_name = %s\n", tomlString(name))
	fmt.Fprintf(&sb, "communication_language = %s\n", tomlString(lang))
	fmt.Fprintf(&sb, "document_output_language = %s\n", tomlString(lang))
	fmt.Fprintf(&sb, "project_name = %s\n", tomlString(project))
	sb.WriteString("\n[modules.bmm]\nuser_skill_level = \"beginner\"\n")
	return sb.String()
}

// CustomOverrides renders _bmad/custom/<skill>.toml for each allowed skill,
// adding the business-mode facts as persistent facts (agent or workflow block).
func CustomOverrides(b *Bundle, allowed []string, in OverlayInput) map[string]string {
	facts := append([]string{}, BusinessFacts...)
	if in.HasSystems {
		facts = append(facts, "file:{project-root}/docs/systems/*.md")
	}
	out := map[string]string{}
	names := append([]string{}, allowed...)
	sort.Strings(names)
	for _, name := range names {
		s, ok := b.Skills[name]
		if !ok {
			continue
		}
		block := "workflow"
		if s.IsAgent {
			block = "agent"
		}
		var sb strings.Builder
		sb.WriteString("# Customização do time (modo negócio), gerada pela aplicação.\n\n")
		fmt.Fprintf(&sb, "[%s]\npersistent_facts = [\n", block)
		for _, f := range facts {
			fmt.Fprintf(&sb, "  %s,\n", tomlString(f))
		}
		sb.WriteString("]\n")
		out["custom/"+name+".toml"] = sb.String()
	}
	return out
}
