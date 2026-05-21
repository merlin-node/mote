# mote — 修改记录

---

# v2.2 Docker 支持 + 端口统一（2026-05）

## 默认监听端口改为 1888

`internal/server/config.go` `applyDefaults()` 中默认端口从 `:25774` 改为 `:1888`。
同步更新 `scripts/install-zk.sh` 回退端口、README 及设计文档全部引用。

## Docker 支持

- 新增 `Dockerfile`：多阶段构建，`golang:1.22-alpine` 编译，`debian:12-slim` 运行，纯 Go 无 CGO，支持 `linux/amd64` / `linux/arm64`。
- 新增 `docker-compose.yml`：端口绑定 `127.0.0.1:1888:1888`（仅本机可达），配置/数据目录挂载为 `./data/config` / `./data/db`。
- 新增 `.github/workflows/docker.yml`：推送 `v*` tag 时自动构建多架构镜像并推送至 `ghcr.io/merlin-node/mote`。
- 新增 `.github/workflows/release.yml`：推送 `v*` tag 时自动编译 `zk` / `bk` 四个平台二进制并发布到 GitHub Releases。
- Docker 部署下 `zk` 命令通过 `docker exec -it mote-zk zk` 调用，建议在宿主机添加 alias：
  ```bash
  echo "alias zk='docker exec -it mote-zk zk'" >> ~/.bashrc && source ~/.bashrc
  ```

---

基于原 `probe` 项目重构。本次改动只动 Go 代码与脚本,不改架构、协议、表结构。

## 1. 重命名

- Go module: `probe` → `mote`(`go.mod` 与全部 `import "probe/..."`)
- 安装脚本默认仓库: `your-org/probe` → `merlin-node/mote`
- systemd 单元 Description 同步更新
- README 标题改为 `mote`
- 二进制 / CLI / systemd 服务名保持 `zk` / `bk` 不变
- 配置目录 `/etc/zk` / `/etc/bk`、数据目录 `/var/lib/zk` 保持不变

## 2. Bug 修复

### `internal/agent/runner.go` — reconfig 真正生效
原代码用 `*int` 在读循环里改值,但 ticker 在 goroutine 内 `time.NewTicker(time.Duration(interval)*time.Second)`
已经按旧值跑起来了,改变量对正在运行的 ticker 无效。
现在通过 `chan int` 通知采集/心跳协程 stop + 重建 ticker,新间隔立即生效。

### `internal/agent/collector.go` — CPU 双采样导致每核失真
原代码先 `cpu.Percent(0, false)` 再 `cpu.Percent(0, true)`,
gopsutil 的 0 间隔模式基于"距上次调用"的差值,两次连调让"每核"采样间隔约等于零,
读出来的 per-core 数据基本不可信。
现在只调一次 per-core,整体 CPU 由各核取平均得到。

### `internal/server/hub.go` — SendTo 错误类型混用
原代码缓冲满时也返回 `ErrNotFound`,API 调用方会把缓冲压力误报成"节点离线"。
新增 `ErrNodeOffline` / `ErrSendBufferFull` 两个独立错误,
`api.go` 的 `opUninstall` 区分返回:离线 → 400,缓冲满 → 503。

### `internal/agent/runner.go` — backoff 移位溢出保护
原代码 `1 << min(n, 6)` 在 `n` 极大时若编译器优化不当仍可能有边界问题,
显式 clamp `shift` 到 0..6 范围内。

## 3. 安全加固

### Basic Auth 改用 constant-time 比较(`api.go`)
原代码 `u != cfg.AdminUsername || p != cfg.AdminPassword` 是普通字符串比较,
理论上可以通过响应时间差异爆破密码。改用 `crypto/subtle.ConstantTimeCompare`。
长度不一致时仍做一次比较,避免长度信息本身泄露。

### `/api/install-cmd` 改为 POST + body(`api.go`)
原代码 token 走 URL query,会写进 Nginx access.log、CDN 日志、浏览器历史。
改为只接受 POST,token 走 JSON body;
或者只传 `node_id`,服务端从 store 取 token,前端不需要二次保存。

