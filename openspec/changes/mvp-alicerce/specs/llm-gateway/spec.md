# Spec Delta

## Purpose

Isolar a aplicação de qualquer fornecedor de modelo, para trocar entre o gateway corporativo de IA, o Kong AI Gateway, os provedores gratuitos e o modo demonstração sem mudar código, com controle de tokens e custo.

## ADDED Requirements

### Requirement: Provedor compatível com OpenAI
O sistema SHALL falar com qualquer endpoint compatível com a API Chat Completions da OpenAI, com streaming e chamadas de ferramentas, configurado por URL base, chave e modelo. Modelos distintos MUST poder ser configurados para o interpretador, o roteador e os subagentes.

#### Scenario: Trocar de provedor
- **WHEN** a URL base muda de um provedor gratuito para o gateway corporativo
- **THEN** a aplicação funciona sem alteração de código

### Requirement: Modo demonstração
Sem provedor configurado, o sistema SHALL usar um modelo de demonstração determinístico. Esse modelo exercita o fluxo real (ativar skill, gravar documento, perguntar com opções, concluir) sem custo, e a interface MUST indicar claramente "Modo demonstração: respostas simuladas".

#### Scenario: Sem chave
- **WHEN** a aplicação sobe sem `LLM_BASE_URL`
- **THEN** um product brief de demonstração pode ser criado de ponta a ponta

### Requirement: Contabilização de uso
Cada chamada ao LLM SHALL registrar tokens de entrada e saída por usuário, conversa e modelo. Quando houver preço configurado por modelo, MUST registrar também o custo estimado. A conversa MUST mostrar o consumo acumulado.

#### Scenario: Custo por conversa
- **WHEN** o admin consulta o uso
- **THEN** vê tokens e custo estimado por usuário e por conversa nos últimos 30 dias

### Requirement: Resiliência
Erros transitórios do provedor (429, 5xx, timeout) SHALL ser tentados de novo com backoff exponencial e limite. Falha definitiva MUST encerrar o turno com mensagem amigável, preservando o estado da conversa para nova tentativa.

#### Scenario: Provedor fora
- **WHEN** o provedor responde 503 em todas as tentativas
- **THEN** o usuário vê "O serviço de IA está indisponível agora; tente de novo em instantes" e a mensagem dele não se perde
