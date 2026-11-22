// Command alicerce runs the business BMad assistant.
//
//	alicerce serve                     start the web app (default)
//	alicerce migrate                   apply database migrations
//	alicerce user create-admin ...     create the first administrator
//	alicerce user invite --email ...   print an invite link
//	alicerce user list                 list users
//	alicerce user set-password ...     reset a password
//	alicerce mcp                       MCP server over stdio (for developers' agents)
//	alicerce doctor                    check configuration and dependencies
//	alicerce version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/caio-andres/bmad-business-assistant/internal/app"
	"github.com/caio-andres/bmad-business-assistant/internal/auth"
	"github.com/caio-andres/bmad-business-assistant/internal/config"
	"github.com/caio-andres/bmad-business-assistant/internal/llm"
	"github.com/caio-andres/bmad-business-assistant/internal/store"
	"github.com/caio-andres/bmad-business-assistant/internal/webui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version", "--version":
		fmt.Println("alicerce", app.Version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(strings.TrimSpace(usage))
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "serve":
		return serve(ctx, cfg)
	case "migrate":
		st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		v, _ := st.SchemaVersion(ctx)
		fmt.Printf("banco %s migrado (versão %d)\n", st.Dialect(), v)
		return nil
	case "user":
		return userCmd(ctx, cfg, args)
	case "mcp":
		a, err := app.New(ctx, cfg, nil, nil)
		if err != nil {
			return err
		}
		defer a.Close()
		return a.MCP.Run(ctx, &mcp.StdioTransport{})
	case "doctor":
		return doctor(ctx, cfg)
	}
	return fmt.Errorf("comando desconhecido %q\n\n%s", cmd, strings.TrimSpace(usage))
}

const usage = `
uso: alicerce [comando]

  serve                                    inicia o app web (padrão)
  migrate                                  aplica as migrações do banco
  user create-admin --email E --name N     cria um administrador (senha pedida no terminal ou --password)
  user invite --email E [--admin]          gera um link de convite
  user list                                lista as pessoas cadastradas
  user set-password --email E              redefine a senha
  mcp                                      servidor MCP via stdio (para agentes dos devs)
  doctor                                   verifica configuração e dependências
  version                                  mostra a versão

Configuração por variáveis de ambiente: veja .env.example.`

func serve(ctx context.Context, cfg *config.Config) error {
	logger := app.NewLogger(cfg)
	a, err := app.New(ctx, cfg, logger, webui.FS())
	if err != nil {
		return err
	}
	defer a.Close()
	if webui.FS() == nil {
		logger.Warn("interface web não embutida: rode `make web` (a API funciona normalmente)")
	}
	if cfg.AuthMode == "local" {
		if n, _ := a.Store.CountUsers(ctx); n == 0 {
			if cfg.SetupToken == "" && cfg.Production() {
				logger.Warn("nenhum usuário cadastrado: crie o primeiro admin com `alicerce user create-admin`")
			} else {
				logger.Info("nenhum usuário cadastrado: abra " + cfg.BaseURL + " para criar a conta de administrador")
			}
		}
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: a.Server.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	go a.RunJobs(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info(cfg.AppName+" no ar", "addr", cfg.Addr, "url", cfg.BaseURL, "config", a.Summary())
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("encerrando (aguardando turnos em andamento até 30s)")
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
	return nil
}

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("senha não informada")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("use: alicerce user create-admin|invite|list|set-password")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	email := fs.String("email", "", "e-mail")
	name := fs.String("name", "", "nome")
	password := fs.String("password", "", "senha (evite: prefira digitar quando pedido)")
	admin := fs.Bool("admin", false, "convite com papel de administrador")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	switch sub {
	case "create-admin":
		if *email == "" || *name == "" {
			return errors.New("informe --email e --name")
		}
		pw := *password
		if pw == "" {
			if pw, err = readPassword("Senha (mín. 10 caracteres): "); err != nil {
				return err
			}
		}
		h, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		u := &store.User{Email: *email, Name: *name, Role: store.RoleAdmin, PasswordHash: h}
		if err := st.CreateUser(ctx, u); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return errors.New("já existe uma conta com este e-mail")
			}
			return err
		}
		_ = st.Audit(ctx, store.AuditEntry{UserID: u.ID, Action: "admin_created_cli"})
		fmt.Printf("administrador %s criado\n", u.Email)
	case "invite":
		if *email == "" {
			return errors.New("informe --email")
		}
		role := store.RoleMember
		if *admin {
			role = store.RoleAdmin
		}
		tok := auth.NewToken()
		if err := st.CreateInvite(ctx, &store.Invite{TokenHash: auth.HashToken(tok), Email: *email, Role: role, ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}); err != nil {
			return err
		}
		fmt.Printf("convite válido por 7 dias:\n%s/#/convite/%s\n", cfg.BaseURL, tok)
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%-32s %-28s %-7s %s\n", u.Email, u.Name, u.Role, u.Status)
		}
	case "set-password":
		u, err := st.GetUserByEmail(ctx, *email)
		if err != nil {
			return errors.New("usuário não encontrado")
		}
		pw := *password
		if pw == "" {
			if pw, err = readPassword("Nova senha (mín. 10 caracteres): "); err != nil {
				return err
			}
		}
		if u.PasswordHash, err = auth.HashPassword(pw); err != nil {
			return err
		}
		if err := st.UpdateUser(ctx, u); err != nil {
			return err
		}
		_ = st.DeleteUserSessions(ctx, u.ID)
		fmt.Println("senha redefinida; sessões anteriores encerradas")
	default:
		return fmt.Errorf("subcomando desconhecido: %s", sub)
	}
	return nil
}

