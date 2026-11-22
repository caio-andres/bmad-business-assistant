package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/caio-andres/bmad-business-assistant/internal/orchestrator"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

var statusLabels = map[string]string{
	workspace.StatusDraft:    "Rascunho",
	workspace.StatusInReview: "Em revisão",
	workspace.StatusApproved: "Aprovado",
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListProjects(r.Context())
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"projects": nonNil(list)})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sigla       string   `json:"sigla"`
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		Systems     []string `json:"systems"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	p := &store.Project{Sigla: strings.ToUpper(strings.TrimSpace(in.Sigla)), Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description), Systems: in.Systems, CreatedBy: s.user(r).ID}
	p.Slug = in.Slug
	if p.Slug == "" {
		p.Slug = workspace.Slugify(p.Name)
	}
	if p.Name == "" {
		jsonError(w, 400, "Dê um nome ao projeto.")
		return
	}
	if err := workspace.ValidateProject(p.Sigla, p.Slug); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if err := s.Store.CreateProject(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			jsonError(w, 409, "Já existe um projeto "+p.Sigla+"/"+p.Slug+".")
			return
		}
		jsonError(w, 500, err.Error())
		return
	}
	s.audit(r, "project_created", p.ID, p.Sigla+"/"+p.Slug)
	writeJSON(w, 201, map[string]any{"project": p})
}

func (s *Server) projectStatus(r *http.Request, p *store.Project) map[string]any {
	out := map[string]any{"status": workspace.StatusDraft, "label": statusLabels[workspace.StatusDraft]}
	if s.Publisher == nil {
		return out
	}
	st, err := s.Publisher.Status(r.Context(), p.Sigla, p.Slug)
	if err != nil {
		out["error"] = "Não foi possível consultar o repositório central agora."
		return out
	}
	out["status"], out["label"], out["prUrl"], out["branch"] = st.Status, statusLabels[st.Status], st.PRURL, st.Branch
	return out
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonError(w, 404, "Projeto não encontrado.")
		return
	}
	convs, _ := s.Store.ListProjectConversations(r.Context(), p.ID)
	mine := []*store.Conversation{}
	for _, c := range convs {
		if c.UserID == s.user(r).ID {
			mine = append(mine, c)
		}
	}
	pubs, _ := s.Store.ListPublications(r.Context(), p.ID, "")
	out := map[string]any{"project": p, "review": s.projectStatus(r, p), "conversations": mine, "publications": nonNil(pubs)}
	if s.Publisher != nil {
		if snap, err := s.Publisher.Load(r.Context(), p.Sigla, p.Slug); err == nil {
			var files []map[string]string
			for _, f := range snap.Files {
				files = append(files, map[string]string{"path": f.Path, "name": runtime.FriendlyName(f.Path)})
			}
			out["centralFiles"] = nonNil(files)
			out["centralRef"] = snap.Ref
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonError(w, 404, "Projeto não encontrado.")
		return
	}
	var in struct {
		Name        *string  `json:"name"`
		Description *string  `json:"description"`
		Systems     []string `json:"systems"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		p.Description = strings.TrimSpace(*in.Description)
	}
	if in.Systems != nil {
		p.Systems = in.Systems
	}
	if err := s.Store.UpdateProject(r.Context(), p); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"project": p})
}

func (s *Server) listSystems(w http.ResponseWriter, r *http.Request) {
	if s.Repo == nil {
		writeJSON(w, 200, map[string]any{"systems": []any{}})
		return
	}
	sys, err := s.Repo.Systems(r.Context(), strings.ToUpper(r.URL.Query().Get("sigla")))
	if err != nil {
		jsonError(w, 502, "Não foi possível listar os sistemas agora.")
		return
	}
	writeJSON(w, 200, map[string]any{"systems": nonNil(sys)})
}

func (s *Server) convProject(w http.ResponseWriter, r *http.Request, c *store.Conversation, projectID string) *store.Project {
	if projectID == "" {
		projectID = c.ProjectID
	}
	if projectID == "" {
		jsonError(w, 400, "Vincule a conversa a um projeto primeiro.")
		return nil
	}
	p, err := s.Store.GetProject(r.Context(), projectID)
	if err != nil {
		jsonError(w, 404, "Projeto não encontrado.")
		return nil
	}
	if s.Publisher == nil {
		jsonError(w, 400, "A publicação não está configurada.")
		return nil
	}
	return p
}

