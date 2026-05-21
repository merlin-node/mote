# mote — 极轻量服务器监控探针（v2.3 设计大纲）

> 主控 `zk` + 被控 `bk`，类似 Komari / 哪吒，但更轻、更省心
> GitHub: `merlin-node/mote`
> 当前状态：v2.3 已完成（拨测/详情弹窗/UI），v2.4 进行中（一键更新 ✅、用户名修改 API ✅ UI 待完成、迁移种子 待完成）

---

## 一、定位与目标

### 1.1 产品定位

填补 Komari 与哪吒之间的空白：**比 Komari 更轻量，比哪吒更省心**，专为自托管玩家（囤鸡党、NodeSeek/Hostloc 用户）设计。

### 1.2 性能目标

| 指标 | 目标 | 当前 |
|---|---|---|
| Agent 二进制 | < 3 MB（UPX 后） | 待测 |
| Agent 内存占用 | < 5 MB RSS | 待测 |
| Agent CPU 占用 | < 0.1% 空闲 | v2.1 已优化（sockstat 替代 fd 全扫描） |
| Server 内存 | < 30 MB | 待测 |
| 前端 JS 体积 | < 100 KB | 单文件 HTML ~30 KB |

### 1.3 核心原则

- **单二进制部署**：主控、被控各一个静态二进制，零依赖
- **WebSocket 通信**：单端口、过 CDN、过反代，启用 permessage-deflate 压缩
- **SQLite 默认存储**：零运维，够用即可
- **网页端管理业务，CLI 管理服务**：职责清晰
- **主控自带被控下载源**：被控装机不依赖 GitHub

---

## 二、技术栈

| 模块 | 技术 | 状态 |
|---|---|---|
| 被控 Agent | Go + gopsutil v3 + 标准库 net | ✅ |
| 主控 Server | Go + `net/http` ServeMux | ✅ |
| 数据库 | SQLite (modernc.org 纯 Go) | ✅ |
| WebSocket | gorilla/websocket + 压缩 | ✅ |
| 前端 | 原生 HTML + JS（单文件嵌入） | ✅ MVP |
| CLI 交互 | 标准库 + bufio | ✅ |
| 部署 | 单二进制 + systemd + Docker | ✅ |

**命名约定**：

- 主控命令：`zk`
- 被控命令：`bk`
- 服务名：`zk.service` / `bk.service`
- 默认监听：`:1888`

---

## 三、架构总览

```
┌──────────┐     WSS (443) 或 WS (1888)     ┌──────────────┐
│  bk #1   │ ─────────────────────────────── │              │
├──────────┤                                  │              │
│  bk #2   │ ─────────────────────────────── │    zk 主控   │ ── SQLite
├──────────┤                                  │              │
│  bk #N   │ ─────────────────────────────── │              │
└──────────┘                                  └──────┬───────┘
                                                     │
                                ┌────────────────────┼────────────────────┐
                                ↓                    ↓                    ↓
                          ┌───────────┐       ┌──────────┐       ┌──────────────┐
                          │ Web 面板  │       │ /install/│       │ 通知调度器   │
                          │ Basic Auth│       │ 公开下载 │       │ Telegram     │
                          └───────────┘       └──────────┘       └──────────────┘
```

---

## 四、监控指标 ✅

### 4.1 实时指标（秒级上报）

- **CPU**：总使用率、每核使用率、Load1/5/15
- **内存**：已用/总量、Swap 已用/总量
- **磁盘**：各挂载点已用/总量（按底层设备去重，bind mount / btrfs subvol 不重复计入）
- **网络**：每网卡上下行增量 + 速率（增量上报、Server 累加）
- **进程**：进程数（每周期）、TCP/UDP 连接数（**10 秒采样**，走 `/proc/net/sockstat`）
- **系统**：在线时长、内核版本、发行版、架构

### 4.2 采集优化 ✅

