#!/usr/bin/env bash
set -euo pipefail
# v2box-manager-v2
run_service() {
  command -v timeout >/dev/null || { echo 'Required command timeout is missing. Use the latest panel installation script to repair dependencies.' >&2; return 1; }
  local result=0
  timeout -k 5 90 "$@" 7>&- 8>&- 9>&- || result=$?
  if ((result == 124 || result == 137)); then
    echo 'Service operation timed out; program files, configuration and certificates were retained. Check v2box status and the service manager, then retry.' >&2
  fi
  return "$result"
}
check_runtime() {
  local pid=''
  if command -v systemctl >/dev/null; then
    pid=$(run_service systemctl show v2box --property=MainPID --value) || return 1
    [[ "$pid" != 0 && -n "$pid" ]] || return 0
  else
    [[ -f /run/v2box.pid && ! -L /run/v2box.pid ]] || return 0
    read -r pid < /run/v2box.pid
  fi
  [[ "$pid" =~ ^[1-9][0-9]*$ && -x /proc/$pid/exe ]] || { echo 'Cannot verify running V2Box identity; existing nodes preserved.' >&2; return 1; }
  timeout -k 2 10 /proc/"$pid"/exe capabilities | jq -e --arg required "${1:-multi-panel-v1}" '.features | index("multi-panel-v1") != null and index("reload-signal-v1") != null and index($required) != null' >/dev/null || {
    echo 'Running V2Box lacks independent multi-panel reload. Upgrade it in a maintenance window before deploying another node.' >&2; return 1;
  }
}
binary=/usr/local/lib/v2box/v2box
if (($# == 0)); then set -- help; fi
case "$1" in
  update)
    shift
    (($# <= 1)) || { echo 'Usage: v2box update [release-tag]' >&2; exit 1; }
    [[ -s /usr/local/lib/v2box/install.sh && ! -L /usr/local/lib/v2box/install.sh ]] || { echo 'Update installer is missing or unsafe. Download the current panel bootstrap and use --install with the published version to repair program files; configuration and certificates remain.' >&2; exit 1; }
    exec bash /usr/local/lib/v2box/install.sh --update "$@";;
  resume)
    [[ $EUID -eq 0 && $# -eq 1 ]] || { echo 'Run as root: v2box resume' >&2; exit 1; }
    [[ -x "$binary" ]] || { echo 'V2Box is uninstalled; reinstall program files before resuming. Configuration and certificates remain.' >&2; exit 1; }
    shopt -s nullglob
    states=(/etc/v2box/deployment-state*.json)
    ((${#states[@]} > 0)) || { echo 'No pending deployment.'; exit 0; }
    ((${#states[@]} == 1)) || { echo 'Multiple pending deployments. Re-run the original panel command to resume its selected node.' >&2; exit 1; }
    [[ -f /usr/local/lib/v2box/panel-deploy.py ]] || { echo 'Download the panel installation command and run it with --resume.' >&2; exit 1; }
    [[ ! -L /etc/v2box/.deployment.lock && ! -L /etc/v2box && ! -L /etc/v2box/.bootstrap.lock ]] || { echo 'Refusing symlinked deployment paths' >&2; exit 1; }
    exec 9>/etc/v2box/.bootstrap.lock
    flock -n 9 || { echo 'Another deployment or maintenance operation is running.' >&2; exit 1; }
    check_runtime
    timeout -k 2 10 python3 /usr/local/lib/v2box/node-network.py capabilities | jq -e '.features | index("report-multi-target-v1") != null' >/dev/null || { echo 'Reporting helper lacks multi-target support; update node scripts before resuming.' >&2; exit 1; }
    if grep -q '^def launch_managed(' /usr/local/lib/v2box/panel-deploy.py; then
      exec python3 /usr/local/lib/v2box/panel-deploy.py --managed "${states[0]}"
    fi
    echo 'Legacy deployment helper runs in foreground. Use the latest panel bootstrap with --resume for managed recovery.'
    exec python3 /usr/local/lib/v2box/panel-deploy.py "${states[0]}";;
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
    [[ $EUID -eq 0 && $(uname -s) == Linux ]] || { echo 'Run as root on Linux' >&2; exit 1; }
    [[ ! -L /etc/v2box/.deployment.lock && ! -L /etc/v2box && ! -L /etc/v2box/.bootstrap.lock && ! -L /etc/v2box/.maintenance.lock && ! -L /usr/local/lib/v2box ]] || { echo 'Refusing symlinked installation paths' >&2; exit 1; }
    install -d -m 755 /etc/v2box
    exec 9>/etc/v2box/.bootstrap.lock
    flock -n 9 || { echo 'A deployment is running; retry after it finishes.' >&2; exit 1; }
    exec 7>/etc/v2box/.deployment.lock
    flock -n 7 || { echo 'Deployment is actively running; stop its terminal command before uninstalling.' >&2; exit 1; }
    exec 8>/etc/v2box/.maintenance.lock
    flock -n 8 || { echo 'Another maintenance operation is running.' >&2; exit 1; }
    echo 'This removes all V2Box nodes on this server. Configuration, certificates and DNS records are retained.'
    if $stop_network; then echo 'The shared node-network IP reporting/DDNS schedule will also be stopped.'; fi
    if ! $confirmed; then
      if ! { read -r -p 'Type uninstall to continue: ' answer < /dev/tty; } 2>/dev/null; then
        echo 'Confirmation could not be read. Run interactively, or explicitly use v2box uninstall --yes. Nothing was removed.' >&2; exit 1
      fi
      [[ "$answer" == uninstall ]] || { echo 'Uninstall cancelled.'; exit 1; }
    fi
    if $stop_network; then
      if [[ -s /usr/local/lib/node-network/node-network.py ]]; then
        run_service python3 /usr/local/lib/node-network/node-network.py disable
      elif command -v systemctl >/dev/null; then
        echo 'IP reporting helper is missing; stopping its systemd schedule directly. Reporting settings and DNS records are retained.'
        if [[ -f /etc/systemd/system/node-network.timer ]]; then run_service systemctl disable --now node-network.timer; fi
        if [[ -f /etc/systemd/system/node-network.service ]]; then run_service systemctl stop node-network.service; fi
      elif [[ -f /etc/node-network/cloudflare.json ]]; then
        echo 'IP reporting helper is missing. Restore node-network before uninstalling with --stop-network; nothing was removed.' >&2; exit 1
      fi
    fi
    if command -v systemctl >/dev/null; then
      active=false
      if run_service systemctl is-active --quiet v2box; then active=true;
      else state=$?; [[ $state == 3 || $state == 4 ]] || { echo 'Cannot verify service state; removal stopped before deleting programs.' >&2; exit "$state"; }; fi
      if [[ -f /etc/systemd/system/v2box.service ]] || $active; then
        run_service systemctl disable --now v2box
        if run_service systemctl is-active --quiet v2box; then echo 'Service is still running; removal stopped.' >&2; exit 1;
        else state=$?; [[ $state == 3 || $state == 4 ]] || { echo 'Cannot verify the service stopped; program files retained.' >&2; exit "$state"; }; fi
        rm -f -- /etc/systemd/system/v2box.service
        run_service systemctl daemon-reload
      fi
    elif [[ -f /etc/init.d/v2box ]]; then
      if run_service rc-service v2box status >/dev/null 2>&1; then run_service rc-service v2box stop;
      else state=$?; [[ $state == 3 ]] || exit "$state"; fi
      if run_service rc-update show default | grep -q 'v2box'; then run_service rc-update del v2box default; fi
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
    timeout -k 2 10 "$binary" capabilities | jq -e '.features | index("shared-listener-v1") != null' >/dev/null || { echo 'Upgrade V2Box and scripts before configuring shared listeners.' >&2; exit 1; }
    check_runtime shared-listener-v1
    [[ ! -L /etc/v2box && ! -L /etc/v2box/.bootstrap.lock && ! -L /etc/v2box/.maintenance.lock ]] || exit 1
    exec 9>/etc/v2box/.bootstrap.lock
    flock -n 9 || { echo 'A deployment is running; retry later.' >&2; exit 1; }
    exec 8>/etc/v2box/.maintenance.lock
    flock -n 8 || exit 1
    python3 /usr/local/lib/v2box/node-credentials.py --shared-listener "$@"
    echo 'Initial shared-listener migration can replace the selected listeners. File watching may apply this immediately; otherwise run v2box reload. A stopped service stays stopped.';;
  reload)
    check_runtime
    if command -v systemctl >/dev/null; then
      run_service systemctl kill --kill-whom=main --signal=HUP v2box
    else
      [[ -f /run/v2box.pid && ! -L /run/v2box.pid ]] || { echo 'V2Box is not running' >&2; exit 1; }
      read -r pid < /run/v2box.pid
      [[ "$pid" =~ ^[1-9][0-9]*$ ]] || exit 1
      kill -HUP "$pid"
    fi;;
  status)
    if command -v systemctl >/dev/null; then
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
    if command -v systemctl >/dev/null; then
      run_service systemctl "$action" v2box
    else
      case "$action" in
        enable) run_service rc-update add v2box default;;
        disable) run_service rc-update del v2box default;;
        *) run_service rc-service v2box "$action";;
      esac
    fi;;
  deploy-status)
    command -v systemctl >/dev/null || { echo 'Managed deployment requires systemd. Check the foreground deployment output.'; exit 1; }
    SYSTEMD_PAGER=cat timeout -k 2 10 systemctl status v2box-deployment --no-pager -l;;
  log|deploy-log)
    unit=v2box
    [[ "$1" != deploy-log ]] || unit=v2box-deployment
    if command -v journalctl >/dev/null; then
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
  ip) exec python3 /usr/local/lib/v2box/node-network.py ip;;
  credential) exec python3 /usr/local/lib/v2box/node-credentials.py;;
  ddns) shift; exec python3 /usr/local/lib/v2box/node-network.py "${@:-status}";;
  version|server|capabilities) exec "$binary" "$@";;
  help) echo 'Usage: v2box {start|stop|restart|reload|status|enable|disable|log|version|server|ip|credential|share-listener GROUP NODE_NUMBER...|ddns|resume|deploy-status|deploy-log|update [release-tag]|uninstall [--stop-network] [--yes]|help}';;
  *) echo 'Unknown command. Run: v2box help'; exit 1;;
esac
