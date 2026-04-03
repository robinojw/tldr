#!/bin/sh
# tldr installer
# Usage: curl -sSfL https://raw.githubusercontent.com/robinojw/tldr/main/install.sh | sh
#
# Installs the tldr binary to /usr/local/bin (or $TLDR_INSTALL_DIR).
# Requires: curl or wget, tar, uname

set -e

REPO="robinojw/tldr"
BINARY="tldr"
DEFAULT_INSTALL_DIR="/usr/local/bin"
INSTALL_DIR="${TLDR_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"

# Colors (if terminal supports them)
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info() { printf "${GREEN}[tldr]${NC} %s\n" "$1"; }
warn() { printf "${YELLOW}[tldr]${NC} %s\n" "$1"; }
fail() { printf "${RED}[tldr]${NC} %s\n" "$1"; exit 1; }

detect_os() {
    case "$(uname -s)" in
        Darwin*)  echo "darwin" ;;
        Linux*)   echo "linux" ;;
        MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
        *)        fail "Unsupported OS: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   echo "amd64" ;;
        arm64|aarch64)   echo "arm64" ;;
        armv7l)          echo "armv7" ;;
        *)               fail "Unsupported architecture: $(uname -m)" ;;
    esac
}

latest_version() {
    if command -v curl >/dev/null 2>&1; then
        curl -sSfL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
            | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
            | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
    else
        fail "Neither curl nor wget found. Install one and retry."
    fi
}

download() {
    url="$1"
    dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -sSfL "$url" -o "$dest"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$dest"
    fi
}

main() {
    OS="$(detect_os)"
    ARCH="$(detect_arch)"

    info "Detected platform: ${OS}/${ARCH}"

    VERSION="${TLDR_VERSION:-}"
    if [ -z "$VERSION" ]; then
        info "Fetching latest release..."
        VERSION="$(latest_version)"
    fi

    if [ -z "$VERSION" ]; then
        # No releases yet -- build from source
        warn "No releases found. Building from source..."
        build_from_source
        return
    fi

    info "Installing tldr ${VERSION}"

    # Strip leading v for filename
    VERSION_NUM="${VERSION#v}"

    TARBALL="${BINARY}_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
    URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"

    TMPDIR="$(mktemp -d)"
    trap 'rm -rf "$TMPDIR"' EXIT

    info "Downloading ${URL}..."
    download "$URL" "${TMPDIR}/${TARBALL}" || {
        warn "Download failed. Falling back to build from source..."
        build_from_source
        return
    }

    info "Extracting..."
    tar -xzf "${TMPDIR}/${TARBALL}" -C "$TMPDIR"

    if [ ! -f "${TMPDIR}/${BINARY}" ]; then
        fail "Binary not found in archive. Expected: ${BINARY}"
    fi

    install_binary "${TMPDIR}/${BINARY}"
}

build_from_source() {
    if ! command -v go >/dev/null 2>&1; then
        fail "Go is required to build from source. Install Go 1.22+ from https://go.dev/dl/"
    fi

    GO_VERSION="$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1)"
    info "Building from source with ${GO_VERSION}..."

    TMPDIR="$(mktemp -d)"
    trap 'rm -rf "$TMPDIR"' EXIT

    if command -v git >/dev/null 2>&1; then
        info "Cloning repository..."
        git clone --depth 1 "https://github.com/${REPO}.git" "${TMPDIR}/src" 2>/dev/null
        (
            cd "${TMPDIR}/src"
            COMMIT_SHA="$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')"
            go build -ldflags "-s -w -X github.com/robinojw/tldr/pkg/config.Version=dev-${COMMIT_SHA}" \
                -o "${TMPDIR}/${BINARY}" ./cmd/tldr
        )
    else
        # go install fallback
        info "Installing via go install..."
        GOBIN="${TMPDIR}" go install "github.com/${REPO}/cmd/tldr@latest"
    fi

    if [ ! -f "${TMPDIR}/${BINARY}" ]; then
        fail "Build failed. Binary not produced."
    fi

    install_binary "${TMPDIR}/${BINARY}"
}

install_binary() {
    SRC="$1"
    chmod +x "$SRC"

    if [ -w "$INSTALL_DIR" ]; then
        mv "$SRC" "${INSTALL_DIR}/${BINARY}"
    else
        info "Writing to ${INSTALL_DIR} requires elevated permissions..."
        sudo mv "$SRC" "${INSTALL_DIR}/${BINARY}"
    fi

    info "Installed ${BINARY} to ${INSTALL_DIR}/${BINARY}"

    # Verify
    if command -v "$BINARY" >/dev/null 2>&1; then
        INSTALLED_VERSION="$("$BINARY" --version 2>/dev/null || echo 'unknown')"
        info "Verified: ${BINARY} ${INSTALLED_VERSION}"
    else
        warn "${INSTALL_DIR} may not be in your PATH."
        warn "Add this to your shell profile:"
        warn "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    fi

    info ""
    info "Get started:"
    info "  tldr migrate                        # pull existing MCPs from your harnesses into tldr"
    info "  tldr mcp list                       # see what's registered"
    info "  tldr serve                           # run the MCP gateway"
    info ""
    info "Or add a server manually:"
    info "  tldr mcp add --transport stdio github npx -y @modelcontextprotocol/server-github"
}

main "$@"
