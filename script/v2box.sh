#!/usr/bin/env bash
set -euo pipefail
binary=/usr/local/lib/v2box/v2box
if (($# == 0)); then set -- help; fi
case "$1" in
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
  version|server|help) exec "$binary" "$@";;
  *) echo 'Usage: v2box {start|stop|restart|status|enable|disable|log|version|server|ip|credential|ddns [report-configure|configure|run|status|disable]|help}'; exit 1;;
esac
