package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caio-andres/bmad-business-assistant/internal/events"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/orchestrator"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

func withUser(r *http.Request, u *store.User) context.Context {
	return context.WithValue(r.Context(), userKey, u)
}

func parseInt(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }
func itoa(n int) string              { return strconv.Itoa(n) }

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Svc.Catalog)
}

// conv loads a conversation owned by the current user or writes 404.
func (s *Server) conv(w http.ResponseWriter, r *http.Request) *store.Conversation {
	c, err := s.Svc.Conversation(r.Context(), s.user(r), r.PathValue("id"))
	if err != nil {
		jsonError(w, 404, orchestrator.ErrNotFound.Error())
		return nil
	}
	return c
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListConversations(r.Context(), s.user(r).ID, r.URL.Query().Get("archived") == "1")
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []*store.Conversation{}
	}
	writeJSON(w, 200, map[string]any{"conversations": list})
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title     string `json:"title"`
		ProjectID string `json:"projectId"`
	}
	_ = readJSON(r, &in)
	c := &store.Conversation{UserID: s.user(r).ID, Title: strings.TrimSpace(in.Title)}
	if c.Title == "" {
		c.Title = "Nova conversa"
	}
	if in.ProjectID != "" {
		if _, err := s.Store.GetProject(r.Context(), in.ProjectID); err != nil {
			jsonError(w, 404, "Projeto não encontrado.")
			return
		}
		c.ProjectID = in.ProjectID
	}
	if err := s.Store.CreateConversation(r.Context(), c); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"conversation": c})
}

type fileInfo struct {
	store.WorkspaceFile
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Hidden bool   `json:"hidden"`
}

func hiddenPath(p string) bool {
	return strings.HasPrefix(p, ".view/") || strings.HasPrefix(p, ".conflicts/") || strings.HasSuffix(p, ".memlog.md") || strings.Contains(p, "/.")
}