func doctor(ctx context.Context, cfg *config.Config) error {
	ok := true
	check := func(name string, err error, detail string) {
		if err != nil {
			ok = false
			fmt.Printf("✗ %-22s %v\n", name, err)
			return
		}
		fmt.Printf("✓ %-22s %s\n", name, detail)
	}
	a, err := app.New(ctx, cfg, nil, webui.FS())
	if err != nil {
		check("inicialização", err, "")
		return errors.New("verificação falhou")
	}
	defer a.Close()
	check("BMad", nil, fmt.Sprintf("v%s, %d skills instaladas, %d no catálogo", a.Bundle.Version, len(a.Bundle.Skills), len(a.Catalog.Items)+len(a.Catalog.Agents)))
	v, err := a.Store.SchemaVersion(ctx)
	check("banco", err, fmt.Sprintf("%s, migração %d", a.Store.Dialect(), v))
	check("interface web", nilIf(webui.FS() != nil, errors.New("não embutida (rode make web)")), "embutida")
	if a.Service.Python.Enabled() {
		check("python", nil, a.Service.Python.Bin)
	} else {
		fmt.Printf("! %-22s %s\n", "python", "indisponível: scripts opcionais das skills usarão o caminho alternativo")
	}
	if cfg.Demo() {
		fmt.Printf("! %-22s %s\n", "IA", "modo demonstração (defina LLM_BASE_URL, LLM_API_KEY e LLM_MODEL)")
	} else {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		resp, err := a.LLM.Chat(cctx, llm.Request{Model: cfg.LLMModel, Purpose: "doctor", MaxTokens: 20, Messages: []llm.Message{{Role: llm.RoleUser, Content: "Responda apenas: ok"}}}, nil)
		cancel()
		detail := ""
		if err == nil {
			detail = fmt.Sprintf("%s respondeu (%d tokens)", cfg.LLMModel, resp.Usage.InputTokens+resp.Usage.OutputTokens)
		}
		check("IA", err, detail)
	}
	if a.Publisher != nil {
		_, err := a.Publisher.Status(ctx, "ZZZ", "doctor")
		check("publicação", err, a.Publisher.Name())
	}
	if !ok {
		return errors.New("há itens a corrigir")
	}
	fmt.Println("\ntudo certo:", a.Summary())
	return nil
}

func nilIf(cond bool, err error) error {
	if cond {
		return nil
	}
	return err
}
