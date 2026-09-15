#!/usr/bin/env bash
#
# Cloodsy S3 installer
#
#   curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash -s -- --version v1.2.0
#
# Options:
#   --version vX.Y.Z   install a specific release instead of the latest
#   --dir <path>       install directory (default /usr/local/bin, or $INSTALL_DIR)
#   --force            reinstall even if the same version is already installed
#
# The whole script lives inside main() so a truncated download can never
# execute a partial script.

main() {
    set -euo pipefail

    local REPO="onaonbir/Cloodsy-S3"
    local BINARY="cloodsys3"
    local INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
    local WANT_TAG=""
    local FORCE=false

    local RED='\033[0;31m' GREEN='\033[0;32m' YELLOW='\033[1;33m' NC='\033[0m'

    while [ $# -gt 0 ]; do
        case "$1" in
            --version)
                [ $# -ge 2 ] || { echo -e "${RED}--version needs a value${NC}"; return 1; }
                WANT_TAG="$2"; shift 2 ;;
            --version=*) WANT_TAG="${1#--version=}"; shift ;;
            --dir)
                [ $# -ge 2 ] || { echo -e "${RED}--dir needs a value${NC}"; return 1; }
                INSTALL_DIR="$2"; shift 2 ;;
            --dir=*) INSTALL_DIR="${1#--dir=}"; shift ;;
            --force) FORCE=true; shift ;;
            -h|--help)
                echo "usage: install.sh [--version vX.Y.Z] [--dir <path>] [--force]"
                return 0 ;;
            *) echo -e "${RED}Unknown option: $1${NC}"; return 1 ;;
        esac
    done

    if [ -n "$WANT_TAG" ]; then
        case "$WANT_TAG" in v*) ;; *) WANT_TAG="v${WANT_TAG}" ;; esac
        if ! echo "$WANT_TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
            echo -e "${RED}Invalid version '${WANT_TAG}' (expected vX.Y.Z)${NC}"
            return 1
        fi
    fi

    echo ""
    echo -e "${GREEN}Cloodsy S3 Installer${NC}"
    echo ""

    for tool in curl tar; do
        command -v "$tool" >/dev/null 2>&1 || { echo -e "${RED}Required tool not found: ${tool}${NC}"; return 1; }
    done

    # Detect OS
    local OS
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$OS" in
        linux)  OS="linux" ;;
        darwin) OS="darwin" ;;
        *)
            echo -e "${RED}Unsupported OS: $OS${NC}"
            echo "Download manually: https://github.com/${REPO}/releases/latest"
            return 1 ;;
    esac

    # Detect architecture
    local ARCH
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        armv7l|armhf)   ARCH="armv7" ;;
        *)
            echo -e "${RED}Unsupported architecture: $ARCH${NC}"
            echo "Download manually: https://github.com/${REPO}/releases/latest"
            return 1 ;;
    esac

    # Resolve the release tag (no API call: follow the /releases/latest redirect).
    local TAG
    if [ -n "$WANT_TAG" ]; then
        TAG="$WANT_TAG"
    else
        local final
        final=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" || true)
        TAG="${final##*/}"
        if ! echo "$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
            echo -e "${RED}Could not determine the latest release.${NC}"
            echo "Check: https://github.com/${REPO}/releases/latest"
            return 1
        fi
    fi

    local FILENAME="${BINARY}-${OS}-${ARCH}.tar.gz"
    local BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"

    echo -e "  OS:       ${GREEN}${OS}${NC}"
    echo -e "  Arch:     ${GREEN}${ARCH}${NC}"
    echo -e "  Version:  ${GREEN}${TAG}${NC}"
    echo -e "  File:     ${GREEN}${FILENAME}${NC}"
    echo -e "  Target:   ${GREEN}${INSTALL_DIR}/${BINARY}${NC}"
    echo ""

    # Idempotent re-run: skip when the same version is already installed.
    if [ "$FORCE" = false ] && [ -x "${INSTALL_DIR}/${BINARY}" ]; then
        local have
        have=$("${INSTALL_DIR}/${BINARY}" version 2>/dev/null | head -1 | sed -n 's/.*v\([0-9][0-9.]*\).*/v\1/p' || true)
        if [ -n "$have" ] && [ "$have" = "$TAG" ]; then
            echo -e "  ${GREEN}✓${NC} ${BINARY} ${TAG} is already installed. Use --force to reinstall."
            echo ""
            return 0
        fi
    fi

    local TMP
    TMP=$(mktemp -d)
    trap 'rm -rf "$TMP"' EXIT

    echo -e "  Downloading ${FILENAME}..."
    if ! curl -fsSL "${BASE_URL}/${FILENAME}" -o "${TMP}/${FILENAME}"; then
        echo -e "${RED}Download failed.${NC}"
        echo "URL: ${BASE_URL}/${FILENAME}"
        echo "Check: https://github.com/${REPO}/releases/tag/${TAG}"
        return 1
    fi
    echo -e "  ${GREEN}✓${NC} Downloaded"

    # Verify SHA-256 against the checksums published with the release.
    echo -e "  Downloading checksums.txt..."
    if ! curl -fsSL "${BASE_URL}/checksums.txt" -o "${TMP}/checksums.txt"; then
        echo -e "${RED}checksums.txt is missing from release ${TAG}; refusing to install an unverified binary.${NC}"
        return 1
    fi
    if ! grep -E "[[:space:]]\*?${FILENAME}\$" "${TMP}/checksums.txt" > "${TMP}/expected.txt" || [ ! -s "${TMP}/expected.txt" ]; then
        echo -e "${RED}checksums.txt has no entry for ${FILENAME}; refusing to install.${NC}"
        return 1
    fi
    local verify_ok=false
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$TMP" && sha256sum -c --quiet expected.txt) && verify_ok=true
    elif command -v shasum >/dev/null 2>&1; then
        (cd "$TMP" && shasum -a 256 -c --quiet expected.txt) && verify_ok=true
    else
        echo -e "${RED}Neither sha256sum nor shasum is available; cannot verify the download.${NC}"
        return 1
    fi
    if [ "$verify_ok" != true ]; then
        echo -e "${RED}SHA-256 verification FAILED for ${FILENAME}. The download is corrupt or tampered with. Nothing was installed.${NC}"
        return 1
    fi
    echo -e "  ${GREEN}✓${NC} SHA-256 verified"

    tar xzf "${TMP}/${FILENAME}" -C "$TMP"
    # Archives ship the binary as "cloodsys3"; older releases used a platform suffix.
    local EXTRACTED=""
    for candidate in "${TMP}/${BINARY}" "${TMP}/${BINARY}-${OS}-${ARCH}"; do
        if [ -f "$candidate" ]; then EXTRACTED="$candidate"; break; fi
    done
    if [ -z "$EXTRACTED" ]; then
        echo -e "${RED}Archive does not contain the ${BINARY} binary.${NC}"
        return 1
    fi
    chmod 0755 "$EXTRACTED"
    echo -e "  ${GREEN}✓${NC} Extracted"

    # Install atomically: write next to the target, then rename over it.
    local SUDO=""
    if [ ! -d "$INSTALL_DIR" ] || [ ! -w "$INSTALL_DIR" ]; then
        if [ "$(id -u)" -ne 0 ]; then
            echo -e "  ${YELLOW}Installing to ${INSTALL_DIR} requires sudo${NC}"
            SUDO="sudo"
        fi
    fi
    $SUDO mkdir -p "$INSTALL_DIR"
    $SUDO cp "$EXTRACTED" "${INSTALL_DIR}/${BINARY}.tmp.$$"
    $SUDO chmod 0755 "${INSTALL_DIR}/${BINARY}.tmp.$$"
    $SUDO mv -f "${INSTALL_DIR}/${BINARY}.tmp.$$" "${INSTALL_DIR}/${BINARY}"
    echo -e "  ${GREEN}✓${NC} Installed to ${INSTALL_DIR}/${BINARY}"

    # `cloodsys3 version` prints "Cloodsy S3 vX.Y.Z" on its first line.
    local INSTALLED_VERSION
    INSTALLED_VERSION=$("${INSTALL_DIR}/${BINARY}" version 2>/dev/null | head -1 || echo "Cloodsy S3 (installed)")
    echo ""
    echo -e "${GREEN}${INSTALLED_VERSION} installed successfully!${NC}"
    echo ""
    echo "  Get started:"
    echo "    cloodsys3 bucket create my-bucket"
    echo "    cloodsys3 credential create my-bucket"
    echo "    cloodsys3 serve"
    echo ""
    echo "  Verify anytime:  cloodsys3 version"
    echo "  Upgrade later:   cloodsys3 update   (or re-run this installer)"
    echo ""
}

main "$@"
