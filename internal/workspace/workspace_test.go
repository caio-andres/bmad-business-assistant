package workspace

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestValidateAndSHA(t *testing.T) {
	if err := ValidateProject("pagamentos", "x"); err != ErrSigla {
		t.Fatal("sigla")
	}
	if err := ValidateProject("PAG", "Novo Cashback"); err != ErrSlug {
		t.Fatal("slug")
	}
	if Slugify("Novo Cashback no Pix!") != "novo-cashback-no-pix" {
		t.Fatal(Slugify("Novo Cashback no Pix!"))
	}
	// git hash-object of "hello\n"
	if BlobSHA([]byte("hello\n")) != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatal("blob sha differs from git")
	}
}

// exercise runs the same scenario against any publisher.
func exercise(t *testing.T, p Publisher, approve func()) {
	ctx := context.Background()
	st, err := p.Status(ctx, "PAG", "cashback")
	if err != nil || st.Status != StatusDraft {
		t.Fatalf("initial status: %v %+v", err, st)
	}
	snap, err := p.Load(ctx, "PAG", "cashback")
	if err != nil || len(snap.Files) != 0 {
		t.Fatalf("empty load: %v %+v", err, snap)
	}
	req := PublishRequest{Sigla: "PAG", Slug: "cashback", AuthorName: "Ana", AuthorEmail: "ana@acme.example",
		Files: map[string][]byte{"planning-artifacts/prds/prd-x/prd.md": []byte("# PRD v1\n"), "planning-artifacts/briefs/b/brief.md": []byte("# Brief\n")},
		Base:  map[string]string{}}
	res, err := p.Publish(ctx, req)
	if err != nil || res.Status != "published" || len(res.Changed) != 2 || res.PRURL == "" {
		t.Fatalf("first publish: %v %+v", err, res)
	}
	if st, _ := p.Status(ctx, "PAG", "cashback"); st.Status != StatusInReview {
		t.Fatalf("status after publish: %+v", st)
	}
	// load comes from the PR branch
	snap, _ = p.Load(ctx, "PAG", "cashback")
	if len(snap.Files) != 2 || snap.Ref != BranchName("PAG", "cashback") {
		t.Fatalf("load from branch: %+v", snap)
	}
	base := map[string]string{}
	for _, f := range snap.Files {
		base[f.Path] = f.SHA
	}
	// Bia publishes a new PRD version
	bia := req
	bia.AuthorName, bia.Base = "Bia", base
	bia.Files = map[string][]byte{"planning-artifacts/prds/prd-x/prd.md": []byte("# PRD v2 (Bia)\n")}
	if res, err := p.Publish(ctx, bia); err != nil || res.Status != "published" || len(res.Changed) != 1 {
		t.Fatalf("bia publish: %v %+v", err, res)
	}
	// Ana publishes from the stale base: conflict, nothing written
	ana := req
	ana.Base = base
	ana.Files = map[string][]byte{"planning-artifacts/prds/prd-x/prd.md": []byte("# PRD v2 (Ana)\n"), "planning-artifacts/briefs/b/brief.md": []byte("# Brief\n")}
	res, err = p.Publish(ctx, ana)
	if err != nil || res.Status != "conflict" || len(res.Conflicts) != 1 || !strings.Contains(res.Conflicts[0].Theirs, "Bia") {
		t.Fatalf("conflict: %v %+v", err, res)
	}
	snap, _ = p.Load(ctx, "PAG", "cashback")
	for _, f := range snap.Files {
		if strings.Contains(string(f.Content), "Ana") {
			t.Fatal("conflicting publish wrote data")
		}
	}
	// Ana keeps her version explicitly
	ana.Force = map[string]bool{"planning-artifacts/prds/prd-x/prd.md": true}
	if res, err := p.Publish(ctx, ana); err != nil || res.Status != "published" || len(res.Changed) != 1 {
		t.Fatalf("forced publish: %v %+v", err, res)
	}
	if res, _ := p.Publish(ctx, ana); res.Status != "nochange" {
		t.Fatalf("republish identical: %+v", res)
	}
	if approve != nil {
		approve()
		if st, _ := p.Status(ctx, "PAG", "cashback"); st.Status != StatusApproved {
			t.Fatalf("approved status: %+v", st)
		}
		snap, _ = p.Load(ctx, "PAG", "cashback")
		if snap.Ref == BranchName("PAG", "cashback") || len(snap.Files) != 2 {
			t.Fatalf("load after approval should come from default branch: %+v", snap)
		}
	}
	if err := p.WriteShared(ctx, "_repos/acme-pag-app-pix.md", []byte("resumo"), "docs: resumo"); err != nil {
		t.Fatal(err)
	}
	if d, err := p.ReadShared(ctx, "_repos/acme-pag-app-pix.md"); err != nil || string(d) != "resumo" {
		t.Fatalf("shared: %v %s", err, d)
	}
	if _, err := p.ReadShared(ctx, "_repos/nao-existe.md"); err != ErrNotFound {
		t.Fatalf("missing shared: %v", err)
	}
}

