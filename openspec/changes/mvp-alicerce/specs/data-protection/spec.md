# Spec Delta

## Purpose

Reduzir o risco de vazamento de dados pessoais e segredos para provedores de IA e manter o repositório público livre de informação do cliente.

## ADDED Requirements

### Requirement: Mascaramento antes do LLM
Antes de enviar uma mensagem do usuário ao LLM, o sistema SHALL detectar e substituir por marcadores (por exemplo, `[CPF]`) os seguintes dados: CPF, CNPJ, número de cartão (validado por Luhn), e-mail, telefone brasileiro e padrões de segredo (chaves de API, tokens). O usuário MUST ser avisado do mascaramento. A política MUST ser configurável (`mask`, `block`, `off`).

#### Scenario: CPF na mensagem
- **WHEN** o usuário escreve "o cliente 123.456.789-09 reclamou"
- **THEN** o LLM recebe "o cliente [CPF] reclamou" e o usuário vê o aviso "Mascaramos 1 dado pessoal antes de enviar à IA"

#### Scenario: Política de bloqueio
- **WHEN** a política é `block` e a mensagem contém número de cartão
- **THEN** a mensagem não é enviada e o usuário é orientado a remover o dado

### Requirement: Retenção
O sistema SHALL permitir configurar a retenção de conversas arquivadas, em dias. Conversas além do prazo MUST ser apagadas, com os documentos, por uma rotina periódica.

#### Scenario: Expurgo
- **WHEN** uma conversa arquivada passa do prazo de retenção
- **THEN** a rotina remove mensagens, transcrição e documentos dela

### Requirement: Guardrail do repositório público
O repositório do produto SHALL ter varredura de segredos no CI e no pre-commit. Dados de demonstração (organização, siglas, sistemas, pessoas) MUST ser fictícios.

#### Scenario: Segredo no commit
- **WHEN** um commit contém um token
- **THEN** o pipeline de CI falha na etapa de varredura de segredos