- 采集间隔默认 2 秒，可配置 1-10 秒
- 网卡过滤：默认排除 `lo / docker* / veth* / br-* / tun* / wg* / cni* / flannel* / cali* / kube* / podman* / nerdctl* / zt* / vmnet* / virbr* / dummy* / gre* / sit* / ip6tnl* / teql*`
- 磁盘过滤：仅真实文件系统，排除 tmpfs/overlay/squashfs/proc/sys 等；按 device 字段去重；单条上报 disks 数组上限 32 条（容器化宿主防御性保护）
- CPU 单次采样（修复了 v1 双采样导致每核失真的 bug，详见 CHANGES）
- **TCP/UDP 连接数**：优先读 `/proc/net/sockstat` 与 `sockstat6`，几乎零开销；缓存 10 秒；非 Linux 退回 gopsutil
- 网络回绕保护：counter 重置 / 大于 10GB 增量 → 跳过

### 4.3 协议 ✅

WebSocket Envelope 设计，所有消息统一外层：

```json
{ "type": "metric", "payload": { ... } }
```

**v2.1 协议变更**：

- `Envelope.Payload` 使用 `json.RawMessage`，主控/被控延迟到 handler 一次反序列化到位（避免 `marshal → unmarshal` 二次开销）
- hello / hello_ack 双向带 `protocol_version`，不匹配则断开并提示升级（旧 agent `protocol_version=0` 兼容放行）
- hello 新增 `machine_id`（`/etc/machine-id`）与 `primary_mac`，用于 AD 自动注册指纹去重

支持消息类型：`hello`、`hello_ack`、`metric`、**`metric_batch`**（重连补传）、`ping`、`pong`、`reconfig`、`uninstall`、**`bye`**（agent graceful shutdown）、`error`。

### 4.4 WebSocket Keepalive ✅ **v2.1 重写**

原 v2.0 用自定义 JSON 应用层 ping，没启用底层 keepalive。TCP 半开（NAT 静默丢包）时读循环会阻塞到天荒地老，agent 自以为在线但主控已经判离线。

v2.1 改造：

- 心跳协程发 `websocket.PingMessage`（标准 WS 控制帧）
- `SetPongHandler` / `SetPingHandler` 收到任何控制帧都刷新 read deadline
- read deadline = `心跳间隔 × 2.5`，最低 30 秒
- 任何应用层消息也刷新一次 deadline
- 为兼容旧主控/旧 agent，仍处理收到的应用层 `ping` → 回 `pong`

主控侧对称改造，read deadline 默认 75 秒（30s 心跳 × 2.5）。

### 4.5 重连补传 ✅ **v2.1 新增**

WS 写失败时 metric 不直接丢，塞进 60 槽 ring buffer（FIFO 覆盖最旧的）。重连后第一件事是发 `metric_batch`，主控循环按单条 metric 处理。Buffer 跨 `runOnce` 调用持久存在于 Agent 进程生命周期内。

---

## 五、存储设计

### 5.1 表结构（已实现）✅

| 表名 | 用途 |
|---|---|
| `nodes` | 节点本体（id、name、token、硬件信息、last_seen、**machine_id、primary_mac**） |
| `node_meta` | 节点元信息（备注、账单、流量配置/累计） |
| `metrics` | 最新指标（单条 per node，旧的覆盖）；**v2.1 加列存 cpu/mem_used/mem_total/load1** |
| `traffic_history` | 流量周期归档 |
| `notifiers` | 通知渠道 |
| `alert_rules` | 告警规则 |
| `alert_state` | 告警状态机（去抖/冷却） |

### 5.2 metrics 列存（为降采样铺路）✅ **v2.1 新增**

原 metrics 表只有 `(node_id, ts, data BLOB)`。BLOB 是完整 JSON，未来降采样要算"过去 24 小时 CPU 均值"必须反序列化每条出来再聚合，又贵又难走 SQL 索引。

v2.1 加了 `cpu`、`mem_used`、`mem_total`、`load1` 四个常用聚合列。`SaveMetric` 同时写 data blob 与这些列。降采样阶段 `AVG(cpu)` 直接走 SQL。

旧库平滑迁移：`ALTER TABLE ADD COLUMN` 若失败为 "duplicate column" 则忽略。

### 5.3 分层降采样 ✅ **v2.2 已实现**