func (s *Server) files(ctx context.Context, convID string) ([]fileInfo, error) {
	list, err := s.Store.ListFiles(ctx, convID)
	if err != nil {
		return nil, err
	}
	out := make([]fileInfo, 0, len(list))
	for _, f := range list {
		out = append(out, fileInfo{WorkspaceFile: f, Name: runtime.FriendlyName(f.Path), Kind: runtime.DocKind(f.Path), Hidden: hiddenPath(f.Path)})
	}
	return out, nil
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	msgs, err := s.Store.ListMessages(r.Context(), c.ID)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	files, _ := s.files(r.Context(), c.ID)
	out := map[string]any{"conversation": c, "messages": nonNil(msgs), "files": files}
	if c.ProjectID != "" {
		if p, err := s.Store.GetProject(r.Context(), c.ProjectID); err == nil {
			out["project"] = p
		}
	}
	writeJSON(w, 200, out)
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Server) patchConversation(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		Title     *string `json:"title"`
		Archived  *bool   `json:"archived"`
		ProjectID *string `json:"projectId"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || utf8.RuneCountInString(t) > 120 {
			jsonError(w, 400, "O título deve ter entre 1 e 120 caracteres.")
			return
		}
		c.Title = t
	}
	if in.Archived != nil {
		c.Archived = *in.Archived
	}
	if in.ProjectID != nil {
		if *in.ProjectID != "" {
			if _, err := s.Store.GetProject(r.Context(), *in.ProjectID); err != nil {
				jsonError(w, 404, "Projeto não encontrado.")
				return
			}
		}
		if *in.ProjectID != c.ProjectID {
			_ = s.Store.SetBases(r.Context(), c.ID, nil, true)
		}
		c.ProjectID = *in.ProjectID
	}
	if err := s.Store.UpdateConversationMeta(r.Context(), c); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"conversation": c})
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	if c.Busy {
		jsonError(w, 409, orchestrator.ErrBusy.Error())
		return
	}
	if err := s.Store.DeleteConversation(r.Context(), c.ID); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	s.audit(r, "conversation_deleted", c.ID, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- turn (SSE) ---------------------------------------------------------------------------

func statusFor(err error) int {
	switch {
	case errors.Is(err, orchestrator.ErrBusy):
		return 409
	case errors.Is(err, orchestrator.ErrQuota):
		return 429
	case errors.Is(err, orchestrator.ErrBlocked):
		return 422
	case errors.Is(err, orchestrator.ErrNotFound):
		return 404
	default:
		return 400
	}
}

func (s *Server) turn(w http.ResponseWriter, r *http.Request) {
	u := s.user(r)
	var in struct {
		Text       string `json:"text"`
		StartSkill string `json:"startSkill"`
		StartAgent string `json:"startAgent"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if !s.turnLimiter.allow("turn:" + u.ID) {
		jsonError(w, 429, "Muitas mensagens em pouco tempo. Aguarde alguns segundos.")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "streaming não suportado")
		return
	}
	req := orchestrator.TurnRequest{User: u, ConversationID: r.PathValue("id"), Text: in.Text, StartSkill: in.StartSkill, StartAgent: in.StartAgent, IP: s.clientIP(r)}
	ch := make(chan events.Event, 512)
	gone := r.Context().Done()
	emit := func(e events.Event) {
		select {
		case ch <- e:
		case <-gone:
		}
	}
	errCh := make(chan error, 1)
	// The turn runs detached from the request: closing the tab does not stop it.
	timeout := s.Cfg.TurnTimeout + time.Minute
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
		defer cancel()
		defer func() {
			if v := recover(); v != nil {
				s.Logger.Error("turn panic", "err", v)
				errCh <- fmt.Errorf("erro interno")
			}
		}()
		errCh <- s.Svc.RunTurn(ctx, req, emit)
	}()

	var first *events.Event
	select {
	case err := <-errCh:
		if err != nil {
			jsonError(w, statusFor(err), err.Error())
			return
		}
		errCh <- nil
	case e := <-ch:
		first = &e
	case <-gone:
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	send := func(e events.Event) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if first != nil && !send(*first) {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e := <-ch:
			if !send(e) {
				return
			}
		case err := <-errCh:
			// drain what is left
			for {
				select {
				case e := <-ch:
					send(e)
					continue
				default:
				}
				break
			}
			if err != nil {
				send(events.Event{Type: events.Error, Text: err.Error(), Code: "turn_failed"})
			}
			send(events.Event{Type: events.Done})
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-gone:
			return
		}
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	writeJSON(w, 200, map[string]bool{"cancelled": s.Svc.Cancel(c.ID)})
}

func (s *Server) endSkill(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.EndSkill(r.Context(), s.user(r), r.PathValue("id")); err != nil {
		jsonError(w, statusFor(err), err.Error())
		return
	}
	c, _ := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"conversation": c})
}

// --- files ----------------------------------------------------------------------------------

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	files, err := s.files(r.Context(), c.ID)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"files": files})
}

