// Package httpapi exposes the REST + SSE API and serves the embedded SPA.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/auth"
	"github.com/caio-andres/bmad-business-assistant/internal/config"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/orchestrator"
	"github.com/caio-andres/bmad-business-assistant/internal/repoctx"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
)

// Server wires the HTTP layer.
type Server struct {
	Cfg       *config.Config
	Store     *store.Store
	Svc       *orchestrator.Service
	Auth      auth.Authenticator
	Local     *auth.Local // set when AUTH_MODE=local
	Publisher workspace.Publisher
	Repo      *repoctx.Provider
	LLM       llm.Provider
	Model     string
	Logger    *slog.Logger
	SPA       fs.FS
	Version   string
	// MCP is mounted at /mcp when set (bearer MCP_TOKEN).
	MCP http.Handler

	viewKey      []byte
	turnLimiter  *limiter
	loginLimiter *limiter
	once         sync.Once
}

type ctxKey int

const userKey ctxKey = 1

func (s *Server) init() {
	s.once.Do(func() {
		if s.Cfg.SessionSecret != "" {
			h := sha256.Sum256([]byte("view:" + s.Cfg.SessionSecret))
			s.viewKey = h[:]
		} else {
			s.viewKey = make([]byte, 32)
			_, _ = rand.Read(s.viewKey)
		}
		per := s.Cfg.RateLimitPerMinute
		if per <= 0 {
			per = 1000
		}
		s.turnLimiter = newLimiter(per, time.Minute)
		s.loginLimiter = newLimiter(10, time.Minute)
		if s.Logger == nil {
			s.Logger = slog.Default()
		}
	})
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	// health
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	// session & auth
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("POST /api/auth/setup", s.setup)
	mux.HandleFunc("GET /api/auth/invite/{token}", s.inviteInfo)
	mux.HandleFunc("POST /api/auth/accept-invite", s.acceptInvite)
	// catalog
	mux.HandleFunc("GET /api/catalog", s.authed(s.catalog))
	// conversations
	mux.HandleFunc("GET /api/conversations", s.authed(s.listConversations))
	mux.HandleFunc("POST /api/conversations", s.authed(s.createConversation))
	mux.HandleFunc("GET /api/conversations/{id}", s.authed(s.getConversation))
	mux.HandleFunc("PATCH /api/conversations/{id}", s.authed(s.patchConversation))
	mux.HandleFunc("DELETE /api/conversations/{id}", s.authed(s.deleteConversation))
	mux.HandleFunc("POST /api/conversations/{id}/turn", s.authed(s.turn))
	mux.HandleFunc("POST /api/conversations/{id}/cancel", s.authed(s.cancel))
	mux.HandleFunc("POST /api/conversations/{id}/end-skill", s.authed(s.endSkill))
	mux.HandleFunc("GET /api/conversations/{id}/files", s.authed(s.listFiles))
	mux.HandleFunc("GET /api/conversations/{id}/files/raw", s.authed(s.rawFile))
	mux.HandleFunc("PUT /api/conversations/{id}/files/raw", s.authed(s.saveFile))
	mux.HandleFunc("DELETE /api/conversations/{id}/files/raw", s.authed(s.deleteFile))
	mux.HandleFunc("GET /api/conversations/{id}/files/versions", s.authed(s.listVersions))
	mux.HandleFunc("GET /api/conversations/{id}/files/versions/{vid}", s.authed(s.getVersion))
	mux.HandleFunc("POST /api/conversations/{id}/upload", s.authed(s.upload))
	mux.HandleFunc("GET /api/conversations/{id}/export.zip", s.authed(s.exportZip))
	mux.HandleFunc("GET /api/conversations/{id}/view-url", s.authed(s.viewURL))
	mux.HandleFunc("POST /api/conversations/{id}/deck", s.authed(s.deck))
	mux.HandleFunc("POST /api/conversations/{id}/load", s.authed(s.loadProject))
	mux.HandleFunc("POST /api/conversations/{id}/publish", s.authed(s.publish))
	mux.HandleFunc("POST /api/conversations/{id}/resolve", s.authed(s.resolve))
	mux.HandleFunc("GET /v/{token}/{path...}", s.view)
	// projects
	mux.HandleFunc("GET /api/projects", s.authed(s.listProjects))
	mux.HandleFunc("POST /api/projects", s.authed(s.createProject))
	mux.HandleFunc("GET /api/projects/{id}", s.authed(s.getProject))
	mux.HandleFunc("PATCH /api/projects/{id}", s.authed(s.patchProject))
	mux.HandleFunc("GET /api/systems", s.authed(s.listSystems))
	// admin
	mux.HandleFunc("GET /api/admin/status", s.admin(s.adminStatus))
	mux.HandleFunc("GET /api/admin/users", s.admin(s.adminUsers))
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.admin(s.adminPatchUser))
	mux.HandleFunc("GET /api/admin/invites", s.admin(s.adminInvites))
	mux.HandleFunc("POST /api/admin/invites", s.admin(s.adminCreateInvite))
	mux.HandleFunc("GET /api/admin/usage", s.admin(s.adminUsage))
	mux.HandleFunc("GET /api/admin/audit", s.admin(s.adminAudit))
	mux.HandleFunc("POST /api/admin/projects/{id}/approve", s.admin(s.adminApprove))
	mux.HandleFunc("POST /api/admin/retention", s.admin(s.adminRetention))
	if s.MCP != nil {
		mux.Handle("/mcp", s.mcpAuth(s.MCP))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { jsonError(w, 404, "Rota não encontrada.") })
	if s.SPA != nil {
		mux.Handle("/", spaHandler(s.SPA))
	}
	return s.recoverer(s.logRequests(s.securityHeaders(s.csrf(mux))))
}

