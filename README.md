# mote — 极轻量服务器监控探针

> 主控 `zk` + 被控 `bk`，类似 Komari / 哪吒，但更轻、更省心
> 版本：v2.2 | GitHub: `merlin-node/mote`

## 特性

- **极轻量**：被控 Agent 内存 < 5MB，二进制 < 3MB（UPX 压缩后）
- **单二进制**：无外部依赖，WebSocket 单端口，过 CDN 过反代
- **历史曲线**：CPU / 内存 / 流量，支持 1h / 24h / 7d / 30d，分层降采样，纯 SVG 无第三方图表库
- **延迟监测**：ICMP ping + TCP 端口探测，实时延迟 / 丢包率，6 小时历史曲线
- **告警通知**：Telegram 渠道，支持 CPU / 内存 / 磁盘 / 负载 / 离线 / 流量 / 续费到期，冷却 + 静默时段 + 恢复通知
- **完整账单**：月付/季付/年付/长期等周期，多货币，自动算到期日，流量管理
- **节点 Tag**：自由标签，Toolbar 实时筛选
- **主控自监控**：主控本身作为节点上报指标，面板直接可见
- **两步验证（2FA）**：RFC 6238 TOTP，纯标准库，无第三方依赖
- **操作审计日志**：所有写操作均记录 actor / IP / action
- **备份 / 恢复**：一键导出 JSON，一键导入，Token 原样保留
- **公开只读页**：可配置无需登录浏览节点列表与指标
- **主题切换**：深色 / 浅色 / 蓝色，localStorage 记忆
- **移动端响应式**：`@media max-width:640px` 适配小屏
- **CLI 友好**：`zk` / `bk` 交互菜单，start / stop / restart / status / uninstall 一应俱全
- **远程卸载**：网页一键下发，被控自动清理；支持全员卸载

## 项目结构

```
mote/
├── cmd/
│   ├── zk/       主控 CLI
│   └── bk/       被控 CLI
├── internal/
│   ├── shared/   共享协议（WS 消息定义、版本号）
│   ├── agent/    被控核心（采集、上报、重连、卸载）
│   └── server/   主控核心（WS Hub、SQLite、REST API、调度器、告警引擎）
│       └── web/  内嵌前端（单文件 HTML，go:embed）
├── scripts/
│   ├── install-zk.sh   主控一键安装
│   └── install-bk.sh   被控一键安装（部署时由主控动态注入地址）
└── Makefile
```

## 构建

```bash
# 本地构建（当前平台）
make build

# 交叉编译
make linux-amd64
make linux-arm64

# 一并构建所有发布产物
make release
```

构建产物在 `dist/` 目录。

## 本地开发联调

开两个终端：

```bash
# 终端 1：启动主控
make run-zk
# 输出：
# admin user: admin
# admin pass: aB3kP9q2X7yZ
# zk dev listening on :1888
```

浏览器打开 `http://127.0.0.1:1888`，点右上角"登录"，输入上面的用户名密码。
点"+ 添加节点"，创建后得到一个 Token。

```bash
# 终端 2：启动被控（替换 TOKEN）
./dist/bk run -s ws://127.0.0.1:1888 -t TOKEN_HERE
```

刷新面板，节点应该上线。

## 生产部署

三种方式任选，推荐 Docker Compose。

### 方式一：Docker Compose（推荐）

```bash
# 1. 下载 compose 文件
curl -fsSL https://raw.githubusercontent.com/merlin-node/mote/main/docker-compose.yml -o docker-compose.yml

# 2. 放入被控二进制（主控用来分发给小鸡的安装脚本需要它）
mkdir -p ./data/db/dist
# 从 GitHub Releases 下载，或自行编译后 cp 过来
curl -fsSL https://github.com/merlin-node/mote/releases/download/v2.2/bk-linux-amd64 -o ./data/db/dist/bk-linux-amd64
curl -fsSL https://github.com/merlin-node/mote/releases/download/v2.2/bk-linux-arm64  -o ./data/db/dist/bk-linux-arm64
chmod +x ./data/db/dist/bk-linux-*

# 3. 启动
docker compose up -d

# 4. 查看初始密码
docker logs mote-zk 2>&1 | grep -E "admin (user|pass)"
```

compose 默认把配置挂载到 `./data/config`，数据库和被控二进制挂载到 `./data/db`。
`docker-compose.yml` 完整内容：

```yaml
services:
  zk:
    image: ghcr.io/merlin-node/mote:latest
    container_name: mote-zk
    restart: unless-stopped
    ports:
      - "1888:1888"
    volumes:
      - ./data/config:/etc/zk
      - ./data/db:/var/lib/zk
    environment:
      - TZ=Asia/Shanghai
```

### 方式二：Docker 一行启动

```bash
docker run -d \
  --name mote-zk \
  --restart unless-stopped \
  -p 1888:1888 \
  -v /etc/zk:/etc/zk \
  -v /var/lib/zk:/var/lib/zk \
  -e TZ=Asia/Shanghai \
  ghcr.io/merlin-node/mote:latest
```

### 方式三：二进制 + systemd

