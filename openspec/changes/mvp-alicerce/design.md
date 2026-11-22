# Design

## Context

- O brainstorming (`docs/brainstorming/2026-10-06-bmad-business-assistant-brainstorm.md`) aprovou as decisões D1–D13 e as Seções 1–3. Este design fecha as Seções 4–7 para o **MVP sem custo**. O alvo AWS fica documentado em `docs/aws/` e `infra/terraform/`, sem aplicar.
- O BMad Method mudou desde o brainstorming. A v6.12.4 (atual) é baseada em **skills** (`SKILL.md` + `customize.toml` + `references/` + `assets/` + scripts Python), instaladas em `.claude/skills/` mais `_bmad/` (config TOML em camadas, scripts compartilhados e manifestos). Workflows XML e *step files* sobrevivem só em `bmad-create-epics-and-stories`. As skills assumem uma CLI de agente: ler e escrever arquivos, `uv run` de scripts, subagentes, pesquisa web e abrir HTML no navegador.
- Restrições: o negócio não tem CLI; o ambiente-alvo exige stacks aprováveis (Go, Angular, AWS, gateway corporativo de IA compatível com OpenAI); o repositório é público, então os dados de demonstração são fictícios; o custo é zero nesta fase (sem AWS, só planos gratuitos).

## Goals / Non-Goals

**Goals:**
- Um binário Go (com o SPA Angular embutido) que roda local, em Docker ou em hospedagem gratuita, e que se leva para ECS sem reescrita.
- Fidelidade ao BMad: executar os arquivos originais, traduzindo o ambiente de CLI em vez de reescrever os workflows.
- Portas e adaptadores em todos os pontos que mudam entre MVP, AWS e ambiente-alvo: armazenamento, autenticação, LLM, publicador, fonte de repositórios, pesquisa web e roteador.

**Non-Goals:**
- Provisionar AWS, usar Bedrock direto ou pagar qualquer serviço.
- Fases de arquitetura e implementação do BMad para o negócio (D1).
- Integração com ferramenta de gestão do cliente (ponto de extensão via `external_handoffs` do BMad).
- Colaboração em tempo real na mesma conversa (o compartilhamento segue via Git, D7).

## Decisions

### D14 — BMad v6.12.4 vendorizado como "instalação real"
Rodamos o instalador oficial (`npx bmad-method@6.12.4 install --modules bmm --tools claude-code --no-shims -y`) e versionamos o resultado em `bmad/dist/` (`_bmad/` + `.claude/skills/`), embutido no binário (`go:embed`) e sobrescrevível por `BMAD_DIR`. **Alternativas:** ler `src/` do repositório (não é o que o dev usa) e baixar em tempo de execução (frágil e sem revisão). Atualizar é um PR que roda `scripts/update-bmad.sh`. A v7 está em beta e fica em `A-DECIDIR.md`.

### D15 — VFS com montagens em vez de disco por conversa
`{project-root}` = `/project`, com estas montagens:
- `_bmad/` (bundle, somente leitura, mais *overlay* gerado: `config.user.toml` com usuário, idioma e projeto, e `custom/*.toml` do modo negócio);
- `.claude/skills/` (somente leitura);
- `_bmad-output/` (documentos da conversa, armazenados no banco);
- `docs/systems/` (resumos de produto, somente leitura).

**Por quê:** é sem estado (horizontal no ECS), cabe em banco ou S3 e impede escrita fora do workspace. **Alternativa:** diretório real por conversa (exige disco persistente ou EFS e sessão grudada).

### D16 — Scripts: nativos onde é crítico, Python isolado no resto
`resolve_config.py`, `resolve_customization.py` e `memlog.py` (90% dos usos) são reimplementados em Go, com testes de equivalência contra os scripts originais. Os demais scripts das skills (`brain.py`, `pick_methods.py`, `resolve_party.py`, `recon_kit.py` etc.) rodam assim:
- `python` sem shell, com lista de scripts permitidos;
- diretório temporário materializado com o `_bmad/` e o workspace;
- tempo limite e saída limitada;
- de volta, só sincronizam os arquivos alterados em `_bmad-output/`.

Sem Python, a ferramenta devolve erro e a skill usa o próprio fallback. **Alternativa:** portar tudo para Go (muito código e quebra a cada versão do BMad).

### D17 — Ferramentas internas com fachada MCP (ajuste de D11)
O interpretador chama um registro de ferramentas em processo (menor latência e um só binário). O mesmo registro é exposto como **servidor MCP** (`alicerce mcp`, com `modelcontextprotocol/go-sdk`, stdio e HTTP *streamable*) para devs na IDE e para a futura separação do `bmad-mcp` em serviço próprio no ECS. **Alternativa:** cliente MCP em memória já no MVP (indireção sem ganho funcional agora).

### D18 — LLM só via API compatível com OpenAI
Um único cliente (Chat Completions, streaming, `tools`, `stream_options.include_usage`) cobre:
- gateway corporativo e Kong AI Gateway (`ai-proxy`, que traduz para Bedrock e Anthropic);
- OpenRouter, Gemini (endpoint OpenAI), Groq e Ollama.

O provedor `demo` (roteirizado e determinístico) sustenta os testes E2E e as demonstrações. **Alternativa:** SDKs por fornecedor (mais dependências para aprovar). O adaptador nativo do Bedrock (Converse) fica para a fase AWS, se o gateway não bastar.

