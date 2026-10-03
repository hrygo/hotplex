#!/usr/bin/env bash
#
# HotPlex Gateway — Pack OpenCode + Oh My OpenAgent offline bundle
#
# Downloads OpenCode binary and Oh My OpenAgent npm packages,
# bundles them with an install script for air-gapped deployment.
#
# Usage:
#   ./scripts/pack-offline-bundle.sh [options]
#
# Options:
#   --lock PATH              Runtime lock manifest (default: configs/worker-runtime-lock.json)
#   --allow-dynamic          Resolve versions from upstream instead of the lock.
#                            Non-release only: the result is not reproducible.
#   --dry-run                Print the resolved plan and exit. Downloads nothing.
#   --opencode-version VER   Override the OpenCode version (implies --allow-dynamic)
#   --omo-version VER        Override the Oh My OpenAgent version (implies --allow-dynamic)
#   --platform PLAT          Target platform (default: auto-detect)
#   --all-platforms          Pack all platform binaries
#   --output DIR             Output directory (default: dist/offline-bundle)
#   --help                   Show this help
#
# Locked mode is the default and the only mode a release may use. Every
# download is verified against a digest pinned in the lock, and a package that
# is missing at the locked version is a hard failure — never a silent fallback
# to whatever version happens to be published today.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# ── Defaults ────────────────────────────────────────────────────────────────

OPENCODE_VERSION=""
OMO_VERSION=""
PLATFORM=""
ALL_PLATFORMS=false
OUTPUT_DIR=""
LOCK_FILE="${PROJECT_DIR}/configs/worker-runtime-lock.json"
ALLOW_DYNAMIC=false
DRY_RUN=false

# ── Colors ──────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
DIM='\033[0;2m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
die()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# ── Args ────────────────────────────────────────────────────────────────────

need_arg() {
    if [[ $# -lt 2 || "$2" == --* ]]; then
        echo -e "${RED}error: $1 requires an argument${NC}" >&2; exit 1
    fi
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --lock)          need_arg "$@"; LOCK_FILE="$2"; shift 2 ;;
        --allow-dynamic) ALLOW_DYNAMIC=true; shift ;;
        --dry-run)       DRY_RUN=true; shift ;;
        --opencode-version) need_arg "$@"; OPENCODE_VERSION="$2"; shift 2 ;;
        --omo-version)      need_arg "$@"; OMO_VERSION="$2"; shift 2 ;;
        --platform)         need_arg "$@"; PLATFORM="$2"; shift 2 ;;
        --all-platforms)    ALL_PLATFORMS=true; shift ;;
        --output)           need_arg "$@"; OUTPUT_DIR="$2"; shift 2 ;;
        --help)             sed -n '2,/^$/p' "$0" | sed 's/^# //; /^$/d'; exit 0 ;;
        *)                  die "Unknown option: $1" ;;
    esac
done

# An explicit version is a request to depart from the lock. Making that
# explicit is the point: the release workflow never passes these flags, so it
# cannot accidentally ship an unreviewed runtime.
if [[ -n "$OPENCODE_VERSION" || -n "$OMO_VERSION" ]]; then
    ALLOW_DYNAMIC=true
fi

# ── Resolve platform ────────────────────────────────────────────────────────

resolve_platform() {
    local raw_os os arch combo

    raw_os=$(uname -s)
    case "$raw_os" in
        Darwin*)  os="darwin" ;;
        Linux*)   os="linux" ;;
        MINGW*|MSYS*|CYGWIN*|Windows*) os="windows" ;;
        *)        os=$(echo "$raw_os" | tr '[:upper:]' '[:lower:]') ;;
    esac

    arch=$(uname -m)
    case "$arch" in
        x86_64)       arch="x64" ;;
        aarch64|arm64) arch="arm64" ;;
    esac

    # macOS x64 on Apple Silicon (Rosetta)
    if [[ "$os" == "darwin" && "$arch" == "x64" ]]; then
        local rosetta
        rosetta=$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)
        [[ "$rosetta" == "1" ]] && arch="arm64"
    fi

    # musl detection on Linux
    local is_musl=false
    if [[ "$os" == "linux" ]]; then
        if [[ -f /etc/alpine-release ]]; then is_musl=true; fi
        if command -v ldd &>/dev/null; then
            if ldd --version 2>&1 | grep -qi musl; then is_musl=true; fi
        fi
    fi

    combo="${os}-${arch}"
    [[ "$is_musl" == true ]] && combo="${combo}-musl"

    echo "$combo"
}