func filePath(r *http.Request) (string, error) {
	return workspace.CleanPath(r.URL.Query().Get("path"))
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".yaml", ".yml", ".toml", ".txt", "":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func (s *Server) rawFile(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	p, err := filePath(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	data, err := s.Store.ReadFile(r.Context(), c.ID, p)
	if err != nil {
		jsonError(w, 404, "Documento não encontrado.")
		return
	}
	ct := contentType(p)
	if strings.HasPrefix(ct, "text/html") {
		ct = "text/plain; charset=utf-8" // never render HTML on the app origin
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	}
	w.Write(data)
}

func (s *Server) saveFile(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	if c.Busy {
		jsonError(w, 409, "Aguarde a resposta terminar antes de editar.")
		return
	}
	p, err := filePath(r)
	if err != nil || hiddenPath(p) {
		jsonError(w, 400, "Caminho inválido.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.Cfg.MaxUploadBytes+1))
	if err != nil || int64(len(body)) > s.Cfg.MaxUploadBytes {
		jsonError(w, 413, "Documento grande demais.")
		return
	}
	if !utf8.Valid(body) {
		jsonError(w, 400, "O documento precisa ser texto (UTF-8).")
		return
	}
	scan := s.Svc.Guard.Scan(string(body))
	if scan.Blocked {
		jsonError(w, 422, orchestrator.ErrBlocked.Error())
		return
	}
	if err := s.Store.WriteFile(r.Context(), c.ID, p, []byte(scan.Text), "user"); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	out := map[string]any{"ok": true}
	if scan.Any() {
		out["masked"] = scan.Summary()
		s.audit(r, "pii_masked", c.ID, scan.Summary())
	}
	writeJSON(w, 200, out)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	p, err := filePath(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if err := s.Store.DeleteFile(r.Context(), c.ID, p); err != nil {
		jsonError(w, 404, "Documento não encontrado.")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	p, err := filePath(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	v, err := s.Store.ListVersions(r.Context(), c.ID, p)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"versions": nonNil(v)})
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	_, data, err := s.Store.ReadVersion(r.Context(), c.ID, r.PathValue("vid"))
	if err != nil {
		jsonError(w, 404, "Versão não encontrada.")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

var uploadExt = map[string]bool{".md": true, ".txt": true, ".csv": true, ".json": true, ".yaml": true, ".yml": true, ".html": true}

// upload stores reference material under _bmad-output/inputs/ (text only;
// the browser converts .docx/.pdf to text before sending).
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxUploadBytes*2)
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 413, "Arquivo grande demais ou inválido.")
		return
	}
	name := workspace.Slugify(strings.TrimSuffix(path.Base(in.Name), path.Ext(in.Name)))
	ext := strings.ToLower(path.Ext(in.Name))
	if name == "" || !uploadExt[ext] {
		if ext == ".docx" || ext == ".pdf" {
			ext = ".md"
		} else {
			jsonError(w, 400, "Formato não suportado. Envie .md, .txt, .csv, .json, .docx ou .pdf.")
			return
		}
	}
	if int64(len(in.Content)) > s.Cfg.MaxUploadBytes {
		jsonError(w, 413, "Arquivo grande demais.")
		return
	}
	scan := s.Svc.Guard.Scan(in.Content)
	if scan.Blocked {
		jsonError(w, 422, orchestrator.ErrBlocked.Error())
		return
	}
	p := "inputs/" + name + ext
	if err := s.Store.WriteFile(r.Context(), c.ID, p, []byte(scan.Text), "user"); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	out := map[string]any{"path": p, "name": runtime.FriendlyName(p)}
	if scan.Any() {
		out["masked"] = scan.Summary()
		s.audit(r, "pii_masked", c.ID, "upload "+scan.Summary())
	}
	s.audit(r, "upload", c.ID, p)
	writeJSON(w, 201, out)
}

func (s *Server) exportZip(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	files, err := s.Store.ListFiles(r.Context(), c.ID)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	name := workspace.Slugify(c.Title)
	if name == "" {
		name = "documentos"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	zw := zip.NewWriter(w)
	for _, f := range files {
		if strings.HasPrefix(f.Path, ".view/") || strings.HasPrefix(f.Path, ".conflicts/") {
			continue
		}
		data, err := s.Store.ReadFile(r.Context(), c.ID, f.Path)
		if err != nil {
			continue
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: f.UpdatedAt})
		if err == nil {
			fw.Write(data)
		}
	}
	zw.Close()
	s.audit(r, "export_zip", c.ID, itoa(len(files))+" arquivos")
}

// --- sandboxed HTML preview ---------------------------------------------------------------

func (s *Server) viewURL(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	p, err := filePath(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"url": "/v/" + s.viewToken(c.ID, 2*time.Hour) + "/" + p})
}

const sandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals; default-src 'none'; script-src 'unsafe-inline' https:; style-src 'unsafe-inline' https:; img-src data: blob: https: 'self'; font-src data: https:; media-src data: https:; connect-src 'none'; form-action 'none'; frame-ancestors 'self'; base-uri 'none'"

// view serves workspace files to the preview iframe. The CSP sandbox gives the
// document an opaque origin: it cannot read cookies, call the API or the parent.
func (s *Server) view(w http.ResponseWriter, r *http.Request) {
	convID, ok := s.parseViewToken(r.PathValue("token"))
	if !ok {
		http.Error(w, "link expirado", 404)
		return
	}
	p, err := workspace.CleanPath(r.PathValue("path"))
	if err != nil {
		http.Error(w, "caminho inválido", 400)
		return
	}
	data, err := s.Store.ReadFile(r.Context(), convID, p)
	if err != nil {
		http.Error(w, "não encontrado", 404)
		return
	}
	w.Header().Set("Content-Security-Policy", sandboxCSP)
	w.Header().Set("Content-Type", contentType(p))
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Write(data)
}

// --- executive deck ---------------------------------------------------------------------------

type deckSlide struct {
	Title   string   `json:"title"`
	Bullets []string `json:"bullets"`
	Notes   string   `json:"notes,omitempty"`
}

type deckDoc struct {
	Title     string      `json:"title"`
	Subtitle  string      `json:"subtitle"`
	Slides    []deckSlide `json:"slides"`
	NextSteps []string    `json:"nextSteps"`
	Source    string      `json:"source"` // llm | fallback
}

func (s *Server) deck(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	p, err := workspace.CleanPath(in.Path)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	data, err := s.Store.ReadFile(r.Context(), c.ID, p)
	if err != nil {
		jsonError(w, 404, "Documento não encontrado.")
		return
	}
	md := string(data)
	if !s.Cfg.Demo() && s.LLM != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		resp, err := s.LLM.Chat(ctx, llm.Request{Model: s.Model, Purpose: "deck", JSONMode: true, Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: `Transforme o documento em um deck executivo em português do Brasil. Responda SOMENTE JSON: {"title": "...", "subtitle": "...", "slides": [{"title": "...", "bullets": ["até 5 frases curtas"], "notes": "fala do apresentador"}], "nextSteps": ["..."]}. De 5 a 10 slides, foco em decisão: problema, proposta, valor, métricas, riscos, escopo e próximos passos. Não invente números.`},
			{Role: llm.RoleUser, Content: runtime.Truncate(md, 60000)},
		}}, nil)
		if err == nil {
			u := s.user(r)
			_ = s.Store.AddUsage(context.WithoutCancel(r.Context()), store.UsageRecord{UserID: u.ID, ConversationID: c.ID, Purpose: "deck", Model: resp.Model, InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: s.Svc.Pricing.Cost(resp.Usage)})
			var d deckDoc
			txt := resp.Message.Content
			if i, j := strings.Index(txt, "{"), strings.LastIndex(txt, "}"); i >= 0 && j > i && json.Unmarshal([]byte(txt[i:j+1]), &d) == nil && len(d.Slides) > 0 {
				d.Source = "llm"
				writeJSON(w, 200, d)
				return
			}
		}
	}
	writeJSON(w, 200, fallbackDeck(md, runtime.FriendlyName(p)))
}