三张表：`metrics`（原始秒级）、`metrics_1m`（1 分钟均值，保留 24h）、`metrics_5m`（5 分钟均值，保留 30d）。
调度器每分钟聚合一次。`GET /api/nodes/{id}/history?range=1h|24h|7d|30d` 按范围自动选表。

---

## 六、续费与账单

### 6.1 周期 ✅

| cycle | 含义 |
|---|---|
| `monthly` | 月付 |
| `quarterly` | 季付 |
| `semiannually` | 半年付 |
| `yearly` | 年付 |
| `biennially` | 两年付 |
| `once` | 一次性 |
| `lifetime` | 终身（永不到期） |

月底加月边界处理 ✅：`addMonths(1月31日, 1) = 2月28日`。

### 6.2 到期提醒 ✅

由"告警规则"系统覆盖（kind=due），见 §十一。
默认 7/3/1 天，可自定义任意天数列表（如 30,7,3,1）。


---

## 七、流量管理

### 7.1 四种统计模式 ✅

| type | 含义 | 用量计算 |
|---|---|---|
| `sum` | 双向合计 | in + out |
| `max` | 取大单向 | max(in, out) |
| `out` | 仅出站 | out |
| `in` | 仅入站 | in |

### 7.2 周期重置 ✅

每天 10 分钟扫一次，到 `traffic_reset_day` 当天 00:00（指定时区）自动归档历史 + 清零。
时区感知，默认 `Asia/Shanghai`。

### 7.3 Agent 增量上报 ✅

不依赖 `/proc/net/dev` 累计值。每次采集发增量（dIn/dOut），Server 累加并持久化，Agent 重启无影响。
回绕和异常增量（>10GB）自动跳过。

### 7.4 阈值告警 ✅

50% / 80% / 95% / 100% 各推一次（由"告警规则"系统覆盖）。
状态机以 `p<period_start>-<stage>` 为 scope，周期一换自动重走一轮。

---

## 八、备注系统 ✅

纯文本备注，用户输入什么显示什么。

- 节点编辑页一个 textarea，支持换行
- 卡片名称下方小灰字，`white-space: pre-line` 渲染换行
- 单条 500 字内
- HTML 转义防注入
- emoji 自然支持
- `note_visible=0` 时只管理页可见

---

## 九、CLI 设计 ✅

### 9.1 主控 `zk`

```bash
zk              # 交互式菜单
zk status       # 查看状态、端口、健康检查
zk start        # 启动 systemd 服务
zk stop         # 停止
zk restart      # 重启
zk uninstall    # 卸载（清理 systemd + /etc/zk + /var/lib/zk）
zk version      # 版本
```

### 9.2 被控 `bk`

```bash
bk              # 交互式菜单
bk status       # 查看连接状态、主控连通性
bk start/stop/restart
bk reconfig     # 修改主控地址 / Token
bk uninstall    # 本地卸载
bk version
```

### 9.3 交互式菜单 ✅

显示状态概览 + 数字键操作（1-6）。

### 9.4 一键更新 `zk update` 🔜 **v2.4 计划**

`zk` 交互菜单新增"更新"选项：从 GitHub Releases 下载最新二进制 → SHA256 校验 → 备份旧版本 → 替换 → 健康检查；检查失败自动换回旧版本。
DB schema 只前进不回退，改动向后兼容。

---

## 十、卸载与生命周期

### 10.1 卸载四层覆盖

| 场景 | 方式 | 状态 |
|---|---|---|
| 主控在线、统一卸载所有被控 | 网页"全员卸载" | ✅ |
| 主控在线、单机卸载 | 网页节点"远程卸载"按钮 | ✅ |
| 主控离线、有 Agent 机器 | `bk uninstall` 命令 | ✅ |
| 主控离线 + Agent 异常 | 手动 systemctl + rm | 文档说明 |

### 10.2 自毁脚本安全性 ✅

被控收到 `uninstall` WS 消息后：

- 用 `os.CreateTemp` 写一个随机文件名的自毁脚本（修复了 v1 用 `/tmp/bk-uninstall.sh` 固定名的竞态漏洞）
- **v2.1**：用 `Setsid` 起新 session，`cmd.Process.Release()` 解除父子关系
- 脚本里 `sleep 5`（v2 是 2 秒，太乐观）
- **v2.1**：systemd unit 改 `Restart=on-failure`，agent `Exit(0)` 后不会被拉起跟自毁脚本打架

