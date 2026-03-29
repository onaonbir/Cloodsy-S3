#!/bin/bash
set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Parse args
FORCE=false
VERSION=""
MESSAGE=""

for arg in "$@"; do
    case $arg in
        --force) FORCE=true ;;
        -m) ;;
        *)
            if [ -z "$VERSION" ]; then
                VERSION="$arg"
            elif [ "$PREV" = "-m" ]; then
                MESSAGE="$arg"
            fi
            ;;
    esac
    PREV="$arg"
done

# Usage
if [ -z "$VERSION" ]; then
    CURRENT=$(cat VERSION 2>/dev/null || echo "unknown")
    echo ""
    echo -e "  ${YELLOW}Cloodsy S3 Release Tool${NC}"
    echo ""
    echo -e "  Current version: ${GREEN}${CURRENT}${NC}"
    echo ""
    echo "  Usage:"
    echo "    ./release.sh <version>                          # e.g. ./release.sh 1.1.0"
    echo "    ./release.sh <version> -m \"message\"              # with custom commit message"
    echo "    ./release.sh <version> --force                   # overwrite existing release"
    echo "    ./release.sh <version> --force -m \"hotfix\"       # overwrite with message"
    echo ""
    echo "  This script will:"
    echo "    1. Update VERSION file"
    echo "    2. Commit all changes"
    echo "    3. Create git tag v<version>"
    echo "    4. Push to GitHub"
    echo "    5. GitHub Actions will build & create the release"
    echo ""
    echo "  --force will delete the existing tag/release and recreate it."
    echo ""
    exit 1
fi

TAG="v${VERSION}"

# Build commit message
if [ -n "$MESSAGE" ]; then
    COMMIT_MSG="release: ${TAG} — ${MESSAGE}"
else
    COMMIT_MSG="release: ${TAG}"
fi

# Validate version format (x.y.z)
if ! echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo -e "${RED}Error: Invalid version format '${VERSION}'. Use x.y.z (e.g. 1.2.3)${NC}"
    exit 1
fi

# Check if tag already exists
TAG_EXISTS=false
if git rev-parse "$TAG" >/dev/null 2>&1; then
    TAG_EXISTS=true
fi

if [ "$TAG_EXISTS" = true ] && [ "$FORCE" = false ]; then
    echo -e "${RED}Error: Tag '${TAG}' already exists. Use --force to overwrite.${NC}"
    exit 1
fi

# Check we're on main branch
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
if [ "$FORCE" = true ] && [ "$TAG_EXISTS" = true ]; then
    echo -e "  ${YELLOW}(force mode — overwriting existing release)${NC}"
fi
echo ""

# Force: delete existing tag locally and remotely
if [ "$TAG_EXISTS" = true ] && [ "$FORCE" = true ]; then
    # Delete GitHub release via gh CLI (if available)
    if command -v gh &>/dev/null; then
        gh release delete "$TAG" --yes 2>/dev/null && \
            echo -e "  ${GREEN}✓${NC} Deleted GitHub release ${TAG}" || true
    fi

    # Delete remote tag
    git push origin --delete "$TAG" 2>/dev/null && \
        echo -e "  ${GREEN}✓${NC} Deleted remote tag ${TAG}" || true

    # Delete local tag
    git tag -d "$TAG" 2>/dev/null && \
        echo -e "  ${GREEN}✓${NC} Deleted local tag ${TAG}" || true
fi

# Update VERSION file
echo "$VERSION" > VERSION
echo -e "  ${GREEN}✓${NC} VERSION → ${VERSION}"

# Stage all changes
git add -A

# Check if there are changes to commit
if git diff --cached --quiet 2>/dev/null; then
    echo -e "  ${YELLOW}○${NC} No changes to commit (version already set)"
else
    CHANGED=$(git diff --cached --stat | tail -1)
    echo -e "  ${GREEN}✓${NC} Changes: ${CHANGED}"
    git commit -m "$COMMIT_MSG" --quiet
    echo -e "  ${GREEN}✓${NC} Committed: ${COMMIT_MSG}"
fi

# Create tag
git tag -a "$TAG" -m "Release ${TAG}"
echo -e "  ${GREEN}✓${NC} Tag created: ${TAG}"

# Push
echo ""
echo -e "  Pushing to GitHub..."
git push origin "$BRANCH" --quiet
git push origin "$TAG" --quiet
echo -e "  ${GREEN}✓${NC} Pushed to origin/${BRANCH}"
echo -e "  ${GREEN}✓${NC} Pushed tag ${TAG}"

# Done
echo ""
echo -e "${GREEN}=== Release ${TAG} complete! ===${NC}"
echo ""
echo "  GitHub Actions will now build and create the release."
echo "  Check: https://github.com/onaonbir/Cloodsy-S3/actions"
echo "  Release: https://github.com/onaonbir/Cloodsy-S3/releases/tag/${TAG}"
echo ""