// fallbackDeck builds slides from the document headings (no LLM).
func fallbackDeck(md, name string) deckDoc {
	d := deckDoc{Title: name, Subtitle: "Resumo executivo", Source: "fallback"}
	body := md
	if strings.HasPrefix(body, "---") {
		if end := strings.Index(body[3:], "\n---"); end >= 0 {
			body = body[3+end+4:]
		}
	}
	var cur *deckSlide
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "# ") && d.Title == name:
			d.Title = strings.TrimPrefix(t, "# ")
		case strings.HasPrefix(t, "## "):
			d.Slides = append(d.Slides, deckSlide{Title: strings.TrimPrefix(t, "## ")})
			cur = &d.Slides[len(d.Slides)-1]
		case cur != nil && t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ">") && !strings.HasPrefix(t, "|") && len(cur.Bullets) < 5:
			b := strings.TrimLeft(t, "-*0123456789. ")
			b = strings.NewReplacer("**", "", "__", "", "`", "").Replace(b)
			if r := []rune(b); len(r) > 160 {
				b = string(r[:157]) + "…"
			}
			if b != "" {
				cur.Bullets = append(cur.Bullets, b)
			}
		}
		if len(d.Slides) >= 12 {
			break
		}
	}
	for _, sl := range d.Slides {
		if strings.Contains(strings.ToLower(sl.Title), "próximos passos") {
			d.NextSteps = sl.Bullets
		}
	}
	return d
}
