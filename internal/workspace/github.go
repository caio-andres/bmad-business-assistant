package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GitHub publishes through the REST Git Data API with one technical identity
// (GitHub App installation token or a fine-grained service token).
type GitHub struct {
	APIURL     string // https://api.github.com or GHES https://host/api/v3
	Token      string
	Repo       string // owner/name
	BaseBranch string
	HTTPClient *http.Client
}

func (g *GitHub) Name() string { return "github" }

// APIError is a non-2xx GitHub response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Body) }

func (g *GitHub) base() string {
	if g.BaseBranch == "" {
		return "main"
	}
	return g.BaseBranch
}

func (g *GitHub) do(ctx context.Context, method, p string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	api := strings.TrimRight(g.APIURL, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, api+p, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "alicerce-publisher")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := g.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if resp.StatusCode >= 300 {
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &APIError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

func (g *GitHub) repoPath(p string) string { return "/repos/" + g.Repo + p }

func (g *GitHub) refSHA(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := g.do(ctx, "GET", g.repoPath("/git/ref/heads/"+escapeRef(branch)), nil, &ref)
	return ref.Object.SHA, err
}

func escapeRef(b string) string {
	parts := strings.Split(b, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

type ghPR struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
}

func (g *GitHub) owner() string { return strings.SplitN(g.Repo, "/", 2)[0] }

func (g *GitHub) prs(ctx context.Context, branch, state string) ([]ghPR, error) {
	var out []ghPR
	q := url.Values{"head": {g.owner() + ":" + branch}, "state": {state}, "per_page": {"20"}}
	err := g.do(ctx, "GET", g.repoPath("/pulls?"+q.Encode()), nil, &out)
	return out, err
}

func (g *GitHub) Status(ctx context.Context, sigla, slug string) (*ProjectStatus, error) {
	b := BranchName(sigla, slug)
	st := &ProjectStatus{Status: StatusDraft, Branch: b}
	open, err := g.prs(ctx, b, "open")
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		st.Status, st.PRURL = StatusInReview, open[0].HTMLURL
		return st, nil
	}
	closed, err := g.prs(ctx, b, "closed")
	if err != nil {
		return nil, err
	}
	for _, pr := range closed {
		if pr.MergedAt != nil {
			st.Status, st.PRURL = StatusApproved, pr.HTMLURL
			break
		}
	}
	return st, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha,omitempty"`
}

// tree lists blobs under prefix at a commit.
func (g *GitHub) tree(ctx context.Context, commitSHA, prefix string) (map[string]File, error) {
	var t struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/git/trees/"+commitSHA+"?recursive=1"), nil, &t); err != nil {
		return nil, err
	}
	out := map[string]File{}
	pre := strings.TrimSuffix(prefix, "/") + "/"
	for _, e := range t.Tree {
		if e.Type == "blob" && strings.HasPrefix(e.Path, pre) {
			rel := strings.TrimPrefix(e.Path, pre)
			out[rel] = File{Path: rel, SHA: e.SHA}
		}
	}
	return out, nil
}

func (g *GitHub) blob(ctx context.Context, sha string) ([]byte, error) {
	var b struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/git/blobs/"+sha), nil, &b); err != nil {
		return nil, err
	}
	if b.Encoding != "base64" {
		return []byte(b.Content), nil
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(b.Content, "\n", ""))
}

func (g *GitHub) Load(ctx context.Context, sigla, slug string) (*Snapshot, error) {
	if err := ValidateProject(sigla, slug); err != nil {
		return nil, err
	}
	st, err := g.Status(ctx, sigla, slug)
	if err != nil {
		return nil, err
	}
	ref := g.base()
	if st.Status == StatusInReview {
		ref = st.Branch
	}
	head, err := g.refSHA(ctx, ref)
	if err != nil {
		return nil, err
	}
	tree, err := g.tree(ctx, head, ProjectDir(sigla, slug))
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Ref: ref, Status: st.Status, PRURL: st.PRURL}
	for _, f := range tree {
		data, err := g.blob(ctx, f.SHA)
		if err != nil {
			return nil, err
		}
		f.Content = data
		snap.Files = append(snap.Files, f)
	}
	sort.Slice(snap.Files, func(i, j int) bool { return snap.Files[i].Path < snap.Files[j].Path })
	return snap, nil
}

