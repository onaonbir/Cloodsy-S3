#!/usr/bin/env bash
set -euo pipefail

# Cloodsy S3 release tool: bumps VERSION, commits that single file, tags and
# pushes. GitHub Actions (.github/workflows/release.yml) builds the binaries.

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

VERSION=""
MESSAGE=""

while [ $# -gt 0 ]; do
    case "$1" in
        -m)
            [ $# -ge 2 ] || { echo -e "${RED}Error: -m needs a message${NC}"; exit 1; }
            MESSAGE="$2"; shift 2 ;;
        --force)
            echo -e "${RED}Error: --force was removed. Tags are immutable; release a new patch version instead.${NC}"
            exit 1 ;;
        -*)
            echo -e "${RED}Error: unknown option '$1'${NC}"; exit 1 ;;
        *)
            if [ -z "$VERSION" ]; then VERSION="$1"; else echo -e "${RED}Error: unexpected argument '$1'${NC}"; exit 1; fi
            shift ;;
    esac
done

if [ -z "$VERSION" ]; then
    CURRENT=$(cat VERSION 2>/dev/null || echo "unknown")
    echo ""
    echo -e "  ${YELLOW}Cloodsy S3 Release Tool${NC}"
    echo ""
    echo -e "  Current version: ${GREEN}${CURRENT}${NC}"
    echo ""
    echo "  Usage:"
    echo "    ./release.sh <version>                 # e.g. ./release.sh 1.2.0"
    echo "    ./release.sh <version> -m \"message\"    # with custom commit message"
    echo ""
    echo "  This script will:"
    echo "    1. Refuse to run on a dirty working tree or an existing tag"
    echo "    2. Run go vet ./... and go test ./..."
    echo "    3. Update and commit the VERSION file (nothing else)"
    echo "    4. Create git tag v<version> and push branch + tag"
    echo "    5. GitHub Actions builds the binaries and publishes the release"
    echo ""
    exit 1
fi

if ! echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo -e "${RED}Error: Invalid version format '${VERSION}'. Use x.y.z (e.g. 1.2.3)${NC}"
    exit 1
fi

TAG="v${VERSION}"
if [ -n "$MESSAGE" ]; then
    COMMIT_MSG="release: ${TAG} — ${MESSAGE}"
else
    COMMIT_MSG="release: ${TAG}"
fi

# Must run from the repository root.
cd "$(git rev-parse --show-toplevel)"

# Refuse a dirty tree: every change must be committed (and reviewed) first.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
    echo -e "${RED}Error: working tree has uncommitted changes. Commit or stash them first.${NC}"
    git status --short --untracked-files=no
    exit 1
fi

# Refuse an existing tag, locally or on the remote.
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
    echo -e "${RED}Error: Tag '${TAG}' already exists locally. Choose a new version.${NC}"
    exit 1
fi
if git ls-remote --exit-code --tags origin "refs/tags/${TAG}" >/dev/null 2>&1; then
    echo -e "${RED}Error: Tag '${TAG}' already exists on origin. Choose a new version.${NC}"
    exit 1
fi

BRANCH=$(git branch --show-current)
if [ "$BRANCH" != "main" ]; then
    echo -e "${YELLOW}Warning: You are on branch '${BRANCH}', not 'main'.${NC}"
    read -p "Continue anyway? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
fi

echo ""
echo -e "${YELLOW}=== Release: Cloodsy S3 ${TAG} ===${NC}"
echo ""

echo -e "  Running go vet ./..."
CGO_ENABLED=0 go vet ./...
echo -e "  ${GREEN}✓${NC} vet passed"
echo -e "  Running go test ./..."
CGO_ENABLED=0 go test ./...
echo -e "  ${GREEN}✓${NC} tests passed"

echo "$VERSION" > VERSION
echo -e "  ${GREEN}✓${NC} VERSION → ${VERSION}"

# Stage the VERSION file only; nothing else may sneak into a release commit.
git add VERSION
if git diff --cached --quiet; then
    echo -e "  ${YELLOW}○${NC} VERSION already at ${VERSION}, no commit needed"
else
    git commit -m "$COMMIT_MSG" --quiet
    echo -e "  ${GREEN}✓${NC} Committed: ${COMMIT_MSG}"
fi

git tag -a "$TAG" -m "Release ${TAG}"
echo -e "  ${GREEN}✓${NC} Tag created: ${TAG}"

echo ""
echo -e "  Pushing to GitHub..."
git push origin "$BRANCH" --quiet
git push origin "$TAG" --quiet
echo -e "  ${GREEN}✓${NC} Pushed to origin/${BRANCH}"
echo -e "  ${GREEN}✓${NC} Pushed tag ${TAG}"

echo ""
echo -e "${GREEN}=== Release ${TAG} complete! ===${NC}"
echo ""
echo "  GitHub Actions will now build and create the release."
echo "  Check: https://github.com/onaonbir/Cloodsy-S3/actions"
echo "  Release: https://github.com/onaonbir/Cloodsy-S3/releases/tag/${TAG}"
echo ""
