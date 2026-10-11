# v2box

面向修改版 V2board 的纯 sing-box 节点后端。基于 **sing-box v1.14.3**，不再使用 Xray。为支持用户热更新，仓库内维护了公开的入站适配和认证并发补丁，来源及修改范围见 `internal/README.md`。

## 面板兼容

V2Box 节点默认使用 `/api/v10/nodes/v2box/{id}/...`，包括 configuration、users、online-counts、traffic、online-users 和 capabilities。每个 `Nodes[]` 设置 `"NodeType": "v2box"`；`ApiVersion` 留空即使用 V10，也可显式填写 `"ApiVersion": "v10"`。凭据放在 Authorization Bearer 请求头，配置和用户列表支持 ETag。节点上报核心版本及能力，面板仅向声明支持原生路由的节点下发高级配置。

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
curl -fSL https://raw.githubusercontent.com/lenaanderson5566-ops/v2box/main/script/install.sh -o install.sh
sudo bash install.sh <release-tag> --api-host https://panel.example.com --node-id 1 --api-key YOUR_KEY
```

安装路径 `/usr/local/lib/v2box`；配置 `/etc/v2box/config.json`；服务 `v2box`。更新保留配置和上一份二进制 `v2box.previous`。检查日志使用 `v2box log`。

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
