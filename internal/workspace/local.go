package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Local simulates the central repository in a folder:
//
//	<root>/main/<SIGLA>/<projeto>/...      default branch
//	<root>/branches/<branch>/<SIGLA>/...   publication branches
//	<root>/prs.json                        "pull requests" (open/merged)
//
// It lets the whole flow (load, publish, conflict, review, approval) run with
// no account. Approve merges a branch into main (admin action in the UI).
type Local struct {
	Root    string
	BaseURL string // to build "PR" links into the app

	mu sync.Mutex
}

type localPR struct {
	Branch   string    `json:"branch"`
	Title    string    `json:"title"`
	State    string    `json:"state"` // open | merged
	Created  time.Time `json:"created"`
	MergedAt time.Time `json:"mergedAt,omitempty"`
	Commits  []string  `json:"commits"`
}

func (l *Local) Name() string { return "local" }

func (l *Local) branchDir(branch string) string {
	if branch == "" || branch == "main" {
		return filepath.Join(l.Root, "main")
	}
	return filepath.Join(l.Root, "branches", filepath.FromSlash(strings.ReplaceAll(branch, "/", "__")))
}

func (l *Local) prs() (map[string]*localPR, error) {
	out := map[string]*localPR{}
	data, err := os.ReadFile(filepath.Join(l.Root, "prs.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(data, &out)
}

func (l *Local) savePRs(p map[string]*localPR) error {
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(filepath.Join(l.Root, "prs.json"), data, 0o644)
}

func (l *Local) prURL(branch string) string {
	return strings.TrimRight(l.BaseURL, "/") + "/#/revisao/" + strings.ReplaceAll(branch, "/", "~")
}

func readTree(dir string) (map[string]File, error) {
	out := map[string]File{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		out[rel] = File{Path: rel, SHA: BlobSHA(data), Content: data}
		return nil
	})
	return out, err
}

func (l *Local) status(prs map[string]*localPR, sigla, slug string) *ProjectStatus {
	b := BranchName(sigla, slug)
	st := &ProjectStatus{Status: StatusDraft, Branch: b}
	if pr, ok := prs[b]; ok {
		st.PRURL = l.prURL(b)
		if pr.State == "open" {
			st.Status = StatusInReview
		} else {
			st.Status = StatusApproved
		}
	}
	return st
}

func (l *Local) Status(_ context.Context, sigla, slug string) (*ProjectStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prs, err := l.prs()
	if err != nil {
		return nil, err
	}
	return l.status(prs, sigla, slug), nil
}

func (l *Local) Load(_ context.Context, sigla, slug string) (*Snapshot, error) {
	if err := ValidateProject(sigla, slug); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prs, err := l.prs()
	if err != nil {
		return nil, err
	}
	st := l.status(prs, sigla, slug)
	ref := "main"
	if st.Status == StatusInReview {
		ref = st.Branch
	}
	tree, err := readTree(filepath.Join(l.branchDir(ref), sigla, slug))
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Ref: ref, Status: st.Status, PRURL: st.PRURL}
	for _, f := range tree {
		snap.Files = append(snap.Files, f)
	}
	sortFiles(snap.Files)
	return snap, nil
}

func (l *Local) Publish(_ context.Context, req PublishRequest) (*PublishResult, error) {
	if err := ValidateProject(req.Sigla, req.Slug); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prs, err := l.prs()
	if err != nil {
		return nil, err
	}
	branch := BranchName(req.Sigla, req.Slug)
	pr, open := prs[branch]
	open = open && pr.State == "open"
	bdir := l.branchDir(branch)
	if !open {
		// fresh branch from main
		_ = os.RemoveAll(bdir)
		if err := copyDir(filepath.Join(l.branchDir("main"), req.Sigla, req.Slug), filepath.Join(bdir, req.Sigla, req.Slug)); err != nil {
			return nil, err
		}
	}
	projDir := filepath.Join(bdir, req.Sigla, req.Slug)
	remote, err := readTree(projDir)
	if err != nil {
		return nil, err
	}
	res := &PublishResult{Branch: branch}
	changed, conflicts := plan(req, remote)
	if len(conflicts) > 0 {
		res.Status, res.Conflicts = "conflict", conflicts
		return res, nil
	}
	if len(changed) == 0 {
		res.Status = "nochange"
		if open {
			res.PRURL = l.prURL(branch)
		}
		return res, nil
	}
	res.NewBase = map[string]string{}
	for _, p := range changed {
		dst := filepath.Join(projDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, req.Files[p], 0o644); err != nil {
			return nil, err
		}
		res.NewBase[p] = BlobSHA(req.Files[p])
	}
	msg := CommitMessage(req, changed)
	commit := BlobSHA([]byte(msg + time.Now().String()))
	if !open {
		title := req.Title
		if title == "" {
			title = "Projeto " + req.Sigla + "/" + req.Slug
		}
		pr = &localPR{Branch: branch, Title: title, State: "open", Created: time.Now()}
		prs[branch] = pr
	}
	pr.Commits = append(pr.Commits, commit+" "+strings.SplitN(msg, "\n", 2)[0])
	if err := l.savePRs(prs); err != nil {
		return nil, err
	}
	res.Status, res.Changed, res.CommitSHA, res.PRURL = "published", changed, commit, l.prURL(branch)
	return res, nil
}

// Approve merges the project's open branch into main (simulates the human PR review).
func (l *Local) Approve(_ context.Context, sigla, slug string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	prs, err := l.prs()
	if err != nil {
		return err
	}
	branch := BranchName(sigla, slug)
	pr, ok := prs[branch]
	if !ok || pr.State != "open" {
		return errors.New("não há publicação em revisão para aprovar")
	}
	src := filepath.Join(l.branchDir(branch), sigla, slug)
	dst := filepath.Join(l.branchDir("main"), sigla, slug)
	_ = os.RemoveAll(dst)
	if err := copyDir(src, dst); err != nil {
		return err
	}
	pr.State, pr.MergedAt = "merged", time.Now()
	return l.savePRs(prs)
}

func (l *Local) ReadShared(_ context.Context, p string) ([]byte, error) {
	c, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(l.branchDir("main"), filepath.FromSlash(c)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (l *Local) WriteShared(_ context.Context, p string, data []byte, _ string) error {
	c, err := CleanPath(p)
	if err != nil {
		return err
	}
	dst := filepath.Join(l.branchDir("main"), filepath.FromSlash(c))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func sortFiles(f []File) { sort.Slice(f, func(i, j int) bool { return f[i].Path < f[j].Path }) }
