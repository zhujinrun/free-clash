# freeclash

一个用 Go 编写的 Clash 订阅节点巡检工具。它定时拉取公共订阅，对每个节点做**真实链路探测**，把当前网络环境下确实可用的节点筛选出来，重新生成一份干净的订阅供 Clash 使用。

## 解决的问题

公共免费订阅的典型问题是节点数量虚高、失效率极高。一份 4000 节点的订阅，实际可用通常在百分之一到百分之几之间，而且每天变化。直接订阅会带来两个后果：客户端测速耗时、自动选路频繁命中死节点、切换时长时间无网。

本工具的做法是：在订阅和客户端之间加一层巡检，只把**当场验证过能连通境外目标**的节点交给 Clash。

## 探测原理

只测端口是否开放是不够的——端口活着但密码失效、协议不匹配、出口被封的情况非常普遍。因此采用三级过滤：

| 级别 | 动作 | 目的 | 单节点成本 |
|---|---|---|---|
| 一 | 解析订阅，按协议构造客户端 | 淘汰不支持的协议与传输组合 | 纳秒级 |
| 二 | TCP 三次握手 | 淘汰服务器下线、端口封闭的节点（约占七成） | 2 秒超时 |
| 三 | 建立协议隧道并请求境外 `generate_204` | 验证密码、协议、传输、出境链路全链路可用 | 6 秒超时 |

第三级是关键：它在本进程内完整实现了 Shadowsocks、Trojan、VMess 的客户端握手，真正把请求送进隧道，再通过隧道向境外发起一次 TLS 握手和 HTTP 请求。因此测出的延迟是**端到端真实延迟**（含节点握手与出口 RTT），而不是到节点服务器的 ping 值。

完整流程：

```
精选订阅 + 节点文章里的通用订阅 → 解析去重 → 关键字过滤 → TCP 预筛 → 隧道探测 → 复测排序 → 生成订阅
                                                                              ↓
                                                                ├── Clash 格式  /best.yaml
                                                                └── v2ray 格式  /best.txt
```

## 协议支持

| 协议 | 传输 | 加密 | 支持 |
|---|---|---|---|
| Shadowsocks | tcp | aes-128/192/256-gcm | 是 |
| Shadowsocks | tcp | chacha20-ietf-poly1305 | 是 |
| Shadowsocks | tcp | aes-128/192/256-cfb | 是 |
| Shadowsocks | tcp | rc4-md5 | 是 |
| Shadowsocks | tcp | 2022-blake3-* | 否 |
| Trojan | tcp / ws | TLS | 是 |
| Trojan | grpc | TLS | 否 |
| VMess (AEAD, alterId=0) | tcp / ws | auto / aes-128-gcm | 是 |
| VMess (alterId>0) | — | 旧版非 AEAD | 否 |
| VLESS | tcp / ws | 无加密层，TLS / 明文 | 是 |
| VLESS + REALITY | — | 需伪造 ClientHello 指纹 | 否 |
| VLESS | grpc / xhttp | — | 否 |
| Hysteria2 | — | QUIC | 否 |

不支持的节点会被标记为「跳过」并计入报告，不影响其他节点的探测。

## 快速开始

```bash
# 从分享站点自动解析精选订阅，然后单次探测
go run . -site https://freeclash.zhaotizi.best/ -once

# 常驻运行：每 30 分钟巡检一次，并在 :9898 提供订阅服务
go run . -site https://freeclash.zhaotizi.best/ -serve :9898 -interval 30m

# 只想看当前解析到的订阅地址
go run . -site https://freeclash.zhaotizi.best/ -resolve

# 直接给订阅地址（跳过页面解析）
go run . -url https://clashnodefree.com/sub/clash.yml -once

# 编译为单文件
go build -ldflags "-s -w" -o freeclash.exe .
```

首次运行会拉取订阅并对全部节点做探测，耗时取决于节点数量与并发配置。4000 个节点、默认并发下约需 2–3 分钟。

## 安装为系统服务

常驻巡检适合交给系统服务管理器托管：开机自启、崩溃自动重启、退出登录也不影响。

```bash
# 编译当前平台的二进制
make dev          # 产出 dist/freeclash（Windows 为 freeclash.exe）

# 安装为服务并启动（Windows 需管理员终端，Linux 需 sudo）
dist/freeclash install -site https://freeclash.zhaotizi.best/

# 查看状态
dist/freeclash status

# 停止 / 卸载
dist/freeclash stop
dist/freeclash uninstall
```

构建默认用 UPX 压缩（约 8.1MB → 2.9MB），没装 upx 会自动跳过，macOS 二进制不支持压缩。若杀软对压缩过的服务端二进制误报，用 `make dev NO_UPX=1` 重新构建。

`install` 做了三件事：

