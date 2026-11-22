package httpapi_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/app"
	"github.com/caio-andres/bmad-business-assistant/internal/config"
)

type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newApp(t *testing.T, env map[string]string) (*httptest.Server, *app.App) {
	t.Helper()
	dir := t.TempDir()
	defaults := map[string]string{
		"DATA_DIR": dir, "AUTH_MODE": "local", "DEMO_DELAY": "0", "PYTHON_BIN": "off", "APP_ENV": "test",
		"REPO_LOCAL_DIR": filepath.Join("..", "..", "demo", "repos"), "RATE_LIMIT_PER_MINUTE": "1000",
		"LLM_BASE_URL": "", "DATABASE_URL": "", "PUBLISHER": "local", "SETUP_TOKEN": "",
	}
	for k, v := range env {
		defaults[k] = v
	}
	for k, v := range defaults {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Server.Handler())
	t.Cleanup(func() { srv.Close(); a.Close() })
	a.Cfg.BaseURL = srv.URL
	return srv, a
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, c: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("X-Requested-With", "alicerce")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if bp, ok := out.(*[]byte); ok {
			*bp = data
		} else if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
		}
	}
	return resp.StatusCode
}

// turn posts a message and collects SSE events.
func (c *client) turn(convID string, body map[string]string) (int, []map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+"/api/conversations/"+convID+"/turn", bytes.NewReader(b))
	req.Header.Set("X-Requested-With", "alicerce")
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, nil
	}
	var evs []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			var e map[string]any
			_ = json.Unmarshal([]byte(line[6:]), &e)
			evs = append(evs, e)
		}
	}
	return 200, evs
}

func countType(evs []map[string]any, typ string) int {
	n := 0
	for _, e := range evs {
		if e["type"] == typ {
			n++
		}
	}
	return n
}

