// Package config reads the environment (see .env.example). Every setting is
// optional: with nothing set the app runs in demo mode on SQLite.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration.
type Config struct {
	AppName  string
	Addr     string
	BaseURL  string
	Env      string
	Timezone string
	DataDir  string

	DatabaseURL string

	AuthMode             string // local | oidc | dev
	SessionSecret        string
	SessionTTL           time.Duration
	SetupToken           string
	SignupAllowedDomains []string
	OIDCIssuer           string
	OIDCAudience         string
	OIDCJWKSURL          string
	OIDCEmailClaim       string
	OIDCNameClaim        string
	OIDCTokenHeader      string
	OIDCALBRegion        string

	LLMBaseURL          string
	LLMAPIKey           string
	LLMModel            string
	LLMRouterModel      string
	LLMSubagentModel    string
	LLMStream           bool
	LLMStreamUsage      bool
	LLMExtraHeaders     map[string]string
	LLMTimeout          time.Duration
	LLMTemperature      *float64
	PriceInputPerMTok   float64
	PriceOutputPerMTok  float64
	ContextBudgetTokens int

	RateLimitPerMinute int
	DailyTokenQuota    int
	MaxToolIterations  int
	SubagentsEnabled   bool
	TurnTimeout        time.Duration

	BMadDir       string
	AllowedSkills []string
	PythonBin     string

	PIIPolicy         string
	PIIAllowDomains   []string
	RetentionDays     int
	WebSearch         string
	SearXNGURL        string
	WebFetchAllowHTTP bool

	Publisher           string
	WorkspaceLocalDir   string
	GitHubToken         string
	GitHubWorkspaceRepo string
	GitHubBaseBranch    string
	GitHubAPIURL        string

	RepoSource            string
	RepoLocalDir          string
	RepoOrg               string
	RepoSummaryMaxAgeDays int

	Router              string
	RouterThreshold     float64
	JevAPIKey           string
	MCPToken            string
	DemoDelay           time.Duration
	TrustProxyHeaders   bool
	CORSAllowedOrigins  []string
	MaxUploadBytes      int64
	MetricsEnabled      bool
	LogLevel            string
	LogFormat           string
	SecureCookieForced  bool
	AllowSetupInProduct bool
}

// Demo reports whether no LLM is configured.
func (c *Config) Demo() bool { return c.LLMBaseURL == "" || strings.EqualFold(c.LLMBaseURL, "demo") }

// Production reports APP_ENV=production.
func (c *Config) Production() bool { return strings.EqualFold(c.Env, "production") }