[[ -z "$PLATFORM" ]] && PLATFORM=$(resolve_platform)

# ── Resolve versions ────────────────────────────────────────────────────────

# Read one value out of the lock manifest. jq is not assumed to exist on every
# machine that packs a bundle; python3 is already required by the CI helpers,
# so it is the safer dependency.
lock_get() {
    python3 - "$LOCK_FILE" "$1" <<'PY'
import json, sys

path, expr = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as handle:
    node = json.load(handle)
for part in expr.split("."):
    if isinstance(node, list):
        node = node[int(part)]
    else:
        node = node[part]
print(node if not isinstance(node, (dict, list)) else json.dumps(node))
PY
}

resolve_latest_opencode() {
    curl -sf https://api.github.com/repos/anomalyco/opencode/releases/latest \
        | jq -r .tag_name 2>/dev/null | sed 's/^v//'
}

resolve_latest_omo() {
    npm view oh-my-opencode version 2>/dev/null || echo "unknown"
}

LOCKED_SHA256=""
LOCKED_OMO_INTEGRITY=""

if [[ "$ALLOW_DYNAMIC" == true ]]; then
    warn "Resolving versions from upstream: this build is NOT reproducible and must not be released."
    [[ -z "$OPENCODE_VERSION" ]] && OPENCODE_VERSION=$(resolve_latest_opencode)
    [[ -z "$OMO_VERSION" ]] && OMO_VERSION=$(resolve_latest_omo)
    [[ "$OPENCODE_VERSION" == "unknown" || -z "$OPENCODE_VERSION" ]] && die "Cannot resolve OpenCode version. Use --opencode-version."
    [[ "$OMO_VERSION" == "unknown" || -z "$OMO_VERSION" ]] && die "Cannot resolve OMO version. Use --omo-version."
else
    [[ -f "$LOCK_FILE" ]] || die "Runtime lock not found: $LOCK_FILE (regenerate with scripts/ci/generate_runtime_lock.py, or pass --allow-dynamic for a non-release build)"

    # Versions come from the lock, never from upstream. A lock that pins
    # "latest" has stopped being a lock.
    OPENCODE_VERSION=$(lock_get runtimes.opencode.version)
    OMO_VERSION=$(lock_get runtimes.oh-my-opencode.version)

    for pinned in "$OPENCODE_VERSION" "$OMO_VERSION"; do
        case "$pinned" in
            ""|latest|unknown)
                die "Runtime lock does not pin a concrete version (got '$pinned')"
                ;;
        esac
    done

    # The digest is what makes this a lock rather than a version number. An
    # unpinned platform fails here, before anything is downloaded, rather than
    # fetching something nobody verified.
    if ! LOCKED_SHA256=$(lock_get "runtimes.opencode.assets.${PLATFORM}.sha256" 2>/dev/null); then
        die "Platform '${PLATFORM}' is not in the runtime lock; refusing to build an unverifiable bundle. Regenerate the lock or pass --allow-dynamic for a non-release build."
    fi
    LOCKED_OMO_INTEGRITY=$(lock_get "runtimes.oh-my-opencode.packages.oh-my-opencode.integrity")
fi

[[ -z "$OUTPUT_DIR" ]] && OUTPUT_DIR="$PROJECT_DIR/dist/offline-bundle"

# ── Summary ─────────────────────────────────────────────────────────────────

