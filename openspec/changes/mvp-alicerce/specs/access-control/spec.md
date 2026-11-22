# Spec Delta

## Purpose

Garantir que só pessoas autorizadas usem o assistente, com papéis simples, sessões seguras, auditoria e limites que protejam custo e disponibilidade.

## ADDED Requirements

### Requirement: Modos de autenticação
O sistema SHALL suportar três modos de autenticação:
- `local`: e-mail e senha, com conta criada só por convite ou por domínio de e-mail permitido;
- `oidc`: JWT de um provedor externo (Cognito, SSO corporativo, Supabase Auth), validado via JWKS;
- `dev`: usuário fixo, só para desenvolvimento e demonstração.

Em produção, o modo `dev` MUST NOT ser habilitado sem aviso explícito no log de inicialização.

#### Scenario: Token OIDC inválido
- **WHEN** chega um JWT com assinatura inválida ou expirado
- **THEN** a API responde 401

### Requirement: Sessão segura
No modo `local`, a sessão SHALL usar um cookie `HttpOnly`, `SameSite=Lax`, `Secure` quando servido por HTTPS e com expiração. Requisições que alteram estado MUST exigir proteção contra CSRF (cabeçalho customizado obrigatório). Senhas MUST ser armazenadas com hash adaptativo (bcrypt).

#### Scenario: Requisição sem cabeçalho
- **WHEN** um POST chega sem o cabeçalho `X-Requested-With`
- **THEN** a API responde 403

### Requirement: Papéis e convites
Os papéis SHALL ser `admin` e `member`. O admin MUST poder convidar pessoas (link de uso único com expiração), desativar contas e ver o uso agregado. O primeiro admin MUST ser criado pela linha de comando.

#### Scenario: Convite expirado
- **WHEN** alguém abre um convite com mais de 7 dias
- **THEN** vê "Convite expirado, peça um novo ao administrador"

### Requirement: Auditoria
O sistema SHALL registrar, com usuário e horário, estas ações: login, criação de conversa, início de skill, carregamento, publicação, exportação e ações administrativas. O registro MUST NOT incluir o conteúdo das mensagens.

#### Scenario: Publicação auditada
- **WHEN** um usuário publica um projeto
- **THEN** o log de auditoria tem a ação `publish` com sigla, projeto e commit

### Requirement: Limites de uso
O sistema SHALL limitar a taxa de mensagens por usuário e o consumo diário de tokens por usuário. Ao atingir um limite, MUST responder 429 com mensagem amigável que diz quando o uso volta a ser liberado.

#### Scenario: Cota diária
- **WHEN** o usuário ultrapassa a cota diária de tokens
- **THEN** recebe "Você atingiu o limite de uso de hoje; ele renova à meia-noite"
