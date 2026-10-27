#!/usr/bin/env bash
set -euo pipefail
# v2box-script-overlay-v1
run_service() {
  local result=0
  timeout -k 5 90 "$@" 7>&- 8>&- 9>&- || result=$?
  if ((result == 124 || result == 137)); then
    echo 'Service operation timed out. Check v2box status and journalctl -u v2box; configuration and certificates were retained.' >&2
  fi
  return "$result"
}
repo='lenaanderson5566-ops/v2box'
version=''
api_host=''
node_id=''
api_key=''
prompt_api_key=false
node_type='v2box'
landing_page=false
landing_dir=''
ddns=false
ddns_domain=''
ddns_zone=''
ddns_token_file=''
ddns_family='ipv4'
update_only=false
while (($#)); do
  case "$1" in
    --api-host|--node-id|--api-key|--node-type|--landing-dir|--ddns-domain|--ddns-zone-id|--ddns-token-file|--ddns-family)
      (($# >= 2)) || { echo "Missing value for $1" >&2; exit 1; }
      case "$1" in
        --api-host) api_host=$2;;
        --node-id) node_id=$2;;
        --api-key) api_key=$2;;
        --node-type) node_type=$2;;
        --landing-dir) landing_dir=$2; landing_page=true;;
        --ddns-domain) ddns_domain=$2; ddns=true;;
        --ddns-zone-id) ddns_zone=$2;;
        --ddns-token-file) ddns_token_file=$2;;
        --ddns-family) ddns_family=$2;;
      esac
      shift 2;;
    --ddns) ddns=true; shift;;
    --update) update_only=true; shift;;
    --prompt-api-key) prompt_api_key=true; shift;;
    --landing-page) landing_page=true; shift;;
    -h|--help) echo "Usage: $0 [release-tag] [--update] [--api-host URL --node-id ID --prompt-api-key] [--node-type v2node|v2box] [--landing-page | --landing-dir /path/to/site] [--ddns --ddns-domain HOST --ddns-zone-id ZONE --ddns-token-file FILE --ddns-family ipv4|ipv6|dual]"; exit 0;;
    --*) echo "Unknown option: $1" >&2; exit 1;;
    *) [[ -z "$version" ]] || { echo 'Only one version is allowed' >&2; exit 1; }; version=$1; shift;;
  esac