### 10.3 长期失联自我休眠 ✅

被控连续重连失败超过 1000 次（约半小时），进入低频重试（每小时一次），不爆盘不爆 CPU。

**v2.1 修复**：原 v2.0 `failCount++` 在每次连接结束后无条件执行，长跑几天后任何一次网络抖动都会把 agent 推进低频重试。现在：

- 只在 `runOnce` 返回非 nil 时递增
- 一次连接存活超过 60 秒视为健康连接，断开后清零
- 第一次失败立即重连（网络抖动通常瞬时恢复），之后 1/2/4/8/... 秒指数退避（上限 5 分钟）

### 10.4 Graceful Shutdown ✅ **v2.1 新增**

Agent 监听 SIGTERM / SIGINT，触发 root context 取消。所有 goroutine 退出前向主控发一帧 `bye`，再写 WS Close 控制帧。主控的 reader 收到 `bye` 即知节点下线，不必等 `last_seen` 过期再判离线（默认 180 秒）。

systemd unit 配套：

- `KillMode=mixed`：先给主进程发 SIGTERM
- `TimeoutStopSec=15`：留 15 秒让 agent 把 bye 帧送出去
- `Restart=on-failure`：不要在 graceful 退出后自动拉起

---

## 十一、告警通知系统 ✅

### 11.1 告警类型

| kind | 触发条件 | threshold | duration |
|---|---|---|---|
| `offline` | 节点失联超 N 秒 | 失联秒数（默认 180） | 0 |
| `online` | 节点首次上线 | - | - |
| `cpu` | CPU 使用率 ≥ 阈值且持续 N 秒 | 百分比 | 持续秒数 |
| `mem` | 内存使用率 ≥ 阈值且持续 N 秒 | 百分比 | 持续秒数 |
| `disk` | 任一挂载点 ≥ 阈值 | 百分比 | 0（单点判定） |
| `load` | Load1 ≥ 阈值且持续 N 秒 | 数值 | 持续秒数 |
| `traffic` | 周期流量到 50/80/95/100% | 固定 | 每阶段一次 |
| `due` | 续费到期前 N 天 | 0 | 0 |

### 11.2 控制项

- **target**：`all` / `node:1,2,3` / `tag:foo`，按 ID 或标签筛选
- **notifier_ids**：JSON 数组，一条规则可推多个渠道
- **cooldown**：同节点同规则两次推送的最小间隔（秒，默认 1800）
- **silent_from/to**：HH:MM 静默时段，支持跨天（`22:00`-`08:00`）
- **enabled**：一键停用

### 11.3 通知渠道

| 渠道 | 状态 |
|---|---|
| Telegram Bot | ✅ |
| Email (SMTP) | 🔒 保留，不在近期路线图，等待启动指令 |

### 11.4 状态机 ✅

`alert_state` 表以 `(rule_id, node_id, scope)` 为主键：

- 持续型规则（CPU/内存/负载）：scope 为空，记录 first_breach
- 阶段型规则（流量 50/80/95/100）：scope=`p<period_start>-<stage>`，周期一换自动重新走一轮
- 续费规则：scope=`due<next_due>-<day>`，下次续费后自动重新触发

### 11.5 恢复通知 ✅

持续型规则在状态从越界变回正常时，自动发一条 🟢 "已恢复" 通知。

### 11.6 评估频率

Scheduler 分两层：

- **1 分钟**：跑 `AlertEngine.Tick()`，扫所有规则
- **10 分钟**：流量重置 / 续费滚动 / 清理过期 metrics

### 11.7 Telegram 接入

1. TG 找 `@BotFather` → `/newbot` → 拿 token
2. 把 bot 拉进群或私聊，访问 `https://api.telegram.org/bot<TOKEN>/getUpdates` 拿 chat_id
3. 面板 → 🔔 通知渠道 → 填 token + chat_id → 保存 → 测试推送
4. 面板 → ⚠ 告警规则 → 新建规则，勾选刚才那个渠道