// loadProject is "Carregar": copies the project's central documents into the conversation.
func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		ProjectID string `json:"projectId"`
	}
	_ = readJSON(r, &in)
	p := s.convProject(w, r, c, in.ProjectID)
	if p == nil {
		return
	}
	if c.Busy {
		jsonError(w, 409, orchestrator.ErrBusy.Error())
		return
	}
	snap, err := s.Publisher.Load(r.Context(), p.Sigla, p.Slug)
	if err != nil {
		s.Logger.Warn("load failed", "err", err)
		jsonError(w, 502, "Não foi possível carregar do repositório central agora.")
		return
	}
	bases := map[string]string{}
	for _, f := range snap.Files {
		if err := s.Store.WriteFile(r.Context(), c.ID, f.Path, f.Content, "central"); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		bases[f.Path] = f.SHA
	}
	if err := s.Store.SetBases(r.Context(), c.ID, bases, true); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	if c.ProjectID != p.ID {
		c.ProjectID = p.ID
		_ = s.Store.UpdateConversationMeta(r.Context(), c)
	}
	s.audit(r, "project_loaded", c.ID, p.Sigla+"/"+p.Slug+" ref="+snap.Ref)
	writeJSON(w, 200, map[string]any{"loaded": len(snap.Files), "ref": snap.Ref, "review": map[string]string{"status": snap.Status, "label": statusLabels[snap.Status], "prUrl": snap.PRURL}})
}