1. **确保有配置**。找不到 `config.yaml` 时，如果命令行给了 `-site` 或 `-url`，会按可执行文件所在目录生成一份默认配置再继续；两者都没给则直接报错，避免装出一个必然失败的 service。
2. **校验能常驻**。服务模式下 `interval` 必须大于 0，或配置了 `serve_addr`，否则进程跑完一轮就退出，服务会立刻变为已停止。
3. **注册并启动**。Windows 注册为自动（延迟启动）服务，并设置失败重启；systemd 生成 unit 文件并 `Restart=always`。

Windows 上注册的服务名是 `freeclash`，可用 `sc query freeclash` 查看；Linux 上生成 `/etc/systemd/system/freeclash.service`，也可用 `systemctl status freeclash` 管理。

### 路径怎么定的

服务运行时的工作目录不可信（Windows 会把它设成 `system32`），因此所有相对路径都以**配置文件所在目录**为基准：

- 配置查找顺序：`-config` 指定 → 可执行文件旁的 `config.yaml` → 当前目录的 `config.yaml`
- `output_dir`、`log_file` 及本地订阅文件路径都相对配置文件目录展开
- 服务定义里会写入 `-config <绝对路径>`，装完再改配置也能被读到

### 日志

服务模式没有终端，日志写入 `<配置目录>/logs/freeclash.log`，单文件超过 10MB 自动轮转，保留 5 份（最多约 50MB）。前台运行时日志直接打在终端上；配置了 `log_file` 则同时写文件。路径可用 `-log` 或配置项 `log_file` 覆盖。

```bash
# Windows
type logs\freeclash.log
# Linux / macOS
tail -f logs/freeclash.log
```

## 自动解析精选订阅

公司的分享站点需要手动点「复制精选订阅」才能拿到地址。配置 `site` 后，程序自己去页面取，不必再手工操作。

**为什么不能直接匹配按钮。** 页面上那个按钮是 `<button type="button">复制精选订阅</button>`，没有 `data-clipboard-text` 之类的属性，地址是点击时由 JS 从数据对象里取的；地址本身也不在 HTML 里，而是编译进带内容哈希的 JS chunk（如 `theme.BdP30UdW.js`），站点重新构建后文件名就会变，因此无法硬编码。

**实际做法**分三步：

1. **动态发现资源**：抓首页，解析出它引用的全部 JS（文件名随构建变化，所以每次现算）；
2. **按按钮锚定**：用按钮文案（默认「复制精选订阅」，可用 `site_button` 改）找出按钮所在的 JS 模块，再只在模块内找 `subscription:` 形式的赋值。按钮与地址同处一个模块，这一步能排除页面里的大量推广与返佣链接；
3. **实测验证**：候选地址逐个真实拉取，能被 Clash 解析出节点才算命中。

打分只是排序手段，最终判据是第 3 步。实测该站点上目标地址得分 240（`按钮模块内订阅赋值` + `订阅后缀`），而 13 个干扰链接远低于它。

失败时有三级兜底：上次解析出的地址 → `out/last-sub.yaml` → 报错并保持上一轮的订阅服务可用。

```bash
# 页面改版或按钮文案变了，可临时覆盖
go run . -site https://freeclash.zhaotizi.best/ -button "复制订阅" -resolve -v
```

`-v` 会打印候选地址的分数与命中依据，页面改版时用它定位问题最直接。

### 跟进节点文章

首页那份「精选订阅」只有 4000 多个节点，而且整体质量差。真正的量在**每日节点文章**里：首页的「查看节点文章 →」会进到当天的汇总页，一篇文章挂着 50 条「通用订阅」，单篇可识别节点数可达 5 万。

所以程序默认会顺着这条路再走一遍（`article_limit` 控制跟进几篇，默认 2）：

```text
首页 → 「查看节点文章」链接 ─┐
       /nodes 列表页 ────────┴→ 最近 N 篇（按日期倒序）
                                  └→ 每篇的 <div class="copy-box"><code>URL</code>
                                       └→ 50 条订阅逐个拉取 → 解析 → 合并去重 → 一起探测
```

几个处理上的取舍：

- **只认卡片结构里的地址**。文章页同样混着推广、教程、返佣链接，直接扫全页外链会拉进几十个无效请求。现在优先取 `copy-box` 里的 `<code>`，抽不到时才退化为「标签 + 链接」，最后才扫全页。
- **三种订阅格式都能读**。这些「通用订阅」基本是 base64 包裹的分享链接（`vmess://`、`trojan://`、`ss://`、`vless://`），而不是 Clash YAML。程序会依次尝试 YAML、明文链接、base64 链接，并丢弃本程序不支持探测的协议（hysteria2、ssr、VLESS+REALITY 等），避免浪费探测配额。
- **失败不影响整体**。50 条订阅里通常有个位数拉不动（仓库被墙、301 跳转、返回 404），逐条记录在 `-v` 日志里，其余照常合并。
- **注意耗时**。2 篇文章约 100 条订阅、上万节点，一轮大约 5–10 分钟。嫌久可以调小 `article_limit` / `article_max_subs`，或用 `max_nodes` 截断总量。

