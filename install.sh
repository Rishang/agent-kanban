#!/usr/bin/env bash
#
# Installer for agent-kanban (github.com/Rishang/agent-kanban).
#
#   curl -fsSL https://raw.githubusercontent.com/Rishang/agent-kanban/main/install.sh | bash
#
# Env overrides:
#   VERSION=v1.0.0   install a specific tag (default: latest)
#   INSTALL_DIR=...  install into this directory instead of the auto-detected one
#
set -euo pipefail

REPO="Rishang/agent-kanban"
BINARY="agent-kanban"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-}"

info() { printf '\033[0;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33mwarn:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[0;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

# --- platform -----------------------------------------------------------------
# goreleaser names archives agent-kanban_<version>_<goos>_<goarch>.tar.gz, so map
# uname output onto GOOS/GOARCH. Releases cover linux amd64/arm64 and macOS arm64
# (the turso native library has no darwin/amd64 build here). Windows: download the
# .zip from the releases page.
detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported OS: $(uname -s) (this installer covers Linux and macOS; Windows users: download the .zip from https://github.com/${REPO}/releases)" ;;
  esac

  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac

  if [ "$os" = darwin ] && [ "$arch" != arm64 ]; then
    die "no macOS ${arch} build is published; only Apple Silicon (arm64) is supported"
  fi

  printf '%s %s' "$os" "$arch"
}

# --- install directory --------------------------------------------------------
# Prefer a user-level bin directory that is already on $PATH: no sudo needed and
# the shell will find it. Fall back to /usr/local/bin when $PATH has no writable
# home-owned entry.
resolve_install_dir() {
  local dir
  local IFS=:
  for dir in $PATH; do
    [ -n "$dir" ] || continue
    case "$dir" in
      "$HOME"/*)
        if [ -d "$dir" ] && [ -w "$dir" ]; then
          printf '%s' "$dir"
          return 0
        fi
        ;;
    esac
  done

  printf '%s' "/usr/local/bin"
}

# --- version ------------------------------------------------------------------
latest_version() {
  # The redirect target of /releases/latest carries the tag, which avoids
  # needing jq to parse the API response.
  curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest" | sed 's#.*/tag/##'
}

# --- main ---------------------------------------------------------------------
need curl
need tar

read -r OS ARCH <<<"$(detect_platform)"

if [ "$VERSION" = "latest" ]; then
  info "Resolving latest release..."
  VERSION="$(latest_version)"
  # With no releases, /releases/latest redirects to /releases and the tag stays a URL.
  case "$VERSION" in
    v[0-9]*) ;;
    *) die "could not determine the latest release tag (does ${REPO} have a release yet?)" ;;
  esac
fi
# Archive names carry the bare version, tags carry the leading v.
VERSION_NUM="${VERSION#v}"

ARCHIVE="${BINARY}_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

[ -n "$INSTALL_DIR" ] || INSTALL_DIR="$(resolve_install_dir)"

info "Installing ${BINARY} ${VERSION} (${OS}/${ARCH})"

TMPDIR_="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_"' EXIT

info "Downloading ${ARCHIVE}"
curl -fsSL "${BASE_URL}/${ARCHIVE}" -o "${TMPDIR_}/${ARCHIVE}" ||
  die "download failed: ${BASE_URL}/${ARCHIVE}"

# A checksum mismatch aborts. A missing checksums.txt or missing sha256 tool only warns.
if curl -fsSL "${BASE_URL}/checksums.txt" -o "${TMPDIR_}/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$TMPDIR_" && grep " ${ARCHIVE}\$" checksums.txt | sha256sum -c -) >/dev/null ||
      die "checksum mismatch for ${ARCHIVE}"
    info "Checksum verified"
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$TMPDIR_" && grep " ${ARCHIVE}\$" checksums.txt | shasum -a 256 -c -) >/dev/null ||
      die "checksum mismatch for ${ARCHIVE}"
    info "Checksum verified"
  else
    warn "no sha256sum/shasum found, skipping checksum verification"
  fi
else
  warn "no checksums.txt in the release, skipping checksum verification"
fi

tar -xzf "${TMPDIR_}/${ARCHIVE}" -C "$TMPDIR_"
[ -f "${TMPDIR_}/${BINARY}" ] || die "${BINARY} not found inside ${ARCHIVE}"
chmod +x "${TMPDIR_}/${BINARY}"

# Only escalate with sudo when the target directory is not writable.
mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [ -w "$INSTALL_DIR" ]; then
  mv "${TMPDIR_}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
else
  info "${INSTALL_DIR} is not writable, escalating with sudo"
  need sudo
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "${TMPDIR_}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
fi

printf '\n'
info "Installed ${BINARY} ${VERSION} to ${INSTALL_DIR}/${BINARY}"

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    warn "${INSTALL_DIR} is not on your \$PATH"
    printf '     Add it with:  export PATH="%s:$PATH"\n' "$INSTALL_DIR"
    ;;
esac
info "Next: cd your-project && ${BINARY}   (then: ${BINARY} skill sync)"
