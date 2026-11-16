#!/bin/bash
# End-to-end test of the packaged macOS app's service lifecycle.
#
# Registers the LaunchAgent embedded in Tabtender.app through SMAppService,
# drives real browser sessions through the launchd-managed daemon, and checks
# the lifecycle guarantees: the GUI can come and go, launchd restarts the
# daemon after a crash, stop/start work, and app.startOnLogin is honored.
#
# Usage: packaging/macos/e2e-launchd.sh [path/to/Tabtender.app]
#
# The test uses the real per-user locations (launchd, Keychain,
# ~/Library/Application Support/Tabtender) and unregisters the service when
# it finishes. Set TABTENDER_CHROME_PATH to use a specific browser.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
APP="$(cd "${1:-$ROOT/dist/Tabtender.app}" && pwd)"
GUI="$APP/Contents/MacOS/Tabtender"
LABEL="com.skillfulagents.tabtender.daemon"
TARGET="gui/$(id -u)/$LABEL"
SOCKET="$HOME/Library/Application Support/Tabtender/tabtenderd.sock"

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

mgmt() { # method path [json]
	curl --silent --show-error --fail-with-body --max-time 60 --unix-socket "$SOCKET" \
		-X "$1" ${3:+-H 'Content-Type: application/json' -d "$3"} "http://tabtender$2"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
daemon_pid() { mgmt GET /mgmt/v1/status 2>/dev/null | json 'd["pid"]' 2>/dev/null || true; }
wait_until() { # description timeout-seconds command...
	local what="$1" timeout="$2"; shift 2
	for _ in $(seq 1 $((timeout * 5))); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	fail "timed out waiting for $what"
}
daemon_up() { [ -n "$(daemon_pid)" ]; }
daemon_down() { [ -z "$(daemon_pid)" ]; }

cleanup() {
	status=$?
	pkill -f "$GUI\$" 2>/dev/null || true
	"$GUI" --unregister-service >/dev/null 2>&1 || true
	if [ $status -ne 0 ]; then
		echo "--- daemon log tail ---"
		tail -n 40 "$HOME/Library/Application Support/Tabtender/logs/tabtenderd.log" 2>/dev/null || true
	fi
	exit $status
}
trap cleanup EXIT

step "Register the service through SMAppService"
"$GUI" --register-service
"$GUI" --service-status
wait_until "the daemon to answer" 30 daemon_up
launchctl print "$TARGET" | grep -q "state = running" || fail "launchd does not report the service as running"
PID1="$(daemon_pid)"
echo "launchd runs tabtenderd as pid $PID1"

if [ -n "${TABTENDER_CHROME_PATH:-}" ]; then
	step "Point the service at $TABTENDER_CHROME_PATH (through the management channel)"
	mgmt PUT /mgmt/v1/config "$(python3 -c 'import json,os; print(json.dumps({"browser": {"runtime": "chrome", "executablePath": os.environ["TABTENDER_CHROME_PATH"], "headless": False}}))')" >/dev/null
fi
mgmt GET /mgmt/v1/status | json '"browser available: %s" % d["browser"]["available"]' | grep -q True || fail "no browser available to the service"

step "Retrieve the generated API key (stored in the Keychain)"
KEY_JSON="$(mgmt GET /mgmt/v1/apikey)"
API_KEY="$(echo "$KEY_JSON" | json 'd["apiKey"]')"
[ "$(echo "$KEY_JSON" | json 'd["source"]')" = "keychain" ] || fail "expected the key to live in the Keychain: $KEY_JSON"
URL="$(mgmt GET /mgmt/v1/status | json 'd["localUrl"]')"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/health")" = 401 ] || fail "unauthenticated request was not rejected"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $API_KEY" "$URL/v1/health")" = 200 ] || fail "API key rejected"

step "Drive headed browser sessions with Playwright and Puppeteer"
(cd "$ROOT/test/clients" && TABTENDER_URL="$URL" TABTENDER_API_KEY="$API_KEY" TABTENDER_HEADLESS=false npm test --silent) |
	grep -E '^(ok|not ok|# (pass|fail))' || fail "client conformance suite failed"

