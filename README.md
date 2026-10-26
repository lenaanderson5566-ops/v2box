# v2box

面向修改版 V2board 的纯 sing-box 节点后端。基于 **sing-box v1.14.3**，不再使用 Xray。为支持用户热更新，仓库内维护了公开的入站适配和认证并发补丁，来源及修改范围见 `internal/README.md`。

## 面板通讯 HTTPS

节点的 `ApiHost` 必须使用 HTTPS，不接受 URL 内嵌账号、查询参数或片段。面板 API 请求不跟随重定向，请直接填写最终 HTTPS 地址。TLS 使用系统信任链校验证书，不提供跳过校验或 HTTP 回退开关；如使用私有 CA，需先安装到节点系统信任库。

升级前先将每个节点的 `ApiHost` 改为已可用的 HTTPS 地址。安装脚本检查现有配置，发现 HTTP 地址会在替换程序与重启服务前退出。绕过安装脚本直接替换二进制时，HTTP 配置会被新程序拒绝。已运行的旧节点不会被本次后台更新主动断开。

Cloudflare 公网 HTTPS 地址可用于节点通讯，后台不以源站连接是否为 HTTP 判断节点是否使用 HTTPS。Cloudflare 到源站的 TLS 需另行配置。旧接口暂时仍保留查询参数凭据格式，本次不更换密钥或迁移认证。

## 面板兼容

V2Box 节点默认使用 `/api/v10/server/...`，包括 configuration、users、online-counts、traffic、online-users 和 capabilities。每个 `Nodes[]` 设置 `"NodeType": "v2box"`；`ApiVersion` 留空即使用 V10，也可显式填写 `"ApiVersion": "v10"`。凭据放在 Authorization Bearer 请求头，配置和用户列表支持 ETag。节点上报核心版本及能力，面板仅向声明支持原生路由的节点下发高级配置。

**升级顺序：先更新面板并运行数据库迁移，再更新节点。** 旧面板可显式设置 `"ApiVersion": "legacy"`，沿用 `/api/v2/server/config` 和 `/api/v1/server/UniProxy/{user,alivelist,push,alive}`，但无法使用新的原生配置功能；不会自动降级到旧接口。未填写 NodeType 的既有配置仍按 v2node 走旧接口，避免节点编号串用。新安装器及示例配置默认 v2box。V10 用户列表使用 JSON；旧接口保留 JSON / MessagePack。


节点协议：VMess、VLESS、Trojan、Shadowsocks AEAD / SS2022 AES、Hysteria2、TUIC、AnyTLS、Naive。VMess/VLESS/Trojan 支持 TCP、WebSocket、gRPC、HTTPUpgrade。支持 TLS 证书文件、面板远程证书、原有 ACME 申请/续期和 VLESS Reality。

Naive 首版使用 HTTPS/HTTP2（`network=tcp`、`tls=1`），支持多用户热更新、撤销现有及空闲 HTTP2 会话、计费和共用限速/设备限制。用户名和密码均为用户 UUID，内部认证代次不下发客户端。面板使用 HTTP/DNS ACME 或已有可信证书文件；不提供自签名、关闭校验、Reality、WS/gRPC 或 QUIC/HTTP3 选项。设备来源使用真实 TCP 对端，不信任 X-Forwarded-For。

Naive 订阅适配为 sing-box JSON 和通用 `naive+https://` 链接。客户端需包含 Naive 支持（sing-box 1.13+ 且满足其平台/libcronet 依赖）；不代表所有 Clash/Mihomo 客户端均可使用。服务端不依赖 Chromium/libcronet。当前未注册 Naive 出站；支持下述静态落地页，不提供 Caddy 的反向代理或应用前置功能。

Clash Verge Rev / Mihomo 等客户端仍通过面板订阅连接节点；请确保面板订阅输出与所选协议、传输、TLS 参数一致。不要将 sing-box JSON 直接作为 Clash YAML 导入。

## V10 原生路由和 DNS

在面板 V2Box 编辑页的“sing-box 路由与 DNS”中填写原生 JSON（数据库字段 `sing_box`，V10 字段 `singBox`）。留空或 null 使用原来的路由；启用时**整体替代该节点的旧 route_id 路由**，不会混合两套规则。示例：

```json
{
  "dns": {
    "servers": [{"type": "https", "tag": "resolver", "server": "1.1.1.1"}],
    "final": "resolver"
  },
  "route": {
    "rule_set": [{"type": "inline", "tag": "blocked", "rules": [{"domain_suffix": ["blocked.example"]}]}],
    "rules": [
      {"action": "sniff"},
      {"rule_set": ["blocked"], "action": "reject"},
      {"domain_suffix": ["example.org"], "action": "route", "outbound": "egress"}
    ],
    "final": "direct"
  },
  "outbounds": [{"type": "socks", "tag": "egress", "server": "127.0.0.1", "server_port": 1080, "version": "5"}]
}
```

DNS 支持 local、UDP、TCP、TLS、HTTPS，以及原生 DNS 规则和解析策略。规则集支持 inline 或 HTTPS remote（JSON/source、SRS/binary）；不允许面板下发本机规则文件路径。出站支持 direct、SOCKS、HTTP、VMess、VLESS、Trojan、Shadowsocks、Hysteria2、TUIC、AnyTLS。`direct` 是保留的直连出口，不要重复定义；出站、DNS、规则集引用必须存在，具体字段按 sing-box 1.14.3 校验。

仅开放 dns、route、outbounds，不开放 inbounds、services、endpoints、实验控制接口或认证用户覆盖；用户、计费、限速仍由 V2Box 管理。高级出站凭据不会进入用户订阅。配置语法、字段或引用校验失败时，重载保留当前节点；涉及监听端口或网络资源的启动失败仍可能中断服务，建议先在测试节点验证。

## ACME 失败保护

本版证书申请联系邮箱为 `fastdog.ltd@gmail.com`，独立于面板站点和发信邮箱。新 ACME 账户使用该联系邮箱；现有证书和旧账户文件不会被删除，也不会仅为了更换邮箱强制重签证书。节点需要升级到包含本次修改的正式版本后生效。

首次签发或续签失败后，会在证书所在目录记录 `.acme-<域名摘要>.json`，同目录同域名共享保护状态；重启、重载、修改 DNS 凭据不会自动清除它。