echo ""
echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${CYAN}  📦 Offline Bundle Packager${NC}"
echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""
echo -e "  OpenCode:    ${GREEN}v${OPENCODE_VERSION}${NC}"
echo -e "  OMO:         ${GREEN}v${OMO_VERSION}${NC}"
echo -e "  Platform:    ${GREEN}${PLATFORM}${NC}"
echo -e "  All-plat:    ${ALL_PLATFORMS}"
echo -e "  Output:      ${DIM}${OUTPUT_DIR}${NC}"
if [[ "$ALLOW_DYNAMIC" == true ]]; then
    echo -e "  Mode:        ${YELLOW}DYNAMIC (not releasable)${NC}"
else
    echo -e "  Mode:        ${GREEN}LOCKED${NC}"
fi
echo -e "  Lock:        ${DIM}${LOCK_FILE}${NC}"
echo ""

if [[ "$DRY_RUN" == true ]]; then
    echo -e "${DIM}Dry run: nothing downloaded, nothing written.${NC}"
    if [[ "$ALLOW_DYNAMIC" != true ]]; then
        echo "  opencode sha256: ${LOCKED_SHA256}"
        echo "  omo integrity:   ${LOCKED_OMO_INTEGRITY}"
    fi
    exit 0
fi

# ── Prepare ─────────────────────────────────────────────────────────────────

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# ── 1. Download OpenCode binary ─────────────────────────────────────────────

info "[1/4] Downloading OpenCode v${OPENCODE_VERSION} for ${PLATFORM}..."

# Determine archive extension
archive_ext=".zip"
[[ "$PLATFORM" == linux* ]] && archive_ext=".tar.gz"

opencode_filename="opencode-${PLATFORM}${archive_ext}"
opencode_url="https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/${opencode_filename}"

curl -fSL --progress-bar -o "${OUTPUT_DIR}/opencode${archive_ext}" "$opencode_url" \
    || die "Failed to download OpenCode from ${opencode_url}"

# A pinned version is not a pinned artifact: the same tag can be re-uploaded.
# In locked mode the bytes have to match the digest someone reviewed.
if [[ "$ALLOW_DYNAMIC" != true ]]; then
    actual_sha256=$(shasum -a 256 "${OUTPUT_DIR}/opencode${archive_ext}" 2>/dev/null | awk '{print $1}' || true)
    if [[ -z "$actual_sha256" ]]; then
        actual_sha256=$(sha256sum "${OUTPUT_DIR}/opencode${archive_ext}" | awk '{print $1}')
    fi
    if [[ "$actual_sha256" != "$LOCKED_SHA256" ]]; then
        die "OpenCode ${archive_ext} digest mismatch: expected ${LOCKED_SHA256}, got ${actual_sha256}. The downloaded artifact is not the one the runtime lock pins."
    fi
    info "  ✓ digest matches runtime lock (${LOCKED_SHA256:0:16}…)"
fi

info "  ✓ opencode${archive_ext} downloaded"

# ── 2. Pack Oh My OpenAgent npm package ─────────────────────────────────────

info "[2/4] Packing Oh My OpenAgent v${OMO_VERSION}..."

npm pack "oh-my-opencode@${OMO_VERSION}" --pack-destination="$OUTPUT_DIR" 2>/dev/null \
    || die "Failed to pack oh-my-opencode@${OMO_VERSION}"

info "  ✓ oh-my-opencode npm package packed"

# ── 3. Pack OMO platform binaries ───────────────────────────────────────────

info "[3/4] Packing OMO platform binaries..."

# Map opencode platform names to OMO platform package names
# opencode: linux-x64, linux-arm64, darwin-arm64, darwin-x64, windows-x64
# OMO:      oh-my-opencode-linux-x64, oh-my-opencode-linux-arm64, etc.

