# Spec Delta

## Purpose

Conduzir conversas individuais entre uma pessoa de negócio e os agentes do BMad Method, com estado explícito, roteamento de intenção, streaming das respostas e retomada a qualquer momento.

## ADDED Requirements

### Requirement: Conversas individuais e persistentes
O sistema SHALL permitir que cada usuário crie, liste, renomeie, arquive e reabra as próprias conversas. Cada conversa MUST guardar mensagens, estado, skill e agente ativos, projeto vinculado (opcional) e documentos. Um usuário MUST NOT acessar conversas de outro usuário.

#### Scenario: Retomar conversa
- **WHEN** o usuário reabre uma conversa criada em outro dia
- **THEN** o histórico, o documento em andamento e a skill ativa são restaurados e a próxima mensagem continua de onde parou

#### Scenario: Isolamento entre usuários
- **WHEN** um usuário tenta ler uma conversa de outro usuário pela API
- **THEN** a API responde 404 sem revelar que a conversa existe

### Requirement: Máquina de estados explícita
Cada conversa SHALL estar em exatamente um estado: `idle`, `routing`, `executing`, `awaiting_user` ou `error`. As transições MUST ser determinísticas e registradas: mensagem livre em `idle` vai para `routing`; decisão confiante vai para `executing`; pergunta ao usuário vai para `awaiting_user`; resposta do usuário vai para `executing`; fim da skill vai para `idle`.

#### Scenario: Pergunta encerra o turno
- **WHEN** o agente pede uma informação ao usuário
- **THEN** a conversa passa para `awaiting_user`, o turno termina e nenhuma nova chamada ao LLM é feita até o usuário responder

### Requirement: Um turno por vez
O sistema MUST processar no máximo um turno por conversa ao mesmo tempo, mesmo com várias instâncias da aplicação. Uma segunda mensagem enviada durante um turno em andamento MUST ser rejeitada com erro 409 e mensagem amigável.

#### Scenario: Duplo envio
- **WHEN** o usuário envia duas mensagens em sequência rápida na mesma conversa
- **THEN** a primeira é processada e a segunda recebe 409 "Aguarde a resposta anterior terminar"

### Requirement: Roteamento de intenção com confiança
Em `idle`, o sistema SHALL classificar a mensagem do usuário como uma destas saídas: iniciar uma skill do catálogo permitido, conversar com um agente, tirar dúvida sobre o método ou recusar pedido fora do escopo. A classificação MUST ter nível de confiança. Abaixo do limiar configurado, o sistema MUST oferecer até três opções em botões em vez de decidir sozinho. Botões e atalhos explícitos MUST ser roteados sem chamar o LLM.

#### Scenario: Intenção clara
- **WHEN** o usuário escreve "quero escrever o PRD do novo app de cashback"
- **THEN** a skill de PRD é iniciada com a agente correspondente e a mensagem original é repassada como contexto

#### Scenario: Intenção ambígua
- **WHEN** a confiança fica abaixo do limiar
- **THEN** o usuário vê uma pergunta curta com até três botões de skills candidatas

#### Scenario: Fora do escopo
- **WHEN** o usuário pede para implementar código ou desenhar a arquitetura técnica
- **THEN** o assistente explica, sem jargão, que essa etapa é do time técnico e sugere publicar os artefatos para eles

### Requirement: Streaming do turno
O sistema SHALL transmitir cada turno por Server-Sent Events, com eventos tipados: texto incremental, atividade das ferramentas em linguagem de negócio, documento atualizado, opções de resposta, mudança de estado, uso de tokens, erro e fim. Se o cliente desconectar, o turno MUST continuar até o fim no servidor e o resultado MUST ficar disponível ao recarregar a conversa.

#### Scenario: Atividade legível
- **WHEN** o agente lê o template do PRD e atualiza o documento
- **THEN** o usuário vê "Consultando o modelo do PRD" e "Atualizando o documento", sem caminhos de arquivo

#### Scenario: Queda de conexão
- **WHEN** a aba é fechada no meio de um turno
- **THEN** ao reabrir a conversa a resposta completa já aparece no histórico

### Requirement: Respostas rápidas
Quando o agente oferecer escolhas (menus do BMad, caminhos rápido e guiado, continuar ou aprofundar), o sistema SHALL apresentá-las como botões, sem perder a opção de resposta livre. Clicar num botão MUST enviar o texto do botão como mensagem do usuário.

#### Scenario: Menu numerado vira botões
- **WHEN** a skill apresenta as opções de caminho rápido ou guiado
- **THEN** a interface mostra dois botões e um campo livre

### Requirement: Comandos determinísticos
As ações Carregar, Exportar, Publicar, Nova conversa e Encerrar skill SHALL funcionar sem chamar o LLM.

#### Scenario: Encerrar skill
- **WHEN** o usuário clica em "Encerrar etapa"
- **THEN** a skill ativa é desativada, os documentos são mantidos e a conversa volta para `idle`