支持群话题（thread_id）和 HTML 格式消息。

---

## 十二、节点身份与去重 ✅ **v2.1 新增**

### 12.1 两种鉴权方式

| 方式 | 说明 | 使用场景 |
|---|---|---|
| Token | 主控生成 token，每节点独有 | 标准方式，面板"添加节点"流程 |
| AutoDiscovery Key | 全局共享密钥，节点自报家门后主控自动建 node | 批量铺设、CI/CD、Ansible |

### 12.2 机器指纹去重

AutoDiscovery 流程下，同一台机器反复 `bk uninstall` + reinstall 会产生多个"unnamed"节点。

v2.1 加入指纹去重：

1. agent hello 上报 `machine_id`（`/etc/machine-id`）和 `primary_mac`（第一块物理网卡 MAC）
2. 主控收到 AD 注册请求时，先按 `machine_id` 精确匹配现有节点；再按 `primary_mac` 退化匹配
3. 命中则复用原 node ID（保留原 token、备注、账单等元信息）
4. 不命中才新建

查重 + 新建放在 `Store.FindOrCreateNodeByFingerprint` 同一 `s.mu` 互斥块内，并发同指纹不会建出两条。

### 12.3 AutoDiscovery 速率限制 ✅

每 IP 每分钟最多 5 次注册尝试。`X-Real-IP` / `X-Forwarded-For` 解析时 trim 空格（v2.1 修复，原版 `"a "` 与 `"a"` 会被当成不同 IP）。

---

## 十三、拨测（延迟检测） ✅ **v2.3 已实现**

### 13.1 架构

由被控（bk）分布式执行探测，主控下发目标配置，被控上报结果：

```
管理员配置目标 → zk 推送 probe_config → bk 每 60s 探测 → bk 推送 probe_result → zk 存库
```

### 13.2 探测类型

| 类型 | 实现 | 前提 |
|---|---|---|
| ICMP | 原始 socket `ip4:icmp`，手写 Echo Request + 校验和 | bk 以 root 运行（默认如此） |
| TCP | `net.DialTimeout("tcp", ip:port, timeout)` | 无额外要求 |
| Both | 同时执行 ICMP 和 TCP | — |

### 13.3 数据存储

| 表 | 内容 | 保留时长 |
|---|---|---|
| `probe_targets` | 目标配置（名称/IP/端口/协议/指定节点） | 永久 |
| `probe_results` | 探测结果（target_id/node_id/ts/proto/latency_ms/success） | 7 天（调度器自动清理） |

### 13.4 前端

面板右上角 📡 探针按钮打开管理弹窗：

- 目标列表：ICMP/TCP 徽章、已分配节点 Tag
- 新建/编辑：填名称、IP、端口、协议、勾选参与探测的节点
- 查看结果：按节点展示延迟折线图（1h / 6h / 24h / 7d / 30d）

节点详情弹窗右栏底部同步展示该节点参与的拨测结果。


---

## 十四、迁移机制 🔜 **v2.4 计划**

### 14.1 使用场景

旧主控换机器或换域名时，无需手动到每台被控运行 `bk reconfig`。

| 场景 | 方法 |
|---|---|
| 同域名换服务器 | 迁移数据 + 改 DNS，被控零改动 |
| **换域名 / 换 IP** | 迁移种子机制（本节） |
| 仅保留节点信息 | 导出配置 + 重装被控 |

### 14.2 迁移种子机制

面板设置"迁移"按钮 → 填入新主控地址（支持 `wss://新域名` 或 `ws://IP:端口`）→ 一键推送给所有在线被控。

被控收到后：
1. 将新地址写入本地 config（`migration_url`、`migration_fallback_after`）
2. 继续连接当前主控
3. 当前主控失联超过 N 秒（默认 300s）后，自动用新地址重连
4. 连接新主控成功后清除迁移记录

**安全**：推送消息带 HMAC 签名（密钥 = 被控 Token），防止中间人伪造迁移指令。

协议常量 `MsgTypeMigration` 已在 `shared/protocol.go` 中定义。

---

## 十五、备份 ✅