```bash
# 1. 把 dist/zk-linux-amd64 上传到服务器的某个目录
# 2. 在该目录执行：
sudo bash scripts/install-zk.sh

# 安装完成后输出：
#   访问地址 : http://1.2.3.4:1888
#   用户名   : admin
#   密码     : aB3kP9q2X7yZ
```

被控二进制放到主控可找到的位置（主控会自动提供被控下载）：

```bash
sudo mkdir -p /var/lib/zk/dist
sudo cp dist/bk-linux-amd64 dist/bk-linux-arm64 /var/lib/zk/dist/
```

### Nginx 反代（三种方式通用）

**强烈建议用域名 + Nginx 反代**：

```nginx
server {
  listen 443 ssl http2;
  server_name panel.example.com;

  location / {
    proxy_pass http://127.0.0.1:1888;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_read_timeout 86400;
  }
}
```

### 被控安装

被控（bk）**不走 Docker**，直接以 systemd 服务安装在被监控的机器上。

在面板上"+ 添加节点"创建节点，得到安装命令：

```bash
bash <(curl -fsSL https://panel.example.com/install-bk.sh) \
  -s wss://panel.example.com -t TOKEN_HERE
```

或者从面板复制已生成的一键安装命令，主控地址已注入，无需手动填写。

## CLI 速查

### 主控 `zk`

| 命令 | 作用 |
|---|---|
| `zk` | 打开交互菜单 |
| `zk status` | 健康检查（端口 / SQLite / 磁盘 / 可达性）|
| `zk start/stop/restart` | 服务控制 |
| `zk uninstall` | 卸载主控 |
| `zk version` | 版本 |

### 被控 `bk`

| 命令 | 作用 |
|---|---|
| `bk` | 打开交互菜单 |
| `bk status` | 查看连接状态 |
| `bk start/stop/restart` | 服务控制 |
| `bk reconfig` | 修改主控地址 / Token（60s 自动回滚保护）|
| `bk uninstall` | 卸载被控 |
| `bk version` | 版本 |

## 配置文件

### 主控 `/etc/zk/config.json`

```json
{
  "listen": ":1888",
  "data_dir": "/var/lib/zk",
  "admin_username": "admin",
  "admin_password": "改这个",
  "auto_discovery_key": "",
  "main_currency": "CNY",
  "panel_title": "mote 监控面板",
  "public_enabled": false,
  "agent_interval": 2,
  "agent_heartbeat": 30
}
```

修改后 `systemctl restart zk` 生效。`panel_title` 和 `main_currency` 也可在面板内直接修改。

### 被控 `/etc/bk/config.json`

```json
{
  "server": "wss://panel.example.com",
  "token": "abcd1234...",
  "auto_discovery": "",
  "interval": 2,
  "heartbeat": 30,
  "nic_include": "",
  "nic_exclude": "^(lo|docker|veth|br-|tun|tap|wg|cni|flannel|cali|kube|podman|nerdctl|zt|vmnet|vnet|virbr|dummy|gre|sit|ip6tnl|teql)",
  "disable_compression": false
}
```

修改后 `bk restart` 生效；也可用 `bk reconfig` 交互式修改主控地址和 Token。

## 数据存储

- 主控数据：`/var/lib/zk/data.db`（SQLite，单文件，定期 cp 即可备份）
- 日志：`/var/log/zk/` 和 `/var/log/bk/`
- 面板内置备份导出：设置 → 备份/恢复 → 导出 JSON

## 流量统计模式

| 模式 | 公式 | 典型场景 |
|---|---|---|
| 双向合计 `sum` | `in + out` | 大部分廉价 VPS |
| 取大单向 `max` | `max(in, out)` | Hetzner 等 |
| 仅出站 `out` | `out` | AWS / GCP |
| 仅入站 `in` | `in` | 极少见 |

## 续费周期

支持月付 / 季付 / 半年付 / 年付 / 两年付 / 一次性 / 长期（终身），月底加月做边界处理（如 1/31 月付下次是 2/28）。

## 告警规则

| 类型 | 触发条件 |
|---|---|
| `offline` | 节点失联超 N 秒 |
| `online` | 节点首次上线 |
| `cpu` | CPU 使用率 ≥ 阈值且持续 N 秒 |
| `mem` | 内存使用率 ≥ 阈值且持续 N 秒 |
| `disk` | 任一挂载点 ≥ 阈值 |
| `load` | Load1 ≥ 阈值且持续 N 秒 |
| `traffic` | 周期流量到 50/80/95/100% |
| `due` | 续费到期前 N 天（可自定义天数列表）|

支持：目标筛选（全部 / 节点 ID / Tag）、冷却时间、静默时段（含跨天）、恢复通知、一键测试。

## 两步验证（2FA）

面板设置 → 两步验证：生成 TOTP 密钥，复制 URI 到 Authenticator App（Google Authenticator / Aegis 等）手动录入，验证首个 code 后启用。启用后登录需额外输入 6 位动态码。纯标准库实现，无第三方依赖，无 QR 码图片。

## 卸载

**主控**：`sudo zk uninstall`

**被控**：`sudo bk uninstall`（或从面板远程下发）

## 许可

MIT
