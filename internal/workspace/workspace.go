// Package workspace shares project documents through a central versioned
// repository: <SIGLA>/<projeto>/ on GitHub (Git Data API, one technical
// identity, PR per project) or a local folder with the same layout.
package workspace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Review statuses shown to the business user.
const (
	StatusDraft    = "draft"     // never published
	StatusInReview = "in_review" // PR open
	StatusApproved = "approved"  // PR merged
)

var (
	siglaRe = regexp.MustCompile(`^[A-Z0-9]{3}$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

// Errors with business-friendly text.
var (
	ErrSigla = errors.New("A sigla deve ter 3 letras ou números, como PAG")
	ErrSlug  = errors.New("O nome do projeto deve usar letras minúsculas, números e hífens, como novo-cashback")
)

// ValidateProject checks the <SIGLA>/<projeto> convention.
func ValidateProject(sigla, slug string) error {
	if !siglaRe.MatchString(sigla) {
		return ErrSigla
	}
	if !slugRe.MatchString(slug) || len(slug) > 60 {
		return ErrSlug
	}
	return nil
}

// Slugify turns a project name into kebab-case.
func Slugify(name string) string {
	repl := strings.NewReplacer("á", "a", "à", "a", "ã", "a", "â", "a", "é", "e", "ê", "e", "í", "i", "ó", "o", "ô", "o", "õ", "o", "ú", "u", "ü", "u", "ç", "c")
	s := repl.Replace(strings.ToLower(name))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	return out
}

// BranchName is the publication branch of a project.
func BranchName(sigla, slug string) string {
	return "publish/" + strings.ToLower(sigla) + "/" + slug
}

// ProjectDir is the folder of a project in the central repository.
func ProjectDir(sigla, slug string) string { return sigla + "/" + slug }

// BlobSHA returns the git blob SHA-1 of content (same as `git hash-object`).
func BlobSHA(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// File is a document in the central repository (path relative to the project dir).
type File struct {
	Path    string `json:"path"`
	SHA     string `json:"sha"`
	Content []byte `json:"-"`
}

// Snapshot is what Load returns.
type Snapshot struct {
	Ref    string `json:"ref"` // branch the files came from
	Status string `json:"status"`
	PRURL  string `json:"prUrl,omitempty"`
	Files  []File `json:"files"`
}

// ProjectStatus is the review state of a project.
type ProjectStatus struct {
	Status string `json:"status"`
	PRURL  string `json:"prUrl,omitempty"`
	Branch string `json:"branch"`
}

// PublishRequest sends changed documents.
type PublishRequest struct {
	Sigla, Slug, Title string
	Files              map[string][]byte // path relative to project dir -> content
	Base               map[string]string // path -> blob SHA at load time ("" = new file)
	Force              map[string]bool   // paths the user chose to keep their version
	AuthorName         string
	AuthorEmail        string
	Message            string
}

// Conflict is a file changed remotely since load.
type Conflict struct {
	Path      string `json:"path"`
	TheirsSHA string `json:"theirsSha"`
	Theirs    string `json:"theirs"`
	Mine      string `json:"mine"`
}

// PublishResult is the outcome of Publish.
type PublishResult struct {
	Status    string            `json:"status"` // published | conflict | nochange
	Branch    string            `json:"branch"`
	PRURL     string            `json:"prUrl,omitempty"`
	CommitSHA string            `json:"commitSha,omitempty"`
	Changed   []string          `json:"changed,omitempty"`
	Conflicts []Conflict        `json:"conflicts,omitempty"`
	NewBase   map[string]string `json:"-"` // path -> blob SHA after publishing
}

// Publisher is implemented by the GitHub and local backends.
type Publisher interface {
	Name() string
	Load(ctx context.Context, sigla, slug string) (*Snapshot, error)
	Status(ctx context.Context, sigla, slug string) (*ProjectStatus, error)
	Publish(ctx context.Context, req PublishRequest) (*PublishResult, error)
	// ReadShared / WriteShared access files outside projects on the default
	// branch (e.g. _repos/<repo>.md summaries).
	ReadShared(ctx context.Context, p string) ([]byte, error)
	WriteShared(ctx context.Context, p string, data []byte, message string) error
}

// ErrNotFound means the file or project does not exist remotely.
var ErrNotFound = errors.New("não encontrado no repositório central")

// CommitMessage builds the message with the author trailer.
func CommitMessage(req PublishRequest, changed []string) string {
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = fmt.Sprintf("docs(%s/%s): atualiza %d documento(s)", req.Sigla, req.Slug, len(changed))
	}
	var b strings.Builder
	b.WriteString(msg + "\n\n")
	for _, c := range changed {
		b.WriteString("- " + c + "\n")
	}
	fmt.Fprintf(&b, "\nPublicado-por: %s <%s>\n", req.AuthorName, req.AuthorEmail)
	return b.String()
}

// plan compares requested files with the remote state.
func plan(req PublishRequest, remote map[string]File) (changed []string, conflicts []Conflict) {
	paths := make([]string, 0, len(req.Files))
	for p := range req.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		data := req.Files[p]
		mine := BlobSHA(data)
		r, exists := remote[p]
		if exists && r.SHA == mine {
			continue // identical
		}
		base := req.Base[p]
		remoteSHA := ""
		if exists {
			remoteSHA = r.SHA
		}
		if remoteSHA != base && !req.Force[p] {
			conflicts = append(conflicts, Conflict{Path: p, TheirsSHA: remoteSHA, Theirs: string(r.Content), Mine: string(data)})
			continue
		}
		changed = append(changed, p)
	}
	return changed, conflicts
}

// CleanPath validates a relative document path.
func CleanPath(p string) (string, error) {
	c := path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))[1:]
	if c == "" || c == "." || strings.HasPrefix(c, "../") || strings.Contains(c, "/../") {
		return "", fmt.Errorf("caminho inválido: %s", p)
	}
	return c, nil
}
