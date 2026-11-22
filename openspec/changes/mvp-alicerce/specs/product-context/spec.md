# Spec Delta

## Purpose

Ancorar os artefatos de negócio nos sistemas reais (modo brownfield) com resumos dos repositórios de produto de cada sigla, escritos para leitores de negócio.

## ADDED Requirements

### Requirement: Descoberta por convenção
Os repositórios de produto SHALL seguir a convenção `<org>-<sigla>-<app|infra>-<nome>`. Dada uma sigla, o sistema MUST listar os repositórios dela e classificá-los como `app` (computação) ou `infra` (recursos).

#### Scenario: Listar sistemas da sigla
- **WHEN** a conversa é vinculada a um projeto da sigla PAG
- **THEN** a lista de sistemas contém só os repositórios `<org>-pag-*`

### Requirement: Prioridade das fontes de resumo
Para cada repositório, o resumo SHALL vir, nesta ordem: do `bmad-context.md` na raiz do repositório de produto (usado como está); do resumo gerado em `_repos/<repo>.md` no repositório central; ou de uma geração sob demanda.

#### Scenario: Resumo escrito pelo time
- **WHEN** o repositório tem `bmad-context.md`
- **THEN** esse arquivo é usado e nenhum resumo é gerado

### Requirement: Geração e atualização preguiçosa
Quando falta o resumo, o sistema SHALL gerá-lo em segundo plano com o LLM, a partir de README, árvore de arquivos e manifestos. O resumo MUST ter frontmatter `source_sha` e `generated_at` e ser salvo em `_repos/<repo>.md` direto na branch padrão. Um resumo com mais de N dias, cujo repositório mudou, MUST ser regenerado em segundo plano no próximo uso.

#### Scenario: Resumo ausente
- **WHEN** a conversa usa um repositório sem resumo
- **THEN** o chat avisa que o resumo está sendo preparado e o resumo fica disponível nas conversas seguintes

### Requirement: Disponível para as skills
Os resumos do projeto vinculado SHALL aparecer para as skills em `{project-root}/docs/systems/` (o `project_knowledge` do BMad). O conteúdo MUST ser voltado ao negócio: o que o sistema faz, para quem, principais fluxos, integrações, recursos de infraestrutura e limitações.

#### Scenario: PRD brownfield
- **WHEN** o PRD é escrito para um projeto da sigla PAG
- **THEN** o agente consegue ler e citar os resumos dos sistemas da sigla