- 验证失败、DNS 提供商配置错误及无法分类的错误：立即暂停，修复后人工恢复。
- 已识别的网络故障或 ACME 5xx：首次失败至少等 1 小时，第二次至少等 6 小时，连续第 3 次失败后暂停。定时任务仅在允许时间之后尝试，不保证准点执行。
- CA 限流：暂停并保存 Retry-After；缺少有效值时至少等待 24 小时。不自动创建新订单。
- 进程异常退出留下 `.lock` 或未完成状态时：停止新申请，先确认没有其他签发进程。

恢复步骤：先修复域名解析、80 端口或 DNS 凭据；若有限流，等待日志中的 not-before 时间。停止相关 V2Box 进程，删除日志明确指出的**该域名状态文件**（确认无签发进程后才可删除遗留 `.lock`），再启动。不要删除 ACME 账户或现有证书来反复试申请。续签失败保留现有证书；首次签发失败不启动缺少有效证书的节点。不同目录、不同主机或其他 ACME 客户端不共享此保护。

测试域名验证流程应使用 [Let’s Encrypt 测试环境](https://letsencrypt.org/docs/staging-environment/)的专门工具；本项目当前仍使用生产 CA，没有新增测试环境切换参数。[CA 限流说明](https://letsencrypt.org/docs/rate-limits/)。

## 可选静态落地页

仅 Naive 和 Hysteria2 支持，默认关闭。配置写在节点本机 `config.json` 的对应 `Nodes[]` 对象中，不由面板下发，也不需要数据库变更：

```json
"LandingPage": {"Enabled": true, "Directory": ""}
```

`Directory` 留空使用内置静态页；自定义时填写节点上的绝对目录，例如 `/var/www/node-site`，目录必须包含可读的 `index.html`。该目录只放准备公开的网页及资源文件，不要指向 `/etc/v2box` 或证书、配置目录。静态文件更新立即生效；启用开关或目录配置变化走现有配置重载。

- Naive 在原 HTTPS 端口提供 HTTP/1.1、HTTP/2 的 GET/HEAD 页面请求；CONNECT 仍需正确认证，空用户表也不会开放代理。浏览页面不计入用户流量。
- Hysteria2 在原 UDP 端口提供 HTTP/3 masquerade；不另开 TCP 网站端口。普通浏览器首次访问不一定使用 HTTP/3，可用支持 HTTP/3 的 curl 执行 `curl --http3-only https://域名:端口/` 验证。启用时必须关闭 `obfs`，否则普通 HTTP/3 客户端无法识别其混淆数据。
- 自定义目录不提供目录列表，不读取隐藏路径，也禁止通过路径穿越或符号链接读取目录外文件。其他协议误启用会明确报错。

首次安装且提供面板参数时，追加 `--landing-page` 使用内置页，或 `--landing-dir /var/www/node-site` 使用已有目录。安装器保留已有配置，升级时需自行编辑对应节点的开关。Docker 使用内置页无需新增挂载；自定义目录需额外以只读方式挂载，例如 `-v /var/www/node-site:/site:ro` 并设置 `Directory` 为 `/site`。页面只是普通访问响应，不代表能够消除协议特征。

## 迁移限制

- **用户增加不重建节点、不关闭其他用户的现有连接。删除、UUID 变化或同一 UUID 对应的用户 ID 变化，只撤销对应账号的连接和后续转发权限。** 单纯限速/设备限制变更也无需重建。节点端口、协议、TLS 等配置变化仍走配置重载，可能中断连接。
- 空用户列表清空认证表并关闭全部用户连接，保留监听器以接受后续用户热更新，不提供匿名访问；304 响应保留现有用户。账号删除后重新添加会获得新的内部认证代次，旧多路复用会话不能恢复访问权限。
- 用户流量计数独立于核心实例；用户删除和实例替换不清除待上报流量，配置重载时保留仍在配置中的节点计数。HTTP 上报失败恢复计数供下一轮重试。计数目前保存在内存中，进程崩溃无法恢复；面板接口没有幂等确认，超时后的重试存在重复计费可能。
- 用户限速覆盖 TCP/UDP 的应用数据，上下行共享用户限速桶。设备限制依据源 IP，结合面板在线计数和本地最近两分钟活动；不是硬件设备识别，也不是跨服务器强一致配额。
- XHTTP/SplitHTTP、TCP HTTP 伪装、PROXY protocol、trusted X-Forwarded-For、Xray VLESS 加密、Reality ML-DSA/xver、多 Reality server_names、reject_unknown_sni 当前明确拒绝，需先迁移配置。
- 支持域名/IP/端口/协议封禁及自定义出站路由。`route` / `route_ip` / `default_out` 的 action_value 必须改为包含 `type`、`tag` 的 **sing-box 出站 JSON**；不接受 Xray 出站 JSON。当前注册的出站为 direct、SOCKS、HTTP、VMess、VLESS、Trojan、Shadowsocks、Hysteria2、TUIC、AnyTLS。
- 旧 GeoIP/Geosite `.dat` 规则不能直接使用。DNS 路由目前接受 DNS 服务器 IP；不自动转换所有旧 DNS URL 格式。
- 旧 `Log.Access` 保留为配置兼容字段；访问日志使用 sing-box 日志系统。请先在测试节点验证面板计费、在线人数及订阅，再切换生产节点。

## 构建和测试

使用 go.mod 指定的 Go 工具链：

```sh
GOEXPERIMENT=jsonv2 go test -tags with_quic,with_utls ./...
GOEXPERIMENT=jsonv2 go build -tags with_quic,with_utls -trimpath -o v2box .
./v2box version
./v2box server --config ./config.json
```

复制 `config.example.json` 为 `config.json`，填写面板地址、节点 ID 和 API Key。QUIC 构建标签为 Hysteria2/TUIC 所必需。CI 运行协议连接、用户撤销、计数重试、限速及面板响应测试，并构建 Linux/macOS/Windows amd64/arm64 包。

Reality/Vision 已加入本地 TLS 握手及数据转发测试。当前上游 sing-vmess 的 Vision 实现使用不兼容 checkptr 的 TLS 指针访问，并存在读写共享状态的竞态报告，因此该项单独以普通模式测试；其他后端测试使用 `-race`。这不代表 Vision 已通过竞态验证，生产使用前应做连接压力测试并跟进上游修复。

可选资源循环测试覆盖 8 种协议的 144 次启动/关闭，以及 Naive 的 1280 次 CONNECT、40 个 TLS 会话，并在客户端缓存到期和 GC 后比较堆内存与协程。它使用本地回环网络和独立 HTTP2 测试客户端，不等于生产容量测试或 Chromium Naive 客户端互通验证：

```sh
GOEXPERIMENT=jsonv2 V2BOX_SOAK=1 go test -race -tags with_quic,with_utls ./... -count=1 -timeout=3m
GOEXPERIMENT=jsonv2 go test ./limiter -run '^$' -bench BenchmarkCheckLimit -benchmem
```

设置 `V2BOX_PROFILE_DIR` 为已存在的目录可保存每轮存活堆快照，再用 `go tool pprof` 比较。PowerShell 请用 `$env:变量名='值'` 设置上述环境变量。

## Linux 安装

安装器支持 amd64/arm64，systemd 或 OpenRC。需要 curl、unzip、jq、sha256sum。**先发布本仓库构建出的 Release，再运行安装器**；上游 v2node 二进制不可代替。

```sh
curl -fSL https://raw.githubusercontent.com/lenaanderson5566-ops/v2box/v1.0.1/script/install.sh -o install.sh
sudo bash install.sh v1.0.1 --api-host https://panel.example.com --node-id 1 --prompt-api-key
```

安装路径 `/usr/local/lib/v2box`；配置 `/etc/v2box/config.json`；服务 `v2box`。更新保留配置和上一份二进制 `v2box.previous`。检查日志使用 `v2box log`。升级到本版可重新运行 `sudo bash install.sh v1.0.1`，不需要再次传入面板地址或密钥。

### 更新与卸载

管理入口固定为 `/usr/local/bin/v2box`；核心程序 `/usr/local/lib/v2box/v2box` 也会将运维命令转交给固定路径的管理脚本，不依赖 PATH 搜索。升级后运行 `hash -r` 清除旧命令路径缓存。

包含本次运维功能的安装包会同时安装更新器。旧版先使用面板提供的升级命令升级到对应正式版，之后执行：

```sh
v2box update             # 更新至 GitHub 最新正式版
v2box update RELEASE_TAG # 指定已有正式版本
v2box resume             # 继续保留的未完成部署
v2box uninstall         # 交互确认后卸载，保留独立 IP 上报服务
v2box uninstall --stop-network # 同时停用本机共享的 IP 上报 / DDNS 调度
```

更新校验包摘要与必需文件，保留配置、证书、自定义 systemd/OpenRC 服务文件及原服务启用状态；运行中的节点更新后重启，原本停止的节点不会被自动启动。重启或短时服务存活检查失败时恢复旧程序并尝试恢复原服务，失败返回非零；存活检查不等于所有协议、面板通讯或实际客户端连接均通过。更新与卸载互斥；未完成部署允许更新程序，保留恢复状态，随后执行 `v2box resume`。部署进程仍在运行时应先结束该命令，再更新。独立 `node-network` 安装副本不由节点更新器覆盖，修复入口管理它的配置与版本。

卸载影响服务器上同一 V2Box 服务的所有节点，需要输入 `uninstall` 确认；自动化可以显式传入 `--yes`。移除程序和服务入口，保留 `/etc/v2box` 配置、证书、部署恢复状态及 `/etc/node-network` 配置，不调用 Cloudflare 删除解析，也不自动撤销面板凭据。`--stop-network` 会停用共享的独立上报服务，其他程序仍依赖该服务时不要添加此选项。卸载后在面板撤销凭据、解除不再使用的域名绑定；重新安装可复用保留的配置。

生命周期测试复用实际安装器与命令包装脚本，在临时目录内模拟 Release 下载及服务管理：`python3 -m unittest discover -s script -p '*_test.py'`。需要 Linux、bash、jq、unzip、flock 和 sha256sum；测试不会运行真实系统服务或调用证书服务。

旧 `v2node` 服务独立保留。迁移同一端口时先停止旧节点服务，再启动 v2box；回退时停止 v2box，启动旧服务。不要同时监听同一端口。

## Docker

```sh
docker build -t v2box .
docker run --rm --network host -v /etc/v2box:/etc/v2box:ro v2box
```

使用自动证书申请/续期时，证书目录需要可写挂载。

## 许可证

原 v2node 的 MPL-2.0 许可证保存在 LICENSE。嵌入 sing-box 的版权及 GPL-3.0-or-later 许可证保存在 LICENSE.sing-box；分发组合二进制时须同时提供相应源码。参见 NOTICE.md。

独立 V2Box 一键安装参数：`--node-type v2box`。安装器保留已有配置；已有机器需手动核对并更新 `NodeType` 和面板对应的 `NodeID`。面板和包含 NodeType 支持的 V2Box 二进制须一起升级。

V10 节点身份通过 `X-Node-Type: v2box` 和 `X-Node-ID: 节点编号` 请求头传递；URL 统一为 `/api/v10/server/...`。当前仅接受 v2box 类型，V2Node 继续使用旧接口。类型或编号缺失、非法时返回 422；未知节点返回 404。旧 `/api/v10/nodes/v2box/{id}/...` 路径已移除，面板与节点应同步升级。


### Public IP and optional Cloudflare DDNS

The Linux release includes `node-network.py` (Python 3.8+, standard library only).
`v2box ip` queries and validates public IPv4/IPv6 addresses using HTTPS lookup services.
For panel-managed DDNS, save the Cloudflare credential and bind the node under the
panel's **Cloudflare DDNS** page, then generate a scoped report key. Run:

```bash
v2box ddns report-configure --family ipv4
```

Enter the HTTPS panel origin and report key at the hidden prompt. The node reports public
addresses to `POST /api/v10/server/addresses` every five minutes; only the panel holds the
Cloudflare token. The report key authorizes one configured node and cannot use other panel
APIs. `--panel-url` and `--token-file` support unattended setup without putting the key in
command arguments. Choose `ipv6` or `dual` as needed. Missing families are not sent as empty
values. The panel refuses stale addresses and pauses/backoffs failed DNS operations.

Legacy direct mode remains available with `v2box ddns configure --domain node.example.com
--family ipv4` or installer `--ddns --ddns-domain node.example.com --ddns-family ipv4`.
That mode prompts for the zone-scoped Cloudflare DNS Edit token on the node. Switching to
`report-configure` replaces the local direct-mode credential after the panel accepts a report.

Both modes use Python 3.8+, standard library only, and systemd or OpenRC with crontab/crond.
Settings are mode 0600 under `/etc/node-network/`. HTTPS requests do not follow redirects.
Authentication/configuration errors pause reporting until reconfiguration; 429 responses
use bounded backoff. Existing DNS records are retained on disable or lookup failure.
Use `v2box ddns status`, `v2box ddns disable`, or the independent `node-network` command,
which also supports V2Node hosts. Ordinary reinstall preserves the configured mode.
Allow DNS propagation before issuing an ACME certificate.

Tests: `python3 -m unittest discover -s script -p '*_test.py'` (mock DNS APIs only).
AnyTLS panel presets can be integration-tested with
`V2BOX_PADDING_PRESETS=/path/to/v2board/resources/client/anytls-padding-presets.json`
and `go test -tags with_quic,with_utls ./core -run TestPanelAnyTLSPresets`.

### 面板断连与重载恢复

已经运行的节点在配置、用户或在线状态拉取失败时保留现有状态。重载先获取用户和在线状态并验证协议凭据，成功后才关闭旧监听；503 或无效用户配置不会先停止旧节点。重载被拒绝后继续按拉取周期重试，即使配置接口返回 304，也能在面板恢复后应用之前的变更。

旧接口在线状态请求的网络、服务端和解析错误会保留原有限制状态；404 仍兼容不提供此可选接口的旧面板。首次启动或节点进程重启仍需面板可访问。没有增加离线用户快照。新版面板支持批次去重后，V10 流量会上报前落盘并按原批次 ID 补报，见下节。

### V10 流量批次去重与补报

先升级 V2Board 并迁移数据库，再升级节点。配置接口声明 `trafficBatchDedup: true` 时启用批次 ID、上报前持久化和失败补报；V2Node/legacy 模式及旧面板继续使用原上报方式。

待确认批次默认保存在 `/var/lib/v2box/traffic`（目录 0700、文件 0600），可通过 Nodes 中的 `TrafficSpoolDirectory` 修改路径。服务运行用户需要写权限。面板地址和节点 ID 隔离，每个节点最多占用 64 MiB。明确确认后删除批次；每周期最多补报 16 批，重启、重载后继续补报。存在待确认批次时，面板降级为不支持去重的版本会暂停流量上报。

写入失败或达到容量上限时记录错误，并将尚未持久化的新增计数保留在内存。持久化批次可恢复；尚未到上报周期的计数或写入失败留在内存的计数，在进程崩溃后仍可能丢失。不要删除待确认目录，也不要把它放在临时文件系统。

### 服务器月度流量额度

面板“节点管理 → 服务器流量”可将同一实际服务器的多个父节点绑定到一份额度，支持自然月或指定结算日、上传/下载/双向统计，限额 0 表示不限。子节点沿用父节点。

新版 V2Box 根据 `/api/v10/server/traffic-budget` 执行本地准入和连接终止。同一进程的绑定节点共享计数，多个进程通过面板周期同步。提高或取消限额后下一次同步自动恢复，节点无需重装；新周期联网后自动恢复。面板断连时保留限制并继续计数，到期后等待面板确认新周期。

额度状态随拉取/上报周期保存到 `TrafficSpoolDirectory` 下的 `quota-state`，重载保留内存计数，重启读取已保存状态。未保存的计数可能在进程崩溃后丢失。多个进程、同步间隔及网络缓冲存在少量超额风险；代理字节不含服务器其他流量及全部协议开销，应给服务商账单预留余量。旧节点通过用户列表同步限制。


### 并发计量与验证

限速和设备状态按用户分成 32 组锁，同一用户的所有连接仍共用一个速度桶。流量和在线活动计数分成 64 组锁，报告逐组取走计数，失败时仍可恢复。共享服务器额度使用原子计数；周期、额度配置和认证表更新继续使用准入同步屏障，耗尽额度时取消现有连接。

可靠上报将落盘锁与发送锁分开：网络等待不会阻止另一个批次持久化，每个 Client 同时只有一个发送者，其他调用返回持久化结果，后续周期继续补报。64 MiB 容量、每周期最多 16 批、确认后删除和批次去重规则保持不变。节点拉取及上报在每次执行完成后，等待配置间隔再加 0–10% 的正向错峰延迟；证书更新任务保留原周期，所有任务仍串行执行。

复用现有测试文件执行验证，不需要另建检查脚本：

```bash
GOEXPERIMENT=jsonv2 go test -tags with_quic,with_utls ./...
GOEXPERIMENT=jsonv2 go test -race -tags with_quic,with_utls ./...
go test ./limiter ./core -run '^$' -bench 'Benchmark(CheckLimit|TrafficAccount)' -benchmem -benchtime=500ms -count=3
```

2026-10-14 本机微基准对比原始提交 b2c0cda，Windows amd64、Go 1.27.1、8 个逻辑处理器，1024 用户并行调用，三次结果的中位数：

| 场景 | 修改前 | 修改后 |
| --- | ---: | ---: |
| 限速检查 | 270.6 ns/次 | 63.3 ns/次 |
| 流量计量，无共享额度 | 249.5 ns/次 | 42.6 ns/次 |
| 流量计量，有共享额度 | 305.0 ns/次 | 83.8 ns/次 |

以上场景均为 0 B/次、0 次分配。单用户无共享额度计量约为 188 → 196 ns/次，有共享额度约为 228 → 222 ns/次；分片优化主要收益来自多用户并行。上述结果不是网络吞吐或线上承载上限；部署时仍应按实际协议、带宽、连接数及服务器规格验证。生产诊断接口不要公开到互联网。


本轮完整非 race 回归：128 项通过、1 项失败、4 项跳过；原始提交为 121 项通过、同一项失败、4 项跳过（统计包含子测试）。新增 7 项并发/调度回归均通过。完整 race 回归为 127 项通过、同一项失败、4 项跳过，未报告数据竞争；Vision 上游兼容测试按已有规则仅在非 race 模式执行。`go vet -tags with_quic,with_utls ./...` 通过。

未解决的既有测试：`node/TestRejectedReloadIsRetriedWithoutConfigChange` 使用 HTTP 测试地址，与已启用的 HTTPS 地址校验冲突；原始提交与修改后均在相同位置失败。此次没有放宽 HTTPS 或跳过该测试，完整 CI 不能标为全通过。4 项跳过为既有按条件启用的测试，实际原因保留在测试输出中。


现有 opt-in 压力回归在 race 模式通过：144 次跨 8 种协议的启动/关闭循环，以及 40 个 TLS 会话上的 1280 个 Naive CONNECT 流（约 50 MiB 回显）。goroutine 数未持续增长，堆内存增长符合现有回归阈值；此结果只覆盖有限循环，不证明长期负载无泄漏。

```bash
GOEXPERIMENT=jsonv2 V2BOX_SOAK=1 go test -race -tags with_quic,with_utls ./core -run 'Test(CoreLifecycleSoak|NaiveMultiplexChurnSoak)$' -count=1 -timeout=3m
```

## Optional destination statistics

Update both the panel and node binary, then enable the default-off switch in the panel log settings. Requires V10 API; older panels returning 404 leave collection disabled. Targets use the original domain or IP, without reverse DNS or replacing IPs with sniffed domains. No URL paths or payloads are collected. The panel log settings offer reporting every 30/60/120/300 seconds (default 60). Nodes advertise target-statistics-interval-v1; older nodes keep their fixed cadence. Policy sync runs in its own cancellable task at most every 60 seconds (plus up to 10% jitter), independently of the upload interval, with a 120-second lease; failed policy reads disable collection. Each node holds at most 500 active rows and 500 pending rows; TCP retains a single fixed target without allocating target maps. UDP caches at most 64 targets per connection, ignoring ports, with per-node limits of 8,192 cached addresses and 1,024 cache-holding sessions. Close and policy retirement release these maps. Per-node/user shards avoid a global packet-path lock. Failed uploads back off for 30, 60, then 120 seconds plus scheduling jitter, while policy/disable synchronization continues at most every 60 seconds plus jitter. Recovery resets the backoff. Batches retry with stable IDs for at most five minutes and are never spooled to disk. Compatible panels attach a policy lease to configuration responses (including 304); the node reuses only the remaining lease and falls back to the policy endpoint when needed. Statistics requests have a five-second deadline, a 256 KiB outgoing body limit and a 4 KiB response limit, and do not delay billing reports. The panel displays recent detailed aggregates and optional daily summaries under Logs → Node destinations; restarts, capacity limits and outages can lose this optional telemetry.


## Independent node credentials (manual rotation)

Upgrade the panel before migrating a node. Generate its scoped credential in the panel node's credentials dialog; the full value is displayed once. First issuance immediately blocks the shared token for that identity. Later manual rotations allow a 30-minute overlap. Confirm the new fingerprint's last authentication in the panel, then revoke the previous key. No scheduled rotation is implemented. Revoking API access does not terminate established proxy sessions.

Fresh installs can use `--prompt-api-key` for hidden terminal input. For an existing configuration, use `v2box credential` (Python 3 required), select the node, paste the independent key, then run `v2box restart`. The helper updates only that entry atomically with mode 0600, creates no credential backup and does not restart automatically. Independent keys also use Authorization in legacy API mode; older shared-token deployments retain their compatibility behavior until migrated. The helper is included in new release archives and is installed by the updated installer.


### 转发与访问目标统计的性能边界

共用 TCP/UDP 计量层在连接准入时缓存流量计数分片，避免每次收发重复计算节点标签哈希；没有共享服务器额度时跳过额度读锁，已配置额度仍实时检查，撤权、设备限制、动态限速和按用户共享速度桶保持生效。

兼容路由只在域名、协议和定制 DNS 规则需要时添加嗅探；纯直连及仅 IP/端口规则不等待客户端应用首包。原生 sing-box 路由完整保留管理员显式配置的 sniff 动作；未提供路由规则时不隐式添加。这项优化作用于各代理协议共用的路由流程，尤其是服务器先发送欢迎消息的连接；不是修改加密、拥塞控制或协议兼容参数。

目标聚合在现有分片锁内原位增加计数，避免每个包反复复制整条记录及写回映射；切批次时复制成独立快照，重试期间仍保持不可变。新目标会分配一条记录，稳定目标的每包更新不分配内存。保留每节点 500 活动行 + 500 待确认行、UDP 8192 地址/1024 会话缓存及关闭时释放的限制；不会将统计写入节点磁盘。过期批次会携带原有截断提示，避免丢失容量不足信息。这些提示是截断/丢弃信号，不是精确的用户请求数。

持续上报失败按 30/60/120 秒退避，正常后台上报周期仍为 30/60/120/300 秒（默认 60 秒），采集租约单独保持同步。120/300 秒减少请求频率，但单个周期仍只收集最多 500 个用户与目标组合，高基数节点更容易截断；不能把降低上报频率理解为统计完整性不变。面板近期明细保留期与日汇总保留期独立，延长保留期仍需要日汇总全局行数预算，不能单靠减少上传次数解决数据库增长。本轮没有更改面板的已配置保留期或增加数据维度。

本机 Windows amd64 / Go 1.27.1、8 逻辑 CPU，目标统计 300ms、连接计量 500ms，各 3 次微基准中位数：

| 计量路径 | 修改前 | 修改后 |
| --- | ---: | ---: |
| TCP 固定目标统计 | 31.53 ns/次 | 17.52 ns/次 |
| UDP 域名目标统计 | 47.88 ns/次 | 35.70 ns/次 |
| 连接计量分片（长节点标签，缓存与每次计算对照） | 34.80 ns/次 | 27.50 ns/次 |

稳态场景均为 0 B/次、0 allocs/次；新连接、新目标和生成批次仍有受上限约束的分配。结果衡量计量函数，不是外网吞吐、网络时延或节点容量保证。复用 `core/target_statistics_test.go`、`core/soak_test.go`、`core/config_test.go`、`core/integration_test.go` 和 `node/reload_retry_test.go` 验证并发切批、失败退避、嗅探规则和各协议首包；运行日志及本地构建产物保存在工作区 outputs，不纳入发布源码。


带宽容量样例（不是实际站点预测）：500 条常见长度域名、连接数 100、上行 1 MiB/下行 10 MiB 的完整批次，JSON 为 63,405 字节。20 个节点始终每分钟装满并发送，约 1.70 GiB/天（不含 HTTP/TLS）；按两分钟满批约减半，但长域名、更大的计数和重试会增加实际值。这说明即使数据库以小时或日聚合存储，网络开销仍由节点批次决定，不能用数据库行数推算上传带宽。正常未满批时按实际记录发送，没有填充到 500 条。

验证结果：`GOEXPERIMENT=jsonv2 go test -tags with_quic,with_utls ./... -count=1`、完整 `-race` 回归和 `go vet` 通过。额外执行生命周期/Naive 多路复用/目标缓存压力回归与最后的退避、真实域名拦截测试，均通过；域名黑名单仍能阻止发往 IP、但 HTTP Host 命中规则的连接。上述压力回归复用已有资源增长阈值，只覆盖有限测试时段，不能证明生产长期运行绝无泄漏。

Linux amd64 与 arm64 均以 `CGO_ENABLED=0 GOEXPERIMENT=jsonv2 go build -tags with_quic,with_utls -trimpath` 交叉构建通过；未在真实 Linux 节点运行本轮压测。需要发布并更新节点二进制才能应用转发及统计优化，仅更新面板静态资源不会改变运行中的节点。

### 依赖安全复核

构建最低 Go 版本已提升到 1.27.3，修复标准库证书信任、HTTP/2 与 TLS 等安全问题；x/net 更新至 0.60.0，gRPC 更新至 1.83.2，Chi 更新至 5.3.0，并同步其必要传递依赖。现有 CI 从 go.mod 读取版本，无需修改节点配置。已有二进制不会自动获得源码或工具链修复，需要发布并安装新构建。

官方 Go 1.27.3 Linux 容器中的全包 `go test -race -tags with_quic,with_utls ./...`、`go vet` 和单独的 `TestVLESSRealityVision` 通过。govulncheck v1.9.0 从 16 项静态可达告警降到 0；导入包告警为 0；仍有 1 项所需模块告警，来自未导入的 x/crypto/openpgp（GO-2026-5932，废弃包无修复版本）。项目其他密码功能仍需要 x/crypto 模块。静态扫描不等于生产可利用性证明或零漏洞保证。安装脚本 Python 测试 20 项执行、2 项平台相关跳过；Windows 新工具链包解析未完成，中止后改在 Linux 验证。本轮未发布 Release 或执行生产部署。

### 配置应用回执

V10 能力增加 `configuration-receipt-v1`。节点记录面板配置 ETag；所有控制器启动成功后报告 applied，配置解析、内核校验、用户准备、证书准备或启动失败时报告对应固定错误码。回执只包含哈希和状态，不包含配置或原始异常。失败的监控请求不阻止代理转发；即时上报共用最多 3 秒截止时间，后续既有能力上报携带最新状态重试，无新增后台任务或无界缓冲。面板会忽略不同配置哈希的迟到回执；仍需查看节点日志定位详细原因。

此功能需同时更新面板并发布/安装新版 V2Box。旧节点没有回执时面板显示未确认，不把配置拉取或 304 当作应用成功。维护模式由面板管理，订阅隐藏不停止节点进程或中断已有连接。


### Configuration hashes through proxies

v1.0.5 reads the panel-issued SHA-256 from `meta.configurationHash` and accepts strong or weak HTTP ETags from older panels. The original ETag is retained only for conditional HTTP requests. Missing, malformed or non-hex versions are rejected before certificate preparation or listener replacement, without caching the rejected response. Upgrade the panel first so proxies that remove ETag entirely cannot prevent configuration receipts. This fixes `invalid configuration hash` rollback after a prepared domain transition; it does not bypass certificate checks or publication acknowledgement.

### Verified managed TLS domain transitions

The `certificate-transition-v1` capability accepts a candidate domain from the panel without changing published subscriptions first. V2Box waits up to two minutes for both public DNS address families to match, verifies the certificate identity, lifetime, server usage and key pair (including public trust for HTTP/DNS/file modes), and prepares replacement users before closing existing listeners. Domain-specific ACME/self-signed files preserve the old certificate; remote certificates remain in memory. Failed listener startup restores the previous controllers, authentication snapshot and traffic budget. Successful startup reports the candidate ID, configuration hash and leaf SHA-256 fingerprint; the panel explicitly acknowledges publication, with bounded retries for lost acknowledgements. A rejected or unconfirmed publication restores the previous listener. Subsequent configuration polling reconciles an ambiguous acknowledgement loss with the panel's committed state.

Reality identities are not changed. Manual files must cover the new hostname with a trusted chain. Reloads can interrupt existing sessions; clients must refresh subscriptions when domain/SNI/pins change. Old certificate files are retained, and managed DNS removal remains panel-owned. This capability is included in v1.0.4; upgrade nodes running v1.0.3 or older before enabling TLS domain transitions.


### 四层中转与服务器上报

面板的服务器记录代表实际机器。入口机仅执行服务器 IP 上报命令，V2Box 安装在回源节点。共享入口由面板引用管理，入口域名指向入口 IP、回源域名指向节点 IP；上报凭据不会在每次新增同机节点时自动轮换。

连接地址、回源地址和 TLS 身份域名独立管理。四层中转默认保留代理服务器的落地证书，客户端连接入口地址，SNI 使用证书覆盖的 TLS 身份域名；入口域名无需与证书域名相同。DNS 验证凭据匹配证书域名所属区域。HTTP 验证仅在证书域名的公网 80 端口能够到达申请证书的代理服务器时使用；中转不满足此条件时使用 DNS 验证。连接入口解析检查使用入口 IP，回源解析检查使用代理服务器 IP，并分别按配置的地址类型验证。DNS 校验只访问固定解析器并拒绝 HTTP 重定向。失败保留原配置，成功验证及面板确认后完成切换；Reality 身份保持独立。

面板可生成 Nginx 1.27.3+ stream 转发片段，采用动态 DNS 回源。管理员需在入口应用并测试该片段；V2Box 本机监听就绪不能证明入口转发或防火墙已经正确。更换回源域名时，确认入口更新后才允许面板延迟清理旧解析。停止共享服务器的上报服务会影响该服务器的全部关联域名。

### 配置实际应用回归

`cmd/TestReloadAppliesProtocolPortDomainAndRelay` 使用本地 HTTPS 面板、真实 sing-box 监听和回显服务器，覆盖 VLESS/Trojan 协议切换、监听端口迁移、证书域名变化、直连/四层 TCP 中转往返、DNS 模式凭据轮换进入活动控制器，以及错误证书拒绝后原连接仍可传输数据。测试不只断言面板回执。

测试使用自签名及本地受信证书，不会访问 Cloudflare 或公网 ACME，不代表生产 DNS 权限、证书服务可达性或外部 Nginx 配置已经验证。DNS 凭据环境作用域和签发失败保护由 `node/acme_guard_test.go` 覆盖。

本轮在 Go 1.27.3 Linux 隔离容器执行 `go test -tags with_quic,with_utls ./cmd ./node ./core ./api/v2board`，四包全部通过，包括上述真实重载测试。未调用真实 Cloudflare 或公网 ACME。

V2Box v1.0.8 起，V10 capabilities 上报独立的 `agentVersion` 与 `coreVersion`，发布版本取自与 CLI 相同的构建值。面板“部署与状态”展示版本和最近上报时间；此功能不提供远程更新控制。

### 单实例接入多个后台

一个进程的 `Nodes` 可以配置不同后台，各条目按“后台地址 + 节点类型 + 节点 ID”独立运行。不同后台允许使用相同节点 ID、用户 ID 和服务器 ID；后台地址的大小写及末尾斜杠别名不能重复配置同一个节点。例如：

```json
{
  "Log": {"Level": "info"},
  "Nodes": [
    {"NodeType": "v2box", "ApiVersion": "v10", "ApiHost": "https://panel-a.example.com", "NodeID": 1, "ApiKey": "填入后台 A 的独立节点密钥"},
    {"NodeType": "v2box", "ApiVersion": "v10", "ApiHost": "https://panel-b.example.com", "NodeID": 1, "ApiKey": "填入后台 B 的独立节点密钥"}
  ]
}
```

文件保存为 `/etc/v2box/config.json`，权限保持 600。每个后台独立下发用户、协议、路由与监听设置；同一监听地址上的 TCP/UDP 端口需要分别避免冲突。启用默认文件监控时，原子替换文件也会在约 5 秒防抖后生效；条目重排不会重启节点，增加、删除或修改条目只处理对应身份。某后台断线、认证失败或配置错误不会阻止其他条目启动，失败条目按 30 秒至 5 分钟退避重试；已有服务在准备失败时保留，切换失败只恢复该节点。一个后台的协议/端口变更可能中断该节点自己的连接，其他后台已有连接继续工作。

流量、用户和目标统计按节点分开。同一后台内属于相同物理服务器的流量配额继续共享累加；不同后台相同服务器 ID 不共享配额。默认磁盘流量队列仍使用原来的后台地址与节点 ID，避免升级时遗留待确认批次。所有进程级设置仍共同生效；程序更新、`v2box restart`、进程崩溃和主机故障会影响全部节点。`--watch=false` 不自动接收本地文件编辑，修改后重启会影响全部节点。

默认文件证书、ACME 账户和保护状态分目录存放在 `/etc/v2box/panels/<后台摘要>/<节点类型>-<节点ID>/`，增加第二个后台不会再次改变第一个后台的路径。旧默认证书只有在证书/私钥、域名及有效性校验通过后才复制使用，旧文件保留，签发失败保护也继续继承；不会因为换目录绕过暂停状态。可通过本地条目的 `CertificateDirectory` 指定目录。后台显式下发的证书路径保持原意，但不同条目不能占用同一证书或私钥文件，否则拒绝该候选配置。

IP 上报可依次配置多个目标；每次仅更新选定后台的凭据和重试状态，保留其他后台及既有直接 DNS 更新目标：

```sh
node-network report-configure --panel-url https://panel-a.example.com --server-id 1 --token-file /root/panel-a-report.key
node-network report-configure --panel-url https://panel-b.example.com --server-id 1 --token-file /root/panel-b-report.key
node-network status
```

这里的 `--server-id` 是对应后台的物理服务器 ID，不是 `Nodes[].NodeID`。密钥文件应为权限 600 的独立 IP 上报凭据，不能使用节点 API 密钥代替。上报目标最多 64 个，调度最多并发处理 8 个；某后台超时、401 或 429 只影响自己的上报和退避。旧单目标配置可直接读取，添加目标后自动转换为多目标配置。`node-network disable` 或卸载时显式 `--stop-network` 会停止全部上报，DNS 记录保留。

面板一键部署已支持按后台与节点身份自动追加 `Nodes` 和 IP 上报目标，保留已有节点、证书及自定义设置。需要发布并安装匹配的 v1.0.12 程序与脚本；能力不足的旧实例会在消耗部署授权前被拒绝，不会自动重启或降级已有服务。版本升级使用统一更新命令并安排全部节点的维护窗口。

多后台专项回归覆盖无响应后台独立恢复、另一后台长连接保活、端口冲突局部回滚、条目删除/重排、重复身份及共享证书路径拒绝、配额作用域、旧证书/签发保护迁移，以及上报并发、凭据轮换和独立退避。未使用生产凭据或公网 ACME。

### 重载、证书与退出超时

每个节点的启动或重载独立使用 5 分钟准备期限，涵盖面板配置、用户、在线设备、流量预算、DNS 验证及证书准备。DNS 验证仍最多等待 2 分钟，同时遵守外层取消；单次 ACME 操作最多 4 分钟，排队等待也计入期限。准备失败或超时保留旧监听，后续轮询仍可重试；切换阶段失败使用独立的 1 分钟恢复期限重新启动旧配置。

ACME 在独立子进程执行。DNS 凭据通过私有标准输入传入，不进入命令参数，也不修改主进程环境；取消时终止并回收该子进程。子进程只返回证书材料，由主进程校验并发布证书/私钥，防止超时任务迟到覆盖活动证书。ACME 账户文件也改为临时文件写入后原子替换。429、Retry-After、临时网络故障和三次上限仍沿用原保护规则；已开始签发后被中断会保留保护状态，需要核实订单、DNS 验证记录及失败原因后人工恢复，避免重启反复申请。

退出信号从启动阶段起即可取消网络准备；关闭时先取消全部控制器任务，再等待回调结束，最后流量上报共享 15 秒期限。已启动监听的生命周期不再关联准备阶段的短期限。若第三方组件完全不响应取消，退出超过 30 秒仍未结束，则以失败状态退出，避免永久挂起；安装器的 systemd `Restart=on-failure` 可恢复服务，手工运行或其他服务管理器需提供相应拉起机制。强制退出属于异常兜底，不能保证最后一批尚未持久化的数据完成写入。

隔离验证已通过节点全包 `go test -race -count=1 -tags with_quic,with_utls ./...`、`go vet`、构建及 48 项脚本回归。新增覆盖无响应面板超时后旧监听保留及再次重载、ACME 取消/超时回收、无迟到发布、无效密钥拒绝、取消等待不消耗申请次数、错误分类及 Retry-After 保留、DNS 父级取消、活动监听不随准备上下文结束，以及退出兜底。真实二进制在本地 HTTPS 面板不响应时收到 SIGTERM，正常退出；测试未调用公网 DNS 服务商或 ACME。

### 安装生命周期故障处理

安装和更新先验证发布包必需文件、脚本语法与程序版本；服务管理操作设有超时，更新在连续健康观测后确认。下载中断或启动失败会恢复原程序，配置、证书和部署状态保留。若磁盘或权限错误导致回滚不完整，保留临时备份并显示路径，修复后再恢复程序。卸载在确认服务停止后移除程序，默认保留 IP 上报；显式 --stop-network 才停止共享上报。复用 script/lifecycle_test.py 的隔离故障测试验证这些边界。

管理命令在更新脚本缺失时提示通过面板引导脚本修复；非交互卸载必须明确传入 --yes。使用 --stop-network 且上报组件缺失时，systemd 环境直接停用既有上报定时器和服务，保留上报配置及 DNS 记录。


### 资源与安装生命周期复核

AnyTLS 客户端关闭会取消尚未完成的拨号，并拒绝和关闭迟到连接；回收会话时在锁内再次确认关闭状态。控制器关闭使用与周期上报相同的持久化流量批次，不把已保留批次重复恢复到实时计数；即使没有新流量，也尝试发送既有待确认批次。候选配置被拒绝、初始化失败及控制器关闭后释放面板 HTTP 空闲连接，回滚时客户端仍可重新建立连接。

定时任务拒绝非正间隔或空回调，超时计算先限制间隔再相乘，避免时长溢出。安装器移除重复公网 IP 探测和不可达的旧 DDNS 缺文件分支；首次配置与 systemd/OpenRC 服务文件在完整写入后原子替换，失败清理未发布临时文件。原配置、证书、部署授权状态和 DNS 记录仍保留。安装、更新、卸载、锁冲突、失败回滚及首次写入失败重试复用 `script/lifecycle_test.py`，网络及凭据测试共 48 项。

复核命令：

```sh
GOEXPERIMENT=jsonv2 go test -race -tags with_quic,with_utls ./...
GOEXPERIMENT=jsonv2 go test -tags with_quic,with_utls ./core -run TestVLESSRealityVision -count=1
GOEXPERIMENT=jsonv2 go vet -tags with_quic,with_utls ./...
V2BOX_SOAK=1 GOEXPERIMENT=jsonv2 go test -tags with_quic,with_utls ./core -run 'TestCoreLifecycleSoak|TestNaiveMultiplexChurnSoak' -count=1 -v
python3 -B -m unittest discover -s script -p '*_test.py'
bash -n script/install.sh script/v2box.sh
govulncheck -tags with_quic,with_utls ./...
```

漏洞扫描按当前生产构建标签执行，实际可达符号和导入包告警均为 0。仍有未导入的 `golang.org/x/crypto/openpgp` 模块级告警 GO-2026-5932；该废弃包没有修复版本，项目其他密码功能需要 x/crypto，不能直接移除整个模块。资源测试是有界回环测试，不等于长期生产环境绝无泄漏；没有调用生产 Cloudflare、ACME 或 Nginx。上述修复包含于 v1.0.11；升级保留配置、证书和部署状态。

本次隔离回归：全包竞态回归、Reality Vision 连接测试及 `go vet` 通过；八协议 144 次启停连接后，回收采样协程数始终为 3，最后活跃堆较基线减少约 26 KiB。Naive 多路复用采样协程数始终为 7，最后活跃堆增加约 34 KiB，未观察到持续增长。计量、流量累加及限速稳态基准均为 0 B/op、0 allocs/op；这些结果不代表公网吞吐或生产容量保证。

### 默认出站安全策略

节点默认拒绝代理用户访问回环、私网、链路本地、保留地址及云元数据地址，覆盖 IPv4、IPv6、解析后的域名和 UDP 会话中的每个目标。该策略先于面板下发的路由执行，面板的原生路由不能将其覆盖。

确实需要访问内部中转或专用服务时，在节点本地配置中显式添加最小范围白名单，例如：

```json
{"OutboundAllowCIDRs": ["10.20.30.40/32"]}
```

该白名单对该节点的全部代理用户生效，请勿为了兼容而放开整个私网或 `0.0.0.0/0`。配置错误的 CIDR 会阻止新配置应用；更改后重新加载节点。现有公网代理协议、流量记账和面板接口保持原有行为。以往通过节点访问本机/内网的用途需要先配置明确白名单。


### 一键部署能力检查与局部应用

`v2box capabilities` 输出节点部署能力；`node-network capabilities` 输出多目标上报能力。管理命令 `v2box reload` 核对当前运行进程的能力后发送 SIGHUP，节点只更新发生变化的身份，即使使用 --watch=false 也生效。`v2box check-runtime` 只核对能力；旧进程、无法验证的 PID 或不兼容脚本不能作为无中断追加的依据。

后台部署脚本按目标保存私有恢复状态和完成回执。不同后台相同节点 ID 可分别追加；正在执行的部署由锁串行化，未完成的任务可各自保留。重复执行已经完成的节点部署命令会验证本机身份和凭据指纹，不再次兑换一次性授权，不改写原配置。发生错误时只撤回本次新增条目，不恢复整份旧快照覆盖别人的后续修改；管理员修改过该新增节点时则停止自动回滚。

新安装与程序修复不会在检查完成前自动启动待部署节点。已有运行实例缺失程序文件时，先安排维护修复，不让另一后台部署触发全局程序替换。全新节点的独立凭据先保留在私有恢复文件，通过 IP/DNS 和必要端口检查后才写入活动 Nodes。

v1.0.12 发行包包含这些能力。安装器和后台固定版本与该发行包同步；程序更新会重启运行中的节点进程，应安排维护窗口。

### 安装生命周期与证书热更新回归

新安装、完整示例配置、已有多后台配置重装，以及连续卸载/重装循环由
`script/lifecycle_test.py` 验证。卸载保留证书、私钥、ACME 账户与保护状态，
默认不停止独立 IP 上报。卸载影响本机整个 V2Box 实例；原二进制缺失时应
重新安装，不能调用仅升级模式。

HTTP/DNS 证书按周期检查续期窗口；文件/远程/自签证书不自动向 ACME 续期。
真实 TLS 握手回归覆盖证书/私钥同时原子替换、无效文件时保留内存中的可用
证书、恢复有效证书，以及其他节点和既有代理长连接的保留。`internal/fswatch`
对 v0.1.2 做最小局部适配：同一监听器的文件回调串行执行，不阻塞不同监听器，
避免证书和私钥文件同时变更时的竞争。该修复在后续节点发布后生效；不能把
仅更新后台视为已升级节点二进制。