### AutoDiscovery 注册限流(`hub.go`)
原代码只要 AD key 匹配就 `CreateNode`,AD key 泄露后攻击者可以批量灌爆节点表。
新增 `adRateLimiter`:每 IP 每分钟最多 5 次注册,超出直接关连接。
正确解析 `X-Real-IP` / `X-Forwarded-For`(因为前面有 Nginx),
避免反代后所有请求都来自 127.0.0.1 被同一个 bucket 拦截。

### 自毁脚本临时文件竞态(`uninstall.go`)
原代码硬编码写到 `/tmp/bk-uninstall.sh`,虽然 `0700` 但 `/tmp` 是世界可读目录,
且 `sleep 2` 给了攻击者预测路径并通过符号链接挟持的窗口。
改用 `os.CreateTemp("", "bk-uninstall-*.sh")` 生成不可预测的随机文件名,
任一步骤失败均清理临时文件。

## 4. 未触及的部分

下列设计文档里提到但 MVP 没实现的能力,本次没有补,因为它们不属于"完善脚本",
属于新增模块:

- 历史曲线 + 降采样(1s/1m/5m 三张表)
- 告警通知、拨测、备份导入导出、迁移种子、一键更新

如果要补,建议作为独立 PR 分别提交。

## 5. 新增功能(v2 改动)

### 主控自带 `/install-bk.sh` 与二进制下载
不再依赖 GitHub releases。主控启动后:
- `GET http://主控/install-bk.sh` → 返回注入了主控 URL 的安装脚本
- `GET http://主控/install/bk-linux-amd64` → 返回被控二进制
- `GET http://主控/install/bk-linux-arm64` → 同上

主控会在以下三个位置寻找二进制(顺序):
1. `<DataDir>/dist/bk-linux-${ARCH}`(默认 `/var/lib/zk/dist/`)
2. `./dist/bk-linux-${ARCH}`(主控的当前工作目录)
3. `/usr/local/share/zk/dist/bk-linux-${ARCH}`

部署建议:
```bash
sudo mkdir -p /var/lib/zk/dist
sudo cp dist/bk-linux-amd64 dist/bk-linux-arm64 /var/lib/zk/dist/
sudo chown -R root:root /var/lib/zk/dist
```

之后小鸡装机:
```bash
curl -fsSL http://主控/install-bk.sh | sudo bash -s -- -t TOKEN
```
不需要参数 `-s`,脚本里已注入主控地址作为默认值。

这些路径不需要 Basic Auth(否则 curl 拉不到)。

### 网页可改面板标题
- 默认标题:`mote 监控面板`
- 点 header 上的标题 → 弹 prompt → 输入新名 → PATCH `/api/config` → 写入 `/etc/zk/config.json` 的 `panel_title` 字段
- 长度上限 60 字符
- 留空恢复默认

### `/api/config` 加 PATCH 方法
支持 `panel_title` 和 `main_currency` 两个字段的就地修改。GET 不变。

## 6. 告警与 Telegram 通知

### 新增表
- `notifiers` — 通知渠道(目前 type='telegram',config 为 JSON)
- `alert_rules` — 告警规则
- `alert_state` — 告警状态机(去抖、冷却、关联恢复)

### 告警类型
| kind | 触发条件 | threshold 用法 | duration 用法 |
|---|---|---|---|
| `offline` | 节点失联超 N 秒 | 失联秒数(默认 180) | 0 |
| `online` | 节点首次上线 | - | - |
| `cpu` | CPU 使用率 ≥ 阈值且持续 N 秒 | 百分比 | 持续秒数 |
| `mem` | 内存使用率 ≥ 阈值且持续 N 秒 | 百分比 | 持续秒数 |
| `disk` | 任一挂载点 ≥ 阈值 | 百分比 | 0(单点判定) |
| `load` | Load1 ≥ 阈值且持续 N 秒 | 数值 | 持续秒数 |
| `traffic` | 周期流量到 50/80/95/100% | 固定 | 0(每个阶段一次) |
| `due` | 续费到期前 N 天(可自定义 days) | 0 | 0 |

### 控制项
- **target**: `all` / `node:1,2,3` / `tag:foo`,按节点 ID 或标签筛选
- **notifier_ids**: JSON 数组,一条规则可同时推多个渠道
- **cooldown**: 同节点同规则两次推送的最小间隔(秒,默认 1800)
- **silent_from/to**: HH:MM 静默时段,支持跨天(`22:00`-`08:00`)
- **enabled**: 一键停用

