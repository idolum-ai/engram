#!/usr/bin/env bash
set -euo pipefail

action=${1:-}
binary=${2:-"$HOME/.local/bin/engram"}
env_file=${3:-"$HOME/.engram/.env"}
label=ai.idolum.engram
plist="$HOME/Library/LaunchAgents/$label.plist"
unit="$HOME/.config/systemd/user/engram.service"
status_file="$HOME/.engram/service.identity"

usage() {
  echo "usage: user-service.sh install|start|stop|restart|status|logs|uninstall [binary] [env-file]" >&2
  exit 2
}

platform=$(uname -s)
case "$platform" in
  Darwin|Linux) ;;
  *) echo "unsupported service platform: $platform" >&2; exit 1 ;;
esac

install_common() {
  test -x "$binary" || { echo "missing executable: $binary" >&2; exit 1; }
  install -d -m 0700 "$HOME/.engram"
  if [[ ! -f "$env_file" ]]; then
    echo "missing environment file: $env_file" >&2
    echo "create it with mode 0600 before installing the service definition" >&2
    exit 1
  fi
}

install_darwin() {
  install_common
  install -d -m 0700 "$HOME/Library/LaunchAgents"
  local escaped_binary escaped_env escaped_status escaped_home
  escaped_binary=$(printf '%s' "$binary" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g')
  escaped_env=$(printf '%s' "$env_file" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g')
  escaped_status=$(printf '%s' "$status_file" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g')
  escaped_home=$(printf '%s' "$HOME" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g')
  local candidate
  candidate=$(mktemp "${TMPDIR:-/tmp}/engram-launchagent.XXXXXX")
  trap 'rm -f "$candidate"' RETURN
  cat >"$candidate" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>
    <string>$escaped_binary</string><string>run</string><string>--env</string><string>$escaped_env</string>
  </array>
  <key>EnvironmentVariables</key><dict><key>ENGRAM_SERVICE_STATUS_FILE</key><string>$escaped_status</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>AbandonProcessGroup</key><true/>
  <key>StandardOutPath</key><string>$escaped_home/.engram/service.stdout.log</string>
  <key>StandardErrorPath</key><string>$escaped_home/.engram/service.stderr.log</string>
</dict></plist>
EOF
  plutil -lint "$candidate" >/dev/null
  install -m 0600 "$candidate" "$plist"
  echo "installed $plist"
  echo "the running service was not changed; run 'make service-start' or 'make service-restart' explicitly"
}

install_linux() {
  install_common
  install -d -m 0700 "$HOME/.config/systemd/user"
  cat >"$unit" <<EOF
[Unit]
Description=Engram Telegram tmux client
After=default.target

[Service]
Type=simple
Environment="ENGRAM_SERVICE_STATUS_FILE=$status_file"
ExecStart="$binary" run --env "$env_file"
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now engram.service
  echo "installed $unit"
}

darwin_start() { launchctl bootstrap "gui/$UID" "$plist"; }
darwin_stop() { launchctl bootout "gui/$UID/$label"; }

print_live_identity() {
  local manager_pid=$1 identity_pid identity_build
  [[ $manager_pid =~ ^[1-9][0-9]*$ ]] || { echo "service manager did not report a live PID" >&2; exit 1; }
  [[ -f $status_file ]] || { echo "running service identity is unavailable: $status_file" >&2; exit 1; }
  identity_pid=$(sed -n 's/^pid=//p' "$status_file")
  identity_build=$(sed -n 's/^build=//p' "$status_file")
  [[ $identity_pid =~ ^[1-9][0-9]*$ && $identity_pid == "$manager_pid" ]] || {
    echo "running service identity does not match manager PID $manager_pid" >&2
    exit 1
  }
  [[ -n $identity_build ]] || { echo "running service build is unavailable" >&2; exit 1; }
  echo "verified pid = $identity_pid"
  echo "running build = $identity_build"
}

case "$action" in
  install)
    if [[ $platform == Darwin ]]; then install_darwin; else install_linux; fi
    ;;
  start)
    if [[ $platform == Darwin ]]; then darwin_start; else systemctl --user start engram.service; fi
    ;;
  stop)
    if [[ $platform == Darwin ]]; then darwin_stop; else systemctl --user stop engram.service; fi
    ;;
  restart)
    if [[ $platform == Darwin ]]; then
      launchctl bootout "gui/$UID/$label" 2>/dev/null || true
      darwin_start
    else
      systemctl --user restart engram.service
    fi
    ;;
  status)
    if [[ $platform == Darwin ]]; then
      launch_status=$(launchctl print "gui/$UID/$label")
      printf '%s\n' "$launch_status" | sed -n -e '/^[[:space:]]*pid = /p' -e '/^[[:space:]]*state = /p'
      manager_pid=$(printf '%s\n' "$launch_status" | sed -n 's/^[[:space:]]*pid = //p' | head -n 1)
      print_live_identity "$manager_pid"
    else
      systemctl --user status --no-pager engram.service
      manager_pid=$(systemctl --user show --property MainPID --value engram.service)
      print_live_identity "$manager_pid"
    fi
    ;;
  logs)
    lines=${ENGRAM_LOG_LINES:-200}
    [[ $lines =~ ^[0-9]+$ ]] && (( lines >= 1 && lines <= 1000 )) || { echo "ENGRAM_LOG_LINES must be between 1 and 1000" >&2; exit 2; }
    if [[ $platform == Darwin ]]; then
      tail -n "$lines" "$HOME/.engram/service.stdout.log" "$HOME/.engram/service.stderr.log" "$HOME/.engram/audit.jsonl" 2>/dev/null || true
    else
      journalctl --user-unit engram.service -n "$lines" --no-pager
    fi
    ;;
  uninstall)
    if [[ $platform == Darwin ]]; then
      launchctl bootout "gui/$UID/$label" 2>/dev/null || true
      rm -f "$plist"
    else
      systemctl --user disable --now engram.service 2>/dev/null || true
      rm -f "$unit"
      systemctl --user daemon-reload
    fi
    ;;
  *) usage ;;
esac
