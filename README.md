# v2box

面向修改版 V2board 的纯 sing-box 节点后端。基于 **sing-box v1.14.3**，不再使用 Xray。为支持用户热更新，仓库内维护了公开的入站适配和认证并发补丁，来源及修改范围见 `internal/README.md`。

## 面板兼容

沿用上游 `/api/v2/server/config` 与 `/api/v1/server/UniProxy/{user,alivelist,push,alive}` 接口。改进版面板支持独立 V2Box 节点：在对应 Nodes 配置项填写 `"NodeType": "v2box"`。未填写时保留 `node_type=v2node`，兼容原有部署；不要把已有 V2Node 的编号直接当成 V2Box 编号。支持 JSON / MessagePack 用户列表。

节点协议：VMess、VLESS、Trojan、Shadowsocks AEAD / SS2022 AES、Hysteria2、TUIC、AnyTLS。VMess/VLESS/Trojan 支持 TCP、WebSocket、gRPC、HTTPUpgrade。支持 TLS 证书文件、面板远程证书、原有 ACME 申请/续期和 VLESS Reality。

Clash Verge Rev / Mihomo 等客户端仍通过面板订阅连接节点；请确保面板订阅输出与所选协议、传输、TLS 参数一致。不要将 sing-box JSON 直接作为 Clash YAML 导入。

## 迁移限制

- **用户增加不重建节点、不关闭其他用户的现有连接。删除、UUID 变化或同一 UUID 对应的用户 ID 变化，只撤销对应账号的连接和后续转发权限。** 单纯限速/设备限制变更也无需重建。节点端口、协议、TLS 等配置变化仍走配置重载，可能中断连接。
- 空用户列表清空认证表并关闭全部用户连接，保留监听器以接受后续用户热更新，不提供匿名访问；304 响应保留现有用户。账号删除后重新添加会获得新的内部认证代次，旧多路复用会话不能恢复访问权限。
- 用户流量计数独立于核心实例；用户删除和实例替换不清除待上报流量，配置重载时保留仍在配置中的节点计数。HTTP 上报失败恢复计数供下一轮重试。计数目前保存在内存中，进程崩溃无法恢复；面板接口没有幂等确认，超时后的重试存在重复计费可能。
- 用户限速覆盖 TCP/UDP 的应用数据，上下行共享用户限速桶。设备限制依据源 IP，结合面板在线计数和本地最近两分钟活动；不是硬件设备识别，也不是跨服务器强一致配额。
- XHTTP/SplitHTTP、TCP HTTP 伪装、PROXY protocol、trusted X-Forwarded-For、Xray VLESS 加密、Reality ML-DSA/xver、多 Reality server_names、reject_unknown_sni 当前明确拒绝，需先迁移配置。
- 支持域名/IP/端口/协议封禁及自定义出站路由。`route` / `route_ip` / `default_out` 的 action_value 必须改为包含 `type`、`tag` 的 **sing-box 出站 JSON**；不接受 Xray 出站 JSON。当前注册的出站为 direct、SOCKS、HTTP，以及上述七类代理协议。
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
