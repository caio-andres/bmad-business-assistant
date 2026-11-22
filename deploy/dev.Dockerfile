# Imagem de desenvolvimento: Go + Python (scripts originais do BMad) + git.
# Usada por scripts/dev-go.sh para rodar testes e o servidor em Linux,
# inclusive em máquinas Windows com Smart App Control ativo.
FROM golang:1.27-trixie
RUN apt-get update && apt-get install -y --no-install-recommends python3 git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