step "The GUI can open and close without disturbing the service"
SESSION="$(curl -s -H "Authorization: Bearer $API_KEY" -X POST -d '{"ttlSeconds": 600}' "$URL/v1/sessions" | json 'd["id"]')"
open -g "$APP"
wait_until "the menu-bar app to start" 20 pgrep -f "$GUI\$"
sleep 3
[ "$(daemon_pid)" = "$PID1" ] || fail "launching the GUI restarted the daemon"
pkill -f "$GUI\$"
wait_until "the menu-bar app to exit" 10 sh -c "! pgrep -f '$GUI\$'"
sleep 1
[ "$(daemon_pid)" = "$PID1" ] || fail "closing the GUI stopped the daemon"
[ "$(curl -s -H "Authorization: Bearer $API_KEY" "$URL/v1/sessions/$SESSION" | json 'd["status"]')" = running ] ||
	fail "the session did not survive the GUI closing"
open -g "$APP"
wait_until "the menu-bar app to restart" 20 pgrep -f "$GUI\$"
sleep 2
[ "$(daemon_pid)" = "$PID1" ] || fail "reopening the GUI restarted the daemon"
pkill -f "$GUI\$"

step "launchd restarts the daemon after a crash, and it reconciles"
kill -9 "$PID1"
wait_until "launchd to restart the daemon" 30 sh -c '[ -n "$(curl -s --max-time 2 --unix-socket "'"$SOCKET"'" http://t/mgmt/v1/status)" ]'
PID2="$(daemon_pid)"
[ -n "$PID2" ] && [ "$PID2" != "$PID1" ] || fail "the daemon was not restarted (pid $PID2)"
[ "$(mgmt GET /v1/sessions | json 'len(d["sessions"])')" = 0 ] || fail "sessions survived a daemon crash"
! pgrep -f "user-data-dir=.*Tabtender/profiles/ephemeral/$SESSION" >/dev/null || fail "an orphaned browser was left running"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $API_KEY" "$URL/v1/health")" = 200 ] ||
	fail "the API key did not survive the restart"
echo "restarted as pid $PID2; orphaned browser reclaimed"

step "Stop and start through launchd"
"$GUI" --stop-service
wait_until "the daemon to stop" 30 daemon_down
sleep 6 # longer than the launchd throttle interval: it must stay down
daemon_down || fail "launchd restarted the daemon after a clean stop"
"$GUI" --start-service
wait_until "the daemon to start" 30 daemon_up

step "Settings persist across restarts; app.startOnLogin gates launchd starts"
mgmt PUT /mgmt/v1/config '{"app": {"startOnLogin": false, "preventSleep": true}, "sessions": {"defaultTTL": 1234, "maxTTL": 14400, "maxConcurrent": 8}}' >/dev/null
"$GUI" --stop-service
wait_until "the daemon to stop" 30 daemon_down
# What launchd does at login: start the agent with no user request.
launchctl kickstart "$TARGET"
sleep 4
daemon_down || fail "the daemon stayed up although app.startOnLogin is off"
# An explicit start still works.
"$GUI" --start-service
wait_until "the daemon to start on request" 30 daemon_up
[ "$(mgmt GET /mgmt/v1/config | json 'd["effective"]["sessions"]["defaultTTL"]')" = 1234 ] || fail "settings did not persist"
mgmt PUT /mgmt/v1/config '{"app": {"startOnLogin": true, "preventSleep": true}, "sessions": {"defaultTTL": 3600, "maxTTL": 14400, "maxConcurrent": 8}}' >/dev/null
"$GUI" --stop-service
wait_until "the daemon to stop" 30 daemon_down
launchctl kickstart "$TARGET"
wait_until "the daemon to start at (simulated) login" 30 daemon_up

step "Unregister"
"$GUI" --unregister-service
wait_until "the daemon to stop" 30 daemon_down
! launchctl print "$TARGET" >/dev/null 2>&1 || fail "the service is still loaded after unregistering"

printf '\nPASS: macOS service lifecycle\n'