面板设置 → 备份/恢复：一键导出 JSON（含节点、账单、告警规则、通知渠道，Token 原样保留），一键导入。
完整数据备份：直接 `cp /var/lib/zk/data.db /backup/`。

---

## 十六、部署 ✅

### 16.1 主控一键安装

```bash
sudo bash scripts/install-zk.sh
```

完成：下载二进制 → `/usr/local/bin/zk` → 创建 `/etc/zk/config.json` 和 `/var/lib/zk/` → 注册 systemd → 启动 → 输出登录信息。

### 16.2 被控一键安装（**主控自带下载源**） ✅

不再依赖 GitHub：

```bash
curl -fsSL http://主控/install-bk.sh | sudo bash -s -- -t TOKEN
```

主控会在三个位置依次寻找二进制：

1. `<DataDir>/dist/bk-linux-${ARCH}`（默认 `/var/lib/zk/dist/`）
2. `./dist/bk-linux-${ARCH}`
3. `/usr/local/share/zk/dist/bk-linux-${ARCH}`

**部署步骤**：

```bash
sudo mkdir -p /var/lib/zk/dist
sudo cp dist/bk-linux-amd64 dist/bk-linux-arm64 /var/lib/zk/dist/
```

`/install-bk.sh` 路径绕过 Basic Auth，小鸡能匿名拉。

### 16.3 systemd unit 配置 ✅ **v2.1 调整**

```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/bk run -c /etc/bk/config.json
Restart=on-failure        # ← 不是 always:agent 收到 uninstall 后 Exit(0) 不该重启
RestartSec=5
KillMode=mixed            # ← systemctl stop 时先给主进程发 SIGTERM
TimeoutStopSec=15         # ← 留 15 秒让 agent 发 bye 帧给主控
StandardOutput=append:/var/log/bk/bk.log
StandardError=append:/var/log/bk/bk.log
```

### 16.4 Docker 部署 ✅ 已实现

多阶段构建（`golang:1.22-alpine` → `debian:12-slim`），纯 Go 无 CGO，支持 `linux/amd64` + `linux/arm64`。
镜像发布到 `ghcr.io/merlin-node/mote:latest`，打 `v*` tag 由 GitHub Actions 自动构建推送。
默认监听端口 `:1888`，compose 绑定 `127.0.0.1:1888:1888` 仅本机可达，配合 Nginx/Caddy 反代使用。

### 16.5 反向代理（Nginx / Caddy） ✅ 文档

Nginx 模板已写在 README。Caddy 一行 `reverse_proxy 127.0.0.1:1888` 即可。
主控通过 `r.Host` 和 `r.TLS` 自动判断 HTTPS/HTTP，安装命令自动生成 `wss://` 或 `ws://`。

---

## 十七、安全 ✅ 已加固

### 17.1 已修

- ✅ Basic Auth 改用 `crypto/subtle.ConstantTimeCompare`（防时序攻击）
- ✅ AutoDiscovery 注册按 IP 限流（每 IP 每分钟 5 次）
- ✅ 正确解析 `X-Real-IP` / `X-Forwarded-For`（反代后限流不至于把所有人当一个 IP），**v2.1 加 trim 空格**
- ✅ `/api/install-cmd` 改 POST + JSON body（token 不再走 URL，避免 Nginx log 泄露）
- ✅ 自毁脚本用 `os.CreateTemp`（防 `/tmp` 竞态）
- ✅ Telegram 消息 HTML 转义（防 parse_mode 注入）
- ✅ panel_title 长度上限 60 字符，prompt 输入做了清洗
- ✅ **v2.1**：`/etc/bk` 目录 0700，agent config.json 原子写入（.tmp + rename）

### 17.2 v2.2 加固

- ✅ Cookie 会话登录（不再每次发密码）
- ✅ 操作审计日志（actor / IP / action，保留 90 天）
- ✅ 面板改密码（POST /api/change-password，同时吊销所有 session）
- ✅ 2FA TOTP（RFC 6238，纯标准库）

---

## 十八、网页面板 ✅

### 18.1 功能