### 告警评估
Scheduler 现在分两个 ticker:
- **1 分钟**: 跑 `AlertEngine.Tick()`,扫描所有规则
- **10 分钟**: 原有的流量重置 / 续费滚动 / 清理 metrics

### Telegram 接入步骤
1. TG 找 `@BotFather` → `/newbot` 创建 bot,记下 token
2. 把 bot 拉进群(或私聊),获取 chat_id:
   `https://api.telegram.org/bot<TOKEN>/getUpdates`
3. 面板点 🔔 通知渠道 → 填 token 和 chat_id → 保存 → 测试推送
4. 面板点 ⚠ 告警规则 → 新建规则,勾选刚才的通知渠道

### 续费天数自定义
`due` 类型规则的 extra JSON 字段可填 `{"days":[30,7,3,1]}`,
不填则默认 `[7, 3, 1]`。
面板里直接输入 `30,7,3,1` 即可,前端自动拼成 JSON。

### 去抖语义
- 状态机表 `alert_state` 以 `(rule_id, node_id, scope)` 为主键
- 持续型规则(CPU/内存/负载等):scope 为空,记录 first_breach
- 阶段型规则(流量 50/80/95/100):scope 为 `p<period_start>-<stage>`,周期一换自动重新走一轮
- 续费规则:scope 为 `due<next_due>-<day>`,下次续费后自动重新触发

### 恢复通知
持续型规则(offline / cpu / mem / disk / load)在状态从越界变回正常时,
会发一条"已恢复"通知,然后清状态。

### 测试通知渠道
`POST /api/notifiers/{id}/test` → 立刻推一条测试消息(独立于告警规则)。
面板上"测试推送"按钮就是用这个。

---

# v2.1 探针程序一轮回归优化(2026-05)

> 这次只动 agent 与主控 hub 中跟"探针"相关的部分,
> 不改业务面板逻辑、不改告警系统、不破坏旧数据库结构。
> DB 用 `ALTER TABLE ADD COLUMN` 平滑加列;旧 agent(无 protocol_version)兼容继续工作。

## 协议层

### `shared/protocol.go` — Envelope.Payload 改为 json.RawMessage
原本 `Payload any` 在反序列化后是 `map[string]any`,handler 还要再
`Marshal → Unmarshal` 一次拿强类型。每秒数百节点上报时这是冗余成本,
直接用 `RawMessage` 延迟到 handler 一次到位。
所有发送侧统一走新的 `shared.MakeEnvelope(msgType, payload)`,负责把 payload 预编码。

### `shared/protocol.go` — 加入 protocol_version 握手
hello / hello_ack 都带 `ProtocolVersion`。
不匹配时主控发 `error` 帧并断开,日志打印 agent 版本与 server 版本。
旧 agent 不带这个字段时 `ProtocolVersion=0`,放行(向后兼容)。

### `shared/protocol.go` — 加入 metric_batch 与 bye
- `metric_batch`:agent 重连后批量补传离线期间缓存的 metric
- `bye`:agent 收到 SIGTERM 时主动通知主控下线,避免等 last_seen 过期

## Agent 行为

### `runner.go` — 失败计数器只对失败递增,健康连接重置
原本 `failCount++` 在每次连接结束后无条件执行,长跑几天后
任何一次网络抖动都会把 agent 推进"每小时一次"的低频重试。
现在:`runOnce` 返回非 nil 才递增;一次连接存活超过 60 秒视为健康,
断开后清零。

### `runner.go` — backoff 序列从 1s 起步,首次失败立即重连
原本 `1 << n`(n=1 起)第一次重连就要等 2 秒。
现在 `shift = n-1`,序列 1, 2, 4, 8, 16, ..., 上限 5 分钟,
加 0~25% 抖动避免雪崩;`failCount == 1` 时直接 0 秒重连。

