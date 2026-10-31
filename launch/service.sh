#!/usr/bin/env bash
set -euo pipefail

# Manage the smolllm-server LaunchAgent.

readonly SESSION="gui/$(id -u)"
readonly PLIST="personal.smolllm-server.plist"
readonly LABEL="personal.smolllm-server"
# Must match StandardOutPath/StandardErrorPath in the plist.
readonly LOG_FILE="${HOME}/Library/Logs/${LABEL}.log"
readonly BIN_DIR="${HOME}/.local/bin"
readonly BIN_PATH="${BIN_DIR}/smolllm-server"
readonly CONFIG_DIR="${HOME}/.config/smolllm-server"
readonly CONFIG_PATH="${CONFIG_DIR}/config.yaml"
readonly TARGET="${HOME}/Library/LaunchAgents/${PLIST}"
# Matches server.bind in config.yaml; the port is hardcoded across the smolllm repos.
readonly HEALTH_URL="http://127.0.0.1:11435/healthz"
# ~/.env.smolllm links into ~/Documents (TCC-protected). A launchd process that
# opens it blocks in open() on an unanswered Documents prompt: alive, unbound,
# invisible to KeepAlive (2026-10-31, 1h outage). The service therefore reads a
# private copy outside every TCC folder, written by ifiles' sync-secrets.sh.
readonly ENV_COPY="${HOME}/Library/Application Support/smolllm-server/env"
# A stable identity keeps the binary's own signature constant across rebuilds.
readonly CODESIGN_IDENTITY="${SMOLLLM_SERVER_CODESIGN_IDENTITY:-Developer ID Application: Tian Xia (C9FPMJGKC4)}"
readonly CODESIGN_ID="${LABEL}"

usage() {
    cat >&2 <<EOF
Usage: $0 {install|reinstall|reload|uninstall|start|stop|status|logs|build}

  install     Build the binary, seed the config, symlink the plist, and bootstrap the agent.
  reinstall   Tear down and reinstall the agent (use after editing the plist).
  reload      Rebuild the binary, kickstart the service, and wait for /healthz.
  uninstall   Stop the agent and remove the plist symlink. Binary and config are kept.
  start       Bootstrap the agent (no rebuild).
  stop        Bootout the agent.
  status      Show running state.
  logs        Tail the StandardOut/Err log.
  build       Just build the binary (no service action).
EOF
    exit 64
}

resolve_repo_root() {
    local script_dir
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    cd "${script_dir}/.." && pwd -P
}

# Build beside the live binary, then rename over it. Writing in place let
# launchd respawn the service from a half-written file, which the kernel killed
# as OS_REASON_CODESIGNING (2026-09-24). A rename swaps the inode atomically: the
# running process keeps the old file and the next spawn sees only the new one.
build_binary() {
    local repo tmp
    repo="$(resolve_repo_root)"
    mkdir -p "${BIN_DIR}"
    tmp="$(mktemp "${BIN_PATH}.build.XXXXXX")"
    echo "→ building ${BIN_PATH} (from ${repo})"
    if ! (cd "${repo}" && go build -o "${tmp}" ./cmd/server); then
        rm -f "${tmp}"
        echo "✗ build failed; ${BIN_PATH} untouched" >&2
        return 1
    fi
    chmod 755 "${tmp}"
    if ! codesign --force --sign "${CODESIGN_IDENTITY}" --identifier "${CODESIGN_ID}" "${tmp}"; then
        rm -f "${tmp}"
        echo "✗ codesign failed; ${BIN_PATH} untouched" >&2
        return 1
    fi
    mv -f "${tmp}" "${BIN_PATH}"
    echo "✓ built ${BIN_PATH}"
}

require_env() {
    if [[ ! -f "${ENV_COPY}" ]]; then
        echo "✗ ${ENV_COPY} missing: run ~/Documents/ifiles/scripts/sync-secrets.sh" >&2
        return 1
    fi
}

seed_config() {
    local repo example
    repo="$(resolve_repo_root)"
    example="${repo}/config.example.yaml"
    if [[ -f "${CONFIG_PATH}" ]]; then
        echo "  config exists: ${CONFIG_PATH}"
        return 0
    fi
    if [[ ! -f "${example}" ]]; then
        echo "  config.example.yaml not found at ${example}; skipping seed" >&2
        return 0
    fi
    mkdir -p "${CONFIG_DIR}"
    cp "${example}" "${CONFIG_PATH}"
    echo "✓ seeded ${CONFIG_PATH} from config.example.yaml"
}

