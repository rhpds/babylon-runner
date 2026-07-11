#!/bin/bash
set -euo pipefail

die () {
    echo >&2 "$@"
    exit 1
}

if [[ "${1:-}" == "--dev" || "${1:-}" == "--force" ]]; then
    BRANCH_OVERRIDE=1
    shift
else
    BRANCH_OVERRIDE=0
fi

VERSION=${1:-}

CURRENT_VERSION=$(git tag | grep "^v[0-9]" | sort -V | tail -1 || true)

echo "CURRENT_VERSION: $CURRENT_VERSION"

if [[ -z "${VERSION}" ]]; then
    if [[ -z "${CURRENT_VERSION}" ]]; then
        VERSION="v0.1.0"
        echo "No existing tags found. Starting at: ${VERSION}"
    else
        VERSION=$(echo "${CURRENT_VERSION}" | awk -F. '{$NF+=1} 1' OFS=".")
        echo "New version is: ${VERSION}"
    fi
fi
echo "VERSION: $VERSION"

if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "VERSION must be a semantic version: vMAJOR.MINOR.PATCH"
    exit 1
fi

CURRENT_BRANCH=$(git branch --show-current)
if [[ 'main' != "$CURRENT_BRANCH" && $BRANCH_OVERRIDE -eq 0 ]]; then
    echo "Not on main git branch!"
    exit 1
fi

if [[ -n "$(git tag -l "$VERSION")" ]]; then
    echo "VERSION $VERSION already exists!"
    exit 1
fi

if [[ -n "${CURRENT_VERSION}" ]] && [[ $VERSION != $( (echo "$VERSION"; git tag | grep "^v[0-9]" || true) | sort -V | tail -1) ]]; then
    echo "$VERSION is not semantically newest!"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Generating version $VERSION - Last Version: $CURRENT_VERSION"

CHART="$SCRIPT_DIR/helm/Chart.yaml"
sed "s/^version:.*/version: ${VERSION:1}/" "$CHART" | sed "s/^appVersion:.*/appVersion: ${VERSION:1}/" > "$CHART.tmp"
mv "$CHART.tmp" "$CHART"

git add helm/Chart.yaml
git commit -m "Release $VERSION"
git tag "$VERSION"
git push origin "$CURRENT_BRANCH" "$VERSION"

exit 0