func str(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func integer(k string, def int) int {
	if v := str(k, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func float(k string, def float64) float64 {
	if v := str(k, ""); v != "" {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64); err == nil {
			return f
		}
	}
	return def
}

func boolean(k string, def bool) bool {
	if v := str(k, ""); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func list(k string) []string {
	var out []string
	for _, p := range strings.Split(str(k, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func duration(k string, def time.Duration) time.Duration {
	if v := str(k, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// headers parses "Name: value; Other: value".
func headers(k string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(str(k, ""), ";") {
		name, val, ok := strings.Cut(p, ":")
		if ok && strings.TrimSpace(name) != "" {
			out[strings.TrimSpace(name)] = os.ExpandEnv(strings.TrimSpace(val))
		}
	}
	return out
}

// Load reads the environment.
func Load() (*Config, error) {
	c := &Config{
		AppName:  str("APP_NAME", "Alicerce"),
		Addr:     str("APP_ADDR", ""),
		BaseURL:  strings.TrimRight(str("APP_BASE_URL", ""), "/"),
		Env:      str("APP_ENV", "development"),
		Timezone: str("APP_TIMEZONE", "America/Sao_Paulo"),
		DataDir:  str("DATA_DIR", "./data"),

		DatabaseURL: str("DATABASE_URL", ""),

		AuthMode:             strings.ToLower(str("AUTH_MODE", "local")),
		SessionSecret:        str("SESSION_SECRET", ""),
		SessionTTL:           duration("SESSION_TTL", 7*24*time.Hour),
		SetupToken:           str("SETUP_TOKEN", ""),
		SignupAllowedDomains: list("SIGNUP_ALLOWED_DOMAINS"),
		OIDCIssuer:           str("OIDC_ISSUER", ""),
		OIDCAudience:         str("OIDC_AUDIENCE", ""),
		OIDCJWKSURL:          str("OIDC_JWKS_URL", ""),
		OIDCEmailClaim:       str("OIDC_EMAIL_CLAIM", "email"),
		OIDCNameClaim:        str("OIDC_NAME_CLAIM", "name"),
		OIDCTokenHeader:      str("OIDC_TOKEN_HEADER", ""),
		OIDCALBRegion:        str("OIDC_ALB_REGION", ""),

		LLMBaseURL:          strings.TrimRight(str("LLM_BASE_URL", ""), "/"),
		LLMAPIKey:           str("LLM_API_KEY", ""),
		LLMModel:            str("LLM_MODEL", ""),
		LLMRouterModel:      str("LLM_ROUTER_MODEL", ""),
		LLMSubagentModel:    str("LLM_SUBAGENT_MODEL", ""),
		LLMStream:           boolean("LLM_STREAM", true),
		LLMStreamUsage:      boolean("LLM_STREAM_USAGE", true),
		LLMExtraHeaders:     headers("LLM_EXTRA_HEADERS"),
		LLMTimeout:          duration("LLM_TIMEOUT", 3*time.Minute),
		PriceInputPerMTok:   float("LLM_PRICE_INPUT_PER_MTOK", 0),
		PriceOutputPerMTok:  float("LLM_PRICE_OUTPUT_PER_MTOK", 0),
		ContextBudgetTokens: integer("LLM_CONTEXT_BUDGET_TOKENS", 60000),

		RateLimitPerMinute: integer("RATE_LIMIT_PER_MINUTE", 12),
		DailyTokenQuota:    integer("DAILY_TOKEN_QUOTA", 2000000),
		MaxToolIterations:  integer("MAX_TOOL_ITERATIONS", 40),
		SubagentsEnabled:   boolean("SUBAGENTS_ENABLED", true),
		TurnTimeout:        duration("TURN_TIMEOUT", 10*time.Minute),

		BMadDir:       str("BMAD_DIR", ""),
		AllowedSkills: list("BMAD_ALLOWED_SKILLS"),
		PythonBin:     str("PYTHON_BIN", "auto"),

		PIIPolicy:         strings.ToLower(str("PII_POLICY", "mask")),
		PIIAllowDomains:   list("PII_ALLOW_EMAIL_DOMAINS"),
		RetentionDays:     integer("RETENTION_DAYS", 180),
		WebSearch:         strings.ToLower(str("WEB_SEARCH", "disabled")),
		SearXNGURL:        strings.TrimRight(str("SEARXNG_URL", ""), "/"),
		WebFetchAllowHTTP: boolean("WEB_FETCH_ALLOW_HTTP", true),

		Publisher:           strings.ToLower(str("PUBLISHER", "local")),
		WorkspaceLocalDir:   str("WORKSPACE_LOCAL_DIR", ""),
		GitHubToken:         str("GITHUB_TOKEN", ""),
		GitHubWorkspaceRepo: str("GITHUB_WORKSPACE_REPO", ""),
		GitHubBaseBranch:    str("GITHUB_BASE_BRANCH", "main"),
		GitHubAPIURL:        strings.TrimRight(str("GITHUB_API_URL", "https://api.github.com"), "/"),

		RepoSource:            strings.ToLower(str("REPO_SOURCE", "local")),
		RepoLocalDir:          str("REPO_LOCAL_DIR", "./demo/repos"),
		RepoOrg:               str("REPO_ORG", "acme"),
		RepoSummaryMaxAgeDays: integer("REPO_SUMMARY_MAX_AGE_DAYS", 7),

		Router:             strings.ToLower(str("ROUTER", "chain")),
		RouterThreshold:    float("ROUTER_CONFIDENCE_THRESHOLD", 0.7),
		JevAPIKey:          str("JEV_API_KEY", ""),
		MCPToken:           str("MCP_TOKEN", ""),
		DemoDelay:          duration("DEMO_DELAY", 25*time.Millisecond),
		TrustProxyHeaders:  boolean("TRUST_PROXY_HEADERS", false),
		CORSAllowedOrigins: list("CORS_ALLOWED_ORIGINS"),
		MaxUploadBytes:     int64(integer("MAX_UPLOAD_BYTES", 2*1024*1024)),
		LogLevel:           strings.ToLower(str("LOG_LEVEL", "info")),
		LogFormat:          strings.ToLower(str("LOG_FORMAT", "text")),
		SecureCookieForced: boolean("SECURE_COOKIES", false),
	}
	if c.Addr == "" {
		// PaaS (Render, Fly, Cloud Run, Koyeb) inject PORT.
		c.Addr = ":" + str("PORT", "8080")
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://localhost" + c.Addr
		if strings.HasPrefix(c.Addr, ":") {
			c.BaseURL = "http://localhost" + c.Addr
		}
	}
	if c.WorkspaceLocalDir == "" {
		c.WorkspaceLocalDir = strings.TrimRight(c.DataDir, "/\\") + "/workspace-repo"
	}
	if t := str("LLM_TEMPERATURE", ""); t != "" {
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			c.LLMTemperature = &f
		}
	}
	return c, c.Validate()
}

// Validate rejects inconsistent settings early with actionable messages.
func (c *Config) Validate() error {
	var errs []string
	switch c.AuthMode {
	case "local", "dev":
	case "oidc":
		if c.OIDCJWKSURL == "" && c.OIDCIssuer == "" && c.OIDCALBRegion == "" {
			errs = append(errs, "AUTH_MODE=oidc exige OIDC_ISSUER ou OIDC_JWKS_URL")
		}
	default:
		errs = append(errs, "AUTH_MODE deve ser local, oidc ou dev")
	}
	if c.Production() && c.AuthMode == "dev" {
		errs = append(errs, "AUTH_MODE=dev não é permitido com APP_ENV=production")
	}
	if !c.Demo() && c.LLMModel == "" {
		errs = append(errs, "LLM_MODEL é obrigatório quando LLM_BASE_URL está definido")
	}
	switch c.PIIPolicy {
	case "mask", "block", "off":
	default:
		errs = append(errs, "PII_POLICY deve ser mask, block ou off")
	}
	if c.WebSearch == "searxng" && c.SearXNGURL == "" {
		errs = append(errs, "WEB_SEARCH=searxng exige SEARXNG_URL")
	}
	if c.Publisher == "github" && (c.GitHubToken == "" || c.GitHubWorkspaceRepo == "") {
		errs = append(errs, "PUBLISHER=github exige GITHUB_TOKEN e GITHUB_WORKSPACE_REPO (owner/repo)")
	}
	if c.RepoSource == "github" && c.GitHubToken == "" {
		errs = append(errs, "REPO_SOURCE=github exige GITHUB_TOKEN")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, "APP_TIMEZONE inválido: "+c.Timezone)
	}
	if len(errs) > 0 {
		return fmt.Errorf("configuração inválida:\n- %s", strings.Join(errs, "\n- "))
	}
	return nil
}

// SecureCookies reports whether cookies must carry the Secure flag.
func (c *Config) SecureCookies() bool {
	return c.SecureCookieForced || strings.HasPrefix(c.BaseURL, "https://")
}
