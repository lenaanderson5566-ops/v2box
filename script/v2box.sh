#!/usr/bin/env bash
set -euo pipefail
binary=/usr/local/lib/v2box/v2box
if (($# == 0)); then set -- help; fi
case "$1" in
  update)
    shift
    (($# <= 1)) || { echo 'Usage: v2box update [release-tag]' >&2; exit 1; }
    exec bash /usr/local/lib/v2box/install.sh --update "$@";;
  resume)
    [[ $EUID -eq 0 && $# -eq 1 ]] || { echo 'Run as root: v2box resume' >&2; exit 1; }
    [[ -x "$binary" ]] || { echo 'V2Box is uninstalled; reinstall program files before resuming. Configuration and certificates remain.' >&2; exit 1; }
    [[ -f /etc/v2box/deployment-state.json ]] || { echo 'No pending deployment.'; exit 0; }
    [[ -f /usr/local/lib/v2box/panel-deploy.py ]] || { echo 'Download the panel installation command and run it with --resume.' >&2; exit 1; }
    [[ ! -L /etc/v2box/.deployment.lock && ! -L /etc/v2box && ! -L /etc/v2box/.bootstrap.lock ]] || { echo 'Refusing symlinked deployment paths' >&2; exit 1; }
    exec 9>/etc/v2box/.bootstrap.lock
    flock -n 9 || { echo 'Another deployment or maintenance operation is running.' >&2; exit 1; }
    exec python3 /usr/local/lib/v2box/panel-deploy.py /etc/v2box/deployment-state.json;;
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
      read -r -p 'Type uninstall to continue: ' answer < /dev/tty
      [[ "$answer" == uninstall ]] || { echo 'Uninstall cancelled.'; exit 1; }
    fi
    if $stop_network && [[ -f /etc/node-network/cloudflare.json ]]; then
      python3 /usr/local/lib/node-network/node-network.py disable
    fi
    if command -v systemctl >/dev/null; then
      if [[ -f /etc/systemd/system/v2box.service ]] || systemctl is-active --quiet v2box; then
        systemctl disable --now v2box
        if systemctl is-active --quiet v2box; then echo 'Service is still running; removal stopped.' >&2; exit 1; fi
        rm -f -- /etc/systemd/system/v2box.service
        systemctl daemon-reload
      fi
    elif [[ -f /etc/init.d/v2box ]]; then
      if rc-service v2box status >/dev/null 2>&1; then rc-service v2box stop; fi
      if rc-update show default | grep -q 'v2box'; then rc-update del v2box default; fi
      if rc-service v2box status >/dev/null 2>&1; then echo 'Service is still running; removal stopped.' >&2; exit 1; fi
      rm -f -- /etc/init.d/v2box
    fi
    rm -f -- /usr/local/lib/v2box/v2box /usr/local/lib/v2box/v2box.previous /usr/local/lib/v2box/manager.sh /usr/local/lib/v2box/install.sh /usr/local/lib/v2box/node-credentials.py /usr/local/lib/v2box/node-network.py /usr/local/bin/v2box
    echo 'V2Box uninstalled. Configuration and certificates remain in /etc/v2box. Revoke node credentials and review domain bindings in the panel.';;
  start|stop|restart|status|enable|disable)
    action=$1
    if command -v systemctl >/dev/null; then
      systemctl "$action" v2box
    else
      case "$action" in
        enable) rc-update add v2box default;;
        disable) rc-update del v2box default;;
        *) rc-service v2box "$action";;
      esac
    fi;;
  log)
    if command -v journalctl >/dev/null; then journalctl -u v2box -n 100 -f
    else tail -n 100 -f /var/log/v2box.log; fi;;
  ip) exec python3 /usr/local/lib/v2box/node-network.py ip;;
  credential) exec python3 /usr/local/lib/v2box/node-credentials.py;;
  ddns) shift; exec python3 /usr/local/lib/v2box/node-network.py "${@:-status}";;
  version|server) exec "$binary" "$@";;
  help) echo 'Usage: v2box {start|stop|restart|status|enable|disable|log|version|server|ip|credential|ddns|resume|update [release-tag]|uninstall [--stop-network] [--yes]|help}';;
  *) echo 'Unknown command. Run: v2box help'; exit 1;;
esac
