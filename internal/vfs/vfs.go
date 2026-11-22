// Package vfs implements the virtual {project-root} each conversation sees:
// read-only mounts (BMad bundle, product context) and a writable workspace.
package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Root is the virtual project root ({project-root}).
const Root = "/project"

var (
	ErrNotFound = errors.New("arquivo não encontrado")
	ErrReadOnly = errors.New("área somente leitura")
	ErrOutside  = errors.New("caminho fora do projeto")
)

// Backend stores files addressed by slash-separated relative paths.
type Backend interface {
	Read(rel string) ([]byte, error)
	Write(rel string, data []byte) error
	Delete(rel string) error
	List() ([]string, error)
}

// Mount attaches a backend at a prefix below Root (e.g. "_bmad").
type Mount struct {
	Prefix   string
	Backend  Backend
	ReadOnly bool
}

// FS is a conversation's view of the project.
type FS struct {
	mounts []Mount
	// OnWrite is called after every successful write/delete in a writable mount.
	OnWrite func(abs string, deleted bool)
}

// New builds an FS; longer prefixes win when mounts overlap.
func New(mounts ...Mount) *FS {
	ms := append([]Mount{}, mounts...)
	sort.SliceStable(ms, func(i, j int) bool { return len(ms[i].Prefix) > len(ms[j].Prefix) })
	return &FS{mounts: ms}
}

var placeholderRe = regexp.MustCompile(`\{project-root\}`)

// Normalize resolves a user/model supplied path to an absolute virtual path.
// Relative paths are relative to Root. "{project-root}" is expanded.
func Normalize(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	p = placeholderRe.ReplaceAllString(p, Root)
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" {
		return "", fmt.Errorf("%w: caminho vazio", ErrOutside)
	}
	if !strings.HasPrefix(p, "/") {
		p = Root + "/" + p
	}
	// Reject traversal before cleaning so "/project/../etc" is caught.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %s", ErrOutside, p)
		}
	}
	clean := path.Clean(p)
	if clean != Root && !strings.HasPrefix(clean, Root+"/") {
		return "", fmt.Errorf("%w: %s", ErrOutside, p)
	}
	return clean, nil
}

func (f *FS) resolve(abs string) (*Mount, string, error) {
	rel := strings.TrimPrefix(strings.TrimPrefix(abs, Root), "/")
	for i := range f.mounts {
		m := &f.mounts[i]
		if rel == m.Prefix || strings.HasPrefix(rel, m.Prefix+"/") {
			inner := strings.TrimPrefix(strings.TrimPrefix(rel, m.Prefix), "/")
			return m, inner, nil
		}
	}
	return nil, "", fmt.Errorf("%w: %s", ErrNotFound, abs)
}

// Read returns a file's content.
func (f *FS) Read(p string) ([]byte, error) {
	abs, err := Normalize(p)
	if err != nil {
		return nil, err
	}
	m, inner, err := f.resolve(abs)
	if err != nil {
		return nil, err
	}
	data, err := m.Backend.Read(inner)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, abs)
	}
	return data, nil
}

// Write stores a file, refusing read-only mounts.
func (f *FS) Write(p string, data []byte) (string, error) {
	abs, err := Normalize(p)
	if err != nil {
		return "", err
	}
	m, inner, err := f.resolve(abs)
	if err != nil {
		return "", fmt.Errorf("%w: só é possível gravar documentos em %s/_bmad-output", ErrReadOnly, Root)
	}
	if m.ReadOnly || inner == "" {
		return "", fmt.Errorf("%w: %s", ErrReadOnly, abs)
	}
	if err := m.Backend.Write(inner, data); err != nil {
		return "", err
	}
	if f.OnWrite != nil {
		f.OnWrite(abs, false)
	}
	return abs, nil
}

// Delete removes a file from a writable mount.
func (f *FS) Delete(p string) error {
	abs, err := Normalize(p)
	if err != nil {
		return err
	}
	m, inner, err := f.resolve(abs)
	if err != nil {
		return err
	}
	if m.ReadOnly {
		return fmt.Errorf("%w: %s", ErrReadOnly, abs)
	}
	if err := m.Backend.Delete(inner); err != nil {
		return err
	}
	if f.OnWrite != nil {
		f.OnWrite(abs, true)
	}
	return nil
}

// Exists reports whether a file exists.
func (f *FS) Exists(p string) bool {
	_, err := f.Read(p)
	return err == nil
}

// Writable reports whether abs falls in a writable mount.
func (f *FS) Writable(p string) bool {
	abs, err := Normalize(p)
	if err != nil {
		return false
	}
	m, _, err := f.resolve(abs)
	return err == nil && !m.ReadOnly
}

