# Spec Delta

## Purpose

Mostrar ao vivo os documentos que a conversa produz e permitir levá-los para fora: para a mesa do negócio (Word, PowerPoint, PDF) e para o time técnico (Markdown nativo do BMad).

## ADDED Requirements

### Requirement: Painel de documentos ao vivo
O sistema SHALL listar os documentos da conversa (brief, PRD, UX, épicos, pesquisa e memória da sessão) com nome amigável, tipo e status do frontmatter (`draft`, `complete`). Cada escrita do agente MUST atualizar o painel durante o turno.

#### Scenario: Seção aparece no painel
- **WHEN** o agente grava uma nova seção no brief
- **THEN** o painel exibe o conteúdo renderizado sem recarregar a página

### Requirement: Pré-visualização segura de HTML
Documentos HTML gerados (protótipos de UX, relatórios de validação, seletor de técnicas de brainstorming) SHALL abrir no painel dentro de um iframe isolado, sem acesso à origem da aplicação, a cookies ou à API. Conteúdo Markdown MUST ser sanitizado antes de renderizar.

#### Scenario: Script malicioso em HTML gerado
- **WHEN** um HTML gerado tenta ler `document.cookie` da aplicação
- **THEN** o acesso falha porque o iframe roda sem `allow-same-origin`

### Requirement: Exportação
Todo documento Markdown SHALL ser exportável como `.md` (original), `.docx` (títulos, listas, tabelas, ênfase), `.pptx` (um slide por seção, com resumo executivo opcional gerado pelo LLM) e PDF (impressão do painel). A conversa inteira MUST ser exportável como `.zip` dos documentos.

#### Scenario: Deck executivo
- **WHEN** o usuário pede "Apresentação executiva" de um PRD
- **THEN** recebe um `.pptx` com capa, até 10 slides de tópicos e slide de próximos passos

#### Scenario: Sem LLM disponível
- **WHEN** o resumo executivo falha ou está desligado
- **THEN** o `.pptx` é gerado de forma determinística a partir das seções do documento

### Requirement: Edição manual
O usuário SHALL poder editar um documento Markdown diretamente no painel. A edição MUST ser registrada como mensagem de sistema para que o agente saiba da mudança no próximo turno.

#### Scenario: Ajuste manual
- **WHEN** o usuário corrige um número no PRD e salva
- **THEN** o próximo turno do agente recebe o aviso "o usuário editou prd.md"