### `runner.go` — 用底层 WS Ping 控制帧 + Pong handler 做心跳
原本 agent 发自定义 JSON 应用层 ping,WS 库本身的 keepalive 没启用,
TCP 半开(NAT 静默丢包常见)时读循环可能阻塞到天荒地老。
现在:
- 心跳协程发 `websocket.PingMessage`(标准控制帧)
- `SetPongHandler` 与 `SetPingHandler` 收到任何控制帧都刷新 read deadline
- read deadline = `心跳间隔 × 2.5`,最低 30 秒
- 任何应用层消息也刷新一次
为兼容老主控,仍处理收到的应用层 `ping` → 回 `pong`。

### `collector.go` — TCP/UDP 连接数走 /proc/net/sockstat,10 秒采一次
原本每个采集周期(默认 2s)调一次 `gopsutil net.Connections("tcp"/"udp")`,
该实现要遍历所有 `/proc/*/fd` 与读 `/proc/net/tcp`,fd 多的机器单次几十毫秒。
现在:
- 优先读 `/proc/net/sockstat` 与 `sockstat6`,几乎零开销
- 缓存 10 秒,过期再采
- 非 Linux 或读取失败,退回 gopsutil(仍是低频)

### `collector.go` — 磁盘按底层设备去重
bind mount / btrfs subvol / loop 会让同一块盘出现多次,
原本 disk_total 与 disks 数组都会重复统计。现在按 `partition.Device`
去重,disks 数组同时设了 32 条上限(容器化宿主防御性保护)。

### `collector.go` — round2 用 math.Round
原本 `float64(int(f*100))/100` 对负数会向 0 截断;Load 是 float 可能略低于 0
(虽然实际几乎不会)。改为 `math.Round(f*100)/100`。

### `collector.go` — 机器指纹采集
`hello` 新增 `machine_id`(读 `/etc/machine-id`)与 `primary_mac`(标准库 `net.Interfaces` 取第一块物理网卡 MAC),
用于 AutoDiscovery 自动注册时去重(见下文)。
注:这两个字段仅在主控 AD 注册路径生效,token 路径不使用;
读不到不影响其他逻辑。

### `config.go` — NIC 默认黑名单扩展
追加 `cali|kube|podman|nerdctl|zt|vmnet|vnet|virbr|dummy|gre|sit|ip6tnl|teql`,
覆盖更多容器/虚拟化/隧道场景。
完整正则:`^(lo|docker|veth|br-|tun|tap|wg|cni|flannel|cali|kube|podman|nerdctl|zt|vmnet|vnet|virbr|dummy|gre|sit|ip6tnl|teql)`。
用户可在 `/etc/bk/config.json` 自行覆盖。

### `config.go` — 持久化主控下发的 interval / heartbeat
新增 `Config.SetRuntime(interval, heartbeat)`,在收到 hello_ack 或 reconfig
时调用,既更新内存值,也回写到 `config.json`(原子写 + 0600)。
重启 agent 后 reconfig 设置不丢。

### `runner.go` — metric 重连补传 ring buffer
WS 写失败时 metric 不直接丢,而是塞进 60 槽的 ring buffer
(满了 FIFO 覆盖最旧的)。重连后第一件事是发 `metric_batch`,
主控 handler 端循环把每条按 metric 处理。
buffer 跨 `runOnce` 调用保留(在 `Run` 里持有)。

### `runner.go` — SIGTERM graceful shutdown
监听 SIGTERM / SIGINT,触发 `rootCtx` 取消。所有 goroutine 退出前
向主控发一帧 `bye`,再写 WS Close 控制帧。主控的 reader 收到 bye 即知节点下线,
不必等 `last_seen` 过期再判离线。
systemd unit 配套加 `KillMode=mixed` + `TimeoutStopSec=15`。

### `uninstall.go` — Restart 改 on-failure + Setsid 脱钩
原 systemd unit 是 `Restart=always`,agent 收到 uninstall 后 `Exit(0)`
仍会被立刻拉起,与自毁脚本打架。改 `on-failure` 后 0 退出码不重启。
自毁脚本通过 `Setsid` 起一个新 session,`cmd.Process.Release()` 解除父子关系,
sleep 从 2 秒延长到 5 秒,先 stop 后再 daemon-reload。

## 主控 hub

### `hub.go` — 标准 WS keepalive + read deadline
对称 agent 端的改动:`SetPongHandler` / `SetPingHandler` 刷新 read deadline,
默认 30s/`心跳×2.5` 取大。原本主控无 read deadline,会一直等读。