```bash
# 只看当天一篇
go run . -site https://freeclash.zhaotizi.best/ -articles 1 -once -top 20
```

## 接入客户端

同一批节点会输出两种格式，按客户端选一个：

| 客户端 | 订阅地址 | 格式 |
|---|---|---|
| Clash / mihomo / Clash Verge / Stash | `http://127.0.0.1:9898/best.yaml` | Clash YAML |
| v2rayN / v2rayA / Shadowrocket / NekoBox | `http://127.0.0.1:9898/best.txt` | base64 分享链接 |

v2ray 系客户端不认 Clash 的 YAML，所以 `/best.yaml` 在它们那里会导入失败或显示「不支持的格式」；反过来 Clash 也读不了 base64 订阅。两个地址指向同一份节点列表，只是序列化方式不同。

也可以直接用文件：`out/best.yaml` 给 Clash 本地导入，`out/best.txt` 给 v2rayN 从剪贴板导入。`out/best.links` 是未编码的分享链接，便于人工核对。

生成的 Clash 配置包含：

- 一个 `🚀 节点选择`（select）主组
- 一个 `♻️ 自动选择`（url-test）组，覆盖全部入选节点
- 若干按地区划分的 url-test 组（如 `🌍 HK`），数量由 `per_region_groups` 控制
- 一套最小规则集：内网与 CN 直连，其余走代理

## 命令行参数

### 服务命令

| 命令 | 说明 |
|---|---|
| `install` | 安装为系统服务并启动，可附带下面任意参数 |
| `uninstall` | 停止并卸载服务 |
| `start` / `stop` / `restart` | 控制已安装的服务 |
| `status` | 查看服务状态与配置路径 |
| `run` | 前台运行，等价于不带命令 |
| `init-config` | 在可执行文件旁生成默认配置文件 |
| `version` | 打印版本号 |

### 运行参数

| 参数 | 说明 |
|---|---|
| `-site` | 分享站点首页地址，自动解析精选订阅 |
| `-button` | 页面上「复制精选订阅」按钮的文案，用于定位订阅地址 |
| `-articles` | 跟进「查看节点文章」的篇数，默认 2；0 表示只用精选订阅 |
| `-resolve` | 只解析并打印订阅地址，不执行探测 |
| `-url` | 订阅地址，多个用逗号分隔 |
| `-config` | YAML 配置文件路径，默认自动查找 |
| `-once` | 只探测一次后退出 |
| `-interval` | 定时探测间隔，如 `30m` |
| `-top` | 输出节点数量，默认 30 |
| `-serve` | 本地订阅服务监听地址，如 `:9898` |
| `-out` | 输出目录，默认 `out` |
| `-log` | 日志文件路径 |
| `-concurrency` | 真实探测并发，默认 96 |
| `-v` | 输出详细日志 |
| `-version` | 打印版本号 |

完整配置项见 `config.example.yaml`。程序默认遵循 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境变量，需要经代理访问时无需额外配置。

## 输出产物

| 文件 | 内容 |
|---|---|
| `out/best.yaml` | 可直接导入 Clash 的精简配置，仅含入选节点 |
| `out/best.txt` | 同一批节点的 base64 订阅，供 v2rayN / v2rayA / Shadowrocket 导入 |
| `out/best.links` | 未编码的分享链接，一行一个，便于人工查看 |
| `out/report.json` | 全部节点的逐次探测明细，含失败原因，用于排查 |
| `out/last-sub.yaml` | 最近一次成功拉取的完整节点列表，订阅站不可用时作为兜底缓存 |
| `out/resolved.json` | 最近一次从页面解析出的订阅地址，页面改版或站点临时不可用时作为兜底 |

`report.json` 中每条记录包含 `success`、`fail`、`avg_latency_ms`、`samples`（每轮的延迟或错误信息）以及 `skip_reason`（协议不支持等）。整轮零可用时会自动汇总失败原因分布，便于判断是订阅质量问题还是程序问题。

## HTTP 接口

| 路径 | 方法 | 说明 |
|---|---|---|
| `/best.yaml` | GET | Clash 格式订阅 |
| `/best.txt` | GET | v2ray 格式订阅（base64 分享链接） |
| `/best.links` | GET | 未编码的分享链接，一行一个 |
| `/status` | GET | 最近一次巡检的统计信息（JSON） |
| `/refresh` | POST | 立即触发一轮巡检 |

## 验证方式

协议实现不靠「能连上真实节点」自证——免费节点大量失效，连不上无法区分是节点问题还是实现问题。因此用独立实现作为对端做回环验证：