// AllFiles lists every file as absolute virtual paths, sorted.
func (f *FS) AllFiles() ([]string, error) {
	seen := map[string]bool{}
	var out []string
	// Iterate shortest prefix first is irrelevant; mounts are disjoint in practice
	// except overlays, which we de-duplicate.
	for _, m := range f.mounts {
		files, err := m.Backend.List()
		if err != nil {
			return nil, err
		}
		for _, rel := range files {
			abs := Root + "/" + m.Prefix
			if rel != "" {
				abs += "/" + rel
			}
			// a deeper mount shadows paths of a shallower one
			if owner, _, err := f.resolve(abs); err == nil && owner.Prefix != m.Prefix {
				continue
			}
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Glob matches files against a pattern supporting *, ?, ** and {a,b}.
func (f *FS) Glob(pattern string) ([]string, error) {
	abs, err := Normalize(pattern)
	if err != nil {
		return nil, err
	}
	re, err := globRegexp(abs)
	if err != nil {
		return nil, err
	}
	files, err := f.AllFiles()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range files {
		if re.MatchString(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ListDir returns the immediate children (files and dirs, dirs with a trailing "/").
func (f *FS) ListDir(p string) ([]string, error) {
	abs, err := Normalize(p)
	if err != nil {
		return nil, err
	}
	files, err := f.AllFiles()
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(abs, "/") + "/"
	set := map[string]bool{}
	for _, file := range files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		rest := strings.TrimPrefix(file, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			set[rest[:i]+"/"] = true
		} else {
			set[rest] = true
		}
	}
	// mount points themselves are directories
	for _, m := range f.mounts {
		mp := Root + "/" + m.Prefix
		if strings.HasPrefix(mp, prefix) {
			rest := strings.TrimPrefix(mp, prefix)
			if i := strings.Index(rest, "/"); i >= 0 {
				rest = rest[:i]
			}
			set[rest+"/"] = true
		}
	}
	if len(set) == 0 && !f.Exists(abs) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, abs)
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '{':
			sb.WriteString("(?:")
		case '}':
			sb.WriteString(")")
		case ',':
			sb.WriteString("|")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

// --- backends ---------------------------------------------------------------

// FSBackend serves a read-only fs.FS subtree.
type FSBackend struct {
	FS   fs.FS
	Base string // directory inside FS, "." for root

	once  sync.Once
	files []string
}

func (b *FSBackend) full(rel string) string {
	if b.Base == "" || b.Base == "." {
		return rel
	}
	if rel == "" {
		return b.Base
	}
	return b.Base + "/" + rel
}

func (b *FSBackend) Read(rel string) ([]byte, error) { return fs.ReadFile(b.FS, b.full(rel)) }
func (b *FSBackend) Write(string, []byte) error      { return ErrReadOnly }
func (b *FSBackend) Delete(string) error             { return ErrReadOnly }
func (b *FSBackend) List() ([]string, error) {
	var err error
	b.once.Do(func() {
		root := b.full("")
		if root == "" {
			root = "."
		}
		err = fs.WalkDir(b.FS, root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			rel := p
			if root != "." {
				rel = strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
			}
			b.files = append(b.files, rel)
			return nil
		})
	})
	return b.files, err
}

// MapBackend is an in-memory backend (overlays, tests).
type MapBackend struct {
	mu    sync.RWMutex
	Files map[string][]byte
}

func NewMap(files map[string][]byte) *MapBackend {
	if files == nil {
		files = map[string][]byte{}
	}
	return &MapBackend{Files: files}
}

func (b *MapBackend) Read(rel string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	d, ok := b.Files[rel]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte{}, d...), nil
}
func (b *MapBackend) Write(rel string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Files[rel] = append([]byte{}, data...)
	return nil
}
func (b *MapBackend) Delete(rel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.Files[rel]; !ok {
		return ErrNotFound
	}
	delete(b.Files, rel)
	return nil
}
func (b *MapBackend) List() ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.Files))
	for k := range b.Files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Layered reads from the first layer that has the file; lists the union.
// Writes are refused (overlays are read-only views).
type Layered struct{ Layers []Backend }

func (l *Layered) Read(rel string) ([]byte, error) {
	for _, b := range l.Layers {
		if d, err := b.Read(rel); err == nil {
			return d, nil
		}
	}
	return nil, ErrNotFound
}
func (l *Layered) Write(string, []byte) error { return ErrReadOnly }
func (l *Layered) Delete(string) error        { return ErrReadOnly }
func (l *Layered) List() ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, b := range l.Layers {
		files, err := b.List()
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// AsFS exposes the project as an io/fs.FS rooted at Root (paths like "_bmad/config.toml").
func (f *FS) AsFS() fs.FS { return projectFS{f} }

type projectFS struct{ f *FS }

func (p projectFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	data, err := p.f.Read(Root + "/" + name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memFile{name: path.Base(name), data: data}, nil
}

func (p projectFS) ReadFile(name string) ([]byte, error) {
	data, err := p.f.Read(Root + "/" + name)
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return data, nil
}

type memFile struct {
	name string
	data []byte
	off  int
}

func (m *memFile) Stat() (fs.FileInfo, error) { return memInfo{m.name, int64(len(m.data))}, nil }
func (m *memFile) Close() error               { return nil }
func (m *memFile) Read(b []byte) (int, error) {
	if m.off >= len(m.data) {
		return 0, io.EOF
	}
	n := copy(b, m.data[m.off:])
	m.off += n
	return n, nil
}

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
