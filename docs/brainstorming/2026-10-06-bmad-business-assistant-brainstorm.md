---
status: em andamento
iniciado: 2026-10-05
atualizado: 2026-10-06
proximo_passo: aprovar Seção 3 e seguir para a Seção 4 (exportação)
---

# BMAD Business Assistant — brainstorming (rascunho)

> Registro do brainstorming em andamento. **Não é a spec final.** Quando todas as seções forem aprovadas, vira `docs/superpowers/specs/YYYY-MM-DD-bmad-business-assistant-design.md`.
> Caminho do processo: **arquitetural** (projeto novo, vários subsistemas).

## 1. Entendimento do objetivo

**O que é:** um chat conversacional para que o time de negócio use o BMAD-METHOD sem CLI. Por baixo, a aplicação executa os agentes, skills e workflows do BMAD e mantém os artefatos.

**Critério de sucesso da POC:** pessoas de negócio produzem **artefatos BMAD de verdade** só conversando, com a mesma qualidade do uso via CLI. A linguagem é pouco técnica e mais assertiva.

**Contexto:** a POC será replicada no ambiente de um cliente corporativo e também é portfólio público. Por isso as stacks precisam ser replicáveis lá dentro, e o repositório não pode ter nada sensível nem nome do cliente ou das plataformas internas dele (ver `TECH_SPEC.md`).

## 2. Decisões tomadas

| # | Tema | Decisão | Por quê |
|---|---|---|---|
| D1 | Escopo do ciclo BMAD | **Análise + planejamento**: brainstorming, pesquisa, product brief, PRD, UX, épicos/histórias/tarefas (Analyst, PM, UX, PO). Arquitetura e implementação ficam de fora. | É onde o negócio atua no BMAD; termina em artefatos prontos para o time técnico. |
| D2 | Fonte de conhecimento | **BMAD é o motor** (agentes/workflows/templates/checklists) + **repositórios de produto como contexto opcional** (brownfield), via um `.md` de resumo por repositório. | Artefatos ancorados no sistema real, sem inventar. |
| D3 | Convenção de repositórios de produto | `<org>-<sigla>-<app\|infra>-<nome>`. Sigla = 3 caracteres; `app` = computação (Lambda, ECS…); `infra` = recursos (SQS, API Gateway…). Na POC, prefixo e siglas fictícios. | A sigla permite agrupar os repositórios de um mesmo sistema automaticamente. |
| D4 | Resumo de repositório | **Lê se existir, gera se faltar.** | Muitos repositórios não terão resumo; gerar torna o modo brownfield utilizável. |
| D5 | Hospedagem de código | **GitHub.** | — |
| D6 | Saída dos artefatos | **Download na hora** (`.md`/`.docx`, `.pptx`) + **versionamento no formato nativo do BMAD** num repositório de docs (PR). **Sem integração com ferramenta de gestão** por ora (ponto de extensão). | O cliente usa uma ferramenta própria cuja integração ainda é desconhecida; o formato nativo permite que devs continuem na CLI. |
| D7 | Unidade de trabalho | **Conversas individuais** (estilo ChatGPT) sobre um **"BMAD da sigla/projeto" compartilhado**: botões **Carregar** (pull) e **Publicar** (push). | Começa simples; o compartilhamento passa pelo Git. |
| D8 | Onde mora o BMAD compartilhado | **Um repositório central** (`<org>-bmad-workspace`) com pasta por sigla e subpasta por projeto. | Mais simples para a POC (um único acesso). Trade-off aceito: mistura donos diferentes num só lugar. |
| D9 | Jev (TypeSafe AI) | Roteamento vira a **interface `Router`**. Implementação padrão = classificador via LLM no gateway; **Jev = adaptador experimental atrás de feature flag**, para comparar latência e acurácia. | Jev é SaaS proprietário em early access (sem self-host, SDK Python) e dificilmente passa no compliance do ambiente-alvo tão cedo; mas é bom para decisões tipadas com confiança. |
| D10 | Como executar o BMAD | **Híbrido: casca determinística + núcleo interpretado.** A casca (máquina de estados) cuida de roteamento, estado, pausas, checkpoints, exportação e publicação; o núcleo é um LLM com ferramentas executando os **arquivos originais** do BMAD. | Fidelidade ao BMAD (que evolui) + controle onde importa. Descartadas: só interpretar (imprevisível) e compilar workflows em grafos (caro, se afasta do BMAD). |
| D11 | LangGraph | **Não usar por ora.** A casca é uma máquina de estados explícita em **Go**, com estado no DynamoDB; o núcleo é um loop de tool-calling com cliente MCP (`modelcontextprotocol/go-sdk`). Reavaliar (LangGraph ou Eino) se surgirem agentes em paralelo ou grafos dinâmicos. | Critério do usuário: só usar se necessário. Uma linguagem só e menos dependências para aprovar no ambiente-alvo. |
| D12 | Publicação no GitHub | **Na hora, via GitHub App**, com **PR acumulador por projeto** (branch `publish/<sigla>/<projeto>`). Lote diário via EventBridge foi **descartado**. | O negócio não precisa de conta no GitHub em nenhum dos casos. Publicar na hora detecta conflito com a pessoa presente, mantém o Git como única fonte da verdade e não exige staging próprio. O PR acumulador já reduz o ruído para quem revisa. |
| D13 | Merge do PR | **Revisão humana** do time técnico da sigla (via `CODEOWNERS`). *Assumido; usuário não respondeu explicitamente; confirmar.* | Repositório compartilhado entre donos diferentes; portão de revisão esperado no ambiente-alvo. |