// publish is "Publicar": one commit with only the changed files, never overwriting.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		Files   []string `json:"files"`
		Force   []string `json:"force"`
		Message string   `json:"message"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	p := s.convProject(w, r, c, "")
	if p == nil {
		return
	}
	if c.Busy {
		jsonError(w, 409, orchestrator.ErrBusy.Error())
		return
	}
	if len(in.Files) == 0 {
		all, _ := s.Store.ListFiles(r.Context(), c.ID)
		for _, f := range all {
			if !strings.HasPrefix(f.Path, ".view/") && !strings.HasPrefix(f.Path, ".conflicts/") && !strings.HasPrefix(f.Path, "inputs/") {
				in.Files = append(in.Files, f.Path)
			}
		}
	}
	if len(in.Files) == 0 {
		jsonError(w, 400, "Não há documentos para publicar.")
		return
	}
	files := map[string][]byte{}
	for _, fp := range in.Files {
		cp, err := workspace.CleanPath(fp)
		if err != nil || strings.HasPrefix(cp, ".view/") || strings.HasPrefix(cp, ".conflicts/") {
			continue
		}
		data, err := s.Store.ReadFile(r.Context(), c.ID, cp)
		if err != nil {
			jsonError(w, 404, "Documento não encontrado: "+runtime.FriendlyName(cp))
			return
		}
		files[cp] = data
	}
	bases, _ := s.Store.Bases(r.Context(), c.ID)
	force := map[string]bool{}
	for _, f := range in.Force {
		force[f] = true
	}
	u := s.user(r)
	res, err := s.Publisher.Publish(r.Context(), workspace.PublishRequest{
		Sigla: p.Sigla, Slug: p.Slug, Title: "Projeto " + p.Sigla + "/" + p.Slug + ": " + p.Name,
		Files: files, Base: bases, Force: force, AuthorName: u.Name, AuthorEmail: u.Email, Message: in.Message,
	})
	if err != nil {
		s.Logger.Warn("publish failed", "err", err)
		_ = s.Store.AddPublication(r.Context(), &store.Publication{ConversationID: c.ID, ProjectID: p.ID, Branch: workspace.BranchName(p.Sigla, p.Slug), Status: "failed", CreatedBy: u.ID})
		jsonError(w, 502, "Não foi possível publicar agora: "+friendlyPublishError(err))
		return
	}
	switch res.Status {
	case "published":
		_ = s.Store.SetBases(r.Context(), c.ID, res.NewBase, false)
		_ = s.Store.AddPublication(r.Context(), &store.Publication{ConversationID: c.ID, ProjectID: p.ID, Branch: res.Branch, PRURL: res.PRURL, CommitSHA: res.CommitSHA, Files: res.Changed, Status: "published", CreatedBy: u.ID})
		s.audit(r, "published", p.Sigla+"/"+p.Slug, strings.Join(res.Changed, ", "))
	case "conflict":
		var paths []string
		for _, cf := range res.Conflicts {
			paths = append(paths, cf.Path)
		}
		_ = s.Store.AddPublication(r.Context(), &store.Publication{ConversationID: c.ID, ProjectID: p.ID, Branch: res.Branch, Files: paths, Status: "conflict", CreatedBy: u.ID})
		s.audit(r, "publish_conflict", p.Sigla+"/"+p.Slug, strings.Join(paths, ", "))
	}
	sort.Strings(res.Changed)
	writeJSON(w, 200, map[string]any{"result": res, "review": s.projectStatus(r, p)})
}

func friendlyPublishError(err error) string {
	m := err.Error()
	var ae *workspace.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case 401, 403:
			return "a credencial do repositório central não tem permissão."
		case 404:
			return "o repositório central não foi encontrado."
		}
		return "o repositório central respondeu com erro."
	}
	if len(m) > 160 {
		m = m[:160]
	}
	return m
}

// resolve applies a conflict choice for one file.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	c := s.conv(w, r)
	if c == nil {
		return
	}
	var in struct {
		Path   string `json:"path"`
		Choice string `json:"choice"` // mine | theirs | merge
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	p := s.convProject(w, r, c, "")
	if p == nil {
		return
	}
	fp, err := workspace.CleanPath(in.Path)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	snap, err := s.Publisher.Load(r.Context(), p.Sigla, p.Slug)
	if err != nil {
		jsonError(w, 502, "Não foi possível consultar o repositório central agora.")
		return
	}
	var theirs *workspace.File
	for i := range snap.Files {
		if snap.Files[i].Path == fp {
			theirs = &snap.Files[i]
		}
	}
	sha := ""
	if theirs != nil {
		sha = theirs.SHA
	}
	out := map[string]any{"ok": true}
	switch in.Choice {
	case "mine":
		// accept the remote as the new base: the next publish sends my version
	case "theirs":
		if theirs == nil {
			_ = s.Store.DeleteFile(r.Context(), c.ID, fp)
		} else if err := s.Store.WriteFile(r.Context(), c.ID, fp, theirs.Content, "central"); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
	case "merge":
		if theirs == nil {
			jsonError(w, 400, "O documento não existe mais no repositório central; publique a sua versão.")
			return
		}
		copyPath := ".conflicts/" + fp
		if err := s.Store.WriteFile(r.Context(), c.ID, copyPath, theirs.Content, "central"); err != nil {
			jsonError(w, 500, err.Error())
			return
		}
		name := runtime.FriendlyName(fp)
		out["prompt"] = "Há duas versões do documento \"" + name + "\": a desta conversa e a que outra pessoa publicou no repositório central (cópia em " + runtime.OutputDir + "/" + copyPath + "). " +
			"Leia as duas e reescreva o documento desta conversa com uma versão unificada, preservando as contribuições de ambas e sinalizando pontos contraditórios para eu decidir. Depois me mostre um resumo do que mudou."
	default:
		jsonError(w, 400, "Escolha inválida.")
		return
	}
	if err := s.Store.SetBases(r.Context(), c.ID, map[string]string{fp: sha}, false); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	s.audit(r, "conflict_resolved", c.ID, fp+" "+in.Choice)
	writeJSON(w, 200, out)
}

func (s *Server) adminApprove(w http.ResponseWriter, r *http.Request) {
	l, ok := s.Publisher.(*workspace.Local)
	if !ok {
		jsonError(w, 400, "No GitHub, a aprovação é o merge do PR pelo time técnico.")
		return
	}
	p, err := s.Store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonError(w, 404, "Projeto não encontrado.")
		return
	}
	if err := l.Approve(r.Context(), p.Sigla, p.Slug); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	s.audit(r, "project_approved", p.Sigla+"/"+p.Slug, "local")
	writeJSON(w, 200, map[string]any{"review": s.projectStatus(r, p)})
}