func TestLocalPublisher(t *testing.T) {
	l := &Local{Root: t.TempDir(), BaseURL: "http://localhost:8080"}
	exercise(t, l, func() {
		if err := l.Approve(context.Background(), "PAG", "cashback"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGitHubPublisherAgainstFake(t *testing.T) {
	f := newFakeGitHub()
	srv := httptest.NewServer(f)
	defer srv.Close()
	g := &GitHub{APIURL: srv.URL, Token: "t", Repo: "acme/bmad-workspace", BaseBranch: "main"}
	exercise(t, g, func() { f.merge(BranchName("PAG", "cashback")) })
	if !strings.Contains(f.lastMessage, "Publicado-por: Ana <ana@acme.example>") {
		t.Fatalf("trailer missing: %q", f.lastMessage)
	}
	if f.auth != "Bearer t" {
		t.Fatal("token not sent")
	}
}

// --- minimal in-memory GitHub -----------------------------------------------------------

type fakeCommit struct {
	Tree    string
	Parents []string
	Message string
}

type fakeGitHub struct {
	mu          sync.Mutex
	refs        map[string]string            // branch -> commit
	commits     map[string]fakeCommit        // sha -> commit
	trees       map[string]map[string]string // sha -> path -> blob
	blobs       map[string][]byte
	prs         []map[string]any
	lastMessage string
	auth        string
}

func hash(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func newFakeGitHub() *fakeGitHub {
	f := &fakeGitHub{refs: map[string]string{}, commits: map[string]fakeCommit{}, trees: map[string]map[string]string{}, blobs: map[string][]byte{}}
	f.trees["t0"] = map[string]string{}
	f.commits["c0"] = fakeCommit{Tree: "t0"}
	f.refs["main"] = "c0"
	return f
}

func (f *fakeGitHub) putTree(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k+"="+m[k])
	}
	sort.Strings(keys)
	sha := hash(append([]string{"tree"}, keys...)...)
	f.trees[sha] = m
	return sha
}

func (f *fakeGitHub) merge(branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs["main"] = f.refs[branch]
	for _, pr := range f.prs {
		if pr["head"] == branch && pr["state"] == "open" {
			pr["state"], pr["merged_at"] = "closed", "2026-11-22T00:00:00Z"
		}
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	p := strings.TrimPrefix(r.URL.Path, "/repos/acme/bmad-workspace")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == "GET" && strings.HasPrefix(p, "/git/ref/heads/"):
		b := strings.TrimPrefix(p, "/git/ref/heads/")
		sha, ok := f.refs[b]
		if !ok {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		reply(200, map[string]any{"object": map[string]string{"sha": sha}})
	case r.Method == "POST" && p == "/git/refs":
		f.refs[strings.TrimPrefix(body["ref"].(string), "refs/heads/")] = body["sha"].(string)
		reply(201, map[string]any{})
	case r.Method == "PATCH" && strings.HasPrefix(p, "/git/refs/heads/"):
		b := strings.TrimPrefix(p, "/git/refs/heads/")
		newSHA := body["sha"].(string)
		if force, _ := body["force"].(bool); !force {
			// fast-forward only
			if c := f.commits[newSHA]; len(c.Parents) == 0 || c.Parents[0] != f.refs[b] {
				reply(422, map[string]string{"message": "not a fast forward"})
				return
			}
		}
		f.refs[b] = newSHA
		reply(200, map[string]any{})
	case r.Method == "GET" && p == "/pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")
		state := r.URL.Query().Get("state")
		var out []map[string]any
		for _, pr := range f.prs {
			if pr["head"] == head && pr["state"] == state {
				out = append(out, pr)
			}
		}
		reply(200, out)
	case r.Method == "POST" && p == "/pulls":
		pr := map[string]any{"number": len(f.prs) + 1, "head": body["head"], "state": "open", "merged_at": nil,
			"html_url": fmt.Sprintf("https://github.example/acme/bmad-workspace/pull/%d", len(f.prs)+1)}
		f.prs = append(f.prs, pr)
		reply(201, pr)
	case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
		c := f.commits[strings.TrimPrefix(p, "/git/trees/")]
		var entries []map[string]string
		for path, sha := range f.trees[c.Tree] {
			entries = append(entries, map[string]string{"path": path, "type": "blob", "sha": sha, "mode": "100644"})
		}
		reply(200, map[string]any{"tree": entries})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/blobs/"):
		reply(200, map[string]string{"content": base64.StdEncoding.EncodeToString(f.blobs[strings.TrimPrefix(p, "/git/blobs/")]), "encoding": "base64"})
	case r.Method == "POST" && p == "/git/blobs":
		data, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		sha := BlobSHA(data)
		f.blobs[sha] = data
		reply(201, map[string]string{"sha": sha})
	case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
		c := f.commits[strings.TrimPrefix(p, "/git/commits/")]
		reply(200, map[string]any{"sha": strings.TrimPrefix(p, "/git/commits/"), "tree": map[string]string{"sha": c.Tree}})
	case r.Method == "POST" && p == "/git/trees":
		m := map[string]string{}
		for k, v := range f.trees[body["base_tree"].(string)] {
			m[k] = v
		}
		for _, e := range body["tree"].([]any) {
			en := e.(map[string]any)
			m[en["path"].(string)] = en["sha"].(string)
		}
		reply(201, map[string]string{"sha": f.putTree(m)})
	case r.Method == "POST" && p == "/git/commits":
		var parents []string
		for _, x := range body["parents"].([]any) {
			parents = append(parents, x.(string))
		}
		msg := body["message"].(string)
		sha := hash("commit", body["tree"].(string), strings.Join(parents, ","), msg)
		f.commits[sha] = fakeCommit{Tree: body["tree"].(string), Parents: parents, Message: msg}
		f.lastMessage = msg
		reply(201, map[string]string{"sha": sha})
	case strings.HasPrefix(p, "/contents/"):
		path := strings.TrimPrefix(p, "/contents/")
		tree := f.trees[f.commits[f.refs["main"]].Tree]
		if r.Method == "GET" {
			sha, ok := tree[path]
			if !ok {
				reply(404, map[string]string{"message": "Not Found"})
				return
			}
			reply(200, map[string]string{"sha": sha, "content": base64.StdEncoding.EncodeToString(f.blobs[sha])})
			return
		}
		data, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		sha := BlobSHA(data)
		f.blobs[sha] = data
		m := map[string]string{}
		for k, v := range tree {
			m[k] = v
		}
		m[path] = sha
		parent := f.refs["main"]
		c := hash("commit", path, sha, parent)
		f.commits[c] = fakeCommit{Tree: f.putTree(m), Parents: []string{parent}}
		f.refs["main"] = c
		reply(200, map[string]any{})
	default:
		reply(404, map[string]string{"message": "unhandled " + r.Method + " " + p})
	}
}