## 3. Design — seções

### Seção 1 — Arquitetura geral ✅ aprovada (com D12 aplicada)

```
 Angular 20+ (S3 + CloudFront)
        │  HTTPS + SSE (streaming das respostas)
        ▼
 Kong ─ API Gateway: valida JWT (Cognito), rate limit
        │
        ▼
 api (Go, ECS Fargate) ───────────────► DynamoDB: conversas, mensagens, estado
  ├─ http/BFF                           S3: workspace privado de cada conversa
  ├─ orchestrator (casca: máquina de estados + Router)
  └─ interpreter (loop LLM ↔ ferramentas) ──MCP──► bmad-mcp (Go, ECS)
        │                                           ├─ bmad.*      (lê _bmad/)
        │                                           ├─ artifact.*  (workspace S3)
        │                                           └─ repo.*      (resumos)
        ▼
 Kong ─ AI Gateway (ai-proxy) ──► Amazon Bedrock (Claude) │ OpenRouter
        ▲  stand-in do "gateway corporativo de IA"

 exporter (container com pandoc): md → .docx / .pptx com template
 repo-indexer (worker Go): gera o resumo de repositório que falta
 GitHub (GitHub App): repositório central de workspace + repositórios de produto
```

- **ECS Fargate** nos serviços principais (passos de workflow longos + streaming SSE); Lambda só para workers curtos.
- **Bedrock** como provedor principal atrás do Kong; **OpenRouter** como segunda rota para comparar modelos.
- **`bmad-mcp` separado da `api`**: devs podem plugar o mesmo MCP na IDE; separa "quem decide" de "quem acessa dados".
- **Cognito** na POC → SSO do cliente no ambiente-alvo (mesmo contrato OIDC/JWT).
- **GitHub App** como única identidade que escreve no GitHub; autor real no trailer do commit.

### Seção 2 — A conversa executando o BMAD ✅ aprovada

**Estado da conversa** (DynamoDB, com controle de versão: um turno por vez): usuário, sigla/projeto (opcional), agente e workflow ativos, workspace privado no S3 (`ws/<conversa>/`) com SHAs base, histórico + resumo das partes antigas.

**Máquina de estados:**

```
            mensagem livre
 OCIOSO ──────────────────► ROTEANDO ── baixa confiança ──► pergunta com botões
   ▲                            │
   │                            ▼ {agente, workflow, confiança}
   │                     EXECUTANDO ◄──────────────┐
   │                      │    │    │              │ resposta do usuário
   │       fim do workflow│    │    └──► AGUARDANDO_USUÁRIO
   │                      │    ▼
   └──────────────────────┘  CHECKPOINT (salva seção, atualiza painel)

 Botões (determinísticos, sem LLM): CARREGAR · EXPORTAR · PUBLICAR
```

- **ROTEANDO:** decide entre continuar o workflow ativo, iniciar o workflow X do agente Y, responder uma dúvida sobre o projeto ou recusar algo fora do escopo. Saída tipada com confiança; abaixo do limiar, o usuário escolhe em botões.
- **EXECUTANDO:** o prompt tem três camadas: (1) persona do agente + instruções do passo atual, intactas, via MCP; (2) **"modo negócio"**: linguagem não técnica e assertiva, sem caminhos de arquivo; (3) contexto: artefato atual, resumos de repositório da sigla, mensagens recentes.
- **Progresso determinístico:** workflows recentes do BMAD usam *step files* e registram o progresso no próprio artefato; a casca lê isso. *(Confirmar na versão do BMAD fixada.)*

