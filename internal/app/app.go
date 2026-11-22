// Package app is the composition root: it builds every adapter from the
// configuration (ports and adapters, D14–D26) and runs background jobs.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	bmadassets "github.com/caio-andres/bmad-business-assistant/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/agent"
	"github.com/caio-andres/bmad-business-assistant/internal/auth"
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/config"
	"github.com/caio-andres/bmad-business-assistant/internal/guard"
	"github.com/caio-andres/bmad-business-assistant/internal/httpapi"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/mcpserver"
	"github.com/caio-andres/bmad-business-assistant/internal/orchestrator"
	"github.com/caio-andres/bmad-business-assistant/internal/repoctx"
	"github.com/caio-andres/bmad-business-assistant/internal/runtime"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/web"
	"github.com/caio-andres/bmad-business-assistant/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is set at build time (-ldflags "-X .../internal/app.Version=...").
var Version = "dev"

// App holds the wired services.
type App struct {
	Cfg       *config.Config
	Logger    *slog.Logger
	Store     *store.Store
	Bundle    *bmad.Bundle
	Catalog   *bmad.Catalog
	LLM       llm.Provider
	Service   *orchestrator.Service
	Publisher workspace.Publisher
	Repo      *repoctx.Provider
	Server    *httpapi.Server
	MCP       *mcp.Server
}

// NewLogger builds the structured logger.
func NewLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// LoadBundle loads the vendored BMad (or BMAD_DIR when set).
func LoadBundle(cfg *config.Config) (*bmad.Bundle, error) {
	var fsys fs.FS = bmadassets.FS()
	if cfg.BMadDir != "" {
		fsys = os.DirFS(cfg.BMadDir)
	}
	return bmad.Load(fsys)
}