### D19 — Roteador em camadas (D9 aplicado)
`Router` é uma interface implementada assim:
- `RuleRouter`: botões, atalhos e palavras-chave PT-BR com pontuação;
- `LLMRouter`: classificação JSON com confiança, com modelo barato configurável;
- `JevRouter`: atrás de *feature flag*, devolve `ErrNotConfigured` sem credencial;
- composição em cadeia, com limiar padrão 0,7.

Com skill ativa, a mensagem vai direto ao interpretador (a persona do BMad já despacha entre itens de menu), o que economiza uma chamada por turno.

### D20 — Persistência SQL portátil
`database/sql` com SQLite (`modernc.org/sqlite`, sem cgo) por padrão e Postgres (`pgx`) por `DATABASE_URL`. As migrações são embutidas e o mesmo SQL serve aos dois (placeholders reescritos). A trava de turno é um *lease* com update condicional, equivalente ao `ConditionExpression` do DynamoDB no alvo AWS. **Alternativa:** só Postgres (mata o "rodar com um comando").

### D21 — Exportação no navegador
`.docx` (biblioteca `docx`) e `.pptx` (`pptxgenjs`) são gerados no cliente a partir do Markdown; o PDF sai por impressão com CSS próprio. O deck executivo usa `POST /api/.../deck`: o LLM devolve um JSON de slides e o fallback é determinístico. **Por quê:** zero dependência no servidor, funciona em qualquer hospedagem e não exige pandoc. **Alternativa:** contêiner com pandoc (D6 original), que fica como opção no alvo AWS para templates corporativos `.docx` de referência.

### D22 — Autenticação plugável
- `local`: bcrypt, cookie JWT HS256 e convites;
- `oidc`: verificação de JWT por JWKS com cache, cobrindo Cognito, SSO corporativo e Supabase Auth;
- `dev`.

CSRF: cabeçalho `X-Requested-With` obrigatório em métodos de escrita, mais `SameSite=Lax`.

### D23 — Publicador e fonte de repositórios plugáveis
- `Publisher`: GitHub (REST Git Data API, com token de App ou de serviço) ou `local` (pasta com o mesmo layout e SHA de blob idêntico ao do Git).
- `RepoSource`: GitHub (listagem por prefixo `<org>-<sigla>-`) ou `local` (`demo/repos/`, com organização e sistemas fictícios).

### D24 — Pesquisa web opcional e segura
Os provedores são `disabled` (padrão) ou `searxng` (auto-hospedável e gratuito). O `web_fetch` tem proteção contra SSRF:
- só http/https;
- bloqueio de IPs privados, de loopback e link-local após resolução de DNS;
- limite de 1 MB e tempo limite.

### D25 — Frontend Angular (última estável)
- Componentes standalone, *signals* e *zoneless*.
- CSS próprio com *tokens* de design, sem biblioteca de componentes, para manter o bundle pequeno e a identidade própria.
- `marked` + `DOMPurify` para Markdown.
- `fetch` + `ReadableStream` para SSE com POST.
- HTML gerado em `iframe sandbox="allow-scripts"`, servido de `/sandbox/...` com CSP restrita.

### D26 — Kong opcional no docker-compose
A imagem OSS do Kong está congelada na 3.9.x, que já traz `ai-proxy` OSS desde a 3.6. O perfil `kong` do compose faz duas coisas:
- coloca o Kong na frente da API (rate limit e CORS);
- expõe uma rota de AI Gateway (`ai-proxy`) como stand-in do gateway corporativo.

`ai-proxy-advanced`, `ai-semantic-cache` e `ai-rate-limiting-advanced` são Enterprise.

## Risks / Trade-offs

- [Modelos gratuitos seguem pior as skills longas] → camada modo negócio enxuta, limite de iterações e modelo configurável por papel. No ambiente-alvo, o gateway corporativo dá acesso a modelos de ponta.
- [Cotas gratuitas baixas (RPM/RPD)] → retry com backoff, aviso amigável e subagentes que podem ser desligados (`SUBAGENTS_ENABLED`).
- [Prompt injection via documentos ou resumos] → ferramentas sem rede (salvo pesquisa opcional), VFS somente leitura fora do workspace, scripts em lista branca e HTML em sandbox.
- [Python no contêiner pesa no compliance] → só scripts do bundle fixado, sem shell; pode ser desligado (`PYTHON_BIN=`), e as skills usam o fallback.
- [Contexto longo e caro] → compactação com resumo, truncamento de resultados de ferramenta e orçamento diário por usuário.
- [Marca "BMad"] → produto com nome próprio ("Alicerce", configurável) e a menção "compatível com BMad Method".
- [SQLite em várias instâncias] → SQLite só para uso local ou de instância única; Postgres para compartilhado.

## Migration Plan

1. MVP local e piloto: binário ou Docker com SQLite, ou com Postgres gratuito (Supabase), mais provedor LLM gratuito ou o modo demonstração.
2. AWS: seguir `docs/aws/ARQUITETURA-AWS.md`:
   - Terraform em `infra/terraform`;
   - adaptadores DynamoDB, S3 e Cognito (OIDC);
   - Kong AI Gateway → Bedrock.
3. Ambiente-alvo: trocar `LLM_BASE_URL` para o gateway corporativo, `AUTH_MODE=oidc` para o SSO e o publicador para o GitHub corporativo.

Rollback: cada adaptador é escolhido por variável de ambiente, então voltar é trocar a variável.

## Open Questions

Itens que não mudam specs nem tarefas estão em `A-DECIDIR.md` (nome do produto, provedor do piloto, hospedagem, retenção, valor de N dias etc.).
