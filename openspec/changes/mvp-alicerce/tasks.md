# Tasks

## 1. Fundação do repositório

- [ ] 1.1 Criar módulo Go, `AGENTS.md`, `CLAUDE.md`, `.gitignore`, `.env.example`, `.graphifyignore` e `Makefile`/scripts (`dev`, `test`, `lint`, `build`); verificar com `go build ./...` e `go test ./...` passando com um teste de fumaça
- [ ] 1.2 Criar CI (`.github/workflows/ci.yml`): Go (vet, gofmt, test), web (lint, test, build), E2E Playwright e gitleaks; verificar o YAML com `actionlint` ou com uma execução no GitHub

## 2. Bundle BMad e configuração

- [ ] 2.1 Vendorizar o BMad v6.12.4 instalado em `bmad/dist` (com `LICENSE` e `NOTICE`) mais o script de atualização; verificar se o manifesto lido pelo Go reporta a versão 6.12.4
- [ ] 2.2 Implementar o merge estrutural de TOML (resolve_config e resolve_customization) com testes de equivalência contra os scripts Python originais
- [ ] 2.3 Implementar o catálogo PT-BR (skills permitidas, agentes, fases, ícones) a partir de manifestos e config; verificar com teste que `bmad-build` não aparece e que Mary, John e Sally aparecem
- [ ] 2.4 Gerar o overlay por conversa (`config.user.toml` e `custom/*.toml` do modo negócio); verificar com teste que a config efetiva tem PT-BR e o nome do usuário

## 3. VFS e ferramentas do runtime

- [ ] 3.1 Implementar o VFS com montagens (somente leitura e leitura e escrita), proteção contra `..` e glob `**`; verificar com testes de permissão e de escape
- [ ] 3.2 Implementar as ferramentas de arquivo (read, write, edit, list, search) com truncamento sinalizado; verificar com testes unitários
- [ ] 3.3 Implementar `run_command`: parser com aspas, memlog nativo (init, append, set), resolve_* nativos, open/start/xdg-open, mkdir, ls, cat e date, com recusa do resto; verificar com testes de equivalência do memlog
- [ ] 3.4 Implementar o runner Python isolado (lista branca, temp dir, timeout, sincronização de volta); verificar com teste que roda `brain.py categories` quando há Python
- [ ] 3.5 Implementar `invoke_skill` (catálogo permitido, skill-root resolvido) e `ask_user` (opções); verificar com testes

## 4. Provedor de LLM

- [ ] 4.1 Implementar o cliente compatível com OpenAI (streaming SSE, tools, usage, retry com backoff); verificar com testes contra um servidor `httptest`
- [ ] 4.2 Implementar o provedor `demo` roteirizado que exercita o fluxo real; verificar com teste de ponta a ponta do orquestrador
- [ ] 4.3 Implementar contabilização de tokens e custo estimado por modelo; verificar com teste do agregado de uso

## 5. Interpretador e orquestração

- [ ] 5.1 Implementar o prompt em 3 camadas (skill/persona, modo negócio, contexto) e o sanitizador de caminhos; verificar com testes de snapshot e do sanitizador
- [ ] 5.2 Implementar o loop de ferramentas com limites (iterações, tempo, tamanho) e a execução paralela de subagentes; verificar com testes usando um provedor fake
- [ ] 5.3 Implementar a máquina de estados, o lease de turno (409 em concorrência) e a compactação de histórico; verificar com testes
- [ ] 5.4 Implementar o Router (regras PT-BR, LLM JSON, stub Jev, cadeia com limiar); verificar com testes de classificação

## 6. Persistência

- [ ] 6.1 Criar as migrações SQL (usuários, convites, sessões, conversas, mensagens, transcrição, arquivos, publicações, uso, auditoria) para SQLite e Postgres; verificar com os testes do store nos dois bancos (Postgres via Docker)
- [ ] 6.2 Implementar os stores com isolamento por usuário; verificar com teste de que um usuário não lê a conversa de outro

## 7. Autenticação e acesso

- [ ] 7.1 Implementar os modos local (bcrypt, cookie, convites), dev e oidc (JWKS); verificar com testes, inclusive JWT inválido e expirado
- [ ] 7.2 Implementar CSRF, rate limit por usuário, cota diária de tokens e trilha de auditoria; verificar com testes de 403 e 429
- [ ] 7.3 Implementar os comandos de CLI `alicerce user create-admin` e `alicerce user invite`; verificar com execução manual

