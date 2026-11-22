package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// ErrCommandRefused is returned for commands outside the allow-list.
var ErrCommandRefused = errors.New("comando não disponível neste ambiente")

// Tokenize splits a shell-like command line into argv groups separated by
// "&&" or ";". Pipes, redirections and substitutions are refused.
func Tokenize(line string) ([][]string, error) {
	var groups [][]string
	var cur []string
	var tok strings.Builder
	inTok := false
	flush := func() {
		if inTok {
			cur = append(cur, tok.String())
			tok.Reset()
			inTok = false
		}
	}
	endGroup := func() {
		flush()
		if len(cur) > 0 {
			groups = append(groups, cur)
		}
		cur = nil
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			inTok = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				tok.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("aspas simples não fechadas")
			}
			i = j
		case c == '"':
			inTok = true
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune(`"\$`+"`", rs[j+1]) {
					j++
				} else if rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(' {
					return nil, fmt.Errorf("%w: substituição de comando", ErrCommandRefused)
				}
				tok.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("aspas duplas não fechadas")
			}
			i = j
		case c == '\\' && i+1 < len(rs):
			inTok = true
			i++
			tok.WriteRune(rs[i])
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == ';':
			endGroup()
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&':
			endGroup()
			i++
		case c == '|' || c == '>' || c == '<' || c == '`' || c == '&':
			return nil, fmt.Errorf("%w: pipes, redirecionamentos e execução em segundo plano não são suportados; use as ferramentas de arquivo", ErrCommandRefused)
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			return nil, fmt.Errorf("%w: substituição de comando", ErrCommandRefused)
		default:
			inTok = true
			tok.WriteRune(c)
		}
	}
	endGroup()
	return groups, nil
}

// expandPlaceholders resolves {project-root} and {skill-root} in a token.
func (tc *TurnContext) expandPlaceholders(s string) string {
	s = strings.ReplaceAll(s, "{project-root}", vfs.Root)
	if skill := tc.ActiveSkill(); skill != "" {
		s = strings.ReplaceAll(s, "{skill-root}", SkillRoot(skill))
	} else if agent := tc.ActiveAgent(); agent != "" {
		s = strings.ReplaceAll(s, "{skill-root}", SkillRoot(agent))
	}
	return s
}

// resolveScript turns a script token into an absolute virtual path. Bare
// relative paths resolve from the active skill root (BMad convention).
func (tc *TurnContext) resolveScript(tok string) (string, error) {
	tok = tc.expandPlaceholders(tok)
	if !strings.HasPrefix(tok, "/") {
		for _, base := range []string{tc.ActiveSkill(), tc.ActiveAgent()} {
			if base == "" {
				continue
			}
			cand := SkillRoot(base) + "/" + tok
			if tc.FS.Exists(cand) {
				return vfs.Normalize(cand)
			}
		}
	}
	return vfs.Normalize(tok)
}

func commandLabel(tc *TurnContext, args map[string]any) string {
	cmd, _ := args["command"].(string)
	switch {
	case strings.Contains(cmd, "memlog.py"):
		return "Registrando na memória da sessão"
	case strings.Contains(cmd, "resolve_config.py"), strings.Contains(cmd, "resolve_customization.py"):
		return "Carregando as preferências do método"
	case strings.HasPrefix(strings.TrimSpace(cmd), "open"), strings.HasPrefix(strings.TrimSpace(cmd), "start"), strings.HasPrefix(strings.TrimSpace(cmd), "xdg-open"):
		return "Abrindo o documento no painel"
	case strings.Contains(cmd, ".py"):
		return "Executando uma ferramenta do método"
	}
	return ""
}