- ✅ 节点卡片网格，自适应宽度
- ✅ 添加节点 → 生成安装命令 → 复制
- ✅ 编辑节点（名称、备注、账单、流量）
- ✅ 删除节点 / 远程卸载
- ✅ 自动刷新（3 秒一次）
- ✅ 在线/离线状态指示灯
- ✅ 通知渠道管理（增删改查 + 测试推送）
- ✅ 告警规则管理（增删改查 + 启停）
- ✅ **面板标题可在网页内修改并持久化**

### 18.2 v2.3 新增

- ✅ 节点详情弹窗（点击卡片 → 两栏 Modal：实时指标 + 历史面积图 + 拨测结果）
- ✅ 历史曲线：四张面积图（CPU / 内存 / 网络入 / 出），1h / 24h / 7d / 30d Tab 切换
- ✅ 移动端双断点响应式（768px / 480px），手机浏览器体验完整
- ✅ 全量主题重构：深色 / 浅色 / 蓝色，CSS 自定义属性驱动
- ✅ 探针管理弹窗（目标 CRUD + 节点分配 + 结果折线图）
- ✅ 节点卡片 hover 动效 + 在线状态呼吸动画


---

## 十九、关键差异化总结

| 能力 | 哪吒 | Komari | **mote** |
|---|---|---|---|
| Agent 内存 | ~15MB | ~10MB | < 5MB（目标） |
| Agent CPU | 普通 | 普通 | **v2.1 sockstat 替代 fd 扫描** |
| WS 压缩 | ⚠ | ⚠ | ✅ permessage-deflate |
| WS 标准 keepalive | ⚠ | ⚠ | ✅ Ping/Pong 控制帧 + read deadline |
| 重连补传 metric | ❌ | ❌ | ✅ 60 槽 ring buffer |
| Graceful Shutdown | ⚠ | ⚠ | ✅ SIGTERM 发 bye |
| AD 指纹去重 | ❌ | ❌ | ✅ machine_id + MAC |
| 续费提醒 | ❌ | ⚠ 静态展示 | ✅ 7/3/1 自定义天数 |
| 流量时区感知 | ⚠ | ⚠ | ✅ |
| 流量四种模式 | ⚠ | ⚠ | ✅ 全支持 |
| 告警类型 | 有限 | 有限 | ✅ 8 类，可自定义 |
| TG 告警 | ✅ | ✅ | ✅ + 群话题 |
| 静默时段 | ⚠ | ⚠ | ✅ 跨天 HH:MM |
| 主控自带被控下载源 | ❌ | ❌ | ✅（不依赖 GitHub） |
| 网页改面板名 | ❌ | ❌ | ✅ |
| 分布式拨测（ICMP/TCP） | ❌ | ✅ HTTP | ✅ **v2.3 新增** |
| 节点详情弹窗 | ✅ | ✅ | ✅ **v2.3 新增** |
| 迁移种子机制 | ❌ | ❌ | 🔜 v2.4 计划 |
| CLI 一键更新 | ⚠ | ⚠ | 🔜 v2.4 计划 |
| 长期失联自我休眠 | ❌ | ❌ | ✅ |

---

## 二十、开发路线图

### Phase 1：MVP ✅ 完成

- ✅ 被控基础采集 + WS 上报
- ✅ 主控 WS Hub + SQLite 存储
- ✅ Web 面板（节点列表 + 实时数据）
- ✅ 一键安装脚本
- ✅ `zk` / `bk` 基础 CLI

### Phase 2：业务核心 ✅ 完成

- ✅ 账单管理（续费周期、自动算到期）
- ✅ 流量管理（四种模式、月度重置、阈值检测）
- ✅ 备注系统
- ✅ 告警通知（Telegram，含 8 类规则、状态机、静默时段、恢复通知）
- ❌ 多货币折算（推迟）
- ❌ 历史曲线 + 降采样（**下一步重点，列存已就位**）

### Phase 2.5：探针稳定性大修 ✅ **v2.1 完成**

