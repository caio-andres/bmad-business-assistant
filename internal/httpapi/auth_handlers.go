package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/auth"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
)

type appInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	BMadVersion string `json:"bmadVersion"`
	Demo        bool   `json:"demo"`
	AuthMode    string `json:"authMode"`
	Publisher   string `json:"publisher"`
	Web         bool   `json:"web"`
	Subagents   bool   `json:"subagents"`
	Python      bool   `json:"python"`
	NeedsSetup  bool   `json:"needsSetup"`
	SetupToken  bool   `json:"setupTokenRequired"`
	Model       string `json:"model,omitempty"`
	PIIPolicy   string `json:"piiPolicy"`
}

func (s *Server) info(r *http.Request) appInfo {
	n, _ := s.Store.CountUsers(r.Context())
	pub := "none"
	if s.Publisher != nil {
		pub = s.Publisher.Name()
	}
	model := s.Cfg.LLMModel
	if s.Cfg.Demo() {
		model = "demonstração"
	}
	return appInfo{
		Name: s.Cfg.AppName, Version: s.Version, BMadVersion: s.Svc.Bundle.Version, Demo: s.Cfg.Demo(),
		AuthMode: s.Auth.Mode(), Publisher: pub, Web: s.Svc.Web != nil, Subagents: s.Cfg.SubagentsEnabled,
		Python: s.Svc.Python.Enabled(), NeedsSetup: s.Auth.Mode() == "local" && n == 0, SetupToken: s.Cfg.SetupToken != "",
		Model: model, PIIPolicy: s.Cfg.PIIPolicy,
	}
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"app": s.info(r)}
	if u, err := s.Auth.User(r); err == nil {
		out["user"] = u
	} else {
		out["user"] = nil
	}
	writeJSON(w, 200, out)
}

func (s *Server) localOnly(w http.ResponseWriter) bool {
	if s.Local == nil {
		jsonError(w, 400, "O login por senha não está habilitado neste ambiente.")
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.localOnly(w) {
		return
	}
	if !s.loginLimiter.allow("login:" + s.clientIP(r)) {
		jsonError(w, 429, "Muitas tentativas. Aguarde um minuto.")
		return
	}
	var in struct{ Email, Password string }
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	u, err := s.Local.Login(r.Context(), w, in.Email, in.Password, r.UserAgent())
	if err != nil {
		s.audit(r, "login_failed", store.NormalizeEmail(in.Email), "")
		jsonError(w, 401, err.Error())
		return
	}
	r = r.WithContext(withUser(r, u))
	s.audit(r, "login", u.ID, "")
	writeJSON(w, 200, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.Local != nil {
		s.Local.Logout(r.Context(), w, r)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// setup creates the first admin on an empty database.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.localOnly(w) {
		return
	}
	if !s.loginLimiter.allow("setup:" + s.clientIP(r)) {
		jsonError(w, 429, "Muitas tentativas. Aguarde um minuto.")
		return
	}
	var in struct{ Email, Name, Password, Token string }
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if n, _ := s.Store.CountUsers(r.Context()); n > 0 {
		jsonError(w, 409, "A configuração inicial já foi feita. Peça um convite ao administrador.")
		return
	}
	if s.Cfg.SetupToken != "" {
		if subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.Cfg.SetupToken)) != 1 {
			jsonError(w, 403, "Código de configuração inválido.")
			return
		}
	} else if s.Cfg.Production() {
		jsonError(w, 403, "Em produção, crie o primeiro administrador pela linha de comando (alicerce user create-admin) ou defina SETUP_TOKEN.")
		return
	}
	u, err := s.newLocalUser(r, in.Email, in.Name, in.Password, store.RoleAdmin)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if err := s.Local.StartSession(r.Context(), w, u, r.UserAgent()); err != nil {
		jsonError(w, 500, "Não foi possível iniciar a sessão.")
		return
	}
	r = r.WithContext(withUser(r, u))
	s.audit(r, "setup_admin", u.ID, "")
	writeJSON(w, 201, map[string]any{"user": u})
}

func (s *Server) newLocalUser(r *http.Request, email, name, password, role string) (*store.User, error) {
	email = store.NormalizeEmail(email)
	name = strings.TrimSpace(name)
	if !strings.Contains(email, "@") || len(email) > 200 {
		return nil, errors.New("Informe um e-mail válido.")
	}
	if name == "" || len(name) > 120 {
		return nil, errors.New("Informe o seu nome.")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &store.User{Email: email, Name: name, Role: role, PasswordHash: hash}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, errors.New("Já existe uma conta com este e-mail.")
		}
		return nil, err
	}
	return u, nil
}

func (s *Server) inviteInfo(w http.ResponseWriter, r *http.Request) {
	inv, err := s.Store.GetInvite(r.Context(), auth.HashToken(r.PathValue("token")))
	if err != nil {
		jsonError(w, 404, "Convite inválido ou expirado.")
		return
	}
	writeJSON(w, 200, map[string]any{"email": inv.Email, "expiresAt": inv.ExpiresAt})
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	if !s.localOnly(w) {
		return
	}
	if !s.loginLimiter.allow("invite:" + s.clientIP(r)) {
		jsonError(w, 429, "Muitas tentativas. Aguarde um minuto.")
		return
	}
	var in struct{ Token, Name, Password string }
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	th := auth.HashToken(in.Token)
	inv, err := s.Store.GetInvite(r.Context(), th)
	if err != nil {
		jsonError(w, 404, "Convite inválido ou expirado.")
		return
	}
	if _, err := auth.HashPassword(in.Password); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if err := s.Store.UseInvite(r.Context(), th); err != nil {
		jsonError(w, 404, "Convite já utilizado.")
		return
	}
	u, err := s.newLocalUser(r, inv.Email, in.Name, in.Password, inv.Role)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	if err := s.Local.StartSession(r.Context(), w, u, r.UserAgent()); err != nil {
		jsonError(w, 500, "Não foi possível iniciar a sessão.")
		return
	}
	r = r.WithContext(withUser(r, u))
	s.audit(r, "invite_accepted", u.ID, "")
	writeJSON(w, 201, map[string]any{"user": u})
}

