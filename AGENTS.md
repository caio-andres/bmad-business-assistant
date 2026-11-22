# Alicerce (repo `bmad-business-assistant`)

Assistente web em PT-BR para que o **time de negócio** use o BMad Method (análise e planejamento: brief, PRD, UX, épicos e histórias) sem CLI. A casca em Go é determinística (estados, roteador, publicação). O núcleo interpretado é um LLM com ferramentas, que executa os arquivos **originais** do BMad v6.12.4 (vendorizado em `bmad/dist`).

## Stack
- Go 1.27 (stdlib `net/http`, SQLite `modernc` ou Postgres `pgx`, MCP `go-sdk`), com o SPA embutido no binário.
- Angular (última estável) em `web/`, com exportação `.docx`/`.pptx` no navegador.
- LLM via API compatível com OpenAI (gateway corporativo, Kong AI Gateway, OpenRouter, Gemini, Groq, Ollama). Sem chave, o app roda em **modo demonstração**.
- Alvo AWS (planejado, não provisionado): `docs/aws/` e `infra/terraform/`.

## Comandos
| Para | Comando |
|---|---|
| Testes Go (contêiner Linux) | `scripts/dev-go.sh test ./...` |
| Testes Go (host Linux/macOS) | `go test ./...` |
| Lint Go | `go vet ./... && gofmt -l .` |
| Frontend: instalar, testar, build | `cd web && npm ci && npm test && npm run build` |
| Rodar tudo local (demo, SQLite) | `make run` ou `docker compose -f deploy/docker-compose.yml up --build` |
| E2E | `cd e2e && npx playwright test` |
| Atualizar o BMad | `scripts/update-bmad.sh <versão>` (gera PR revisável) |
| Primeiro admin | `alicerce user create-admin --email ... --name ...` |

## Estrutura
- `cmd/alicerce/`: binário (serve, migrate, user, mcp).
- `internal/bmad/`: bundle, catálogo PT-BR e merge TOML idêntico aos scripts do BMad (testado contra o Python).
- `internal/vfs/`: `{project-root}` virtual (montagens somente leitura e workspace).
- `internal/runtime/`: ferramentas do modelo (arquivos, `run_command`, memlog nativo, Python isolado, skills e perguntas).
- `internal/llm/`: cliente OpenAI-compatível e provedor `demo`.
- `internal/agent/`: loop do interpretador (prompt em 3 camadas, subagentes, compactação, sanitização).
- `internal/orchestrator/`: máquina de estados, roteador e turnos.
- `internal/store/`: persistência SQL.
- `internal/auth/`, `internal/guard/`, `internal/workspace/`, `internal/repoctx/`, `internal/httpapi/`.
- `web/`: Angular. `e2e/`: Playwright. `demo/`: dados **fictícios**. `deploy/`: Docker, compose e Kong.
- `openspec/specs/`: comportamento esperado (fonte da verdade); mudanças em `openspec/changes/`.

## Convenções
- Conventional Commits; branch por mudança e PR para `main`.
- Variáveis de ambiente: veja `.env.example` (nunca ler nem commitar `.env`).
- Arquivos do BMad em `bmad/dist` **não se editam à mão**. O modo negócio entra pela customização nativa (`_bmad/custom/*.toml` gerado em `internal/bmad/overlay.go`) e pelo prompt (`internal/agent/prompt.go`).
- Ferramenta nova para o modelo: registrar em `internal/runtime` com `Label` em linguagem de negócio, sem caminhos.
- Decisões pendentes do usuário: `A-DECIDIR.md`.

## Guardrails (repositório público)
- Nada do cliente corporativo: nem nome da empresa, nem da plataforma interna de IA, nem siglas, pessoas ou repositórios reais. Use "cliente corporativo", "gateway corporativo de IA" e dados fictícios (`acme`, siglas `PAG`/`CRT`/`ONB`).
- Varredura de segredos (gitleaks) no pre-commit e no CI. Se bloquear, remova o segredo; nunca use `--no-verify`.

## Contexto de longo prazo
- Brainstorming e decisões D1–D13: `docs/brainstorming/`. Design do MVP (D14+): `openspec/changes/mvp-alicerce/design.md`.
- Nota no vault: `wiki/BMAD Business Assistant.md`.
- Grafo do código: `graphify-out/GRAPH_REPORT.md` (gerar com `graphify update .`).
