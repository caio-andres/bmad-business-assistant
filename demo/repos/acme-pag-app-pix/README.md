# Pix no App Acme

Aplicativo móvel e BFF que permitem a clientes pessoa física enviar e receber Pix, agendar transferências e consultar o extrato.

## Funcionalidades
- Pix por chave (CPF, e-mail, telefone, aleatória) e por QR Code estático/dinâmico
- Agendamento e Pix recorrente
- Extrato com filtros e comprovantes em PDF
- Limites noturnos configuráveis

## Integrações
- SPI/DICT do Banco Central via gateway interno `acme-pag-infra-pix`
- Antifraude transacional (score síncrono, timeout de 800 ms)
- Serviço de notificações push

## Limitações conhecidas
- Não há cashback nem programa de pontos.
- O extrato mostra no máximo 90 dias.