// CommandTool registers run_command.
func CommandTool(r *Registry) {
	r.Add(&Tool{
		Def: defOf("run_command",
			"Executa um comando como no terminal de uma CLI de agente. Suportado: scripts do BMad (`uv run <script.py> ...` ou `python <script.py> ...`), "+
				"`open`/`start`/`xdg-open <arquivo>` (mostra o arquivo no painel da pessoa), `mkdir`, `ls`, `cat`, `date`, `echo`, `pwd`. "+
				"Encadeie com && se precisar. Pipes, redirecionamentos e outros programas não estão disponíveis.",
			schema(map[string]any{"command": str("Linha de comando completa")}, "command")),
		Label: commandLabel,
		Run: func(ctx context.Context, tc *TurnContext, raw json.RawMessage) (string, error) {
			var a struct {
				Command string `json:"command"`
			}
			if err := decode(raw, &a); err != nil {
				return "", err
			}
			groups, err := Tokenize(a.Command)
			if err != nil {
				return "", err
			}
			if len(groups) == 0 {
				return "", errors.New("comando vazio")
			}
			var outs []string
			for _, argv := range groups {
				out, err := tc.runArgv(ctx, argv)
				if err != nil {
					if len(outs) > 0 {
						return strings.Join(outs, "\n") + "\n" + "ERRO: " + err.Error(), nil
					}
					return "", err
				}
				if out != "" {
					outs = append(outs, out)
				}
			}
			if len(outs) == 0 {
				return "(ok, sem saída)", nil
			}
			return strings.Join(outs, "\n"), nil
		},
	})
}

func (tc *TurnContext) runArgv(ctx context.Context, argv []string) (string, error) {
	for i := range argv {
		argv[i] = tc.expandPlaceholders(argv[i])
	}
	name := argv[0]
	args := argv[1:]
	switch name {
	case "uv":
		if len(args) > 0 && args[0] == "run" {
			args = args[1:]
		}
		// uv run [--with x] python script.py ... | uv run script.py ...
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			if args[0] == "--with" || args[0] == "--python" {
				if len(args) < 2 {
					return "", errors.New("uso inválido de uv run")
				}
				args = args[2:]
			} else {
				args = args[1:]
			}
		}
		if len(args) > 0 && isPython(args[0]) {
			args = args[1:]
		}
		return tc.runScript(ctx, args)
	case "python", "python3", "py":
		return tc.runScript(ctx, args)
	case "open", "xdg-open", "start":
		return tc.openCmd(args)
	case "mkdir":
		return "", nil
	case "pwd":
		return vfs.Root, nil
	case "date":
		return tc.now().Format("2006-01-02 15:04:05 -0700"), nil
	case "echo":
		return strings.Join(args, " "), nil
	case "ls":
		target := vfs.Root
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				target = a
			}
		}
		items, err := tc.FS.ListDir(target)
		if err != nil {
			return "", err
		}
		return strings.Join(items, "\n"), nil
	case "cat":
		var parts []string
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			d, err := tc.FS.Read(a)
			if err != nil {
				return "", err
			}
			parts = append(parts, string(d))
		}
		return Truncate(strings.Join(parts, "\n"), MaxReadBytes), nil
	}
	return "", fmt.Errorf("%w: `%s`. Use as ferramentas de arquivo (read_file, write_file, edit_file, list_files) ou os scripts do BMad", ErrCommandRefused, name)
}

func isPython(s string) bool {
	switch s {
	case "python", "python3", "py":
		return true
	}
	return false
}

func (tc *TurnContext) openCmd(args []string) (string, error) {
	var target string
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") || a == "/b" {
			continue // `start "" "path"` style
		}
		target = a
	}
	if target == "" {
		return "", errors.New("informe o arquivo a abrir")
	}
	abs, err := vfs.Normalize(target)
	if err != nil {
		return "", err
	}
	if !tc.FS.Exists(abs) {
		return "", fmt.Errorf("%w: %s", vfs.ErrNotFound, abs)
	}
	if OutputRel(abs) == "" {
		// Bundle assets (e.g. brain-selector.html) are copied into the workspace
		// so the panel can show them.
		data, _ := tc.FS.Read(abs)
		dest := vfs.Root + "/" + OutputDir + "/.view/" + path.Base(abs)
		if _, err := tc.FS.Write(dest, data); err != nil {
			return "", err
		}
		abs = dest
	}
	if tc.Emit != nil {
		tc.Emit(events.Event{Type: events.Open, Document: DocRef(abs, false)})
	}
	return fmt.Sprintf("Arquivo aberto no painel ao lado do chat: %s. A pessoa já pode vê-lo; não é preciso pedir que ela abra nada.", FriendlyName(abs)), nil
}