## 8. API HTTP e streaming

- [ ] 8.1 Implementar as rotas REST (catálogo, conversas, mensagens, documentos, projetos, admin) e o SSE do turno com desconexão tolerante; verificar com testes de handler
- [ ] 8.2 Servir o SPA embutido e o `/sandbox` com CSP restrita, mais health/ready; verificar com teste de cabeçalhos

## 9. Proteção de dados

- [ ] 9.1 Implementar o detector e mascarador (CPF e CNPJ com dígito verificador, cartão com Luhn, e-mail, telefone, segredos) com as políticas mask, block e off; verificar com testes de tabela
- [ ] 9.2 Implementar a rotina de retenção de conversas arquivadas; verificar com teste

## 10. Frontend Angular

- [ ] 10.1 Criar o app Angular (última estável) com tokens de design, layout responsivo, login e convite; verificar com `ng build` e testes de componente
- [ ] 10.2 Implementar a home (catálogo por fase e agentes, caixa livre) e a lista de conversas; verificar com teste de componente
- [ ] 10.3 Implementar o chat com streaming, atividade, respostas rápidas, estados e erros amigáveis; verificar com teste do parser de SSE
- [ ] 10.4 Implementar o painel de documentos (Markdown sanitizado, HTML em sandbox, edição manual, status); verificar com teste
- [ ] 10.5 Implementar exportação `.md`, `.docx`, `.pptx` (deck executivo), PDF e `.zip`; verificar com teste unitário dos geradores
- [ ] 10.6 Implementar a tela de projetos (sigla e projeto, Carregar, Publicar, status, conflito) e o admin (convites, uso); verificar com E2E

## 11. Workspace e publicação

- [ ] 11.1 Implementar o publicador local (layout `<SIGLA>/<projeto>`, SHA de blob do Git, conflitos); verificar com testes
- [ ] 11.2 Implementar o publicador GitHub (Git Data API, branch `publish/<sigla>/<projeto>`, PR acumulador, trailer, status); verificar com testes contra um GitHub fake (`httptest`)
- [ ] 11.3 Implementar a resolução de conflito (minha, do repositório ou unificada pelo assistente); verificar com teste

## 12. Contexto de produto

- [ ] 12.1 Implementar `RepoSource` local (`demo/repos`, fictício) e GitHub (prefixo da sigla), mais a prioridade de resumos; verificar com testes
- [ ] 12.2 Implementar a geração de resumo em segundo plano com `source_sha` e `generated_at` e a atualização preguiçosa; verificar com teste usando LLM fake

## 13. Empacotamento e deploy gratuito

- [ ] 13.1 Criar o Dockerfile multi-stage (web, Go, runtime com Python) e o docker-compose (app, Postgres, perfis kong e searxng); verificar com `docker compose up` e health OK
- [ ] 13.2 Criar o blueprint de hospedagem gratuita e o guia Supabase (Postgres gratuito); verificar se as migrações aplicam no Postgres
- [ ] 13.3 Configurar o Kong OSS (DB-less) com rate limit e `ai-proxy`; verificar com `kong config parse` no contêiner

## 14. Plano AWS

- [ ] 14.1 Escrever `docs/aws/ARQUITETURA-AWS.md` (diagrama, serviços, segurança, custos estimados, fases de migração); verificar com revisão de conteúdo
- [ ] 14.2 Escrever os módulos Terraform (rede, ECR, ECS, ALB, DynamoDB, S3, Cognito, CloudFront, segredos, observabilidade) sem aplicar; verificar com `terraform validate` e `terraform fmt -check`

## 15. MCP para devs

- [ ] 15.1 Expor catálogo, projetos e artefatos via servidor MCP (go-sdk, stdio); verificar com teste de cliente MCP em memória

## 16. Documentação e verificação integrada

- [ ] 16.1 Escrever o guia do usuário de negócio, o runbook de operação, a arquitetura do MVP e o `A-DECIDIR.md`; verificar se os links internos resolvem
- [ ] 16.2 Rodar a suíte E2E Playwright no modo demo (login, brief de ponta a ponta, exportar, publicar local); verificar com `npx playwright test` verde

## Workflow follow-up

- Abrir PR de `feat/mvp-alicerce` para `main` com `Closes #...` das issues.
- Após o merge: `openspec archive mvp-alicerce`.
- Registrar o status no vault (`wiki/BMAD Business Assistant.md`).
