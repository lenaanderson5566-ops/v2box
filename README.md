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

Update both the panel and node binary, then enable the default-off switch in the panel log settings. Requires V10 API; older panels returning 404 leave collection disabled. Targets use the original domain or IP, without reverse DNS or replacing IPs with sniffed domains. No URL paths or payloads are collected. The panel log settings offer reporting every 30/60/120/300 seconds (default 60). Nodes advertise target-statistics-interval-v1; older nodes keep their fixed cadence. Policy sync runs in its own cancellable task at most every 60 seconds (plus up to 10% jitter), independently of the upload interval, with a 120-second lease; failed policy reads disable collection. Each node holds at most 500 active rows and 500 pending rows; TCP retains a single fixed target without allocating target maps. UDP caches at most 64 targets per connection, ignoring ports, with per-node limits of 8,192 cached addresses and 1,024 cache-holding sessions. Close and policy retirement release these maps. Per-node/user shards avoid a global packet-path lock. Failed uploads retry after 30 seconds plus jitter rather than waiting the full report interval. Batches retry with stable IDs for at most five minutes and are never spooled to disk. Compatible panels attach a policy lease to configuration responses (including 304); the node reuses only the remaining lease and falls back to the policy endpoint when needed. Statistics requests have a five-second deadline, a 256 KiB outgoing body limit and a 4 KiB response limit, and do not delay billing reports. The panel displays short-lived hourly aggregates under access analytics; restarts, capacity limits and outages can lose this optional telemetry.


## Independent node credentials (manual rotation)

Upgrade the panel before migrating a node. Generate its scoped credential in the panel node's credentials dialog; the full value is displayed once. First issuance immediately blocks the shared token for that identity. Later manual rotations allow a 30-minute overlap. Confirm the new fingerprint's last authentication in the panel, then revoke the previous key. No scheduled rotation is implemented. Revoking API access does not terminate established proxy sessions.

Fresh installs can use `--prompt-api-key` for hidden terminal input. For an existing configuration, use `v2box credential` (Python 3 required), select the node, paste the independent key, then run `v2box restart`. The helper updates only that entry atomically with mode 0600, creates no credential backup and does not restart automatically. Independent keys also use Authorization in legacy API mode; older shared-token deployments retain their compatibility behavior until migrated. The helper is included in new release archives and is installed by the updated installer.