link_plist() {
    local source repo
    repo="$(resolve_repo_root)"
    source="${repo}/launch/${PLIST}"
    if [[ ! -f "${source}" ]]; then
        echo "Source plist not found: ${source}" >&2
        exit 1
    fi
    mkdir -p "${HOME}/Library/LaunchAgents"
    # launchd does not create parents for StandardOutPath: a missing directory
    # makes the job fail to spawn with no log to explain why.
    mkdir -p "$(dirname "${LOG_FILE}")"
    if [[ -L "${TARGET}" ]]; then
        local current
        current="$(readlink "${TARGET}")"
        if [[ "${current}" == "${source}" ]]; then
            echo "  plist symlink already correct: ${TARGET}"
            return 0
        fi
        echo "  updating plist symlink (was: ${current})"
        rm -f "${TARGET}"
    elif [[ -e "${TARGET}" ]]; then
        echo "  replacing existing plist file with symlink"
        rm -f "${TARGET}"
    fi
    ln -sf "${source}" "${TARGET}"
    echo "✓ linked ${TARGET} → ${source}"
}

is_loaded() {
    launchctl print "${SESSION}/${LABEL}" &>/dev/null
}

bootstrap() {
    echo "→ bootstrapping ${LABEL}"
    launchctl bootstrap "${SESSION}" "${TARGET}"
    verify_healthy
}

bootout() {
    if ! is_loaded; then
        echo "  ${LABEL} not loaded"
        return 0
    fi
    echo "→ booting out ${LABEL}"
    launchctl bootout "${SESSION}" "${TARGET}" 2>/dev/null || true
}

# A pid alone proves nothing (a dying or hung process still has one); only a
# 200 from /healthz counts. The 20s window covers the two legal slow paths:
# graceful shutdown (≤5s + 10s watchdog) and launchd's spawn throttle (10s).
verify_healthy() {
    local pid
    for _ in $(seq 1 20); do
        if curl -fsS -m 1 -o /dev/null "${HEALTH_URL}" 2>/dev/null; then
            pid=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'pid = [0-9]+' | grep -oE '[0-9]+' || true)
            echo "✓ ${LABEL} healthy (pid ${pid:-unknown})"
            return 0
        fi
        sleep 1
    done
    echo "✗ ${LABEL} not healthy after 20s (${HEALTH_URL})" >&2
    local exit_code
    exit_code=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'last exit code = [0-9-]+' | grep -oE '[0-9-]+$' || echo unknown)
    echo "  last exit code: ${exit_code}" >&2
    if [[ -f "${LOG_FILE}" ]]; then
        echo "  recent log:" >&2
        tail -20 "${LOG_FILE}" | sed 's/^/    /' >&2
    fi
    return 1
}

cmd_install() {
    build_binary
    require_env
    seed_config
    link_plist
    if is_loaded; then
        echo "  ${LABEL} already loaded; use 'reload' to apply binary changes"
        return 0
    fi
    bootstrap
}

cmd_reinstall() {
    build_binary
    require_env
    seed_config
    bootout
    rm -f "${TARGET}"
    link_plist
    bootstrap
}

cmd_reload() {
    build_binary
    require_env
    if ! is_loaded; then
        echo "  ${LABEL} not loaded; bootstrapping"
        link_plist
        bootstrap
        return 0
    fi
    echo "→ kickstarting ${LABEL}"
    launchctl kickstart -k "${SESSION}/${LABEL}"
    if verify_healthy; then
        return 0
    fi
    # The kill already happened; the old process is gone either way. A second
    # kickstart is the recovery that fixed the 2026-07-20 incident by hand:
    # -k restarts a hung instance and plain-starts a dead one.
    echo "→ retrying kickstart ${LABEL}" >&2
    launchctl kickstart -k "${SESSION}/${LABEL}"
    verify_healthy
}

cmd_uninstall() {
    bootout
    if [[ -L "${TARGET}" || -e "${TARGET}" ]]; then
        rm -f "${TARGET}"
        echo "✓ removed ${TARGET}"
    fi
    echo "  binary kept at ${BIN_PATH}"
    echo "  config kept at ${CONFIG_PATH}"
}

cmd_start() {
    if is_loaded; then
        echo "${LABEL} already loaded"
        return 0
    fi
    link_plist
    bootstrap
}

cmd_stop() {
    if ! is_loaded; then
        echo "${LABEL} not loaded; nothing to stop"
        return 1
    fi
    bootout
}

cmd_status() {
    if is_loaded; then
        echo "${LABEL}: running"
        launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -E "pid|state|last exit" | head -10
    else
        echo "${LABEL}: stopped"
    fi
}

cmd_logs() {
    local log="${LOG_FILE}"
    if [[ ! -f "${log}" ]]; then
        echo "log not found: ${log}" >&2
        exit 1
    fi
    tail -f "${log}"
}

main() {
    case "${1-}" in
    install) cmd_install ;;
    reinstall) cmd_reinstall ;;
    reload) cmd_reload ;;
    uninstall) cmd_uninstall ;;
    start) cmd_start ;;
    stop) cmd_stop ;;
    status) cmd_status ;;
    logs) cmd_logs ;;
    build) build_binary ;;
    *) usage ;;
    esac
}

main "$@"