func (tc *TurnContext) runScript(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("informe o script")
	}
	if args[0] == "-c" || args[0] == "-m" {
		return "", fmt.Errorf("%w: só scripts do BMad podem ser executados", ErrCommandRefused)
	}
	script, err := tc.resolveScript(args[0])
	if err != nil {
		return "", err
	}
	rest := args[1:]
	switch {
	case strings.HasSuffix(script, "/_bmad/scripts/memlog.py"):
		return tc.memlog(rest)
	case strings.HasSuffix(script, "/_bmad/scripts/resolve_config.py"):
		return tc.resolveConfig(rest)
	case strings.HasSuffix(script, "/_bmad/scripts/resolve_customization.py"):
		return tc.resolveCustomization(rest)
	}
	if !strings.HasSuffix(script, ".py") || !allowedScript(script) {
		return "", fmt.Errorf("%w: só scripts instalados do BMad podem ser executados", ErrCommandRefused)
	}
	if !tc.FS.Exists(script) {
		return "", fmt.Errorf("%w: %s", vfs.ErrNotFound, script)
	}
	if tc.Python == nil || !tc.Python.Enabled() {
		return "", errors.New("o runtime Python não está disponível neste ambiente; siga o caminho alternativo descrito na skill (faça a etapa você mesmo com as ferramentas de arquivo)")
	}
	return tc.Python.Run(ctx, tc, script, rest)
}

func allowedScript(abs string) bool {
	return strings.HasPrefix(abs, vfs.Root+"/_bmad/scripts/") ||
		(strings.HasPrefix(abs, vfs.Root+"/"+bmad.SkillsDir+"/") && strings.Contains(abs, "/scripts/"))
}

// --- argument parsing helpers ----------------------------------------------

type flagSet struct {
	pos    []string
	values map[string][]string
}

func parseFlags(args []string, boolFlags ...string) flagSet {
	fsx := flagSet{values: map[string][]string{}}
	isBool := map[string]bool{}
	for _, b := range boolFlags {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				fsx.values[name[:eq]] = append(fsx.values[name[:eq]], name[eq+1:])
				continue
			}
			if isBool[name] || i+1 >= len(args) {
				fsx.values[name] = append(fsx.values[name], "true")
				continue
			}
			fsx.values[name] = append(fsx.values[name], args[i+1])
			i++
			continue
		}
		fsx.pos = append(fsx.pos, a)
	}
	return fsx
}

func (f flagSet) get(names ...string) string {
	for _, n := range names {
		if v := f.values[n]; len(v) > 0 {
			return v[len(v)-1]
		}
	}
	return ""
}

func (f flagSet) all(names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, f.values[n]...)
	}
	return out
}

// --- native resolve_* ---------------------------------------------------------

func jsonIndent(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return strings.TrimRight(sb.String(), "\n")
}

func (tc *TurnContext) resolveConfig(args []string) (string, error) {
	f := parseFlags(args)
	merged, err := bmad.ResolveCentralConfig(tc.FS.AsFS())
	if err != nil {
		return "", err
	}
	return jsonIndent(bmad.SelectKeys(merged, f.all("key", "k"))), nil
}

func (tc *TurnContext) resolveCustomization(args []string) (string, error) {
	f := parseFlags(args)
	skill := f.get("skill", "s")
	if skill == "" {
		return "", errors.New("error: the following arguments are required: --skill/-s")
	}
	abs, err := vfs.Normalize(tc.expandPlaceholders(skill))
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(strings.TrimSuffix(abs, "/"), vfs.Root+"/")
	merged, err := bmad.ResolveCustomization(tc.FS.AsFS(), rel)
	if err != nil {
		return "", err
	}
	return jsonIndent(bmad.SelectKeys(merged, f.all("key", "k"))), nil
}

// --- native memlog.py ---------------------------------------------------------

func memlogNow(t time.Time) string { return t.Format("2006-01-02T15:04") }

type memlogDoc struct {
	keys []string
	meta map[string]string
	body string
}