// --- middleware --------------------------------------------------------------------------

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Logger.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				jsonError(w, 500, "Erro interno. Tente de novo.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || sw.status >= 500 {
			s.Logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

const spaCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:; img-src 'self' data: blob:; connect-src 'self'; frame-src 'self' blob:; worker-src 'self' blob:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !strings.HasPrefix(r.URL.Path, "/v/") {
			h.Set("Content-Security-Policy", spaCSP)
			h.Set("X-Frame-Options", "DENY")
		}
		if s.Cfg.SecureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// csrf requires a custom header on state-changing API calls (forms cannot set it cross-site).
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if r.Header.Get("X-Requested-With") == "" {
				jsonError(w, 403, "Requisição recusada (proteção CSRF).")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) user(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Auth.User(r)
		if err != nil || u == nil {
			msg := auth.ErrUnauthenticated.Error()
			if errors.Is(err, auth.ErrDomainNotAllowed) {
				msg = err.Error()
			}
			jsonError(w, 401, msg)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !s.user(r).IsAdmin() {
			jsonError(w, 403, "Somente administradores.")
			return
		}
		h(w, r)
	})
}

func (s *Server) mcpAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + s.Cfg.MCPToken
		if s.Cfg.MCPToken == "" || !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte(want)) {
			jsonError(w, 401, "token MCP inválido")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxyHeaders {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) audit(r *http.Request, action, target, detail string) {
	uid := ""
	if u := s.user(r); u != nil {
		uid = u.ID
	}
	if err := s.Store.Audit(context.WithoutCancel(r.Context()), store.AuditEntry{UserID: uid, Action: action, Target: target, Detail: detail, IP: s.clientIP(r)}); err != nil {
		s.Logger.Warn("audit failed", "err", err)
	}
}

// --- helpers ------------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON inválido: %w", err)
	}
	return nil
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		jsonError(w, 503, "banco indisponível")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready", "version": s.Version})
}

// --- rate limiting ------------------------------------------------------------------------

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// allow records a hit and reports whether it is within the limit.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-l.window)
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= l.max {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

// --- view tokens (sandboxed HTML preview) ---------------------------------------------------

func (s *Server) viewToken(convID string, ttl time.Duration) string {
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 36)
	payload := convID + "." + exp
	mac := hmac.New(sha256.New, s.viewKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

func (s *Server) parseViewToken(tok string) (string, bool) {
	p, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.viewKey)
	mac.Write(payload)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", false
	}
	conv, exp, ok := strings.Cut(string(payload), ".")
	n, err := strconv.ParseInt(exp, 36, 64)
	if !ok || err != nil || time.Now().Unix() > n {
		return "", false
	}
	return conv, true
}
