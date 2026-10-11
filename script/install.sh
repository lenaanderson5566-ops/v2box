#!/usr/bin/env bash
set -euo pipefail
repo='lenaanderson5566-ops/v2box'
version=''
api_host=''
node_id=''
api_key=''
node_type='v2box'
landing_page=false
landing_dir=''
while (($#)); do
  case "$1" in
    --api-host|--node-id|--api-key|--node-type|--landing-dir)
      (($# >= 2)) || { echo "Missing value for $1" >&2; exit 1; }
      case "$1" in
        --api-host) api_host=$2;;
        --node-id) node_id=$2;;
        --api-key) api_key=$2;;
        --node-type) node_type=$2;;
        --landing-dir) landing_dir=$2; landing_page=true;;
      esac
      shift 2;;
    --landing-page) landing_page=true; shift;;
    -h|--help) echo "Usage: $0 [release-tag] [--api-host URL --node-id ID --api-key KEY] [--node-type v2node|v2box] [--landing-page | --landing-dir /path/to/site]"; exit 0;;
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
[[ $EUID -eq 0 ]] || { echo 'Run as root' >&2; exit 1; }
[[ $(uname -s) == Linux ]] || { echo 'This installer requires Linux' >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) arch=amd64;;
  aarch64|arm64) arch=arm64;;
  *) echo 'Supported architectures: amd64, arm64' >&2; exit 1;;
esac
for tool in curl unzip sha256sum jq install; do
  command -v "$tool" >/dev/null || { echo "Install required command: $tool" >&2; exit 1; }
done
if command -v systemctl >/dev/null; then init=systemd
elif command -v rc-service >/dev/null; then init=openrc
else echo 'systemd or OpenRC is required' >&2; exit 1; fi
if [[ -n "$api_host$node_id$api_key" ]]; then
  [[ "$api_host" =~ ^https?:// && "$node_id" =~ ^[1-9][0-9]*$ && -n "$api_key" ]] || { echo 'Supply all three valid panel arguments' >&2; exit 1; }
fi
if [[ -z "$version" ]]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | jq -er .tag_name)
fi
[[ "$version" =~ ^[a-zA-Z0-9._-]+$ ]] || { echo 'Invalid release tag' >&2; exit 1; }
stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT
archive="v2box-linux-${arch}.zip"
base="https://github.com/$repo/releases/download/$version"
curl -fSL "$base/$archive" -o "$stage/$archive"
curl -fSL "$base/$archive.sha256" -o "$stage/$archive.sha256"
(cd "$stage" && sha256sum -c "$archive.sha256" && unzip -q "$archive")
chmod +x "$stage/v2box"
"$stage/v2box" version
install -d -m 755 /usr/local/lib/v2box /etc/v2box
if [[ -f /usr/local/lib/v2box/v2box ]]; then
  cp -p /usr/local/lib/v2box/v2box /usr/local/lib/v2box/v2box.previous
fi
install -m 755 "$stage/v2box" /usr/local/lib/v2box/v2box.new
mv -f /usr/local/lib/v2box/v2box.new /usr/local/lib/v2box/v2box
install -m 755 "$stage/v2box.sh" /usr/local/bin/v2box
for file in LICENSE LICENSE.sing-box NOTICE.md README.md; do
  install -m 644 "$stage/$file" "/usr/local/lib/v2box/$file"
done
if [[ ! -f /etc/v2box/config.json ]]; then
  if [[ -n "$api_host" ]]; then
    umask 077
    jq -n --arg host "$api_host" --argjson id "$node_id" --arg key "$api_key" --arg type "$node_type" --argjson landing "$landing_page" --arg directory "$landing_dir" \
      '{Log:{Level:"info"},Nodes:[{NodeType:$type,ApiHost:$host,NodeID:$id,ApiKey:$key,Timeout:15,RetryCount:1,LandingPage:{Enabled:$landing,Directory:$directory}}]}' > /etc/v2box/config.json
  else
    install -m 600 "$stage/config.example.json" /etc/v2box/config.json
  fi
elif [[ "$landing_page" == true ]]; then
  echo 'Existing config preserved. Set Nodes[].LandingPage.Enabled and Directory manually to enable the page.'
fi
if [[ "$init" == systemd ]]; then
  cat > /etc/systemd/system/v2box.service <<'UNIT'
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
  systemctl daemon-reload
  systemctl enable v2box
else
  cat > /etc/init.d/v2box <<'UNIT'
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
  chmod +x /etc/init.d/v2box
  rc-update add v2box default
fi
if jq -e '.Nodes | length > 0 and all(.[]; .ApiKey != "REPLACE_ME")' /etc/v2box/config.json >/dev/null; then
  v2box restart
else
  echo 'Edit /etc/v2box/config.json, then run: v2box start'
fi
echo "Installed v2box $version. Existing v2node services are separate; avoid listening on the same port."
