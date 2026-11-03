#!/usr/bin/env bash
set -euo pipefail
child_pid=''
child_starting=false
pending_signal=0
capture_file=''
handle_signal() {
  # Do not exit between starting a child and recording its PID.
  if $child_starting; then pending_signal=$1; else exit "$1"; fi
}
cleanup_child() {
  trap - EXIT
  trap '' INT TERM
  if [[ -n "$child_pid" ]]; then
    local active
    for active in $(jobs -pr); do
      [[ "$active" == "$child_pid" ]] || continue
      # Pin our still-running child before the grace period, so its PID/PGID
      # cannot be reused. Do not rely on timeout escalating an external TERM.
      if kill -STOP "$child_pid" 2>/dev/null; then
        kill -TERM -- "-$child_pid" 2>/dev/null || true
        /bin/sleep 1 || true
        kill -KILL -- "-$child_pid" 2>/dev/null || true
        kill -KILL "$child_pid" 2>/dev/null || true
      fi
      break
    done
    wait "$child_pid" 2>/dev/null || true
    child_pid=''
  fi
  [[ -z "$capture_file" ]] || rm -f -- "$capture_file"
}
trap cleanup_child EXIT
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM
using_systemd() { [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null; }
trusted_path() {
  local owner mode
  [[ ! -L "$1" && ( -d "$1" || -f "$1" ) ]] || return 1
  read -r owner mode < <(stat -c '%u %a' -- "$1") || return 1
  [[ "$owner" == "$(id -u)" && "$mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$mode & 0022) == 0 ))
}
trusted_helper() {
  local directory=${1%/*}
  [[ -d "$directory" && -f "$1" ]] && trusted_path "$directory" && trusted_path "$1"
}
require_timeout() {
  command -v timeout >/dev/null && timeout --foreground 1 true </dev/null 7>&- 8>&- 9>&- || {
    echo 'GNU coreutils timeout is required. Use the latest panel bootstrap to repair dependencies (including on OpenRC/Alpine).' >&2; return 1;
  }
}
legacy_installer_restart() {
  [[ "${1:-}" == restart ]] || return 1
  # v1.0.16 closes child lock descriptors but synchronously waits for this
  # restart while retaining all three locks itself. Accept only that complete
  # root Bash transaction, never an arbitrary busy lock or a caller-supplied PID.
  local proc_root=/proc parent_dir parent_exe script_mode parent_uid script_uid current_uid
  local parent_stat parent_start self_stat
  local -a parent_fields self_fields
  parent_dir="$proc_root/$PPID"
  read -r parent_stat < "$parent_dir/stat" || return 1
  read -r -a parent_fields <<< "${parent_stat##*) }"
  parent_start=${parent_fields[19]:-}
  [[ "$parent_start" =~ ^[0-9]+$ ]] || return 1
  [[ -f "$parent_dir/fd/255" ]] || return 1
  parent_exe=$(readlink -- "$parent_dir/exe") || return 1
  parent_exe=${parent_exe%" (deleted)"}
  [[ "$parent_exe" == */bash ]] || return 1
  parent_uid=$(stat -Lc '%u' "$parent_dir") || return 1
  script_uid=$(stat -Lc '%u' "$parent_dir/fd/255") || return 1
  current_uid=$(id -u) || return 1
  # lock_operation already requires EUID 0; matching the actual UID also lets
  # sandboxed tests model root branches without weakening that production guard.
  [[ "$parent_uid" == "$current_uid" && "$script_uid" == "$current_uid" ]] || return 1
  script_mode=$(stat -Lc '%a' "$parent_dir/fd/255") || return 1
  [[ "$script_mode" =~ ^[0-7]{3,4}$ ]] || return 1
  (( (8#$script_mode & 0022) == 0 )) || return 1
  local fd name expected actual device inode major minor locked
  local field sequence kind advisory access owner identity first last remainder
  for fd in 9 7 8; do
    case "$fd" in 9) name=bootstrap;; 7) name=deployment;; 8) name=maintenance;; esac
    [[ -f "/etc/v2box/.$name.lock" && ! -L "/etc/v2box/.$name.lock" ]] || return 1
    expected=$(stat -Lc '%d:%i' "/etc/v2box/.$name.lock") || return 1
    actual=$(stat -Lc '%d:%i' "$parent_dir/fd/$fd") || return 1
    [[ "$actual" == "$expected" ]] || return 1
    device=${expected%%:*}; inode=${expected#*:}
    major=$(( ((device >> 8) & 0xfff) | ((device >> 32) & 0xfffff000) ))
    minor=$(( (device & 0xff) | ((device >> 12) & 0xffffff00) ))
    locked=false
    while read -r field sequence kind advisory access owner identity first last remainder; do
      [[ "$field" == lock: && "$kind" == FLOCK && "$advisory" == ADVISORY && "$access" == WRITE && "$first" == 0 && "$last" == EOF && -z "$remainder" ]] || continue
      [[ "$identity" =~ ^([0-9a-fA-F]+):([0-9a-fA-F]+):([0-9]+)$ ]] || continue
      [[ "${BASH_REMATCH[3]}" == "$inode" ]] || continue
      (( 16#${BASH_REMATCH[1]} == major && 16#${BASH_REMATCH[2]} == minor )) || continue
      locked=true
      break
    done < "$parent_dir/fdinfo/$fd"
    $locked || return 1
  done
  read -r parent_stat < "$parent_dir/stat" || return 1
  read -r -a parent_fields <<< "${parent_stat##*) }"
  [[ "${parent_fields[19]:-}" == "$parent_start" ]] || return 1
  read -r self_stat < "$proc_root/$$/stat" || return 1
  read -r -a self_fields <<< "${self_stat##*) }"
  [[ "${self_fields[1]:-}" == "$PPID" ]]
}
lock_operation() {
  local lock
  [[ $EUID -eq 0 && $(uname -s) == Linux ]] || { echo 'Run as root on Linux' >&2; return 1; }
  umask 077
  for tool in install flock timeout; do command -v "$tool" >/dev/null || { echo "Required command is missing: $tool" >&2; return 1; }; done
  require_timeout
  [[ ! -L /etc/v2box && ! -L /etc/v2box/.bootstrap.lock && ! -L /etc/v2box/.deployment.lock && ! -L /etc/v2box/.maintenance.lock && ! -L /usr/local/lib/v2box ]] || { echo 'Refusing symlinked installation paths' >&2; return 1; }
  if [[ ! -e /etc/v2box ]]; then install -d -m 755 /etc/v2box; fi
  [[ -d /etc/v2box ]] && trusted_path /etc/v2box || { echo 'Unsafe deployment state directory.' >&2; return 1; }
  if [[ -e /usr/local/lib/v2box ]]; then
    [[ -d /usr/local/lib/v2box ]] && trusted_path /usr/local/lib/v2box || { echo 'Unsafe V2Box program directory.' >&2; return 1; }
  fi
  for lock in /etc/v2box/.bootstrap.lock /etc/v2box/.deployment.lock /etc/v2box/.maintenance.lock; do
    if [[ -e "$lock" ]]; then
      [[ -f "$lock" ]] && trusted_path "$lock" || { echo 'Unsafe deployment or maintenance lock.' >&2; return 1; }
    fi
  done
  case "${1:-}" in
    start|stop|restart|reload) ;;
    *) unset V2BOX_BOOTSTRAP_LOCK_HELD V2BOX_DEPLOYMENT_LOCK_HELD;;
  esac
  if legacy_installer_restart "${1:-}" 2>/dev/null; then return 0; fi
  # A panel worker may delegate only the locks it already holds. Child service
  # processes must never retain these descriptors after this command exits.
  if [[ "${V2BOX_BOOTSTRAP_LOCK_HELD:-}" != 1 ]]; then
    exec 9>>/etc/v2box/.bootstrap.lock
    flock -n 9 || { echo 'Another deployment or maintenance operation is running.' >&2; return 1; }
  fi
  if [[ "${V2BOX_DEPLOYMENT_LOCK_HELD:-}" != 1 ]]; then
    exec 7>>/etc/v2box/.deployment.lock
    flock -n 7 || { echo 'Deployment is actively running; retry after it finishes.' >&2; return 1; }
  fi
  unset V2BOX_BOOTSTRAP_LOCK_HELD V2BOX_DEPLOYMENT_LOCK_HELD
  exec 8>>/etc/v2box/.maintenance.lock
  flock -n 8 || { echo 'Another maintenance operation is running.' >&2; return 1; }
}
run_service() {
  require_timeout
  local result=0
  child_starting=true
  timeout -k 5 90 "$@" </dev/null 7>&- 8>&- 9>&- &
  child_pid=$!
  child_starting=false
  ((pending_signal == 0)) || exit "$pending_signal"
  wait "$child_pid" || result=$?
  child_pid=''
  if ((result == 124 || result == 137)); then
    echo 'Service operation timed out; program files, configuration and certificates were retained. Check v2box status and the service manager, then retry.' >&2
  fi
  return "$result"
}
capture_service() {
  local result=0
  capture_file=$(mktemp)
  run_service "$@" > "$capture_file" || result=$?
  service_output=$(<"$capture_file")
  rm -f -- "$capture_file"
  capture_file=''
  return "$result"
}
verify_service() {
  local wanted=$1 healthy=0 state attempt
  for attempt in 1 2 3 4 5; do
    if using_systemd; then
      if run_service systemctl is-active --quiet v2box; then state=0; else state=$?; fi
      [[ $state == 0 || $state == 3 || $state == 4 ]] || return "$state"
    else
      if run_service rc-service v2box status >/dev/null 2>&1; then state=0; else state=$?; fi
      [[ $state == 0 || $state == 3 ]] || return "$state"
    fi
    if { [[ "$wanted" == active ]] && ((state == 0)); } || { [[ "$wanted" == stopped ]] && ((state != 0)); }; then
      healthy=$((healthy+1))
      ((healthy < 3)) || return 0
    else healthy=0; fi
    sleep 2
  done
  echo "Service did not remain $wanted; inspect v2box status and logs. Configuration and certificates were retained." >&2
  return 1
}
require_enrolled_config() {
  if [[ ! -f /etc/v2box/config.json || -L /etc/v2box/config.json ]] || ! command -v jq >/dev/null || ! jq -e '.Nodes | type == "array" and length > 0 and all(.[]; (.ApiHost | type == "string" and test("^https://[^/?#\\s@]+(/[^?#\\s]*)?$") and (test("^https://panel\\.example\\.com([/:]|$)";"i") | not)) and ((.NodeID | tonumber? // 0) | . > 0 and . == floor) and (.ApiKey | type == "string" and test("\\S") and (test("^\\s*replace_me\\s*$";"i") | not)))' /etc/v2box/config.json >/dev/null 2>&1; then
    echo 'Active node configuration is incomplete/example enrollment, or jq is missing. Resume deployment with v2box resume or use the panel deployment command; no service start was requested.' >&2
    return 1
  fi
}
runtime_matches_installation() {
  [[ "$1" =~ ^[1-9][0-9]*$ && -x "$binary" && "/proc/$1/exe" -ef "$binary" ]] && trusted_helper "$binary"
}
check_runtime() {
  runtime_pid=''
  if using_systemd; then
    capture_service systemctl show v2box --property=MainPID --value || return 1
    runtime_pid=$service_output
    if [[ "$runtime_pid" == 0 ]]; then runtime_pid=''; return 0; fi
  else
    [[ -f /run/v2box.pid && ! -L /run/v2box.pid ]] || return 0
    read -r runtime_pid < /run/v2box.pid || return 1
  fi
  runtime_matches_installation "$runtime_pid" || { echo 'Cannot verify running V2Box identity; existing nodes preserved. Check the service and installed version.' >&2; return 1; }
  timeout -k 2 10 "$binary" capabilities 7>&- 8>&- 9>&- | jq -e --arg required "${1:-multi-panel-v1}" '.features | index("multi-panel-v1") != null and index("reload-signal-v1") != null and index($required) != null' >/dev/null || {
    echo 'Running V2Box lacks independent multi-panel reload. Upgrade it in a maintenance window before deploying another node.' >&2; return 1;
  }
}
binary=/usr/local/lib/v2box/v2box
if (($# == 0)); then set -- help; fi
case "$1" in
  update)
    shift
    (($# <= 1)) || { echo 'Usage: v2box update [release-tag]' >&2; exit 1; }
    [[ $EUID -eq 0 ]] || { echo 'Run as root: v2box update' >&2; exit 1; }
    [[ -s /usr/local/lib/v2box/install.sh ]] && trusted_helper /usr/local/lib/v2box/install.sh || { echo 'Update installer is missing or unsafe. Download the current panel bootstrap and use --install with the published version to repair program files; configuration and certificates remain.' >&2; exit 1; }
    unset V2BOX_BOOTSTRAP_LOCK_HELD V2BOX_DEPLOYMENT_LOCK_HELD
    exec bash /usr/local/lib/v2box/install.sh --update "$@";;
  resume)
    [[ $EUID -eq 0 && $# -eq 1 ]] || { echo 'Run as root: v2box resume' >&2; exit 1; }
    umask 077
    require_timeout
    [[ -x "$binary" ]] || { echo 'V2Box is uninstalled; reinstall program files before resuming. Configuration and certificates remain.' >&2; exit 1; }
    [[ -d /etc/v2box ]] && trusted_path /etc/v2box || { echo 'Unsafe deployment state directory.' >&2; exit 1; }
    lock=/etc/v2box/.bootstrap.lock
    if [[ -e "$lock" || -L "$lock" ]]; then
      [[ -f "$lock" ]] && trusted_path "$lock" || { echo 'Unsafe deployment lock.' >&2; exit 1; }
    fi
    exec 9>>"$lock"
    flock -n 9 || { echo 'Another deployment or maintenance operation is running.' >&2; exit 1; }
    shopt -s nullglob
    states=(/etc/v2box/deployment-state*.json)
    ((${#states[@]} > 0)) || { echo 'No pending deployment.'; exit 0; }
    ((${#states[@]} == 1)) || { echo 'Multiple pending deployments. Re-run the original panel command to resume its selected node.' >&2; exit 1; }
    trusted_path "${states[0]}" && [[ -f "${states[0]}" && ! -L /etc/v2box/.deployment.lock ]] || { echo 'Unsafe deployment state or lock.' >&2; exit 1; }
    trusted_helper /usr/local/lib/v2box/panel-deploy.py || { echo 'Deployment helper is missing or unsafe. Download the current panel bootstrap and run it with --resume.' >&2; exit 1; }
    timeout -k 2 10 python3 /usr/local/lib/v2box/panel-deploy.py --capabilities 7>&- 8>&- 9>&- 2>/dev/null | jq -e '.features | index("management-lock-delegation-v1") != null' >/dev/null || {
      echo 'Deployment helper lacks management lock delegation. Update the panel, then re-download its current bootstrap and run it with --resume. Deployment state, configuration and certificates were retained.' >&2; exit 1;
    }
    check_runtime
    trusted_helper /usr/local/lib/v2box/node-network.py || { echo 'Reporting helper is missing or unsafe. Repair the node installation before resuming.' >&2; exit 1; }
    timeout -k 2 10 python3 /usr/local/lib/v2box/node-network.py capabilities | jq -e '.features | index("report-multi-target-v1") != null' >/dev/null || { echo 'Reporting helper lacks multi-target support; update node scripts before resuming.' >&2; exit 1; }
    exec python3 /usr/local/lib/v2box/panel-deploy.py --managed "${states[0]}";;
  uninstall)
    shift
    stop_network=false
    confirmed=false
    for arg in "$@"; do
      case "$arg" in
        --stop-network) stop_network=true;;
        --yes) confirmed=true;;
        *) echo 'Usage: v2box uninstall [--stop-network] [--yes]' >&2; exit 1;;
      esac
    done
    lock_operation
    echo 'This removes all V2Box nodes on this server. Configuration, certificates and DNS records are retained.'
    if $stop_network; then echo 'The shared node-network IP reporting/DDNS schedule will also be stopped.'; fi
    if ! $confirmed; then
      if ! { read -r -t 120 -p 'Type uninstall to continue (two-minute limit): ' answer < /dev/tty; } 2>/dev/null; then
        echo 'Confirmation could not be read or timed out. Run interactively, or explicitly use v2box uninstall --yes. Nothing was removed.' >&2; exit 1
      fi
      [[ "$answer" == uninstall ]] || { echo 'Uninstall cancelled.'; exit 1; }
    fi
    if using_systemd && [[ -e /etc/systemd/system/v2box-deployment.service || -L /etc/systemd/system/v2box-deployment.service ]]; then
      [[ -f /etc/systemd/system/v2box-deployment.service && ! -L /etc/systemd/system/v2box-deployment.service ]] || { echo 'Unsafe deployment service file; uninstall stopped before deleting programs.' >&2; exit 1; }
      run_service systemctl disable --now v2box-deployment.service
    fi
    if $stop_network; then
      if [[ -e /usr/local/lib/node-network/node-network.py || -L /usr/local/lib/node-network/node-network.py ]]; then
        trusted_helper /usr/local/lib/node-network/node-network.py || { echo 'Unsafe IP reporting helper; uninstall stopped before deleting programs.' >&2; exit 1; }
        run_service python3 /usr/local/lib/node-network/node-network.py disable
      elif using_systemd; then
        echo 'IP reporting helper is missing; stopping its systemd schedule directly. Reporting settings and DNS records are retained.'
        if [[ -f /etc/systemd/system/node-network.timer ]]; then run_service systemctl disable --now node-network.timer; fi
        if [[ -f /etc/systemd/system/node-network.service ]]; then run_service systemctl stop node-network.service; fi
      elif [[ -f /etc/node-network/cloudflare.json ]]; then
        echo 'IP reporting helper is missing. Restore node-network before uninstalling with --stop-network; nothing was removed.' >&2; exit 1
      fi
    fi
    if using_systemd; then
      read_uninstall_state() {
        local name value
        load_state= active_state= main_pid=
        capture_service systemctl show v2box --property=LoadState,ActiveState,MainPID || return 1
        while IFS='=' read -r name value; do
          case "$name" in
            LoadState) load_state=$value;;
            ActiveState) active_state=$value;;
            MainPID) main_pid=$value;;
          esac
        done <<< "$service_output"
        if [[ "$load_state" == not-found ]]; then
          [[ "$active_state" == inactive && "${main_pid:-0}" == 0 ]] || return 1
          main_pid=0
          return 0
        fi
        [[ ( "$load_state" == loaded || "$load_state" == masked ) && -n "$active_state" && "$main_pid" =~ ^[0-9]+$ ]]
      }
      read_uninstall_state || { echo 'Cannot verify service state; removal stopped before deleting programs.' >&2; exit 1; }
      if [[ "$load_state" != not-found ]]; then
        run_service systemctl disable --now v2box
        if ! read_uninstall_state || [[ "$main_pid" != 0 || ( "$active_state" != inactive && "$active_state" != failed ) ]]; then
          echo 'Cannot verify the service stopped; program files retained.' >&2
          exit 1
        fi
      fi
      if [[ -f /etc/systemd/system/v2box.service ]]; then
        rm -f -- /etc/systemd/system/v2box.service
        run_service systemctl daemon-reload
      fi
    elif [[ -f /etc/init.d/v2box ]]; then
      if run_service rc-service v2box status >/dev/null 2>&1; then run_service rc-service v2box stop;
      else state=$?; [[ $state == 3 ]] || exit "$state"; fi
      capture_service rc-update show default || { echo 'Cannot inspect OpenRC startup registration; program files retained.' >&2; exit 1; }
      registrations=$service_output
      if grep -q 'v2box' <<< "$registrations"; then run_service rc-update del v2box default; fi
      if run_service rc-service v2box status >/dev/null 2>&1; then echo 'Service is still running; removal stopped.' >&2; exit 1;
      else state=$?; [[ $state == 3 ]] || exit "$state"; fi
      rm -f -- /etc/init.d/v2box
    fi
    rm -f -- /usr/local/lib/v2box/v2box /usr/local/lib/v2box/v2box.previous /usr/local/lib/v2box/manager.sh /usr/local/lib/v2box/install.sh /usr/local/lib/v2box/node-credentials.py /usr/local/lib/v2box/node-network.py /usr/local/bin/v2box
    echo 'V2Box uninstalled. Configuration and certificates remain in /etc/v2box. Revoke node credentials and review domain bindings in the panel.';;
  check-runtime)
    shift
    (($# <= 1)) || exit 1
    check_runtime "${1:-multi-panel-v1}"
    if (($#)); then printf '%s\n' "$1"; fi;;
  share-listener)
    shift
    [[ $EUID -eq 0 && $# -ge 2 ]] || { echo 'Usage: v2box share-listener GROUP NODE_NUMBER [NODE_NUMBER ...] (use - to disable)' >&2; exit 1; }
    lock_operation
    timeout -k 2 10 "$binary" capabilities 7>&- 8>&- 9>&- | jq -e '.features | index("shared-listener-v1") != null' >/dev/null || { echo 'Upgrade V2Box and scripts before configuring shared listeners.' >&2; exit 1; }
    check_runtime shared-listener-v1
    timeout -k 5 30 python3 /usr/local/lib/v2box/node-credentials.py --shared-listener "$@" 7>&- 8>&- 9>&-
    echo 'Initial shared-listener migration can replace the selected listeners. File watching may apply this immediately; otherwise run v2box reload. A stopped service stays stopped.';;
  reload)
    lock_operation reload
    check_runtime
    if using_systemd; then
      run_service systemctl kill --kill-whom=main --signal=HUP v2box
    else
      [[ -n "$runtime_pid" ]] || { echo 'V2Box is not running' >&2; exit 1; }
      runtime_matches_installation "$runtime_pid" || { echo 'Cannot verify running V2Box identity; reload cancelled.' >&2; exit 1; }
      kill -HUP "$runtime_pid"
    fi;;
  status)
    if using_systemd; then
      result=0
      SYSTEMD_PAGER=cat timeout -k 2 10 systemctl status v2box --no-pager -l || result=$?
      if ((result == 124 || result == 137)); then
        echo 'Status query timed out. Check the system service manager; no node settings were changed.' >&2
      elif [[ -f /etc/v2box/.deployment-pending ]]; then
        echo 'Deployment is pending. Run v2box deploy-status and v2box deploy-log; use v2box resume if the task has stopped.'
      fi
      exit "$result"
    fi
    run_service rc-service v2box status;;
  start|stop|restart|enable|disable)
    action=$1
    lock_operation "$action"
    case "$action" in start|restart) require_enrolled_config;; esac
    if using_systemd; then
      run_service systemctl "$action" v2box
    else
      case "$action" in
        enable) run_service rc-update add v2box default;;
        disable) run_service rc-update del v2box default;;
        *) run_service rc-service v2box "$action";;
      esac
    fi
    case "$action" in start|restart) verify_service active;; stop) verify_service stopped;; esac;;
  deploy-cancel)
    [[ $EUID -eq 0 && $# -eq 1 ]] || { echo 'Run as root: v2box deploy-cancel' >&2; exit 1; }
    using_systemd || { echo 'Automatic reboot recovery requires systemd. Stop the foreground deployment with Ctrl+C; private state and certificates remain.' >&2; exit 1; }
    require_timeout
    if [[ -e /usr/local/lib/v2box/panel-deploy.py || -L /usr/local/lib/v2box/panel-deploy.py ]]; then
      trusted_helper /usr/local/lib/v2box/panel-deploy.py || { echo 'Unsafe deployment helper; cancellation stopped.' >&2; exit 1; }
    fi
    if [[ -f /usr/local/lib/v2box/panel-deploy.py ]] && timeout -k 2 10 python3 /usr/local/lib/v2box/panel-deploy.py --capabilities 7>&- 8>&- 9>&- 2>/dev/null | jq -e '.features | index("reboot-recovery-v1") != null' >/dev/null; then
      run_service python3 /usr/local/lib/v2box/panel-deploy.py --cancel
    else
      capture_service systemctl show --property=Transient --value v2box-deployment.service
      if [[ "$service_output" == yes ]]; then run_service systemctl stop v2box-deployment.service
      else run_service systemctl disable --now v2box-deployment.service; fi
      echo 'Automatic deployment recovery disabled. Private state, configuration and certificates retained; resume explicitly using the panel command.'
    fi;;
  deploy-status)
    using_systemd || { echo 'Managed deployment requires systemd. Check the foreground deployment output.'; exit 1; }
    SYSTEMD_PAGER=cat timeout -k 2 10 systemctl status v2box-deployment --no-pager -l;;
  log|deploy-log)
    unit=v2box
    [[ "$1" != deploy-log ]] || unit=v2box-deployment
    if using_systemd && command -v journalctl >/dev/null; then
      if timeout -k 2 10 systemctl is-active --quiet "$unit"; then
        echo "Following $unit logs. Ctrl+C exits this view without stopping the service."
        exec journalctl -u "$unit" -n 100 --no-pager -f
      fi
      echo "$unit is not active (or its status could not be queried). Showing recent logs without waiting."
      if [[ "$unit" == v2box && -f /etc/v2box/.deployment-pending ]]; then
        echo 'Deployment is pending. Run v2box deploy-status and v2box deploy-log.'
      fi
      exec timeout -k 2 10 journalctl -u "$unit" -n 100 --no-pager
    fi
    [[ "$unit" == v2box ]] || { echo 'No systemd journal; check the foreground deployment output.'; exit 1; }
    [[ -f /var/log/v2box.log ]] || { echo 'No V2Box log file yet. Check v2box status or deployment output.'; exit 0; }
    echo 'Following V2Box logs. Ctrl+C exits this view.'
    exec tail -n 100 -f /var/log/v2box.log;;
  ip) require_timeout; exec timeout -k 5 90 python3 /usr/local/lib/v2box/node-network.py ip;;
  credential)
    lock_operation
    echo 'Credential selection and hidden key input must finish within three minutes.'
    timeout -k 5 180 python3 /usr/local/lib/v2box/node-credentials.py 7>&- 8>&- 9>&-;;
  ddns) shift; require_timeout; exec timeout -k 15 600 python3 /usr/local/lib/v2box/node-network.py "${@:-status}";;
  version|server|capabilities) exec "$binary" "$@";;
  help) echo 'Usage: v2box {start|stop|restart|reload|status|enable|disable|log|version|server|ip|credential|share-listener GROUP NODE_NUMBER...|ddns|resume|deploy-status|deploy-log|deploy-cancel|update [release-tag]|uninstall [--stop-network] [--yes]|help}';;
  *) echo 'Unknown command. Run: v2box help'; exit 1;;
esac