| 协议 / 传输 | 对端 | 用例 |
|---|---|---|
| SS：aes-128-gcm、aes-256-gcm、chacha20-ietf-poly1305 | go-shadowsocks2 | `TestSSLoopbackCiphers` |
| VMess：tcp、ws、wss | Xray | `TestXrayLoopback` |
| VLESS：tcp（明文）、tcp+tls、ws、wss | Xray | `TestXrayLoopback` |
| Trojan：tcp、ws | Xray | `TestXrayLoopback` |
| VMess KDF | Xray 官方实现的输出向量 | `TestVMessKDFVector` |

```bash
XRAY_BIN=/path/to/xray go test ./internal/proxy -run TestXrayLoopback -v
SS_SERVER_BIN=/path/to/go-shadowsocks2 go test ./internal/proxy -run TestSSLoopbackCiphers -v
```

产出侧另有一致性校验：把 `best.yaml` 转成 Xray 客户端配置，用各自的 socks 端口请求 `generate_204`。实测一轮 8 个产出节点中 7 个可用（失败的那个在探测阶段本身就是 1/3 成功率）。

> 注意：若环境变量 `NO_PROXY` 包含 `127.0.0.1`，`curl -x socks5h://...` 会被静默绕过，验证时必须显式加 `--noproxy ""`，否则得到的「连通」结论是假的。

## 设计取舍

**为什么自己在进程内实现协议，而不调用 mihomo 内核。** 调用内核需要额外分发二进制、管理子进程与 external-controller，部署成本高；而本工具只需要「能否建立一个到目标的 TCP 隧道」这一个能力，不涉及 UDP、分流规则、DNS 等复杂特性。自实现后，整个工具是单个可执行文件，无外部依赖。

**评分为什么以成功率优先、延迟其次。** 免费节点的延迟变化很大，但「能不能用」是更硬的指标。三轮复测中只成功一次的节点，即使那一次延迟很低，也不值得放进订阅——它会在真实使用中频繁掉线。

**为什么默认忽略节点侧 TLS 证书。** 免费节点普遍使用自签证书。若不忽略，大量本可正常中转的节点会在握手阶段被判失败。该行为不影响对「链路是否通」的判断，且输出配置中保留节点原有的 `skip-cert-verify` 设置。

## 已知限制

- 只探测 TCP 类代理，不涉及 UDP 转发能力；对需要 UDP 的场景（如游戏、部分 QUIC 应用）判断可能偏乐观。
- 探测结果为「当前网络环境」下的结论。换网络、换运营商后应重新巡检。
- 不实现 Shadowsocks 2022 系列加密、gRPC 与 xhttp 传输，这几类节点会被跳过。
- 不实现 VLESS 的 REALITY（需伪造特定 ClientHello 指纹）；这类节点占 VLESS 的一成多。
- VMess `alterId > 0` 的旧版（非 AEAD）实现不支持，这类节点会被跳过（本订阅中约 231 个）。
- SS 的流加密（aes-*-cfb、rc4-md5）按规范实现，但未找到可用的独立对端做回环验证，置信度低于 AEAD 系列。
- 并发默认 96，过高会触发部分节点的限流误判，不建议盲目调大。

## 目录结构

```
freeclash/
├── main.go                  入口、子命令（服务管理）、参数解析
├── internal/
│   ├── app/                 一次完整运行：日志、定时循环、结果落盘
│   ├── svc/                 系统服务封装（Windows SCM / systemd / launchd）
│   ├── logx/                带大小轮转的文件日志
│   ├── config/              配置结构、YAML 加载、路径解析
│   ├── proxy/               协议客户端
│   │   ├── proxy.go         Dialer 接口、地址编码、工厂
│   │   ├── ss.go            Shadowsocks（AEAD + 流加密）
│   │   ├── trojan.go        Trojan
│   │   ├── vmess.go         VMess AEAD + chunk stream
│   │   ├── vless.go         VLESS（裸字节流，TLS / WS / 明文）
│   │   └── ws.go            WebSocket 客户端（RFC 6455 子集）
│   ├── sub/                 订阅拉取、解析、过滤、配置生成
│   │   ├── sub.go           Clash 解析/生成、去重、地区识别
│   │   ├── sublink.go       分享链接解析（YAML / 明文 / base64 三种订阅形态）
│   │   └── links.go         分享链接渲染（ss / trojan / vmess / vless）
│   ├── site/                从分享站点解析订阅地址
│   │   ├── resolve.go       按「复制精选订阅」按钮定位订阅
│   │   └── articles.go      跟进「查看节点文章」，收集文章里的通用订阅
│   ├── probe/               TCP 预筛、隧道探测、排序
│   └── server/              本地 HTTP 订阅服务
├── Makefile                 构建与安装脚本
└── config.example.yaml
```