func (g *GitHub) Publish(ctx context.Context, req PublishRequest) (*PublishResult, error) {
	if err := ValidateProject(req.Sigla, req.Slug); err != nil {
		return nil, err
	}
	branch := BranchName(req.Sigla, req.Slug)
	baseHead, err := g.refSHA(ctx, g.base())
	if err != nil {
		return nil, fmt.Errorf("branch padrão: %w", err)
	}
	open, err := g.prs(ctx, branch, "open")
	if err != nil {
		return nil, err
	}
	head, err := g.refSHA(ctx, branch)
	switch {
	case isStatus(err, 404):
		if err := g.do(ctx, "POST", g.repoPath("/git/refs"), map[string]string{"ref": "refs/heads/" + branch, "sha": baseHead}, nil); err != nil {
			return nil, err
		}
		head = baseHead
	case err != nil:
		return nil, err
	case len(open) == 0 && head != baseHead:
		// stale branch from a closed/merged PR: restart from the default branch
		if err := g.do(ctx, "PATCH", g.repoPath("/git/refs/heads/"+escapeRef(branch)), map[string]any{"sha": baseHead, "force": true}, nil); err != nil {
			return nil, err
		}
		head = baseHead
	}
	dir := ProjectDir(req.Sigla, req.Slug)
	remote, err := g.tree(ctx, head, dir)
	if err != nil {
		return nil, err
	}
	res := &PublishResult{Branch: branch}
	changed, conflicts := plan(req, remote)
	if len(conflicts) > 0 {
		for i := range conflicts {
			if conflicts[i].TheirsSHA != "" {
				if data, err := g.blob(ctx, conflicts[i].TheirsSHA); err == nil {
					conflicts[i].Theirs = string(data)
				}
			}
		}
		res.Status, res.Conflicts = "conflict", conflicts
		return res, nil
	}
	if len(changed) == 0 {
		res.Status = "nochange"
		if len(open) > 0 {
			res.PRURL = open[0].HTMLURL
		}
		return res, nil
	}
	var commit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/git/commits/"+head), nil, &commit); err != nil {
		return nil, err
	}
	var entries []treeEntry
	res.NewBase = map[string]string{}
	for _, p := range changed {
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := g.do(ctx, "POST", g.repoPath("/git/blobs"), map[string]string{"content": base64.StdEncoding.EncodeToString(req.Files[p]), "encoding": "base64"}, &blob); err != nil {
			return nil, err
		}
		entries = append(entries, treeEntry{Path: dir + "/" + p, Mode: "100644", Type: "blob", SHA: blob.SHA})
		res.NewBase[p] = blob.SHA
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, "POST", g.repoPath("/git/trees"), map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}, &tree); err != nil {
		return nil, err
	}
	var newCommit struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, "POST", g.repoPath("/git/commits"), map[string]any{"message": CommitMessage(req, changed), "tree": tree.SHA, "parents": []string{head}}, &newCommit); err != nil {
		return nil, err
	}
	if err := g.do(ctx, "PATCH", g.repoPath("/git/refs/heads/"+escapeRef(branch)), map[string]any{"sha": newCommit.SHA, "force": false}, nil); err != nil {
		if isStatus(err, 422) {
			return nil, errors.New("o projeto foi publicado por outra pessoa neste instante; carregue de novo e tente outra vez")
		}
		return nil, err
	}
	res.Status, res.Changed, res.CommitSHA = "published", changed, newCommit.SHA
	if len(open) > 0 {
		res.PRURL = open[0].HTMLURL
		return res, nil
	}
	title := req.Title
	if title == "" {
		title = "Projeto " + req.Sigla + "/" + req.Slug
	}
	body := fmt.Sprintf("Documentos de negócio do projeto **%s/%s**, publicados pelo assistente.\n\nRevisão obrigatória do time técnico (CODEOWNERS). O assistente nunca faz merge.\n\nPublicado-por: %s <%s>", req.Sigla, req.Slug, req.AuthorName, req.AuthorEmail)
	var pr ghPR
	if err := g.do(ctx, "POST", g.repoPath("/pulls"), map[string]any{"title": title, "head": branch, "base": g.base(), "body": body}, &pr); err != nil {
		return nil, err
	}
	res.PRURL = pr.HTMLURL
	return res, nil
}

func (g *GitHub) ReadShared(ctx context.Context, p string) ([]byte, error) {
	c, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	var f struct {
		Content string `json:"content"`
	}
	err = g.do(ctx, "GET", g.repoPath("/contents/"+escapeRef(c)+"?ref="+url.QueryEscape(g.base())), nil, &f)
	if isStatus(err, 404) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

func (g *GitHub) WriteShared(ctx context.Context, p string, data []byte, message string) error {
	c, err := CleanPath(p)
	if err != nil {
		return err
	}
	var cur struct {
		SHA string `json:"sha"`
	}
	err = g.do(ctx, "GET", g.repoPath("/contents/"+escapeRef(c)+"?ref="+url.QueryEscape(g.base())), nil, &cur)
	if err != nil && !isStatus(err, 404) {
		return err
	}
	body := map[string]any{"message": message, "content": base64.StdEncoding.EncodeToString(data), "branch": g.base()}
	if cur.SHA != "" {
		body["sha"] = cur.SHA
	}
	return g.do(ctx, "PUT", g.repoPath("/contents/"+escapeRef(c)), body, nil)
}
