#!/usr/bin/env bash
# Regenera bmad/dist a partir do instalador oficial do BMad Method.
# Uso: scripts/update-bmad.sh [versao]   (padrao: a versao em bmad/VERSION)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${1:-$(cat "$ROOT/bmad/VERSION")}"
TMP="$(mktemp -d)/alicerce-workspace"
mkdir -p "$TMP"
( cd "$TMP" && npx -y "bmad-method@${VERSION}" install --directory . --modules bmm --tools claude-code \
    --user-name "Time de Negocio" --communication-language "Portuguese (Brazil)" \
    --document-output-language "Portuguese (Brazil)" --no-shims -y >/dev/null )
rm -rf "$ROOT/bmad/dist"
mkdir -p "$ROOT/bmad/dist/.claude"
cp -R "$TMP/_bmad" "$ROOT/bmad/dist/_bmad"
cp -R "$TMP/.claude/skills" "$ROOT/bmad/dist/.claude/skills"
# Arquivos pessoais/gerados nao entram no bundle versionado.
rm -f "$ROOT/bmad/dist/_bmad/config.user.toml" "$ROOT/bmad/dist/_bmad/custom/config.user.toml"
find "$ROOT/bmad/dist" -name "__pycache__" -type d -prune -exec rm -rf {} +
echo "$VERSION" > "$ROOT/bmad/VERSION"
echo "BMad Method $VERSION vendorizado em bmad/dist"
