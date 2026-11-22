# Infraestrutura fictícia (demonstração).
resource "aws_sqs_queue" "liquidacao" { name = "pix-liquidacao" }
resource "aws_dynamodb_table" "chaves_cache" { name = "pix-chaves-cache" billing_mode = "PAY_PER_REQUEST" hash_key = "chave" }
resource "aws_apigatewayv2_api" "pagamentos" { name = "pagamentos-pix" protocol_type = "HTTP" }