### `hub.go` — AutoDiscovery 注册按指纹去重(原子)
原本同一台机器反复 reinstall 会用 AD key 各自注册新 node ID,
面板会出现一堆同名"unnamed"。
现在主控收到 hello 时:
1. 优先按 `machine_id` 精确匹配现有节点
2. 退化按 `primary_mac` 匹配
3. 都不命中才新建 node
查 + 建放在 `Store.FindOrCreateNodeByFingerprint` 内同一 `s.mu` 互斥块,
避免并发同指纹下建出两条。

### `hub.go` — clientIP 解析 trim 空格
`X-Forwarded-For: a , b` 这种带空格的写法,原本会把 `"a "` 当一个 IP,
影响限流(同一 IP 多种空格写法被当多个)。现在 trim 后再返回。

### `hub.go` / `config.go` — 主控统一下发 interval / heartbeat
原本 hello_ack 写死 `Interval=2, HeartbeatSec=30`。
现在主控 `config.json` 新增 `agent_interval` / `agent_heartbeat`,
所有 agent 上线都按这两个值下发。

### `store.go` — metrics 表加列存常用聚合字段
`ALTER TABLE metrics ADD COLUMN cpu REAL, mem_used INT, mem_total INT, load1 REAL`。
`SaveMetric` 同时写 data blob 与这些列。
为未来分层降采样(`1m` / `5m` 表)做准备:
聚合用 SQL `AVG(cpu)` 直接走列,不必反序列化 blob。
旧库平滑迁移:`ALTER` 失败时若是 "duplicate column" 则忽略。

### `store.go` — nodes 表加 machine_id / primary_mac 列
配套 AD 指纹去重。`UpdateNodeFingerprint` 仅在传入值非空时写,
不会因为某次 hello 漏读指纹就把已有的指纹清掉。

## WS 调优

- `Dialer` 与 `Upgrader` 显式设 `ReadBufferSize/WriteBufferSize = 8192`
  (默认 4096 对多核 + 多 mount 的机器单条 metric 可能超)
- 双向 `EnableCompression = true`,permessage-deflate 压缩 JSON,
  跨广域网体感显著(典型压缩比 ~70%)

## 安全 / 工程小项

- `agent/config.go` `Save()` 原子写(.tmp → rename),目录 0700
- `install-bk.sh` 同步 `chmod 700 /etc/bk`
- systemd unit `Restart=on-failure` + `KillMode=mixed` + `TimeoutStopSec=15`
- 去掉了 `errString` 自定义错误,改用 `fmt.Errorf` 带上下文

## 仍未做(本轮不在范围内)

- 历史曲线 + 分层降采样(列存已就位,降采样定时任务下一轮)
- 多货币换算
- `zk update` 一键更新
- S3/WebDAV 备份
- 拨测
- 迁移种子机制

---

# v2.2 功能完善（2026-05）

> 本轮在 v2.1 基础上补完前端鉴权体验、协议细节与告警引擎若干遗漏点。
> 不新增依赖库（尤其：无第三方图表/TOTP/QR 库），不新增 webhook/email 等通知渠道。
> 通知渠道保持仅 Telegram；全程中文界面。

## 前端

### Cookie 会话登录 + 公开只读模式
- 面板不再全局遮罩；未登录状态可直接浏览节点列表与指标（只读）。
- Header 右上角"登录"按钮弹出小浮层，登录成功后显示用户名/改密码/退出。
- 编辑操作按钮（添加节点、编辑、告警、通知渠道等）添加 `.auth-required` 样式，未登录时隐藏；`openEdit()` 也做了登录检查。
- `/api/whoami` 返回 `{logged_in, username}`，前端启动即调用以初始化鉴权状态。
- `/api/nodes` GET：未登录时 Token 字段置空；`/api/config` GET：未登录时不返回 AD Key。

### 修改密码
- Header "改密码"链接弹 `#modal-change-pw`，校验旧密码 → 更新 + 吊销全部 session。
- 服务端：`POST /api/change-password`，`sessionManager.revokeAll()` 立刻使旧 session 失效。

### Swap 行
- 节点卡片在 `swap_t > 0` 时显示 Swap 使用情况（进度条 + 用量）。

