#!/bin/bash
# install-zk.sh — 主控一键安装脚本
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/merlin-node/mote/main/scripts/install-zk.sh | sudo bash

set -e

# ===== 配置(根据你的发布源修改) =====
GITHUB_REPO="${ZK_REPO:-merlin-node/mote}"
VERSION="${ZK_VERSION:-latest}"
BIN_PATH="/usr/local/bin/zk"
CONFIG_DIR="/etc/zk"
DATA_DIR="/var/lib/zk"
LOG_DIR="/var/log/zk"
SERVICE_FILE="/etc/systemd/system/zk.service"

# ===== 检查环境 =====
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

echo "▶ 正在安装 zk 主控 (linux-$ARCH)..."

# ===== 下载二进制 =====
TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

if [ "$VERSION" = "latest" ]; then
  DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/zk-linux-${ARCH}"
else
  DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/zk-linux-${ARCH}"
fi

# 允许本地文件(开发期)
if [ -f "./dist/zk-linux-${ARCH}" ]; then
  cp "./dist/zk-linux-${ARCH}" "$TMP/zk"
  echo "  使用本地构建: ./dist/zk-linux-${ARCH}"
elif [ -f "./dist/zk" ]; then
  cp "./dist/zk" "$TMP/zk"
  echo "  使用本地构建: ./dist/zk"
else
  echo "  下载: $DOWNLOAD_URL"
  curl -fsSL -o "$TMP/zk" "$DOWNLOAD_URL"
fi

chmod +x "$TMP/zk"

# ===== 安装 =====
# 停止旧服务(如果有)
systemctl stop zk 2>/dev/null || true

install -m 755 "$TMP/zk" "$BIN_PATH"
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"

# ===== 写 systemd 服务 =====
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=zk (mote master control)
After=network.target

[Service]
Type=simple
ExecStart=$BIN_PATH run -c $CONFIG_DIR/config.json
Restart=on-failure
RestartSec=5
StandardOutput=append:$LOG_DIR/zk.log
StandardError=append:$LOG_DIR/zk.log
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable zk
systemctl start zk

sleep 2

# ===== 输出登录信息 =====
echo ""
echo "✅ 主控已安装并启动"
echo ""
echo "等待服务就绪..."
for i in 1 2 3 4 5; do
  if [ -f "$CONFIG_DIR/config.json" ]; then
    break
  fi
  sleep 1
done

if [ -f "$CONFIG_DIR/config.json" ]; then
  LISTEN=$(grep -oP '"listen":\s*"\K[^"]+' "$CONFIG_DIR/config.json" || echo ":25774")
  USER=$(grep -oP '"admin_username":\s*"\K[^"]+' "$CONFIG_DIR/config.json")
  PASS=$(grep -oP '"admin_password":\s*"\K[^"]+' "$CONFIG_DIR/config.json")
  IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "<your-ip>")
  echo ""
  echo "  📡 访问地址 : http://${IP}${LISTEN}"
  echo "  👤 用户名   : $USER"
  echo "  🔑 密码     : $PASS"
  echo ""
  echo "建议:"
  echo "  · 使用域名 + Nginx 反代到 ${LISTEN}(便于未来迁移)"
  echo "  · 修改默认密码:编辑 $CONFIG_DIR/config.json 后 systemctl restart zk"
  echo "  · 查看日志:journalctl -u zk -f 或 tail -f $LOG_DIR/zk.log"
  echo ""
  echo "管理命令: zk (交互菜单) / zk status / zk restart / zk uninstall"
else
  echo "⚠️  服务已启动但配置文件未生成,请检查日志:"
  echo "    journalctl -u zk -n 50"
fi