func TestEndToEndLocalAuthAndDemoFlow(t *testing.T) {
	srv, _ := newApp(t, nil)
	admin := newClient(t, srv.URL)

	var sess map[string]any
	admin.do("GET", "/api/session", nil, &sess)
	if sess["user"] != nil || !sess["app"].(map[string]any)["needsSetup"].(bool) || !sess["app"].(map[string]any)["demo"].(bool) {
		t.Fatalf("initial session: %v", sess)
	}
	// CSRF: missing header is refused
	req, _ := http.NewRequest("POST", srv.URL+"/api/auth/setup", strings.NewReader(`{}`))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Fatalf("csrf: %d", resp.StatusCode)
	}
	if code := admin.do("GET", "/api/conversations", nil, nil); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code := admin.do("POST", "/api/auth/setup", map[string]string{"email": "admin@acme.example", "name": "Admin", "password": "curta"}, nil); code != 400 {
		t.Fatalf("weak password: %d", code)
	}
	if code := admin.do("POST", "/api/auth/setup", map[string]string{"email": "admin@acme.example", "name": "Admin Acme", "password": "senha-super-forte"}, nil); code != 201 {
		t.Fatalf("setup: %d", code)
	}
	if code := newClient(t, srv.URL).do("POST", "/api/auth/setup", map[string]string{"email": "x@acme.example", "name": "X", "password": "senha-super-forte"}, nil); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
	// invite a member
	var inv map[string]any
	if code := admin.do("POST", "/api/admin/invites", map[string]string{"email": "bia@acme.example"}, &inv); code != 201 {
		t.Fatalf("invite: %d %v", code, inv)
	}
	link := inv["link"].(string)
	token := link[strings.LastIndex(link, "/")+1:]
	bia := newClient(t, srv.URL)
	if code := bia.do("GET", "/api/auth/invite/"+token, nil, nil); code != 200 {
		t.Fatalf("invite info: %d", code)
	}
	if code := bia.do("POST", "/api/auth/accept-invite", map[string]string{"token": token, "name": "Bia Lima", "password": "outra-senha-forte"}, nil); code != 201 {
		t.Fatalf("accept: %d", code)
	}
	if code := bia.do("GET", "/api/admin/users", nil, nil); code != 403 {
		t.Fatalf("member reached admin: %d", code)
	}
	if code := newClient(t, srv.URL).do("POST", "/api/auth/accept-invite", map[string]string{"token": token, "name": "Z", "password": "outra-senha-forte"}, nil); code != 404 {
		t.Fatalf("invite reuse: %d", code)
	}

	// conversation + demo turns
	var cat map[string]any
	bia.do("GET", "/api/catalog", nil, &cat)
	if len(cat["items"].([]any)) < 8 {
		t.Fatalf("catalog: %v", cat)
	}
	var created struct {
		Conversation struct{ ID string } `json:"conversation"`
	}
	bia.do("POST", "/api/conversations", map[string]string{}, &created)
	cid := created.Conversation.ID
	code, evs := bia.turn(cid, map[string]string{"startSkill": "bmad-product-brief"})
	if code != 200 || countType(evs, "text") == 0 || countType(evs, "options") != 1 || countType(evs, "done") != 1 || countType(evs, "document") == 0 {
		t.Fatalf("first turn: %d %v", code, evs)
	}
	if code := admin.do("GET", "/api/conversations/"+cid, nil, nil); code != 404 {
		t.Fatalf("admin read another user's conversation: %d", code)
	}
	for i := 0; i < 6; i++ {
		if code, evs := bia.turn(cid, map[string]string{"text": "Resposta de teste número " + string(rune('1'+i))}); code != 200 || countType(evs, "done") != 1 {
			t.Fatalf("turn %d: %d %v", i, code, evs)
		}
	}
	var conv struct {
		Conversation struct {
			State, Title string
		} `json:"conversation"`
		Messages []map[string]any `json:"messages"`
		Files    []struct {
			Path, Name, Kind string
			Hidden           bool
		} `json:"files"`
	}
	bia.do("GET", "/api/conversations/"+cid, nil, &conv)
	if conv.Conversation.State != "idle" || conv.Conversation.Title != "Product Brief" || len(conv.Messages) != 14 {
		t.Fatalf("conversation: %+v", conv.Conversation)
	}
	brief := ""
	for _, f := range conv.Files {
		if strings.HasSuffix(f.Path, "brief.md") {
			brief = f.Path
			if f.Hidden || f.Kind != "markdown" {
				t.Fatalf("brief file info: %+v", f)
			}
		}
	}
	if brief == "" {
		t.Fatalf("files: %+v", conv.Files)
	}
	var raw []byte
	bia.do("GET", "/api/conversations/"+cid+"/files/raw?path="+url.QueryEscape(brief), nil, &raw)
	if !strings.Contains(string(raw), "status: complete") {
		t.Fatalf("brief: %s", raw)
	}
	// manual edit masks PII and keeps a version
	var saved map[string]any
	if code := bia.do("PUT", "/api/conversations/"+cid+"/files/raw?path="+url.QueryEscape(brief), string(raw)+"\nContato: joao@gmail.com\n", &saved); code != 200 || saved["masked"] == nil {
		t.Fatalf("save: %d %v", code, saved)
	}
	var vers map[string][]map[string]any
	bia.do("GET", "/api/conversations/"+cid+"/files/versions?path="+url.QueryEscape(brief), nil, &vers)
	if len(vers["versions"]) == 0 {
		t.Fatal("no versions")
	}
	// zip export
	var zipData []byte
	bia.do("GET", "/api/conversations/"+cid+"/export.zip", nil, &zipData)
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil || len(zr.File) < 2 {
		t.Fatalf("zip: %v", err)
	}
	// executive deck (fallback in demo)
	var deck map[string]any
	bia.do("POST", "/api/conversations/"+cid+"/deck", map[string]string{"path": brief}, &deck)
	if len(deck["slides"].([]any)) < 4 {
		t.Fatalf("deck: %v", deck)
	}
	// sandboxed preview
	var vu map[string]string
	bia.do("GET", "/api/conversations/"+cid+"/view-url?path="+url.QueryEscape(brief), nil, &vu)
	resp, err := http.Get(srv.URL + vu["url"])
	if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox allow-scripts") || strings.Contains(resp.Header.Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatalf("view: %v %v", err, resp.Header)
	}
	if resp, _ := http.Get(srv.URL + "/v/forjado.abc/" + brief); resp.StatusCode != 404 {
		t.Fatal("forged view token accepted")
	}

	// projects: create, link, publish, conflict, resolve, approve
	if code := bia.do("POST", "/api/projects", map[string]string{"sigla": "pagamentos", "name": "X"}, nil); code != 400 {
		t.Fatalf("bad sigla: %d", code)
	}
	var proj struct {
		Project struct{ ID, Slug string } `json:"project"`
	}
	if code := bia.do("POST", "/api/projects", map[string]any{"sigla": "PAG", "name": "Cashback no Pix", "systems": []string{"acme-pag-app-pix"}}, &proj); code != 201 || proj.Project.Slug != "cashback-no-pix" {
		t.Fatalf("project: %d %+v", code, proj)
	}
	var systems map[string][]map[string]any
	bia.do("GET", "/api/systems?sigla=PAG", nil, &systems)
	if len(systems["systems"]) != 2 {
		t.Fatalf("systems: %v", systems)
	}
	bia.do("PATCH", "/api/conversations/"+cid, map[string]string{"projectId": proj.Project.ID}, nil)
	var pub map[string]map[string]any
	if code := bia.do("POST", "/api/conversations/"+cid+"/publish", map[string]any{}, &pub); code != 200 || pub["result"]["status"] != "published" || pub["review"]["label"] != "Em revisão" {
		t.Fatalf("publish: %d %v", code, pub)
	}
	// admin loads the project in a new conversation and publishes a change
	var c2 struct {
		Conversation struct{ ID string } `json:"conversation"`
	}
	admin.do("POST", "/api/conversations", map[string]string{"projectId": proj.Project.ID}, &c2)
	var loaded map[string]any
	if code := admin.do("POST", "/api/conversations/"+c2.Conversation.ID+"/load", map[string]string{}, &loaded); code != 200 || loaded["loaded"].(float64) < 2 {
		t.Fatalf("load: %d %v", code, loaded)
	}
	admin.do("PUT", "/api/conversations/"+c2.Conversation.ID+"/files/raw?path="+url.QueryEscape(brief), "# Brief do admin\n", nil)
	if code := admin.do("POST", "/api/conversations/"+c2.Conversation.ID+"/publish", map[string]any{"files": []string{brief}}, &pub); code != 200 || pub["result"]["status"] != "published" {
		t.Fatalf("admin publish: %d %v", code, pub)
	}
	// Bia edits from the stale base: conflict
	bia.do("PUT", "/api/conversations/"+cid+"/files/raw?path="+url.QueryEscape(brief), "# Brief da Bia v2\n", nil)
	bia.do("POST", "/api/conversations/"+cid+"/publish", map[string]any{"files": []string{brief}}, &pub)
	if pub["result"]["status"] != "conflict" {
		t.Fatalf("expected conflict: %v", pub)
	}
	var res map[string]any
	if code := bia.do("POST", "/api/conversations/"+cid+"/resolve", map[string]string{"path": brief, "choice": "merge"}, &res); code != 200 || res["prompt"] == nil {
		t.Fatalf("resolve merge: %d %v", code, res)
	}
	bia.do("POST", "/api/conversations/"+cid+"/resolve", map[string]string{"path": brief, "choice": "mine"}, nil)
	if bia.do("POST", "/api/conversations/"+cid+"/publish", map[string]any{"files": []string{brief}}, &pub); pub["result"]["status"] != "published" {
		t.Fatalf("publish after resolve: %v", pub)
	}
	var appr map[string]map[string]any
	if code := admin.do("POST", "/api/admin/projects/"+proj.Project.ID+"/approve", nil, &appr); code != 200 || appr["review"]["label"] != "Aprovado" {
		t.Fatalf("approve: %d %v", code, appr)
	}
	var pd map[string]any
	bia.do("GET", "/api/projects/"+proj.Project.ID, nil, &pd)
	if len(pd["publications"].([]any)) < 3 {
		t.Fatalf("publications: %v", pd["publications"])
	}
	// admin views
	var usage map[string]any
	admin.do("GET", "/api/admin/usage?days=7", nil, &usage)
	if usage["total"].(map[string]any)["calls"].(float64) == 0 {
		t.Fatalf("usage: %v", usage)
	}
	var audit map[string][]map[string]any
	admin.do("GET", "/api/admin/audit", nil, &audit)
	actions := map[string]bool{}
	for _, e := range audit["entries"] {
		actions[e["action"].(string)] = true
	}
	for _, want := range []string{"setup_admin", "invite_created", "invite_accepted", "published", "publish_conflict", "project_approved", "pii_masked"} {
		if !actions[want] {
			t.Errorf("audit missing %s (%v)", want, actions)
		}
	}
	// logout
	bia.do("POST", "/api/auth/logout", nil, nil)
	if code := bia.do("GET", "/api/conversations", nil, nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

func TestDevAuthAndBusy(t *testing.T) {
	srv, _ := newApp(t, map[string]string{"AUTH_MODE": "dev", "DEMO_DELAY": "40ms"})
	c := newClient(t, srv.URL)
	var created struct {
		Conversation struct{ ID string } `json:"conversation"`
	}
	if code := c.do("POST", "/api/conversations", nil, &created); code != 201 {
		t.Fatalf("create: %d", code)
	}
	done := make(chan int)
	go func() {
		code, _ := c.turn(created.Conversation.ID, map[string]string{"startSkill": "bmad-prd"})
		done <- code
	}()
	time.Sleep(150 * time.Millisecond)
	if code, _ := c.turn(created.Conversation.ID, map[string]string{"text": "oi"}); code != 409 {
		t.Fatalf("concurrent turn: %d", code)
	}
	if code := <-done; code != 200 {
		t.Fatalf("first turn: %d", code)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