// New wires everything. spa may be nil (API only).
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger, spa fs.FS) (*App, error) {
	if logger == nil {
		logger = NewLogger(cfg)
	}
	a := &App{Cfg: cfg, Logger: logger}
	var err error
	if a.Bundle, err = LoadBundle(cfg); err != nil {
		return nil, fmt.Errorf("BMad: %w", err)
	}
	a.Catalog = bmad.NewCatalog(a.Bundle, cfg.AllowedSkills)
	allowed := cfg.AllowedSkills
	if len(allowed) == 0 {
		allowed = bmad.DefaultAllowedSkills
	}
	if a.Store, err = store.Open(ctx, cfg.DatabaseURL, cfg.DataDir); err != nil {
		return nil, err
	}

	model := cfg.LLMModel
	if cfg.Demo() {
		a.LLM = &llm.Demo{Delay: cfg.DemoDelay}
		model = "demo"
	} else {
		a.LLM = llm.NewOpenAI(llm.OpenAIConfig{BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Headers: cfg.LLMExtraHeaders,
			Stream: cfg.LLMStream, StreamUsage: cfg.LLMStreamUsage, Timeout: cfg.LLMTimeout, MaxRetries: 2})
	}
	routerModel := firstNonEmpty(cfg.LLMRouterModel, cfg.LLMSubagentModel, model)

	py := runtime.NewPythonRunner("")
	if cfg.PythonBin != "" && cfg.PythonBin != "off" && cfg.PythonBin != "disabled" {
		py = runtime.NewPythonRunner(cfg.PythonBin)
	}
	var webp runtime.WebProvider
	if cfg.WebSearch == "searxng" {
		webp = web.New(cfg.SearXNGURL, cfg.WebFetchAllowHTTP)
	}
	tools := runtime.DefaultRegistry(webp != nil)
	interp := agent.New(agent.Config{
		Provider: a.LLM, Model: model, SubagentModel: firstNonEmpty(cfg.LLMSubagentModel, model), SummaryModel: firstNonEmpty(cfg.LLMSubagentModel, model),
		MaxIterations: cfg.MaxToolIterations, TurnTimeout: cfg.TurnTimeout, ContextBudget: cfg.ContextBudgetTokens,
		SubagentsEnabled: cfg.SubagentsEnabled, Temperature: cfg.LLMTemperature,
	}, tools)

	var routers []orchestrator.Router
	llmRouter := &orchestrator.LLMRouter{Provider: a.LLM, Model: routerModel, OnUsage: func(m string, u llm.Usage) {
		_ = a.Store.AddUsage(context.Background(), store.UsageRecord{Purpose: "router", Model: m, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
	}}
	switch cfg.Router {
	case "rules":
		routers = []orchestrator.Router{orchestrator.RuleRouter{}}
	case "llm":
		routers = []orchestrator.Router{llmRouter}
	case "jev":
		routers = []orchestrator.Router{orchestrator.JevRouter{APIKey: cfg.JevAPIKey}, orchestrator.RuleRouter{}, llmRouter}
	default:
		routers = []orchestrator.Router{orchestrator.RuleRouter{}, llmRouter}
		if cfg.JevAPIKey != "" {
			routers = append([]orchestrator.Router{orchestrator.JevRouter{APIKey: cfg.JevAPIKey}}, routers...)
		}
	}

	switch cfg.Publisher {
	case "github":
		a.Publisher = &workspace.GitHub{APIURL: cfg.GitHubAPIURL, Token: cfg.GitHubToken, Repo: cfg.GitHubWorkspaceRepo, BaseBranch: cfg.GitHubBaseBranch}
	case "none", "disabled":
	default:
		a.Publisher = &workspace.Local{Root: cfg.WorkspaceLocalDir, BaseURL: cfg.BaseURL}
	}
	var src repoctx.Source
	switch cfg.RepoSource {
	case "github":
		src = &repoctx.GitHubSource{APIURL: cfg.GitHubAPIURL, Token: cfg.GitHubToken, Org: cfg.RepoOrg}
	case "none", "disabled":
	default:
		src = &repoctx.LocalSource{Dir: cfg.RepoLocalDir, Org: cfg.RepoOrg}
	}
	if src != nil {
		a.Repo = &repoctx.Provider{Source: src, Publisher: a.Publisher, Store: a.Store, LLM: a.LLM, Model: routerModel,
			MaxAge: time.Duration(cfg.RepoSummaryMaxAgeDays) * 24 * time.Hour, Logger: logger}
	}

	loc, _ := time.LoadLocation(cfg.Timezone)
	a.Service = &orchestrator.Service{
		Store: a.Store, Bundle: a.Bundle, Catalog: a.Catalog, Allowed: allowed, Interpreter: interp,
		Router: &orchestrator.ChainRouter{Routers: routers, Threshold: cfg.RouterThreshold},
		Guard:  &guard.Guard{Policy: guard.Policy(cfg.PIIPolicy), AllowEmailDomains: cfg.PIIAllowDomains},
		Python: py, Web: webp, AppName: cfg.AppName, Location: loc,
		Pricing:    orchestrator.Pricing{InputPerMTok: cfg.PriceInputPerMTok, OutputPerMTok: cfg.PriceOutputPerMTok},
		DailyQuota: cfg.DailyTokenQuota, LeaseTTL: cfg.TurnTimeout + 2*time.Minute, Demo: cfg.Demo(), Logger: logger,
	}
	if a.Repo != nil {
		a.Service.Context = a.Repo
	}

	var authn auth.Authenticator
	var local *auth.Local
	switch cfg.AuthMode {
	case "dev":
		authn = &auth.Dev{Store: a.Store}
	case "oidc":
		authn = &auth.OIDC{Store: a.Store, Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience, JWKSURL: cfg.OIDCJWKSURL,
			EmailClaim: cfg.OIDCEmailClaim, NameClaim: cfg.OIDCNameClaim, AllowedDomains: cfg.SignupAllowedDomains,
			TokenHeader: cfg.OIDCTokenHeader, ALBRegion: cfg.OIDCALBRegion}
	default:
		local = &auth.Local{Store: a.Store, TTL: cfg.SessionTTL, Secure: cfg.SecureCookies()}
		authn = local
	}

	a.MCP = mcpserver.New(mcpserver.Deps{Store: a.Store, Catalog: a.Catalog, Publisher: a.Publisher, Version: Version})
	var mcpHandler http.Handler
	if cfg.MCPToken != "" {
		srv := a.MCP
		mcpHandler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	}
	a.Server = &httpapi.Server{Cfg: cfg, Store: a.Store, Svc: a.Service, Auth: authn, Local: local, Publisher: a.Publisher,
		Repo: a.Repo, LLM: a.LLM, Model: routerModel, Logger: logger, SPA: spa, Version: Version, MCP: mcpHandler}
	return a, nil
}

// Close releases resources.
func (a *App) Close() error { return a.Store.Close() }

// RunJobs runs periodic maintenance until ctx ends.
func (a *App) RunJobs(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	run := func() {
		if n, err := a.Store.PurgeExpiredSessions(ctx); err == nil && n > 0 {
			a.Logger.Info("sessions purged", "count", n)
		}
		if a.Cfg.RetentionDays > 0 {
			if n, err := a.Store.PurgeInactiveConversations(ctx, time.Now().AddDate(0, 0, -a.Cfg.RetentionDays)); err == nil && n > 0 {
				a.Logger.Info("retention purge", "conversations", n)
				_ = a.Store.Audit(ctx, store.AuditEntry{Action: "retention_purge", Detail: fmt.Sprintf("conversations=%d", n)})
			}
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}

// Summary describes the running configuration (no secrets).
func (a *App) Summary() string {
	llmDesc := "modo demonstração (sem IA real)"
	if !a.Cfg.Demo() {
		llmDesc = a.Cfg.LLMModel + " via " + hostOf(a.Cfg.LLMBaseURL)
	}
	pub := "desligada"
	if a.Publisher != nil {
		pub = a.Publisher.Name()
	}
	return fmt.Sprintf("BMad v%s (%d etapas) · IA: %s · banco: %s · auth: %s · publicação: %s · python: %v · web: %s",
		a.Bundle.Version, len(a.Catalog.Items), llmDesc, a.Store.Dialect(), a.Cfg.AuthMode, pub, a.Service.Python.Enabled(), a.Cfg.WebSearch)
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