done
[[ "$node_type" == v2node || "$node_type" == v2box ]] || { echo "Invalid node type" >&2; exit 1; }
if [[ -n "$landing_dir" ]]; then
  [[ "$landing_dir" == /* && -f "$landing_dir/index.html" && -r "$landing_dir/index.html" ]] || { echo 'Landing directory must be absolute and contain a readable index.html' >&2; exit 1; }
fi
if [[ "$landing_page" == true && -z "$api_host" ]]; then
  echo 'Landing page flags require panel arguments when creating a config' >&2; exit 1
fi
if [[ "$ddns" == true ]]; then
  command -v python3 >/dev/null || { echo 'Install python3 to configure Cloudflare DDNS' >&2; exit 1; }
  [[ "$ddns_family" == ipv4 || "$ddns_family" == ipv6 || "$ddns_family" == dual ]] || { echo 'Invalid DDNS address family' >&2; exit 1; }
fi
[[ $EUID -eq 0 ]] || { echo 'Run as root' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'This installer requires Linux' >&2; exit 1; }
[[ ! -L /etc/v2box/.deployment.lock && ! -L /etc/v2box && ! -L /etc/v2box/config.json && ! -L /etc/v2box/.maintenance.lock && ! -L /etc/v2box/.bootstrap.lock && ! -L /usr/local/lib/v2box ]] || { echo 'Refusing symlinked installation paths' >&2; exit 1; }
if $update_only; then
  [[ -s /etc/v2box/config.json && -x /usr/local/lib/v2box/v2box ]] || { echo 'V2Box is not installed; use the panel deployment command.' >&2; exit 1; }
  [[ -z "$api_host$node_id$api_key" && "$ddns" == false && "$landing_page" == false ]] || { echo 'Update preserves existing settings; do not supply deployment options.' >&2; exit 1; }
fi
pending_deployment=false
if [[ -e /etc/v2box/.deployment-pending ]] || compgen -G '/etc/v2box/deployment-state*.json' >/dev/null; then
  pending_deployment=true
  [[ -z "$api_host$node_id$api_key" && "$ddns" == false && "$landing_page" == false && -z "$landing_dir" && "$prompt_api_key" == false ]] || { echo 'Pending deployment: update program files without new deployment options, then resume.' >&2; exit 1; }
fi
for tool in curl unzip sha256sum jq install flock timeout; do
  command -v "$tool" >/dev/null || { echo "Install required command: $tool" >&2; exit 1; }
done
install -d -m 755 /etc/v2box
# The root-owned panel bootstrap holds its lock while invoking this installer.
# Keep that descriptor out of child services, including older release installers.
if [[ "${V2BOX_BOOTSTRAP_LOCK_HELD:-}" != 1 ]]; then
  exec 9>/etc/v2box/.bootstrap.lock
  flock -n 9 || { echo 'Another deployment is running.' >&2; exit 1; }
fi
unset V2BOX_BOOTSTRAP_LOCK_HELD
exec 7>/etc/v2box/.deployment.lock
flock -n 7 || { echo 'Deployment is actively running. Wait for it to finish or stop its terminal command before updating.' >&2; exit 1; }
exec 8>/etc/v2box/.maintenance.lock
flock -n 8 || { echo 'Another maintenance operation is running.' >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64;;
  aarch64|arm64) arch=arm64;;
  *) echo 'Supported architectures: amd64, arm64' >&2; exit 1;;
esac
if command -v systemctl >/dev/null; then init=systemd
elif command -v rc-service >/dev/null; then init=openrc
else echo 'systemd or OpenRC is required' >&2; exit 1; fi
if [[ "$prompt_api_key" == true && ! -f /etc/v2box/config.json ]]; then
  [[ -z "$api_key" ]] || { echo 'Choose either interactive key input or --api-key' >&2; exit 1; }
  read -r -s -p 'Node API key: ' api_key < /dev/tty
  echo
fi
if [[ -n "$api_host$node_id$api_key" && ! -f /etc/v2box/config.json ]]; then
  [[ "$api_host" =~ ^https://[^/?#[:space:]@]+(/[^?#[:space:]]*)?$ && "$node_id" =~ ^[1-9][0-9]*$ && -n "$api_key" ]] || { echo 'Supply an HTTPS panel URL without credentials/query/fragment, a node ID and an API key' >&2; exit 1; }
fi
if [[ -f /etc/v2box/config.json ]]; then
  jq -e '.Nodes | type == "array" and length > 0 and all(.[]; (.ApiHost | type == "string") and (.ApiHost | test("^https://[^/?#\\s@]+(/[^?#\\s]*)?$")))' /etc/v2box/config.json >/dev/null || {
    echo 'Existing config requires HTTPS ApiHost for every node. Correct it before upgrading; the installed binary and service have not been changed.' >&2
    exit 1
  }
fi
if [[ -z "$version" ]]; then
  version=$(curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 30 "https://api.github.com/repos/$repo/releases/latest" | jq -er .tag_name)
fi
[[ "$version" =~ ^[a-zA-Z0-9._-]+$ ]] || { echo 'Invalid release tag' >&2; exit 1; }
was_running=false
had_binary=false
[[ ! -f /usr/local/lib/v2box/v2box ]] || had_binary=true
if [[ "$init" == systemd ]]; then
  if run_service systemctl is-active --quiet v2box; then was_running=true;
  else state=$?; [[ $state == 3 || $state == 4 ]] || { echo 'Cannot verify existing service state; programs were not changed.' >&2; exit "$state"; }; fi
else
  if run_service rc-service v2box status >/dev/null 2>&1; then was_running=true;
  else state=$?; [[ $state == 3 ]] || { echo 'Cannot verify existing service state; programs were not changed.' >&2; exit "$state"; }; fi
fi
stage=$(mktemp -d)
rollback=false
program_files=(v2box v2box.previous manager.sh install.sh node-credentials.py node-network.py)
cleanup() {
  result=$?
  trap - EXIT
  trap '' INT TERM
  rm -f -- /etc/v2box/config.json.new /etc/systemd/system/v2box.service.new /etc/init.d/v2box.new || true
  if ((result != 0)) && $rollback; then
    echo 'Update failed; restoring the previous program files. Configuration and certificates were preserved.' >&2
    restore_failed=false
    for file in "${program_files[@]}"; do
      if [[ -f "$stage/previous/$file" ]]; then
        if ! cp -p -- "$stage/previous/$file" "/usr/local/lib/v2box/$file.restore" || ! mv -f -- "/usr/local/lib/v2box/$file.restore" "/usr/local/lib/v2box/$file"; then
          echo "Could not restore program file: $file" >&2; restore_failed=true
        fi
      else rm -f -- "/usr/local/lib/v2box/$file" || restore_failed=true; fi
    done
    if [[ -f "$stage/previous/wrapper" ]]; then
      if ! cp -p -- "$stage/previous/wrapper" /usr/local/bin/v2box.restore || ! mv -f -- /usr/local/bin/v2box.restore /usr/local/bin/v2box; then restore_failed=true; fi
    else rm -f -- /usr/local/bin/v2box || restore_failed=true; fi
    if $restore_failed; then
      echo "Rollback incomplete. Backup retained at $stage" >&2
      echo 'Fix disk space or file permissions and restore the retained previous programs before restarting. Configuration and certificates were not removed.' >&2
      exit "$result"
    fi
    if $was_running; then
      if [[ "$init" == systemd ]]; then run_service systemctl restart v2box || echo 'Previous service could not restart; inspect v2box log.' >&2
      else run_service rc-service v2box restart || echo 'Previous service could not restart; inspect v2box log.' >&2; fi
    fi
  fi
  if ((result != 0)) && ! $rollback; then
    echo 'Installation did not complete. Resolve the error above, then rerun the panel installation command; do not remove configuration or certificates.' >&2
  fi
  rm -rf -- "$stage"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
archive="v2box-linux-${arch}.zip"
base="https://github.com/$repo/releases/download/$version"
curl -fSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 180 "$base/$archive" -o "$stage/$archive"
curl -fSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 30 "$base/$archive.sha256" -o "$stage/$archive.sha256"
(cd "$stage" && sha256sum -c "$archive.sha256" && unzip -q "$archive")
for file in v2box v2box.sh install.sh node-network.py node-credentials.py config.example.json LICENSE LICENSE.sing-box NOTICE.md README.md; do
  [[ -s "$stage/$file" && ! -L "$stage/$file" ]] || { echo "Release is missing required file: $file" >&2; exit 1; }
done
# Panel-shipped fixes also apply when installing an older immutable release.
# New releases carrying these capabilities keep their own bundled scripts.
if ! grep -q '^# v2box-manager-v2$' "$stage/v2box.sh" && [[ -f /usr/local/lib/v2box/panel-manager.sh ]]; then
  [[ ! -L /usr/local/lib/v2box/panel-manager.sh ]] || { echo 'Unsafe panel manager overlay' >&2; exit 1; }
  grep -qx '# v2box-manager-v2' /usr/local/lib/v2box/panel-manager.sh || exit 1
  cp -- /usr/local/lib/v2box/panel-manager.sh "$stage/v2box.sh"
fi
if ! grep -q '^# v2box-script-overlay-v1$' "$stage/install.sh" && [[ -f /usr/local/lib/v2box/panel-install.sh ]]; then
  [[ ! -L /usr/local/lib/v2box/panel-install.sh ]] || { echo 'Unsafe panel installer overlay' >&2; exit 1; }
  grep -qx '# v2box-script-overlay-v1' /usr/local/lib/v2box/panel-install.sh || exit 1
  cp -- /usr/local/lib/v2box/panel-install.sh "$stage/install.sh"
fi
for script in v2box.sh install.sh; do
  bash -n "$stage/$script" || { echo "Release contains an invalid shell script: $script; installed programs were not changed." >&2; exit 1; }
done
if command -v python3 >/dev/null; then
  python3 -c 'import ast,sys; [ast.parse(open(path, encoding="utf-8").read(), filename=path) for path in sys.argv[1:]]' "$stage/node-network.py" "$stage/node-credentials.py" || { echo 'Release contains an invalid Python helper; installed programs were not changed.' >&2; exit 1; }
fi
chmod +x "$stage/v2box"
timeout -k 5 15 "$stage/v2box" version || { echo 'Release binary did not pass the version check; installed programs were not changed.' >&2; exit 1; }
install -d -m 755 /usr/local/lib/v2box /etc/v2box
if $had_binary; then
  mkdir "$stage/previous"
  for file in "${program_files[@]}"; do
    [[ ! -f "/usr/local/lib/v2box/$file" ]] || cp -p -- "/usr/local/lib/v2box/$file" "$stage/previous/$file"
  done
  [[ ! -f /usr/local/bin/v2box ]] || cp -p -- /usr/local/bin/v2box "$stage/previous/wrapper"
  rollback=true
fi
if [[ -f /usr/local/lib/v2box/v2box ]]; then
  cp -p /usr/local/lib/v2box/v2box /usr/local/lib/v2box/v2box.previous
fi
install -m 755 "$stage/v2box" /usr/local/lib/v2box/v2box.new
mv -f /usr/local/lib/v2box/v2box.new /usr/local/lib/v2box/v2box
install -m 755 "$stage/v2box.sh" /usr/local/lib/v2box/manager.sh.new
mv -f -- /usr/local/lib/v2box/manager.sh.new /usr/local/lib/v2box/manager.sh
install -m 755 "$stage/v2box.sh" /usr/local/bin/v2box.new
mv -f -- /usr/local/bin/v2box.new /usr/local/bin/v2box
install -m 755 "$stage/install.sh" /usr/local/lib/v2box/install.sh.new
mv -f -- /usr/local/lib/v2box/install.sh.new /usr/local/lib/v2box/install.sh
if [[ -f "$stage/node-credentials.py" ]]; then
  install -m 755 "$stage/node-credentials.py" /usr/local/lib/v2box/node-credentials.py
fi
if [[ -f "$stage/node-network.py" ]]; then
  install -m 755 "$stage/node-network.py" /usr/local/lib/v2box/node-network.py
fi
for file in LICENSE LICENSE.sing-box NOTICE.md README.md; do
  install -m 644 "$stage/$file" "/usr/local/lib/v2box/$file"
done
if [[ ! -f /etc/v2box/config.json ]]; then
  if [[ -n "$api_host" ]]; then
    umask 077
    printf '%s' "$api_key" > "$stage/api-key"
    jq -n --arg host "$api_host" --argjson id "$node_id" --rawfile key "$stage/api-key" --arg type "$node_type" --argjson landing "$landing_page" --arg directory "$landing_dir" \
      '{Log:{Level:"info"},Nodes:[{NodeType:$type,ApiHost:$host,NodeID:$id,ApiKey:$key,Timeout:15,RetryCount:1,LandingPage:{Enabled:$landing,Directory:$directory}}]}' > "$stage/config.json"
    install -m 600 "$stage/config.json" /etc/v2box/config.json.new
    mv -f -- /etc/v2box/config.json.new /etc/v2box/config.json
  else
    install -m 600 "$stage/config.example.json" /etc/v2box/config.json.new
    mv -f -- /etc/v2box/config.json.new /etc/v2box/config.json
  fi
elif [[ "$landing_page" == true ]]; then
  echo 'Existing config preserved. Set Nodes[].LandingPage.Enabled and Directory manually to enable the page.'
fi
if [[ "$init" == systemd && ! -f /etc/systemd/system/v2box.service ]]; then
  cat > "$stage/v2box.service" <<'UNIT'
[Unit]
Description=V2box sing-box panel backend
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=/usr/local/lib/v2box
ExecStart=/usr/local/lib/v2box/v2box server --config /etc/v2box/config.json
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT
  install -m 644 "$stage/v2box.service" /etc/systemd/system/v2box.service.new
  mv -f -- /etc/systemd/system/v2box.service.new /etc/systemd/system/v2box.service
  run_service systemctl daemon-reload
  run_service systemctl enable v2box
elif [[ "$init" == openrc && ! -f /etc/init.d/v2box ]]; then
  cat > "$stage/v2box.openrc" <<'UNIT'
#!/sbin/openrc-run
name="v2box"
command="/usr/local/lib/v2box/v2box"
command_args="server --config /etc/v2box/config.json"
command_background="yes"
pidfile="/run/v2box.pid"
output_log="/var/log/v2box.log"
error_log="/var/log/v2box.log"
depend() { need net; }
UNIT
  install -m 755 "$stage/v2box.openrc" /etc/init.d/v2box.new
  mv -f -- /etc/init.d/v2box.new /etc/init.d/v2box
  run_service rc-update add v2box default
fi
if [[ "$ddns" == true ]]; then
  ddns_args=(configure --family "$ddns_family")
  [[ -z "$ddns_domain" ]] || ddns_args+=(--domain "$ddns_domain")
  [[ -z "$ddns_zone" ]] || ddns_args+=(--zone-id "$ddns_zone")
  [[ -z "$ddns_token_file" ]] || ddns_args+=(--token-file "$ddns_token_file")
  python3 /usr/local/lib/v2box/node-network.py "${ddns_args[@]}"
fi
if jq -e '.Nodes | length > 0 and all(.[]; .ApiKey != "REPLACE_ME")' /etc/v2box/config.json >/dev/null; then
  if $was_running || { ! $had_binary && ! $pending_deployment; }; then
    /usr/local/bin/v2box restart 7>&- 8>&- 9>&-
    healthy=0
    for attempt in 1 2 3 4 5; do
      sleep 2
      if [[ "$init" == systemd ]]; then check=(systemctl is-active --quiet v2box); else check=(rc-service v2box status); fi
      if run_service "${check[@]}"; then
        healthy=$((healthy+1)); ((healthy < 3)) || break
      else
        state=$?; [[ $state == 3 ]] || exit "$state"
        healthy=0
      fi
    done
    ((healthy >= 3)) || { echo 'Updated service did not remain active; inspect v2box log. Rolling back.' >&2; exit 1; }
  else echo 'Updated without starting the previously stopped service. Run: v2box start'; fi
else
  echo 'Edit /etc/v2box/config.json, then run: v2box start'
fi
rollback=false
echo "Installed v2box $version. Existing v2node services are separate; avoid listening on the same port."

if $pending_deployment; then
  echo 'Program updated; pending deployment, configuration and certificates retained. Next: /usr/local/bin/v2box resume'
fi
echo 'Management command: /usr/local/bin/v2box. Run hash -r if your shell cached an older path.'
