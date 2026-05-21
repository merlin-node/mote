#!/bin/bash
# install-bk.sh — 被控一键安装脚本
# 用法(主控架好后,被控由主控网页生成完整命令):
#   bash <(curl -fsSL https://panel.example.com/install-bk.sh) -s wss://panel.example.com -t TOKEN
#   bash <(curl -fsSL https://panel.example.com/install-bk.sh) -s wss://panel.example.com --auto-discovery KEY
#
# 直接从 GitHub 拉脚本(主控没架起来时):
#   bash <(curl -fsSL https://raw.githubusercontent.com/merlin-node/mote/main/scripts/install-bk.sh) \
#     -s wss://panel.example.com -t TOKEN

set -e

GITHUB_REPO="${BK_REPO:-merlin-node/mote}"
VERSION="${BK_VERSION:-latest}"
BIN_PATH="/usr/local/bin/bk"
CONFIG_DIR="/etc/bk"
LOG_DIR="/var/log/bk"
SERVICE_FILE="/etc/systemd/system/bk.service"

SERVER=""
TOKEN=""
AD_KEY=""

# ===== 解析参数 =====
while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--server) SERVER="$2"; shift 2 ;;
    -t|--token)  TOKEN="$2"; shift 2 ;;
    --auto-discovery) AD_KEY="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 -s wss://host -t TOKEN"
      echo "       $0 -s wss://host --auto-discovery KEY"
      exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$SERVER" ]; then
  echo "❌ 必须指定主控地址 (-s wss://...)" >&2
  exit 1
fi
if [ -z "$TOKEN" ] && [ -z "$AD_KEY" ]; then
  echo "❌ 必须指定 Token (-t) 或自动发现密钥 (--auto-discovery)" >&2
  exit 1
fi

if [ "$EUID" -ne 0 ]; then
  echo "❌ 请使用 root 或 sudo 运行" >&2
  exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
  echo "❌ 此脚本要求 systemd" >&2
  exit 1
fi

# ===== 探测架构 =====
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "❌ 不支持的架构: $ARCH" >&2; exit 1 ;;
esac

echo "▶ 正在安装 bk 被控 (linux-$ARCH)..."

# ===== 下载 =====
TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${GITHUB_REPO}/releases/latest/download/bk-linux-${ARCH}"
else
  URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/bk-linux-${ARCH}"
fi

# 允许本地文件
if [ -f "./dist/bk-linux-${ARCH}" ]; then
  cp "./dist/bk-linux-${ARCH}" "$TMP/bk"
elif [ -f "./dist/bk" ]; then
  cp "./dist/bk" "$TMP/bk"
else
  echo "  下载: $URL"
  curl -fsSL -o "$TMP/bk" "$URL"
fi

chmod +x "$TMP/bk"

# ===== 安装 =====
systemctl stop bk 2>/dev/null || true

install -m 755 "$TMP/bk" "$BIN_PATH"
mkdir -p "$CONFIG_DIR" "$LOG_DIR"
# 收紧目录权限:token 文件在里面,避免非 root 用户列目录
chmod 700 "$CONFIG_DIR"

# 写配置
cat > "$CONFIG_DIR/config.json" <<EOF
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "auto_discovery": "$AD_KEY",
  "interval": 2,
  "heartbeat": 30
}
EOF
chmod 600 "$CONFIG_DIR/config.json"

# 写 systemd unit
# - Restart=on-failure:agent 正常退出(收到 uninstall 后 Exit 0)时不重启,
#   避免和自毁脚本打架。崩溃才会被拉起。
# - KillMode=mixed + TimeoutStopSec=15:systemctl stop 时先给主进程发 SIGTERM,
#   留 15 秒让 agent 发 bye 帧给主控,然后再 kill 整个 cgroup。
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=bk (mote agent)
After=network.target

[Service]
Type=simple
ExecStart=$BIN_PATH run -c $CONFIG_DIR/config.json
Restart=on-failure
RestartSec=5
KillMode=mixed
TimeoutStopSec=15
StandardOutput=append:$LOG_DIR/bk.log
StandardError=append:$LOG_DIR/bk.log

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable bk
systemctl start bk

sleep 2

echo ""
echo "✅ 被控已安装并启动"
echo ""
echo "  主控地址 : $SERVER"
echo "  配置文件 : $CONFIG_DIR/config.json"
echo ""
echo "管理命令:"
echo "  bk            # 交互菜单"
echo "  bk status     # 查看状态"
echo "  bk restart    # 重启"
echo "  bk uninstall  # 卸载"
echo ""
echo "查看日志: journalctl -u bk -f 或 tail -f $LOG_DIR/bk.log"