// --- admin ------------------------------------------------------------------------------

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Store.SchemaVersion(r.Context())
	repo := "none"
	if s.Repo != nil && s.Repo.Source != nil {
		repo = s.Repo.Source.Name()
	}
	writeJSON(w, 200, map[string]any{
		"app": s.info(r), "database": s.Store.Dialect(), "schemaVersion": v,
		"llm":    map[string]any{"demo": s.Cfg.Demo(), "model": s.Cfg.LLMModel, "routerModel": s.Cfg.LLMRouterModel, "subagentModel": s.Cfg.LLMSubagentModel, "stream": s.Cfg.LLMStream},
		"router": s.Cfg.Router, "routerThreshold": s.Cfg.RouterThreshold, "repoSource": repo,
		"limits":        map[string]any{"ratePerMinute": s.Cfg.RateLimitPerMinute, "dailyTokens": s.Cfg.DailyTokenQuota, "maxToolIterations": s.Cfg.MaxToolIterations},
		"retentionDays": s.Cfg.RetentionDays, "skills": len(s.Svc.Catalog.Items) + len(s.Svc.Catalog.Agents),
	})
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"users": users})
}

func (s *Server) adminPatchUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonError(w, 404, "Usuário não encontrado.")
		return
	}
	var in struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
		Name   *string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	me := s.user(r)
	if in.Role != nil {
		if *in.Role != store.RoleAdmin && *in.Role != store.RoleMember {
			jsonError(w, 400, "Papel inválido.")
			return
		}
		if u.ID == me.ID && *in.Role != store.RoleAdmin {
			jsonError(w, 400, "Você não pode remover o seu próprio acesso de administrador.")
			return
		}
		u.Role = *in.Role
	}
	if in.Status != nil {
		if *in.Status != store.StatusActive && *in.Status != store.StatusDisabled {
			jsonError(w, 400, "Situação inválida.")
			return
		}
		if u.ID == me.ID && *in.Status == store.StatusDisabled {
			jsonError(w, 400, "Você não pode desativar a sua própria conta.")
			return
		}
		u.Status = *in.Status
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		u.Name = strings.TrimSpace(*in.Name)
	}
	if err := s.Store.UpdateUser(r.Context(), u); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	if u.Status == store.StatusDisabled {
		_ = s.Store.DeleteUserSessions(r.Context(), u.ID)
	}
	s.audit(r, "user_updated", u.ID, "role="+u.Role+" status="+u.Status)
	writeJSON(w, 200, map[string]any{"user": u})
}

func (s *Server) adminInvites(w http.ResponseWriter, r *http.Request) {
	inv, err := s.Store.ListInvites(r.Context())
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"invites": inv})
}

func (s *Server) adminCreateInvite(w http.ResponseWriter, r *http.Request) {
	if s.Local == nil {
		jsonError(w, 400, "Convites só existem no login por senha. Com SSO, o acesso é liberado pelo provedor de identidade.")
		return
	}
	var in struct{ Email, Role string }
	if err := readJSON(r, &in); err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	email := store.NormalizeEmail(in.Email)
	if !strings.Contains(email, "@") {
		jsonError(w, 400, "Informe um e-mail válido.")
		return
	}
	if !auth.DomainAllowed(email, s.Cfg.SignupAllowedDomains) {
		jsonError(w, 400, auth.ErrDomainNotAllowed.Error())
		return
	}
	if _, err := s.Store.GetUserByEmail(r.Context(), email); err == nil {
		jsonError(w, 409, "Já existe uma conta com este e-mail.")
		return
	}
	role := store.RoleMember
	if in.Role == store.RoleAdmin {
		role = store.RoleAdmin
	}
	tok := auth.NewToken()
	if err := s.Store.CreateInvite(r.Context(), &store.Invite{TokenHash: auth.HashToken(tok), Email: email, Role: role, CreatedBy: s.user(r).ID, ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}); err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	s.audit(r, "invite_created", email, role)
	writeJSON(w, 201, map[string]any{"link": s.Cfg.BaseURL + "/#/convite/" + tok, "expiresInDays": 7})
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := parseInt(d); err == nil && n > 0 && n <= 366 {
			days = n
		}
	}
	rep, err := s.Store.UsageReport(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	var tot store.UsageTotals
	for _, u := range rep {
		tot.InputTokens += u.InputTokens
		tot.OutputTokens += u.OutputTokens
		tot.CostUSD += u.CostUSD
		tot.Calls += u.Calls
	}
	writeJSON(w, 200, map[string]any{"days": days, "users": rep, "total": tot, "priced": s.Cfg.PriceInputPerMTok > 0 || s.Cfg.PriceOutputPerMTok > 0})
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := parseInt(r.URL.Query().Get("limit"))
	entries, err := s.Store.ListAudit(r.Context(), limit)
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
}

func (s *Server) adminRetention(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.RetentionDays <= 0 {
		jsonError(w, 400, "A retenção automática está desligada (RETENTION_DAYS=0).")
		return
	}
	n, err := s.Store.PurgeInactiveConversations(r.Context(), time.Now().AddDate(0, 0, -s.Cfg.RetentionDays))
	if err != nil {
		jsonError(w, 500, err.Error())
		return
	}
	s.audit(r, "retention_purge", "", "conversations="+itoa(n))
	writeJSON(w, 200, map[string]any{"deleted": n})
}