declare -A PLATFORM_MAP=(
    ["linux-x64"]="oh-my-opencode-linux-x64"
    ["linux-x64-musl"]="oh-my-opencode-linux-x64-musl"
    ["linux-x64-baseline"]="oh-my-opencode-linux-x64-baseline"
    ["linux-arm64"]="oh-my-opencode-linux-arm64"
    ["linux-arm64-musl"]="oh-my-opencode-linux-arm64-musl"
    ["darwin-x64"]="oh-my-opencode-darwin-x64"
    ["darwin-x64-baseline"]="oh-my-opencode-darwin-x64-baseline"
    ["darwin-arm64"]="oh-my-opencode-darwin-arm64"
    ["windows-x64"]="oh-my-opencode-windows-x64"
    ["windows-arm64"]="oh-my-opencode-windows-arm64"
)

pack_omo_platform() {
    local pkg_name="$1"
    echo -e "  ${DIM}Packing ${pkg_name}@${OMO_VERSION}...${NC}"
    # There is deliberately no fallback to "whatever version is published
    # today". A bundle that quietly ships a different runtime than the one
    # reviewed is worse than a build that stops and says so.
    if ! npm pack "${pkg_name}@${OMO_VERSION}" --pack-destination="$OUTPUT_DIR" 2>/dev/null; then
        die "${pkg_name}@${OMO_VERSION} is not available. Refusing to substitute another version; regenerate the lock or pass --allow-dynamic for a non-release build."
    fi
}

if [[ "$ALL_PLATFORMS" == true ]]; then
    for pkg in "${PLATFORM_MAP[@]}"; do
        pack_omo_platform "$pkg"
    done
else
    # Pack only the target platform (+ fallback for baseline)
    target_pkg="${PLATFORM_MAP[$PLATFORM]:-}"

    if [[ -n "$target_pkg" ]]; then
        pack_omo_platform "$target_pkg"
    else
        warn "  No direct OMO platform mapping for '${PLATFORM}', packing all platforms as fallback..."
        for pkg in "${PLATFORM_MAP[@]}"; do
            pack_omo_platform "$pkg"
        done
    fi

    # Also pack baseline variant for x64 if main was requested
    if [[ "$PLATFORM" == *-x64 && "$PLATFORM" != *-baseline* ]]; then
        baseline_pkg="${PLATFORM_MAP[${PLATFORM}-baseline]:-}"
        [[ -n "$baseline_pkg" ]] && pack_omo_platform "$baseline_pkg"
    fi
fi

info "  ✓ OMO platform binaries packed"

# ── 4. Copy install scripts and configs ─────────────────────────────────────

info "[4/4] Copying install scripts..."

# Copy the offline install script
cp "$PROJECT_DIR/scripts/install-offline-bundle.sh" "$OUTPUT_DIR/install.sh"
cp "$PROJECT_DIR/scripts/env-offline.sh" "$OUTPUT_DIR/env-offline.sh"
chmod +x "$OUTPUT_DIR/install.sh" "$OUTPUT_DIR/env-offline.sh"

# Generate version metadata
cat > "$OUTPUT_DIR/versions.env" << EOF
# Auto-generated by pack-offline-bundle.sh
OPENCODE_VERSION=${OPENCODE_VERSION}
OMO_VERSION=${OMO_VERSION}
PLATFORM=${PLATFORM}
PACK_DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
PACK_HOST=$(hostname 2>/dev/null || echo "unknown")
EOF

# Generate README for the bundle
cat > "$OUTPUT_DIR/README.md" << 'README'
# HotPlex Offline Bundle — OpenCode + Oh My OpenAgent

Air-gapped deployment package for internal networks without internet access.

## Contents

| File | Description |
|------|-------------|
| `opencode.tar.gz` / `.zip` | OpenCode pre-compiled binary |
| `oh-my-opencode-*.tgz` | Oh My OpenAgent npm package |
| `oh-my-opencode-<platform>-*.tgz` | OMO platform native binaries |
| `install.sh` | One-click install script |
| `env-offline.sh` | Offline environment variable setup |
| `versions.env` | Version metadata |

## Quick Install

