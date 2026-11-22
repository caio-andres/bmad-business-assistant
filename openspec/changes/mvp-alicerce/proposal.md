# Proposal

## Why

O time de negócio do cliente corporativo não tem acesso às CLIs de agentes (Claude Code, Devin) que os devs usam para rodar o BMad Method. Por isso fica de fora justamente da fase em que mais deveria atuar: análise e planejamento (brief, PRD, UX, épicos e histórias). O brainstorming de 2026-10-06 aprovou a arquitetura (Seções 1–3). Agora é hora de entregar um MVP funcional, sem custo, que o time possa usar e que se leve para a AWS e para o ambiente-alvo sem reescrita.

## What Changes

- Nova aplicação web **Alicerce**, nome de trabalho; o produto não pode usar "BMad" no nome, ver `A-DECIDIR.md`. Ela tem chat em PT-BR e painel de documento ao lado, pensada para pessoas de negócio.
- Backend em Go com a **casca determinística** (máquina de estados, `Router`, checkpoints, exportação, publicação) e o **núcleo interpretado** (loop LLM com ferramentas executando os arquivos originais do BMad Method v6.12.4, fixado e vendorizado).
- Runtime BMad com sistema de arquivos virtual, ferramentas equivalentes às de uma CLI de agente (ler, escrever, editar, listar e buscar arquivos, executar scripts do BMad, invocar skill, subagente, perguntar ao usuário, abrir documento) e camada "modo negócio".
- Provedor de LLM via API compatível com OpenAI, o que cobre gateway corporativo de IA, Kong AI Gateway, OpenRouter, Gemini, Groq e Ollama. Inclui também um **modo demonstração** determinístico, sem custo, para testes e apresentações.
- Workspace por conversa, com **Carregar** e **Publicar** sobre um repositório central (`<SIGLA>/<projeto>/`). A publicação vai para o GitHub (PR acumulador por projeto) ou para uma pasta local. Conflitos são detectados por SHA de blob.
- Contexto de produto (brownfield): resumos de repositório lidos do `bmad-context.md`, do cache `_repos/` ou gerados por LLM.
- Exportação para `.md`, `.docx`, `.pptx` (deck executivo) e PDF (impressão).
- Autenticação local por convite (com papéis `admin` e `member`), modo `dev` e verificador OIDC/JWKS para o SSO do ambiente-alvo; trilha de auditoria; limites de uso e custo.
- Proteção de dados: mascaramento de dados pessoais (CPF, CNPJ, cartão, e-mail, telefone, segredos) antes de enviar ao LLM.
- Persistência SQL portátil: SQLite por padrão e Postgres (Supabase gratuito, RDS ou Aurora).
- Empacotamento: binário único com o frontend embutido, Dockerfile, docker-compose (com perfis Kong e SearXNG) e blueprint gratuito.
- **Plano AWS** documentado e Terraform validado, sem aplicar: ECS Fargate, DynamoDB, S3, Cognito, CloudFront, Bedrock via gateway.
- Servidor MCP (go-sdk) que expõe o catálogo e os artefatos do workspace para devs plugarem na IDE.

## Capabilities

### New Capabilities
- `conversation-orchestration`: ciclo de vida da conversa, máquina de estados, roteamento de intenção com confiança, turnos com streaming, respostas rápidas e retomada.
- `bmad-runtime`: execução fiel das skills e agentes do BMad Method (VFS, ferramentas, scripts, customização em camadas, subagentes, escopo de análise e planejamento, modo negócio).
- `document-workspace`: documentos produzidos na conversa, painel ao vivo, pré-visualização segura de HTML e exportação.
- `workspace-publishing`: carregar e publicar o "BMad da sigla/projeto" no repositório central, PR acumulador, conflitos e status de revisão.
- `product-context`: resumos de repositórios de produto por sigla (ler, gerar, atualizar) disponíveis para as skills.
- `access-control`: autenticação, papéis, convites, sessões, auditoria e limites de uso.
- `llm-gateway`: abstração de provedor de LLM, contabilização de tokens e custo, orçamento e fallback.
- `data-protection`: detecção e mascaramento de dados sensíveis e guardrails do repositório público.

### Modified Capabilities
- (nenhuma: projeto novo)

## Impact

- Código novo: módulo Go na raiz (`cmd/`, `internal/`), app Angular em `web/`, BMad vendorizado em `bmad/`, infraestrutura em `deploy/` e `infra/terraform/`.
- Dependências novas: Go 1.27, Node 26 e Angular, Python 3.11+ no contêiner (scripts originais do BMad), pandoc opcional.
- Sistemas externos opcionais: GitHub (repositório central de workspace), provedor de LLM compatível com OpenAI, Supabase Postgres (plano gratuito) e SearXNG.
- Sem custo: nada é provisionado na AWS nesta mudança.
