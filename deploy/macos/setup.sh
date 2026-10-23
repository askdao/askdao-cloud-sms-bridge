#!/bin/bash
# =============================================================
# AskDAO SMS Bridge - Mac mini service script (launchd)
# =============================================================
# Usage: ./deploy/macos/setup.sh [install|uninstall|status|logs]
# Build first: go build -o bin/sms-bridge ./cmd/sms-bridge
# =============================================================

set -euo pipefail

SERVICE_LABEL="com.askdao.sms-bridge"
PLIST_NAME="${SERVICE_LABEL}.plist"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PLIST_SRC="${SCRIPT_DIR}/${PLIST_NAME}"
PLIST_DST="$HOME/Library/LaunchAgents/${PLIST_NAME}"
LOG_DIR="${PROJECT_DIR}/logs"
BINARY="${PROJECT_DIR}/bin/sms-bridge"

install_service() {
    echo "==> Installing SMS Bridge service..."

    if [[ ! -x "$BINARY" ]]; then
        echo "ERROR: ${BINARY} not found. Build it first:"
        echo "    cd ${PROJECT_DIR} && go build -o bin/sms-bridge ./cmd/sms-bridge"
        exit 1
    fi

    if [[ ! -f "${PROJECT_DIR}/.env" ]]; then
        echo "ERROR: .env not found. Copy .env.example to .env and configure it first."
        exit 1
    fi

    local session_file
    session_file="$(grep -E '^BRIDGE_SESSION_FILE=' "${PROJECT_DIR}/.env" | tail -1 | cut -d= -f2- || true)"
    session_file="${session_file:-session.json}"
    [[ "$session_file" == /* ]] || session_file="${PROJECT_DIR}/${session_file}"
    if [[ ! -f "$session_file" ]]; then
        echo "WARNING: no session file yet (${session_file}). The service exits until you pair:"
        echo "    cd ${PROJECT_DIR} && ./bin/sms-bridge pair -cookies <cookie file>"
    fi

    local chrome_path
    chrome_path="$(grep -E '^BRIDGE_CHROME_PATH=' "${PROJECT_DIR}/.env" | tail -1 | cut -d= -f2- || true)"
    chrome_path="${chrome_path#[\"\']}"; chrome_path="${chrome_path%[\"\']}"
    chrome_path="${chrome_path:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
    if [[ ! -x "$chrome_path" ]]; then
        echo "WARNING: no Google Chrome at ${chrome_path}. send_image frames will go out as text."
        echo "    Install Google Chrome, or set BRIDGE_CHROME_PATH in .env."
    fi

    mkdir -p "$LOG_DIR"

    echo "==> Generating launchd plist..."
    mkdir -p "$(dirname "$PLIST_DST")"
    sed -e "s|__INSTALL_DIR__|${PROJECT_DIR}|g" "$PLIST_SRC" > "$PLIST_DST"

    if launchctl list "$SERVICE_LABEL" &>/dev/null; then
        echo "==> Unloading existing service..."
        launchctl unload "$PLIST_DST" 2>/dev/null || true
    fi

    echo "==> Loading service..."
    launchctl load "$PLIST_DST"

    echo ""
    echo "==> Done! Service installed and started."
    echo "    Label:  ${SERVICE_LABEL}"
    echo "    Plist:  ${PLIST_DST}"
    echo "    Logs:   ${LOG_DIR}/bridge.log"
    echo "    Errors: ${LOG_DIR}/bridge.error.log"
    echo ""
    echo "    Check status: $0 status"
    echo "    View logs:    $0 logs"
}

uninstall_service() {
    echo "==> Uninstalling SMS Bridge service..."

    if launchctl list "$SERVICE_LABEL" &>/dev/null; then
        launchctl unload "$PLIST_DST" 2>/dev/null || true
        echo "==> Service unloaded."
    else
        echo "==> Service was not loaded."
    fi

    if [[ -f "$PLIST_DST" ]]; then
        rm "$PLIST_DST"
        echo "==> Plist removed: ${PLIST_DST}"
    fi

    echo "==> Done."
}

show_status() {
    echo "==> SMS Bridge service status:"
    echo ""
    if launchctl list "$SERVICE_LABEL" &>/dev/null; then
        launchctl list "$SERVICE_LABEL"
        echo ""
        echo "Service is LOADED."
        local pid
        pid=$(launchctl list | awk -v l="$SERVICE_LABEL" '$3 == l {print $1}')
        if [[ -n "$pid" && "$pid" != "-" ]]; then
            echo "Process PID: $pid"
        else
            echo "Process is NOT running."
        fi
    else
        echo "Service is NOT loaded."
    fi
    echo ""
    echo "Last status change:"
    grep -h "status changed" "${LOG_DIR}/bridge.log" "${LOG_DIR}/bridge.error.log" 2>/dev/null | sort | tail -1 || true
}

show_logs() {
    echo "==> Recent logs (last 50 lines):"
    echo "--- stdout ---"
    tail -50 "${LOG_DIR}/bridge.log" 2>/dev/null || echo "(no logs yet)"
    echo ""
    echo "--- stderr ---"
    tail -50 "${LOG_DIR}/bridge.error.log" 2>/dev/null || echo "(no error logs yet)"
}

case "${1:-}" in
    install)
        install_service
        ;;
    uninstall)
        uninstall_service
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs
        ;;
    *)
        echo "Usage: $0 [install|uninstall|status|logs]"
        exit 1
        ;;
esac