**Tradução CLI → interface:**

| No BMAD (CLI) | No chat |
|---|---|
| pergunta ao usuário | mensagem + respostas rápidas sugeridas |
| menus numerados (`[a] Elicitação avançada [c] Continuar...`) | botões ("Aprofundar", "Seguir") |
| `template-output` (fim de seção) | CHECKPOINT: seção aparece no **painel do documento** ao lado do chat |
| ler/escrever arquivo | ferramentas MCP sobre o workspace privado |

- **Repositórios de produto:** com sigla definida, o interpretador tem as ferramentas `repo.*`. Resumo faltando → `repo-indexer` gera de forma assíncrona e o chat avisa o tempo estimado; o resultado fica salvo.

### Seção 3 — Workspace, GitHub e resumos de repositório ⏳ apresentada, aguardando aprovação

**Repositório central** (`<org>-bmad-workspace`):

```
_bmad/                           BMAD instalado, versão fixada (atualizar = PR)
_repos/<repo>.md                 resumos de repositório gerados pela aplicação
<SIGLA>/<projeto>/               saída nativa do BMAD: brief, PRD, UX, épicos/histórias
<SIGLA>/<projeto>/project.yaml   siglas e repositórios relacionados ao projeto
.github/CODEOWNERS               <SIGLA>/ → time técnico da sigla
```

- `CODEOWNERS` define quem revisa, sem código nosso. Dev na CLI: clona o repositório e aponta a pasta de saída do BMAD para `<SIGLA>/<projeto>/`.

**Carregar:** escolher sigla + projeto (ou criar) → origem = branch `publish/<sigla>/<projeto>` se houver PR aberto, senão `main` → copiar para o workspace privado no S3 guardando o SHA de cada arquivo.

**Publicar:**
1. Calcula o que mudou em relação aos SHAs base.
2. Sem conflito → um único commit na branch do projeto, com trailer `Publicado-por: Nome`; cria o PR se não houver um aberto.
3. Com conflito → nada é sobrescrito; o assistente mostra as diferenças no chat, propõe a versão unificada e só publica depois do "ok".
4. Status na interface: *Em revisão* → *Aprovado* (consultado no GitHub ao abrir o projeto; sem webhook na POC).

**Resumos de repositório:**
- Prioridade 1: `bmad-context.md` na raiz do repositório de produto, escrito pelo time e usado como está.
- Prioridade 2: resumo gerado em `_repos/<repo>.md`, com frontmatter `source_sha` e `generated_at`.
- Atualização preguiçosa: no uso, se o `main` do repositório de produto mudou e o resumo tem mais de N dias, regenera em segundo plano.
- Resumos gerados vão **direto para o `main`**, sem PR (dados derivados e regeneráveis, como cache).
- Conteúdo voltado ao negócio: o que o sistema faz, para quem, principais fluxos, integrações, recursos de infra (para os `infra`) e limitações conhecidas.

**Guardrail do repositório público:** na POC, nomes, siglas, pessoas e repositórios de produto são **fictícios** (o histórico de commits também é público).

### Seções pendentes

- **Seção 4 — Exportação:** `.docx`/`.pptx` via pandoc com template de referência; como transformar PRD/brief em apresentação para o negócio.
- **Seção 5 — LLM, Kong e segurança:** Kong (API Gateway + AI Gateway), Bedrock/OpenRouter, implementação padrão do `Router` + adaptador Jev, guardrails de prompt, dados sensíveis, autenticação.
- **Seção 6 — AWS e Terraform:** rede, ECS, DynamoDB, S3, Cognito, segredos, ambientes, estrutura dos módulos.
- **Seção 7 — Erros, observabilidade e testes:** falhas do LLM e do GitHub, retomada de conversa, custo por conversa, como testar a "mesma qualidade da CLI".

## 4. Pontos em aberto

- [ ] Aprovar a Seção 3.
- [ ] Confirmar D13 (merge com revisão humana vs. auto-merge vs. botão do negócio).
- [ ] Fixar a versão do BMAD-METHOD e confirmar o modelo de *step files* + progresso no artefato.
- [ ] Kong: verificar quais plugins de IA são OSS e quais são Enterprise (ai-proxy, rate limit por tokens, prompt guard, roteamento semântico).
- [ ] Valor de N (dias) para considerar um resumo de repositório desatualizado.
- [ ] Jev: obter acesso ao early access para o adaptador experimental.
