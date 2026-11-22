# Spec Delta

## Purpose

Compartilhar o "BMad da sigla/projeto" entre pessoas de negócio e devs através de um repositório central versionado, sem que o negócio precise de conta ou conhecimento de Git.

## ADDED Requirements

### Requirement: Projetos por sigla
O sistema SHALL organizar o repositório central como `<SIGLA>/<projeto>/` (sigla com 3 caracteres alfanuméricos maiúsculos, projeto em kebab-case). Opcionalmente, `project.yaml` lista as siglas e os repositórios relacionados. O usuário SHALL poder listar e criar projetos e vincular uma conversa a um projeto.

#### Scenario: Sigla inválida
- **WHEN** o usuário cria o projeto com sigla "pagamentos"
- **THEN** recebe a mensagem "A sigla deve ter 3 letras ou números, como PAG"

### Requirement: Carregar
Carregar SHALL copiar `<SIGLA>/<projeto>/**` para os documentos da conversa e registrar o SHA de blob de cada arquivo. A origem é a branch `publish/<sigla>/<projeto>` quando há PR aberto; caso contrário, a branch padrão.

#### Scenario: Projeto com PR aberto
- **WHEN** existe PR aberto do projeto
- **THEN** os documentos vêm da branch do PR, não da branch padrão

### Requirement: Publicar sem sobrescrever
Publicar SHALL enviar, num único commit na branch do projeto, só os arquivos que mudaram em relação aos SHAs base, com o trailer `Publicado-por: <nome> <e-mail>`. O sistema cria o PR quando não há um aberto. Se algum arquivo mudou no remoto desde o carregamento, a publicação MUST ser bloqueada sem sobrescrever nada e o usuário MUST ver as diferenças, com três opções: manter a versão dele, manter a do repositório ou pedir uma versão unificada ao assistente.

#### Scenario: Publicação limpa
- **WHEN** o usuário publica pela primeira vez
- **THEN** é criado um commit na branch `publish/<sigla>/<projeto>` e um PR "Projeto <SIGLA>/<projeto>", e o status passa a "Em revisão"

#### Scenario: Conflito
- **WHEN** outra pessoa publicou o mesmo `prd.md` depois do carregamento
- **THEN** nada é enviado e o painel mostra as duas versões com as opções de resolução

### Requirement: Status de revisão
Ao abrir um projeto, o sistema SHALL mostrar o status: "Rascunho" (nunca publicado), "Em revisão" (PR aberto, com link) ou "Aprovado" (PR mesclado). O merge MUST depender da revisão humana do time técnico (CODEOWNERS); o sistema MUST NOT fazer merge automático.

#### Scenario: Aprovado
- **WHEN** o PR do projeto foi mesclado
- **THEN** o projeto aparece como "Aprovado"

### Requirement: Identidade única de escrita
Toda escrita no repositório central SHALL usar uma única identidade técnica (GitHub App ou token de serviço). O autor real fica só no trailer do commit. Credenciais MUST vir de variáveis de ambiente ou gerenciador de segredos, nunca de arquivos versionados.

#### Scenario: Sem credencial
- **WHEN** o publicador GitHub não está configurado
- **THEN** o sistema usa o publicador local (pasta) e a interface indica "Publicação local (modo demonstração)"
