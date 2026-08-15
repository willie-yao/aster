#!/usr/bin/env bash
# Fails when the AGENTS.md "Repo layout" map and the backend tree diverge, so
# the map cannot silently rot as packages are added or removed. The map is the
# orientation contract for contributors and agents alike.
#
# Enforced for backend/cmd/* and backend/internal/* top-level packages. Nested
# helpers (ai/tools/k8s and friends) are documented but not enforced.
set -o errexit
set -o nounset
set -o pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly map_file=AGENTS.md
missing=()
stale=()

# The map section runs from the "## Repo layout" heading to the next heading.
map_body="$(awk '/^## Repo layout$/{found=1; next} found && /^## /{exit} found' "${map_file}")"

# Only the backend block owns cmd/ and internal/ package entries; the frontend
# block reuses the same indent for src/ subdirectories.
backend_body="$(awk '/^backend\//{found=1; next} found && /^[a-z]/{exit} found' <<<"${map_body}")"

for dir in backend/cmd/*/ backend/internal/*/; do
	pkg="$(basename "${dir}")"
	grep -qE "^[[:space:]]*${pkg}/" <<<"${backend_body}" || missing+=("${dir}")
done

# Every package entry indented directly under cmd/ or internal/ must exist.
while read -r pkg; do
	[[ -d "backend/cmd/${pkg}" || -d "backend/internal/${pkg}" ]] || stale+=("${pkg}")
done < <(grep -oE '^    [a-z][a-z0-9]*/' <<<"${backend_body}" | tr -d ' /' | sort -u)

status=0
if ((${#missing[@]})); then
	status=1
	echo "AGENTS.md repo map does not list these packages:"
	printf '  %s\n' "${missing[@]}"
fi
if ((${#stale[@]})); then
	status=1
	echo "AGENTS.md repo map lists these packages, but they no longer exist:"
	printf '  %s\n' "${stale[@]}"
fi

if ((status)); then
	echo
	echo "Update the \"Repo layout\" section in ${map_file} to match the tree."
	exit 1
fi

echo "AGENTS.md repo map matches backend/cmd and backend/internal."
