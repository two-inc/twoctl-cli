#!/usr/bin/env bash
# Check that the embedded OpenAPI specs match their committed source inputs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

./scripts/codegen.sh

# Include untracked files: adding a generator output must fail until that new
# file is committed, even though git diff alone would not show it.
changes="$(git status --porcelain --untracked-files=all -- cmd/twoctl/cli/specs/)"
if [[ -n "${changes}" ]]; then
  echo "::error::Embedded OpenAPI specs are stale. Run make regen and commit cmd/twoctl/cli/specs/."
  printf '%s\n' "${changes}"
  git --no-pager diff -- cmd/twoctl/cli/specs/
  exit 1
fi

echo "embedded OpenAPI specs are up to date"