### 磁盘列表（编辑面板）
- 节点编辑 Modal 底部显示所有已上报挂载点及用量，超过 32 条时提示已截断。

### 冷却时间单位提示
- 告警规则表单的"冷却(秒)"输入框旁实时显示换算分钟数（如：`30分钟`）。
- 打开/重置表单时同步更新提示。

### 移动端响应式
- 新增 `@media (max-width: 640px)` 块：单列节点网格、header/container 缩减内边距、toolbar 自动折行、login-popup 全宽、modal 内边距缩小。

## 协议 / Agent

### bye 帧携带原因
- `ByePayload.Reason`：`shutdown` / `uninstall` / `reconfig`。
- SIGTERM → `bye{shutdown}`；收到 uninstall 指令 → `bye{uninstall}`。
- 主控 hub 记录 bye reason（TTL 5 分钟），告警引擎据此将通知正文改为"用户主动停止（reason）"。

### metric_batch 乱序保护
- 主控收到 `metric_batch` 后先按 timestamp 升序排序，再逐条写入，避免断线重连期间乱序数据覆盖更新的记录。

### DisksTruncated 字段
- `MetricPayload` 新增 `disks_truncated bool`，agent 磁盘数量超过 32 时置真。

### MachineIDSuspect 字段
- `HelloPayload` 新增 `machine_id_suspect bool`，检测容器/克隆 VM 的不可靠 ID（全同字符、已知 Docker 默认值）。
- 主控 `FindOrCreateNodeByFingerprint` 收到 suspect=true 时跳过 machine_id 去重，仅走 MAC 匹配。

### permessage-deflate 可关闭
- `agent/config.go` 新增 `disable_compression bool`（默认 false，即默认开启压缩）。
- 适用于低内存/高 CPU 受限场景，在 `/etc/bk/config.json` 加 `"disable_compression": true` 即可关闭。

## 告警引擎

### 冷启动宽限期
- `evalOffline`：`node.LastSeen == 0` 时直接返回，不对从未上报 metric 的节点触发离线告警。

### alert_state 过期清理
- `Store.CleanupAlertStateTraffic(maxAge int64)` 删除流量周期（scope `p…`）和续费（scope `due…`）的旧状态记录。
- 每 10 分钟 slow tick 调用一次，保留 90 天，防止无限积累。

### 告警规则测试触发
- `POST /api/alerts/{id}/test`：对已配置规则立即发一条测试消息，验证通知渠道是否正常。
- 面板告警规则列表每行新增"测试"按钮。

## 历史曲线

- 新增 `metrics_1m`（保留 24h）、`metrics_5m`（保留 30d）两张聚合表，`Store.AggregateMetrics(windowSec)` 每分钟将原始 metrics 聚合写入。
- `GET /api/nodes/{id}/history?range=1h|24h|7d|30d`：1h 走 metrics 原始表，24h 走 1m 桶，7d/30d 走 5m 桶。
- 节点详情展开区新增"历史曲线"区块，含 CPU %、内存 %、入站流量、出站流量四张纯 SVG 折线图，无第三方图表库。
- 时间范围标签 Tab 切换，`30d` 为默认。

## 主控自监控（Task 4）

- `internal/server/self_monitor.go`：主控本身作为一个节点上报指标，直接写库不走 WebSocket。
- 节点 ID 存 kv 表（key `self_monitor_node_id`），重启后复用同一节点，不重复创建。
- 主控节点在面板上隐藏"卸载"/"重新配置"按钮，避免误操作。

## 节点 Tag 系统

- `PATCH /api/nodes/{id}` 支持 `{"tags": ["tag1","tag2"]}` 字段更新。
- `Store.UpdateNodeTags` 将标签序列化为 JSON 写入 nodes.tags 列。
- 节点编辑 Modal 新增"标签"输入框（逗号分隔）。
- Toolbar 新增 Tag 筛选输入框，实时过滤节点列表。

## 主题切换

- 新增深色 / 浅色 / 蓝色三套主题，CSS 自定义属性（`--bg`、`--panel`、`--accent` 等）驱动。
- Header 右侧三个色点按钮切换，当前主题存 `localStorage`，刷新后保留。

## 两步验证（2FA TOTP）

