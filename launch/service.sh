#!/usr/bin/env bash
set -euo pipefail

# Manage the smolllm-server LaunchAgent.

readonly SESSION="gui/$(id -u)"
readonly PLIST="personal.smolllm-server.plist"
readonly LABEL="personal.smolllm-server"
readonly BIN_DIR="${HOME}/.local/bin"
readonly BIN_PATH="${BIN_DIR}/smolllm-server"
readonly CONFIG_DIR="${HOME}/.config/smolllm-server"
readonly CONFIG_PATH="${CONFIG_DIR}/config.yaml"
readonly TARGET="${HOME}/Library/LaunchAgents/${PLIST}"

usage() {
    cat >&2 <<EOF
Usage: $0 {install|reinstall|reload|uninstall|start|stop|status|logs|build}

  install     Build the binary, seed the config, symlink the plist, and bootstrap the agent.
  reinstall   Tear down and reinstall the agent (use after editing the plist).
  reload      Rebuild the binary and kickstart the running service.
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

build_binary() {
    local repo
    repo="$(resolve_repo_root)"
    mkdir -p "${BIN_DIR}"
    echo "→ building ${BIN_PATH} (from ${repo})"
    (cd "${repo}" && go build -o "${BIN_PATH}" ./cmd/server)
    echo "✓ built ${BIN_PATH}"
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
    verify_started
}

bootout() {
    if ! is_loaded; then
        echo "  ${LABEL} not loaded"
        return 0
    fi
    echo "→ booting out ${LABEL}"
    launchctl bootout "${SESSION}" "${TARGET}" 2>/dev/null || true
}

verify_started() {
    local pid
    for _ in 1 2 3 4 5; do
        sleep 1
        if pid=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'pid = [0-9]+' | grep -oE '[0-9]+'); then
            echo "✓ ${LABEL} running (pid ${pid})"
            return 0
        fi
    done
    echo "✗ ${LABEL} did not start" >&2
    local exit_code
    exit_code=$(launchctl print "${SESSION}/${LABEL}" 2>/dev/null | grep -oE 'last exit code = [0-9-]+' | grep -oE '[0-9-]+$' || echo unknown)
    echo "  last exit code: ${exit_code}" >&2
    if [[ -f "/tmp/${LABEL}.log" ]]; then
        echo "  recent log:" >&2
        tail -20 "/tmp/${LABEL}.log" | sed 's/^/    /' >&2
    fi
    return 1
}

cmd_install() {
    build_binary
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
    seed_config
    bootout
    rm -f "${TARGET}"
    link_plist
    bootstrap
}

cmd_reload() {
    build_binary
    if ! is_loaded; then
        echo "  ${LABEL} not loaded; bootstrapping"
        link_plist
        bootstrap
        return 0
    fi
    echo "→ kickstarting ${LABEL}"
    launchctl kickstart -k "${SESSION}/${LABEL}"
    verify_started
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
    local log="/tmp/${LABEL}.log"
    if [[ ! -f "${log}" ]]; then
        echo "log not found: ${log}" >&2
        exit 1
    fi
    tail -f "${log}"
}

main() {
    case "${1-}" in
        install)   cmd_install ;;
        reinstall) cmd_reinstall ;;
        reload)    cmd_reload ;;
        uninstall) cmd_uninstall ;;
        start)     cmd_start ;;
        stop)      cmd_stop ;;
        status)    cmd_status ;;
        logs)      cmd_logs ;;
        build)     build_binary ;;
        *)         usage ;;
    esac
}

main "$@"