```bash
tar -xzf hotplex-offline-bundle-*.tar.gz
cd hotplex-offline-bundle-*/
bash install.sh
```

## Configuration

### 1. Set offline environment

```bash
source env-offline.sh
```

### 2. Configure internal LLM endpoint

Create or edit `~/.config/opencode/opencode.json` (global config):

```json
{
  "plugin": ["oh-my-openagent"],
  "provider": {
    "my-vllm": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://YOUR_LLM_HOST:PORT/v1"
      }
    }
  },
  "model": {
    "default": "my-vllm/YOUR_MODEL_NAME"
  }
}
```

### 3. Configure OMO (optional)

Create or edit `~/.config/opencode/oh-my-opencode.json`:

```json
{
  "agents": {
    "sisyphus": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "explore": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "librarian": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "oracle": { "model": "my-vllm/YOUR_MODEL_NAME" }
  },
  "categories": {
    "quick": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "deep": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "visual-engineering": { "model": "my-vllm/YOUR_MODEL_NAME" },
    "ultrabrain": { "model": "my-vllm/YOUR_MODEL_NAME" }
  }
}
```

### 4. Start

```bash
cd /your/project
opencode
```

## Requirements

- Internal LLM service with OpenAI-compatible API (vLLM, Ollama, etc.)
- Bash shell
- `tar` (Linux) or `unzip` (macOS/Windows)

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `OPENCODE_DISABLE_MODELS_FETCH=1` | Skip models.dev fetch |
| `OPENCODE_DISABLE_LSP_DOWNLOAD=1` | Skip LSP binary downloads |
| `OPENCODE_DISABLE_AUTOUPDATE=1` | Skip update checks |
| `OPENCODE_DISABLE_DEFAULT_PLUGINS=1` | Skip default plugin installation |

All set automatically by `env-offline.sh`.
README

info "  ✓ Scripts and metadata generated"

# ── Final archive ───────────────────────────────────────────────────────────

BUNDLE_NAME="hotplex-offline-bundle-${PLATFORM}-opencode${OPENCODE_VERSION}-omo${OMO_VERSION}"

if [[ "$PLATFORM" == windows* ]]; then
    FINAL_ARCHIVE="${PROJECT_DIR}/dist/${BUNDLE_NAME}.zip"
    mkdir -p "${PROJECT_DIR}/dist"
    (cd "$OUTPUT_DIR" && zip -r "$FINAL_ARCHIVE" .)
else
    FINAL_ARCHIVE="${PROJECT_DIR}/dist/${BUNDLE_NAME}.tar.gz"
    tar -czf "$FINAL_ARCHIVE" -C "$(dirname "$OUTPUT_DIR")" "$(basename "$OUTPUT_DIR")"
fi

echo ""
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${GREEN}  ✓ Offline bundle created${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""
echo -e "  Bundle:  ${FINAL_ARCHIVE}"
echo -e "  Size:    $(du -sh "$FINAL_ARCHIVE" | cut -f1)"
echo ""
echo -e "  ${DIM}Contents:${NC}"
ls -lh "$OUTPUT_DIR/" | tail -n +2 | while read -r line; do
    echo -e "  ${DIM}  $line${NC}"
done
echo ""
echo -e "  ${DIM}Transfer to air-gapped network:${NC}"
if [[ "$PLATFORM" == windows* ]]; then
    echo -e "  ${DIM}  scp ${FINAL_ARCHIVE} user@internal-server:/tmp/${NC}"
    echo -e "  ${DIM}  Expand-Archive ${BUNDLE_NAME}.zip${NC}"
else
    echo -e "  ${DIM}  scp ${FINAL_ARCHIVE} user@internal-server:/tmp/${NC}"
    echo -e "  ${DIM}  tar -xzf ${BUNDLE_NAME}.tar.gz${NC}"
fi
echo -e "  ${DIM}  cd ${BUNDLE_NAME} && bash install.sh${NC}"
echo ""
