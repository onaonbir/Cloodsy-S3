#!/bin/bash
set -e

# Cloodsy S3 Installer
# Usage: curl -fsSL https://raw.githubusercontent.com/onaonbir/Cloodsy-S3/main/install.sh | bash

REPO="onaonbir/Cloodsy-S3"
BINARY="cloodsys3"
INSTALL_DIR="/usr/local/bin"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo ""
echo -e "${GREEN}Cloodsy S3 Installer${NC}"
echo ""

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)
        echo -e "${RED}Unsupported OS: $OS${NC}"
        echo "Download manually: https://github.com/${REPO}/releases/latest"
        exit 1
        ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)   ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    armv7l|armhf)    ARCH="armv7" ;;
    *)
        echo -e "${RED}Unsupported architecture: $ARCH${NC}"
        echo "Download manually: https://github.com/${REPO}/releases/latest"
        exit 1
        ;;
esac

FILENAME="${BINARY}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/latest/download/${FILENAME}"

echo -e "  OS:       ${GREEN}${OS}${NC}"
echo -e "  Arch:     ${GREEN}${ARCH}${NC}"
echo -e "  File:     ${GREEN}${FILENAME}${NC}"
echo ""

# Create temp directory
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Download
echo -e "  Downloading..."
if ! curl -fsSL "$URL" -o "${TMP}/${FILENAME}"; then
    echo -e "${RED}Download failed.${NC}"
    echo "URL: $URL"
    echo ""
    echo "Check: https://github.com/${REPO}/releases/latest"
    exit 1
fi
echo -e "  ${GREEN}✓${NC} Downloaded"

# Extract
tar xzf "${TMP}/${FILENAME}" -C "$TMP"
echo -e "  ${GREEN}✓${NC} Extracted"

# Install
if [ -w "$INSTALL_DIR" ]; then
    mv "${TMP}/${BINARY}-${OS}-${ARCH}" "${INSTALL_DIR}/${BINARY}"
else
    echo -e "  ${YELLOW}Installing to ${INSTALL_DIR} requires sudo${NC}"
    sudo mv "${TMP}/${BINARY}-${OS}-${ARCH}" "${INSTALL_DIR}/${BINARY}"
fi
chmod +x "${INSTALL_DIR}/${BINARY}"
echo -e "  ${GREEN}✓${NC} Installed to ${INSTALL_DIR}/${BINARY}"

# Verify
INSTALLED_VERSION=$(${INSTALL_DIR}/${BINARY} version 2>/dev/null | head -1 || echo "installed")
echo ""
echo -e "${GREEN}Cloodsy S3 installed successfully!${NC}"
echo -e "  ${INSTALLED_VERSION}"
echo ""
echo "  Get started:"
echo "    cloodsys3 bucket create my-bucket"
echo "    cloodsys3 credential create my-bucket"
echo "    cloodsys3 serve"
echo ""
