# Spec Delta

## Purpose

Executar as skills e os agentes originais do BMad Method (versão fixada) com a mesma fidelidade do uso via CLI, traduzindo para o chat tudo o que na CLI depende de terminal, arquivos e scripts.

## ADDED Requirements

### Requirement: BMad fixado e vendorizado
O sistema SHALL carregar uma instalação real do BMad Method com versão fixada (inicialmente v6.12.4, gerada pelo instalador oficial), incluindo a licença MIT. Atualizar a versão MUST ser uma mudança revisável (PR) que não exige alteração de código quando o formato de skills se mantém.

#### Scenario: Versão exposta
- **WHEN** a API de catálogo é consultada
- **THEN** a resposta inclui a versão do BMad em uso e a lista de skills e agentes disponíveis

### Requirement: Escopo de análise e planejamento
Por padrão, o catálogo exposto ao negócio SHALL conter apenas as skills de análise e planejamento: brainstorming, pesquisa (deep recon), forjar ideia, PRFAQ, product brief, PRD, UX, épicos e histórias, spec, elicitação avançada, party mode, revisão de documentos e ajuda. Também SHALL conter as agentes Mary (analista), John (PM) e Sally (UX). Skills de implementação MUST ficar indisponíveis e o conjunto MUST ser configurável.

#### Scenario: Skill fora da lista
- **WHEN** o modelo tenta invocar `bmad-build`
- **THEN** a ferramenta responde que a skill não está disponível neste ambiente e o turno segue

### Requirement: Sistema de arquivos virtual do projeto
Cada conversa SHALL enxergar um `{project-root}` virtual com estas montagens:
- `_bmad/` (somente leitura, com a configuração efetiva da conversa);
- `.claude/skills/` (somente leitura);
- `_bmad-output/` (leitura e escrita, documentos da conversa);
- `docs/` (somente leitura, contexto de produto).

Caminhos fora das montagens e escritas em áreas somente leitura MUST ser recusados. Tentativas de escapar com `..` também MUST ser recusadas.

#### Scenario: Escrita fora do workspace
- **WHEN** o modelo tenta escrever em `_bmad/scripts/memlog.py`
- **THEN** a ferramenta recusa com erro de permissão e o arquivo original permanece intacto

### Requirement: Ferramentas equivalentes às da CLI
O interpretador SHALL oferecer ao modelo estas ferramentas:
- ler, escrever, editar, listar (glob) e buscar arquivos;
- executar comandos permitidos;
- invocar skill;
- delegar a subagente;
- perguntar ao usuário com opções;
- abrir documento no painel;
- pesquisar e buscar na web, quando habilitadas.

Ferramentas não habilitadas MUST responder de forma que a skill siga seu próprio caminho de fallback.

#### Scenario: Pesquisa desabilitada
- **WHEN** a skill pede pesquisa web e o recurso está desligado
- **THEN** a ferramenta informa a indisponibilidade e o agente avisa o usuário e continua com o que ele trouxer

### Requirement: Scripts do BMad
Comandos `uv run`/`python` que apontem para scripts do BMad SHALL ser executados com o comportamento do script original. `resolve_config.py`, `resolve_customization.py` e `memlog.py` são implementados nativamente, com saída equivalente. Os demais scripts das skills rodam no runtime Python, quando disponível, em diretório temporário isolado, sem shell, com tempo limite. Qualquer outro comando MUST ser recusado, exceto `open`/`start`/`xdg-open` (que abre o arquivo no painel), `mkdir`, `ls`, `cat` e `date`.

#### Scenario: Memlog
- **WHEN** a skill executa `memlog.py append --workspace <ws> --type decision --text "..."`
- **THEN** uma linha `- (decision) ...` é anexada a `<ws>/.memlog.md`, o `updated` do frontmatter é atualizado e a saída é a confirmação em JSON

#### Scenario: Comando proibido
- **WHEN** o modelo tenta executar `curl` ou `rm -rf`
- **THEN** o comando é recusado sem efeito colateral

### Requirement: Customização em camadas
O sistema SHALL resolver configuração e customização com as mesmas regras do BMad: escalares sobrescrevem, tabelas mesclam em profundidade, arrays de tabelas com `code` ou `id` substituem por chave e os demais arrays são anexados. A configuração efetiva da conversa MUST definir o nome do usuário, PT-BR como idioma de comunicação e de documentos, e o nome do projeto vinculado.

#### Scenario: Idioma
- **WHEN** a configuração é resolvida para uma conversa
- **THEN** `communication_language` e `document_output_language` valem "Portuguese (Brazil)" e `user_name` é o nome da pessoa logada

### Requirement: Modo negócio
O prompt do interpretador SHALL ter três camadas: (1) a skill ou persona original, intacta; (2) o modo negócio (linguagem não técnica e assertiva, sem caminhos de arquivo, sem comandos, menus traduzidos em opções); (3) contexto (data, projeto, documentos atuais, resumos de sistemas). Caminhos internos que escaparem na resposta MUST ser trocados pelo nome amigável do documento antes de chegar ao usuário.

#### Scenario: Caminho vazado
- **WHEN** o modelo escreve "salvei em /project/_bmad-output/planning-artifacts/briefs/x/brief.md"
- **THEN** o usuário vê "salvei em **brief.md**"

### Requirement: Subagentes
A ferramenta de subagente SHALL executar um loop independente com contexto novo, ferramentas de arquivo e pesquisa, sem perguntar ao usuário e sem criar outros subagentes, com limite de iterações. Chamadas paralelas na mesma resposta do modelo MUST rodar em paralelo, com limite de concorrência. O subagente MUST devolver só o resumo final.

#### Scenario: Extração paralela
- **WHEN** o modelo delega três extrações na mesma resposta
- **THEN** as três rodam em paralelo e cada uma devolve um resumo ao modelo principal

### Requirement: Limites de execução
Cada turno MUST respeitar um máximo de iterações de ferramentas, um tempo máximo e um tamanho máximo por resultado de ferramenta (com truncamento sinalizado). O histórico longo MUST ser compactado: um resumo substitui as mensagens antigas e as recentes são mantidas intactas.

#### Scenario: Loop infinito
- **WHEN** o modelo excede o limite de iterações no turno
- **THEN** o turno é encerrado com uma mensagem ao usuário e a conversa segue utilizável