func splitMemlog(text string) (*memlogDoc, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || lines[0] != "---" {
		return nil, errors.New(".memlog.md has no frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New(".memlog.md frontmatter is not terminated")
	}
	d := &memlogDoc{meta: map[string]string{}}
	for _, l := range lines[1:end] {
		if i := strings.Index(l, ":"); i >= 0 {
			k, v := strings.TrimSpace(l[:i]), strings.TrimSpace(l[i+1:])
			if _, ok := d.meta[k]; !ok {
				d.keys = append(d.keys, k)
			}
			d.meta[k] = v
		}
	}
	d.body = strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
	return d, nil
}

func (d *memlogDoc) set(k, v string) {
	if _, ok := d.meta[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.meta[k] = v
}

func (d *memlogDoc) touch(now time.Time) {
	if _, ok := d.meta["updated"]; ok {
		for i, k := range d.keys {
			if k == "updated" {
				d.keys = append(d.keys[:i], d.keys[i+1:]...)
				break
			}
		}
		delete(d.meta, "updated")
	}
	d.set("updated", memlogNow(now))
}

func (d *memlogDoc) render() string {
	var fm []string
	for _, k := range d.keys {
		fm = append(fm, k+": "+strings.Join(strings.Split(strings.ReplaceAll(d.meta[k], "\r\n", "\n"), "\n"), " "))
	}
	return "---\n" + strings.Join(fm, "\n") + "\n---\n\n" + strings.TrimRight(d.body, "\n") + "\n"
}

func entryCount(body string) int {
	n := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "- ") {
			n++
		}
	}
	return n
}

func memlogAck(p string, body string) string {
	b, _ := json.Marshal(p)
	return fmt.Sprintf(`{"ok": true, "memlog": %s, "entries": %d}`, b, entryCount(body))
}

func (tc *TurnContext) memlog(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: memlog.py {init,append,set} ...")
	}
	cmd := args[0]
	f := parseFlags(args[1:])
	target := f.get("path")
	if target == "" {
		ws := f.get("workspace")
		if ws == "" {
			return "", errors.New("error: one of the arguments --workspace --path is required")
		}
		target = strings.TrimRight(ws, "/") + "/.memlog.md"
	}
	abs, err := vfs.Normalize(target)
	if err != nil {
		return "", err
	}
	if strings.Contains(abs, "{") {
		return "", fmt.Errorf("caminho com variável não resolvida: %s (substitua {…} pelo valor real)", abs)
	}
	now := tc.now()
	switch cmd {
	case "init":
		if tc.FS.Exists(abs) {
			return "", fmt.Errorf("error: %s already exists; use append/set to update it", abs)
		}
		d := &memlogDoc{meta: map[string]string{}}
		for _, pair := range f.all("field") {
			i := strings.Index(pair, "=")
			if i < 0 {
				return "", fmt.Errorf("error: --field expects key=value, got %q", pair)
			}
			d.set(strings.TrimSpace(pair[:i]), strings.TrimSpace(pair[i+1:]))
		}
		d.touch(now)
		if _, err := tc.FS.Write(abs, []byte(d.render())); err != nil {
			return "", err
		}
		return memlogAck(abs, ""), nil
	case "append":
		data, err := tc.FS.Read(abs)
		if err != nil {
			return "", fmt.Errorf("FileNotFoundError: %s (rode `memlog.py init` primeiro)", abs)
		}
		d, err := splitMemlog(string(data))
		if err != nil {
			return "", err
		}
		text := strings.Join(strings.Fields(f.get("text")), " ")
		if text == "" && len(f.values["text"]) == 0 {
			return "", errors.New("error: the following arguments are required: --text")
		}
		label := f.get("type")
		if by := f.get("by"); by != "" {
			label = strings.TrimSpace(label + " by " + by)
		}
		tag := ""
		if label != "" {
			tag = "(" + label + ") "
		}
		entry := "- " + tag + text
		if strings.TrimSpace(d.body) != "" {
			d.body = strings.TrimRight(d.body, "\n") + "\n" + entry
		} else {
			d.body = entry
		}
		d.touch(now)
		if _, err := tc.FS.Write(abs, []byte(d.render())); err != nil {
			return "", err
		}
		return memlogAck(abs, d.body), nil
	case "set":
		data, err := tc.FS.Read(abs)
		if err != nil {
			return "", fmt.Errorf("FileNotFoundError: %s", abs)
		}
		d, err := splitMemlog(string(data))
		if err != nil {
			return "", err
		}
		key := f.get("key")
		if key == "" {
			return "", errors.New("error: the following arguments are required: --key, --value")
		}
		d.set(key, f.get("value"))
		d.touch(now)
		if _, err := tc.FS.Write(abs, []byte(d.render())); err != nil {
			return "", err
		}
		return memlogAck(abs, d.body), nil
	}
	return "", fmt.Errorf("error: argument cmd: invalid choice: %q (choose from 'init', 'append', 'set')", cmd)
}