- ✅ Envelope 改 RawMessage，去掉冗余编解码
- ✅ 协议版本握手 + 兼容旧 agent
- ✅ 标准 WS Ping/Pong 控制帧 + read deadline
- ✅ failCount 健康连接重置 + 首次 0 秒重连
- ✅ TCP/UDP 走 sockstat 低开销 + 10s 缓存
- ✅ 磁盘按 device 去重 + 32 条上限
- ✅ machine_id / MAC 指纹 AD 去重
- ✅ metric ring buffer 重连补传
- ✅ SIGTERM graceful shutdown + bye 帧
- ✅ Agent reconfig 持久化
- ✅ 主控 hello_ack 用 config 配置而非硬编码
- ✅ metrics 表列存（cpu/mem_used/mem_total/load1）

### Phase 3：运维体验 ✅ 完成

- ✅ 备份导出/导入（JSON）
- ✅ 拨测（ICMP/TCP） **v2.3 完成**
- ✅ 历史曲线（CPU/内存/网络，1h~30d，面积图） **v2.3 完成**

### Phase 4：v2.4 路线图

- ✅ `zk update` 一键更新 + 自动回滚（`cmd/zk/update.go`）
- ✅ 管理员用户名修改 API（`POST /api/change-username`，UI 待完成）
- 🔜 迁移种子机制（换域名/换 IP 无缝迁移，协议+agent+API+UI 待完成）
- ✅ 长期失联自我休眠

### Phase 5：加分项（已完成）

- ✅ 主题系统（深色/浅色/蓝色）
- ✅ 移动端响应式优化（双断点）
- ✅ Docker 镜像（ghcr.io/merlin-node/mote，多架构）
- ✅ Cookie/JWT 登录 + 修改密码 + 2FA
- 🔒 Email 通知渠道（保留，待启动指令）

---

## 二十一、当前能用 vs 不能用

### ✅ 现在可以做的事

- 装主控、装多个被控、实时看 CPU/内存/磁盘/流量
- 点击节点卡片查看详情弹窗（实时指标 + 历史曲线 + 拨测结果）
- 查看过去 1h / 24h / 7d / 30d 的 CPU / 内存 / 网络历史曲线
- 配置拨测目标（ICMP/TCP），勾选由哪些被控节点发起，查看延迟历史
- 在网页改面板名、改节点备注、配账单
- 设 TG Bot 告警：节点离线、CPU/内存高、磁盘满、流量到 80%、续费前 3 天
- 静默时段，比如夜里 22:00-08:00 不打扰
- 远程卸载被控（点按钮）
- 改名字、改主控、reconfig
- 深色 / 浅色 / 蓝色主题随时切换
- **v2.1**：同机器反复 reinstall 不会产生重复节点（指纹去重）
- **v2.1**：systemctl stop bk 时主控立刻知道下线（不必等 180 秒）
- **v2.1**：长时间网络抖动后 agent 不会被推进低频重试再也叫不醒

### 🔜 v2.4 进行中

- ✅ 一键更新（`zk update` 已完成）
- ✅ 管理员用户名修改（API 已完成，UI 待完成）
- 🔜 迁移种子机制（换主控域名/IP，不需要手动 reconfig）

### 🟡 兜底方案

- 历史数据：手动 `cp /var/lib/zk/data.db /backup/`
- 升级：手动停服务 → 替换二进制 → 启服务

---

## 二十二、文档需要重点写的（待补）

1. **强烈推荐用域名 + Caddy/Nginx 反代**（决定未来迁移难度，且能上 HTTPS/WSS）
2. **本地卸载命令 `bk uninstall`**（主控挂了时的兜底）
3. **Telegram Bot 获取 chat_id 的两种姿势**（私聊 vs 群、群话题）
4. **告警规则编辑界面的 8 种 kind 区别和典型配置**
5. **流量统计模式如何选择**（按 VPS 商家计费方式）
6. **续费时区配置**（避免跨日期边界出错）
7. **如何把 `dist/bk-linux-*` 放到主控的下载目录**（被控装机必读）
8. **`/etc/machine-id` 缺失或被克隆时的行为**（容器/克隆 VM 场景下指纹去重失效，需手动改 token）

---

*v2.4 设计稿 · 2026-05 · v2.4 进行中：一键更新 ✅、用户名修改 API ✅、迁移种子 🔜*
