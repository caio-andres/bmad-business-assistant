#!/usr/bin/env bash
# Roda o toolchain Go dentro de um contêiner Linux (cache de módulos persistente).
# Uso: scripts/dev-go.sh test ./...   |   scripts/dev-go.sh run ./cmd/alicerce serve
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Docker Desktop no Windows (Git Bash) precisa de caminhos do host no formato C:/...
HOST_ROOT="$(cd "$ROOT" && (pwd -W 2>/dev/null || pwd))"
export MSYS_NO_PATHCONV=1
IMAGE="alicerce-dev:latest"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker build -q -t "$IMAGE" -f "$HOST_ROOT/deploy/dev.Dockerfile" "$HOST_ROOT/deploy" >/dev/null
fi
TTY_ARGS=()
[ -t 1 ] && TTY_ARGS=(-t)
exec docker run --rm "${TTY_ARGS[@]}" \
  -v "$HOST_ROOT":/src -v alicerce-gomod:/go/pkg/mod -v alicerce-gocache:/root/.cache/go-build \
  ${DEV_GO_PORTS:-} ${DEV_GO_ENV:-} -w /src "$IMAGE" go "$@"
