package bmad

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrConfig mirrors config_utils.ConfigError: a present layer cannot be used safely.
var ErrConfig = errors.New("bmad config error")

var keyedMergeFields = []string{"code", "id"}

// LoadTOML loads a TOML table from fsys. Missing optional layers yield an empty map.
func LoadTOML(fsys fs.FS, name string, required bool) (map[string]any, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if required {
				return nil, fmt.Errorf("%w: required TOML file not found: %s", ErrConfig, name)
			}
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("%w: failed to read %s: %v", ErrConfig, name, err)
	}
	return ParseTOML(data, name)
}

// ParseTOML decodes TOML bytes into normalized Go values (map[string]any, []any).
func ParseTOML(data []byte, name string) (map[string]any, error) {
	var out map[string]any
	if _, err := toml.Decode(string(data), &out); err != nil {
		return nil, fmt.Errorf("%w: failed to parse %s: %v", ErrConfig, name, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	norm, _ := normalize(out).(map[string]any)
	return norm, nil
}

// normalize converts decoder-specific container types into map[string]any / []any.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = normalize(val)
		}
		return m
	case []map[string]any:
		l := make([]any, len(t))
		for i, val := range t {
			l[i] = normalize(val)
		}
		return l
	case []any:
		l := make([]any, len(t))
		for i, val := range t {
			l[i] = normalize(val)
		}
		return l
	default:
		return v
	}
}

func detectKeyedMergeField(items []any) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	for _, it := range items {
		if _, ok := it.(map[string]any); !ok {
			return "", nil
		}
	}
	for _, candidate := range keyedMergeFields {
		all := true
		for _, it := range items {
			if _, ok := it.(map[string]any)[candidate]; !ok {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		for _, it := range items {
			s, ok := it.(map[string]any)[candidate].(string)
			if !ok {
				return "", fmt.Errorf("%w: keyed array identifier `%s` must be a string", ErrConfig, candidate)
			}
			if s == "" {
				return "", fmt.Errorf("%w: keyed array identifier `%s` must not be empty", ErrConfig, candidate)
			}
		}
		return candidate, nil
	}
	return "", nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mergeArrays(base, override []any) ([]any, error) {
	combined := append(append([]any{}, base...), override...)
	field, err := detectKeyedMergeField(combined)
	if err != nil {
		return nil, err
	}
	if field == "" {
		return combined, nil
	}
	result := []any{}
	index := map[string]int{}
	for _, it := range base {
		c := copyMap(it.(map[string]any))
		index[c[field].(string)] = len(result)
		result = append(result, c)
	}
	for _, it := range override {
		c := copyMap(it.(map[string]any))
		key := c[field].(string)
		if i, ok := index[key]; ok {
			result[i] = c
		} else {
			index[key] = len(result)
			result = append(result, c)
		}
	}
	return result, nil
}

// StructuralMerge merges tables recursively, keyed table arrays by identity,
// and appends other arrays; scalars from override win. Same rules as BMad's
// config_utils.structural_merge.
func StructuralMerge(base, override any) (any, error) {
	bm, bok := base.(map[string]any)
	om, ook := override.(map[string]any)
	if bok && ook {
		result := copyMap(bm)
		for k, v := range om {
			if existing, ok := result[k]; ok {
				merged, err := StructuralMerge(existing, v)
				if err != nil {
					return nil, err
				}
				result[k] = merged
			} else {
				result[k] = v
			}
		}
		return result, nil
	}
	bl, blok := base.([]any)
	ol, olok := override.([]any)
	if blok && olok {
		return mergeArrays(bl, ol)
	}
	return override, nil
}

// MergeLayers folds layers left to right with StructuralMerge.
func MergeLayers(layers ...map[string]any) (map[string]any, error) {
	merged := map[string]any{}
	for _, layer := range layers {
		out, err := StructuralMerge(merged, layer)
		if err != nil {
			return nil, err
		}
		merged = out.(map[string]any)
	}
	return merged, nil
}

// ExtractKey walks a dotted key ("agent.menu") through nested tables.
func ExtractKey(data map[string]any, dotted string) (any, bool) {
	var current any = data
	for _, part := range strings.Split(dotted, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// SelectKeys mimics the --key behaviour of the resolve_* scripts: with keys,
// return {key: value} for each key present; without keys, return everything.
func SelectKeys(merged map[string]any, keys []string) map[string]any {
	if len(keys) == 0 {
		return merged
	}
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := ExtractKey(merged, k); ok {
			out[k] = v
		}
	}
	return out
}

// ResolveCentralConfig merges the four central layers rooted at projectFS
// (which must contain "_bmad/config.toml").
func ResolveCentralConfig(projectFS fs.FS) (map[string]any, error) {
	paths := []struct {
		name     string
		required bool
	}{
		{"_bmad/config.toml", true},
		{"_bmad/config.user.toml", false},
		{"_bmad/custom/config.toml", false},
		{"_bmad/custom/config.user.toml", false},
	}
	layers := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		l, err := LoadTOML(projectFS, p.name, p.required)
		if err != nil {
			return nil, err
		}
		layers = append(layers, l)
	}
	return MergeLayers(layers...)
}

// ResolveCustomization merges a skill's customize.toml with team and user overrides.
// skillDir is the skill directory inside projectFS (e.g. ".claude/skills/bmad-prd").
func ResolveCustomization(projectFS fs.FS, skillDir string) (map[string]any, error) {
	name := skillDir[strings.LastIndex(skillDir, "/")+1:]
	base, err := LoadTOML(projectFS, skillDir+"/customize.toml", true)
	if err != nil {
		return nil, err
	}
	team, err := LoadTOML(projectFS, "_bmad/custom/"+name+".toml", false)
	if err != nil {
		return nil, err
	}
	user, err := LoadTOML(projectFS, "_bmad/custom/"+name+".user.toml", false)
	if err != nil {
		return nil, err
	}
	return MergeLayers(base, team, user)
}