- `internal/server/totp.go`：纯标准库实现 RFC 6238 TOTP，支持 SHA-1 / 6 位 / 30 秒周期，±1 窗口容错。
- 流程：`POST /api/2fa/enable` 生成密钥 + `otpauth://` URI → 用户扫码 → `POST /api/2fa/verify` 验证首个 code 并启用 → `POST /api/2fa/disable`（需提供当前密码）。
- 启用后登录接口需要额外传 `code` 字段，验证失败返回 401。
- 面板设置 → "两步验证" Tab：显示状态、展示 secret 与 URI 供手动输入 Authenticator App，无 QR 码（不引入第三方库）。

## 操作审计日志

- 新增 `audit_log` 表（保留 90 天），记录 actor / IP / action / target / detail。
- `Store.LogAudit` 在所有写操作路径中调用：登录/退出、节点增删改、告警规则变更、2FA 开关、密码修改等。
- `GET /api/audit-log?limit=100&offset=0`：返回最近操作日志。
- 面板设置 → "审计日志" Tab 展示，按时间降序，翻页。

## 备份 / 恢复

- `GET /api/backup/export`：将节点列表、node_meta、通知渠道、告警规则导出为 JSON 文件（`mote-backup.json`），Token 原样保留。
- `POST /api/backup/import`：解析导入 JSON，按 Token 做 upsert，已存在则跳过（不覆盖运行中节点配置）。
- 面板设置 → "备份/恢复" Tab：导出按钮直接触发下载，导入用文件选择器。

## 全员卸载

- `POST /api/uninstall-all`：向所有在线节点（排除自监控节点）下发 `uninstall` 指令，返回发送成功数量。
- 面板设置 → "危险操作" Tab 新增二次确认弹窗。

## CLI 改进

### bk reconfig 自动回滚
- `bk reconfig` 交互流程：输入新主控地址/Token → 保存 + 重启 → 60 秒内每 5 秒探测 `/api/config` 是否可达。
- 60 秒内未连通：自动回滚旧配置并重启，输出提示。

### zk status 健康检查
- `zk status` 除原有配置信息外，新增五项检查：
  1. systemd 服务状态 + PID
  2. 端口监听检查（尝试绑定，失败即表示已在监听）
  3. SQLite 文件存在与大小
  4. 磁盘剩余空间
  5. 面板 HTTP 可达性（带 Basic Auth 探测 `/api/config`）

---

# v2.2 Bug 修复补丁（2026-05）

## `internal/server/api.go`

### `writeJSONResp` 编码失败静默
原代码 `json.NewEncoder(w).Encode(v)` 在 Header 写出后才编码，编码失败时响应头已发送，
状态码无法纠正，客户端收到截断 JSON。
改为先 `json.Marshal` 到内存缓冲，失败则返回 500（此时头未发出），成功再写入 `w`。

### 备份导出（`handleBackupExport`）编码失败
同上问题：`Content-Disposition` 头先设，编码失败时文件已截断。
改用相同的先 marshal 后写入模式。

### 备份导入（`handleBackupImport`）配置落盘失败静默
`_ = a.cfg.Save()` 在配置修改后写盘失败时无任何提示，
下次重启主控后 `panel_title` / `main_currency` 的变更丢失。
改为检查错误并 `log.Printf` 输出，不阻断导入成功响应。

### 告警规则测试触发 `json.Unmarshal` 错误忽略
`rule.NotifierIDs` 反序列化失败时 `notifierIDs` 为 nil，
后续 `len(notifierIDs) == 0` 返回"该规则未配置通知渠道"，
错误原因完全被掩盖。改为显式检查并返回 500 + 实际错误信息。

## `internal/server/store.go`

### `CreateNode` / `FindOrCreateNodeByFingerprint` — node_meta INSERT 未检查
新节点创建后 `INSERT INTO node_meta(node_id)` 的错误被丢弃，
后续 `UPDATE node_meta` 若失败错误才被传播，但真正的根因（INSERT 失败）已丢失。
两处均改为检查 INSERT 错误并向调用方返回。

### `LogAudit` — INSERT audit_log 失败静默丢弃
磁盘满或表结构异常时审计记录静默消失，且无任何日志。
改为检查错误并 `log.Printf` 输出，不影响主流程。
