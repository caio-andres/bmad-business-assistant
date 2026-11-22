// Package bmad loads a pinned BMad Method installation and resolves its
// configuration and customization layers exactly like the original scripts.
package bmad

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillsDir is where the installer places skills for Claude Code.
const SkillsDir = ".claude/skills"

// Skill is one installed BMad skill (agent persona or workflow).
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Module      string `json:"module"`
	Dir         string `json:"-"` // path inside the bundle FS
	IsAgent     bool   `json:"isAgent"`
}

// HelpEntry is one row of _bmad/_config/bmad-help.csv.
type HelpEntry struct {
	Module      string `json:"module"`
	Skill       string `json:"skill"`
	DisplayName string `json:"displayName"`
	MenuCode    string `json:"menuCode"`
	Description string `json:"description"`
	Phase       string `json:"phase"`
	PrecededBy  string `json:"precededBy"`
	FollowedBy  string `json:"followedBy"`
	Required    bool   `json:"required"`
	Outputs     string `json:"outputs"`
}

// AgentInfo is an [agents.<skill>] descriptor from the central config.
type AgentInfo struct {
	Skill       string `json:"skill"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
}

// Bundle is a read-only BMad installation.
type Bundle struct {
	FS      fs.FS
	Version string
	Skills  map[string]Skill
	Help    []HelpEntry
	Agents  map[string]AgentInfo
}

// Load reads manifests, skills and agent descriptors from fsys.
func Load(fsys fs.FS) (*Bundle, error) {
	b := &Bundle{FS: fsys, Skills: map[string]Skill{}, Agents: map[string]AgentInfo{}}

	manifest, err := fs.ReadFile(fsys, "_bmad/_config/manifest.yaml")
	if err != nil {
		return nil, fmt.Errorf("bmad: read manifest: %w", err)
	}
	var m struct {
		Installation struct {
			Version string `yaml:"version"`
		} `yaml:"installation"`
	}
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("bmad: parse manifest: %w", err)
	}
	b.Version = m.Installation.Version

	modules := map[string]string{}
	if rows, err := readCSV(fsys, "_bmad/_config/skill-manifest.csv"); err == nil {
		for _, r := range rows {
			modules[r["name"]] = r["module"]
		}
	}

	entries, err := fs.ReadDir(fsys, SkillsDir)
	if err != nil {
		return nil, fmt.Errorf("bmad: read skills dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(SkillsDir, e.Name())
		data, err := fs.ReadFile(fsys, path.Join(dir, "SKILL.md"))
		if err != nil {
			continue
		}
		fm, _ := Frontmatter(data)
		name, _ := fm["name"].(string)
		if name == "" {
			name = e.Name()
		}
		desc, _ := fm["description"].(string)
		b.Skills[name] = Skill{
			Name: name, Description: desc, Module: modules[name], Dir: dir,
			IsAgent: strings.HasPrefix(name, "bmad-agent-"),
		}
	}

	if rows, err := readCSV(fsys, "_bmad/_config/bmad-help.csv"); err == nil {
		for _, r := range rows {
			if r["skill"] == "_meta" {
				continue
			}
			b.Help = append(b.Help, HelpEntry{
				Module: r["module"], Skill: r["skill"], DisplayName: r["display-name"],
				MenuCode: r["menu-code"], Description: r["description"], Phase: r["phase"],
				PrecededBy: r["preceded-by"], FollowedBy: r["followed-by"],
				Required: r["required"] == "true", Outputs: r["outputs"],
			})
		}
	}

	cfg, err := LoadTOML(fsys, "_bmad/config.toml", true)
	if err != nil {
		return nil, err
	}
	if agents, ok := cfg["agents"].(map[string]any); ok {
		for skill, raw := range agents {
			a, _ := raw.(map[string]any)
			info := AgentInfo{Skill: skill}
			info.Name, _ = a["name"].(string)
			info.Title, _ = a["title"].(string)
			info.Icon, _ = a["icon"].(string)
			info.Description, _ = a["description"].(string)
			b.Agents[skill] = info
		}
	}
	return b, nil
}

// SkillNames returns installed skill names sorted.
func (b *Bundle) SkillNames() []string {
	names := make([]string, 0, len(b.Skills))
	for n := range b.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ReadSkill returns the SKILL.md content of an installed skill.
func (b *Bundle) ReadSkill(name string) ([]byte, Skill, error) {
	s, ok := b.Skills[name]
	if !ok {
		return nil, Skill{}, fmt.Errorf("bmad: skill %q not installed", name)
	}
	data, err := fs.ReadFile(b.FS, path.Join(s.Dir, "SKILL.md"))
	return data, s, err
}

// Frontmatter parses a leading YAML frontmatter block. Missing frontmatter yields an empty map.
func Frontmatter(data []byte) (map[string]any, []byte) {
	out := map[string]any{}
	text := bytes.TrimPrefix(data, []byte("ï»¿"))
	if !bytes.HasPrefix(text, []byte("---")) {
		return out, data
	}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	first := true
	closed := false
	consumed := 0
	for sc.Scan() {
		line := sc.Text()
		consumed += len(sc.Bytes()) + 1
		if first {
			first = false
			if strings.TrimRight(line, "\r") != "---" {
				return out, data
			}
			continue
		}
		if strings.TrimRight(line, "\r") == "---" {
			closed = true
			break
		}
		lines = append(lines, line)
	}
	if !closed {
		return out, data
	}
	_ = yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &out)
	if consumed > len(text) {
		consumed = len(text)
	}
	return out, text[consumed:]
}

func readCSV(fsys fs.FS, name string) ([]map[string]string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("ï»¿"))))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	header := records[0]
	var rows []map[string]string
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, h := range header {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// WalkFiles calls fn for every regular file under root in fsys.
func WalkFiles(fsys fs.FS, root string, fn func(p string, data []byte) error) error {
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return fn(p, data)
	})
}
