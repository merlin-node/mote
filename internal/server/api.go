package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mote/internal/shared"
)

type ctxKey int

const ctxKeyLoggedIn ctxKey = 0

func withLoggedIn(r *http.Request, v bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyLoggedIn, v))
}

func loggedIn(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeyLoggedIn).(bool)
	return v
}

// checkAuth 检查请求是否已认证(Cookie session 或 HTTP Basic)
func checkAuth(a *API, r *http.Request) bool {
	if c, err := r.Cookie(sessionCookieName); err == nil && a.auth.valid(c.Value) {
		return true
	}
	u, p, ok := r.BasicAuth()
	return ok && constantTimeEqualString(u, a.cfg.AdminUsername) && constantTimeEqualString(p, a.cfg.AdminPassword)
}

// isPublicReadPath 返回 true 表示 GET 请求无需登录即可访问
func isPublicReadPath(path string) bool {
	switch path {
	case "/api/nodes", "/api/config", "/api/whoami":
		return true
	}
	// /api/nodes/{id} 及其子路径(metrics、probe-history 等)
	if strings.HasPrefix(path, "/api/nodes/") {
		return true
	}
	return false
}

//go:embed web/*
var webFS embed.FS

// API 提供 REST API + 静态文件
type API struct {
	store   *Store
	hub     *Hub
	cfg     *Config
	auth    *sessionManager
	selfMon *SelfMonitor
}

func NewAPI(store *Store, hub *Hub, cfg *Config, selfMon *SelfMonitor) *API {
	return &API{store: store, hub: hub, cfg: cfg, auth: newSessionManager(), selfMon: selfMon}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// WS
	mux.HandleFunc("/ws/agent", a.hub.HandleWS)

	// API
	mux.HandleFunc("/api/nodes", a.handleNodes)            // GET 列表 / POST 创建
	mux.HandleFunc("/api/nodes/", a.handleNodeOps)         // /api/nodes/{id}, /api/nodes/{id}/meta
	mux.HandleFunc("/api/install-cmd", a.handleInstallCmd) // 生成安装命令(POST,token 走 body)
	mux.HandleFunc("/api/config", a.handleConfig)          // GET/PATCH 面板配置
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/api/whoami", a.handleWhoAmI)
	mux.HandleFunc("/api/change-password", a.handleChangePassword)

	// 通知渠道
	mux.HandleFunc("/api/notifiers", a.handleNotifiers)         // GET 列表 / POST 创建
	mux.HandleFunc("/api/notifiers/", a.handleNotifierOps)      // /{id} PATCH/DELETE / {id}/test POST

	// 告警规则
	mux.HandleFunc("/api/alerts", a.handleAlertRules)
	mux.HandleFunc("/api/alerts/", a.handleAlertRuleOps)

	// 全员卸载
	mux.HandleFunc("/api/uninstall-all", a.handleUninstallAll)

	// 历史曲线（/api/nodes/{id}/history 已在 handleNodeOps 里路由）

	// 审计日志
	mux.HandleFunc("/api/audit-log", a.handleAuditLog)

	// 备份 / 恢复
	mux.HandleFunc("/api/backup/export", a.handleBackupExport)
	mux.HandleFunc("/api/backup/import", a.handleBackupImport)

	// 2FA
	mux.HandleFunc("/api/2fa/status", a.handle2FAStatus)
	mux.HandleFunc("/api/2fa/enable", a.handle2FAEnable)
	mux.HandleFunc("/api/2fa/verify", a.handle2FAVerify)
	mux.HandleFunc("/api/2fa/disable", a.handle2FADisable)

	// 公开下载:安装脚本 + bk 二进制(无需鉴权,小鸡装机要能匿名访问)
	mux.HandleFunc("/install/", a.handleInstallAsset)
	mux.HandleFunc("/install-bk.sh", a.handleInstallAsset) // 兼容旧路径

	// 静态文件(嵌入的 web 目录)
	subFS, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(subFS))
	mux.Handle("/", fileServer)

	return basicAuth(a, mux)
}

// constantTimeEqualString 防时序攻击的字符串比较
func constantTimeEqualString(a, b string) bool {
	if len(a) != len(b) {
		// 仍然比较一次,避免长度差异本身泄露信息
		subtle.ConstantTimeCompare([]byte(a), []byte(a))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// basicAuth 鉴权中间件:
//   - WS、安装资源、/api/login、/api/logout 始终放行
//   - GET 公开读路径(节点列表、节点详情、探针历史、/api/config、/api/whoami)无需登录
//   - 其他 /api/ 路径必须登录;静态文件直接放行
func basicAuth(a *API, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WS 端点跳过(Agent 用自己的 token 鉴权)
		if strings.HasPrefix(r.URL.Path, "/ws/") {
			next.ServeHTTP(w, r)
			return
		}
		// 登录/登出/安装资源 始终放行
		if r.URL.Path == "/api/login" || r.URL.Path == "/api/logout" ||
			strings.HasPrefix(r.URL.Path, "/install/") || r.URL.Path == "/install-bk.sh" {
			next.ServeHTTP(w, r)
			return
		}
		// 非 /api/ 路径(静态文件等)直接放行
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		authed := checkAuth(a, r)

		// 公开读:GET 且路径在白名单内,无需登录
		if !authed && r.Method == http.MethodGet && isPublicReadPath(r.URL.Path) {
			next.ServeHTTP(w, withLoggedIn(r, false))
			return
		}

		if !authed {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, withLoggedIn(r, true))
	})
}

// === handlers ===

type nodeWithStatus struct {
	*Node
	Meta   *NodeMeta        `json:"meta,omitempty"`
	Online bool             `json:"online"`
	Latest *json.RawMessage `json:"latest,omitempty"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"` // TOTP 验证码(仅 2FA 开启时必填)
}

func (a *API) handleNodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		nodes, err := a.store.ListNodes()
		if err != nil {
			httpError(w, err)
			return
		}
		authed := loggedIn(r)
		out := make([]*nodeWithStatus, 0, len(nodes))
		for _, n := range nodes {
			if !authed {
				// 未登录时不暴露 token
				copy := *n
				copy.Token = ""
				n = &copy
			}
			ns := &nodeWithStatus{Node: n, Online: a.hub.IsOnline(n.ID)}
			ns.Meta, _ = a.store.GetMeta(n.ID)
			if data, _, err := a.store.GetLatestMetric(n.ID); err == nil {
				raw := json.RawMessage(data)
				ns.Latest = &raw
			}
			out = append(out, ns)
		}
		writeJSONResp(w, out)

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			req.Name = "new-node"
		}
		n, err := a.store.CreateNode(req.Name)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSONResp(w, n)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// /api/nodes/{id}              GET/PATCH/DELETE  节点本体
// /api/nodes/{id}/meta         GET/PATCH         元信息
// /api/nodes/{id}/uninstall    POST              下发卸载
func (a *API) handleNodeOps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	sub := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}

	switch sub {
	case "":
		a.opNode(w, r, id)
	case "meta":
		a.opMeta(w, r, id)
	case "probe-history":
		a.opProbeHistory(w, r, id)
	case "history":
		a.opHistory(w, r, id)
	case "uninstall":
		a.opUninstall(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (a *API) opNode(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		n, err := a.store.GetNode(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSONResp(w, n)
	case http.MethodPatch:
		var req struct {
			Name *string  `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Name != nil {
			if err := a.store.UpdateNodeName(id, strings.TrimSpace(*req.Name)); err != nil {
				httpError(w, err)
				return
			}
		}
		if req.Tags != nil {
			if err := a.store.UpdateNodeTags(id, req.Tags); err != nil {
				httpError(w, err)
				return
			}
		}
		n, err := a.store.GetNode(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		actor := a.cfg.AdminUsername
		a.store.LogAudit(actor, r.RemoteAddr, "update_node", fmt.Sprintf("node:%d", id), "")
		writeJSONResp(w, n)
	case http.MethodDelete:
		if err := a.store.DeleteNode(id); err != nil {
			httpError(w, err)
			return
		}
		a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "delete_node", fmt.Sprintf("node:%d", id), "")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) opMeta(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		m, err := a.store.GetMeta(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSONResp(w, m)
	case http.MethodPatch, http.MethodPut:
		m, err := a.store.GetMeta(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// 用部分更新:解码到现有结构上
		if err := json.NewDecoder(r.Body).Decode(m); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		m.NodeID = id

		// 如果 cycle/start_date 改了,自动算 next_due
		if m.Cycle != "" && m.StartDate > 0 && m.Cycle != "lifetime" && m.Cycle != "once" {
			m.NextDue = ComputeNextDue(m.StartDate, m.Cycle, time.Now().Unix())
		}

		if err := a.store.UpdateMeta(m); err != nil {
			httpError(w, err)
			return
		}
		writeJSONResp(w, m)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) opUninstall(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	err := a.hub.SendTo(id, "uninstall", nil)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeOffline):
			http.Error(w, "node offline", http.StatusBadRequest)
		case errors.Is(err, ErrSendBufferFull):
			http.Error(w, "node send buffer full, retry later", http.StatusServiceUnavailable)
		default:
			httpError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) opProbeHistory(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	minutes := 360
	if s := strings.TrimSpace(r.URL.Query().Get("minutes")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			minutes = v
		}
	}
	step := 60
	if s := strings.TrimSpace(r.URL.Query().Get("step")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			step = v
		}
	}
	limit := 360
	if s := strings.TrimSpace(r.URL.Query().Get("limit")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()
	points, err := a.store.QueryProbeHistory(id, since, step, limit)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSONResp(w, map[string]any{
		"node_id": id,
		"minutes": minutes,
		"step":    step,
		"items":   points,
	})
}

// handleInstallCmd 只接受 POST,token 通过 JSON body 传递(避免出现在 Nginx/access log 中)
func (a *API) handleInstallCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed; use POST with JSON body", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token  string `json:"token"`
		NodeID int64  `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// 如果提供 node_id,就从 store 取 token(避免前端二次保存)
	if req.Token == "" && req.NodeID > 0 {
		if n, err := a.store.GetNode(req.NodeID); err == nil {
			req.Token = n.Token
		}
	}
	if req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	server := r.Host
	scheme := "wss"
	if r.TLS == nil {
		scheme = "ws"
	}
	httpScheme := "https"
	if r.TLS == nil {
		httpScheme = "http"
	}
	wsURL := scheme + "://" + server
	httpURL := httpScheme + "://" + server

	// 主控自带 /install-bk.sh 与二进制下载,无需 GitHub。
	// 用 curl | bash -s -- 形式,兼容性比 bash <(curl) 好。
	linuxCmd := "curl -fsSL " + httpURL + "/install-bk.sh | sudo bash -s -- -s " + wsURL + " -t " + req.Token
	out := map[string]string{
		"linux":  linuxCmd,
		"ws_url": wsURL,
		"token":  req.Token,
	}
	writeJSONResp(w, out)
}

func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := map[string]any{
			"version":       shared.Version,
			"main_currency": a.cfg.MainCurrency,
			"panel_title":   a.cfg.PanelTitle,
		}
		if loggedIn(r) {
			out["auto_discovery_key"] = a.cfg.AutoDiscoveryKey
			out["public_enabled"] = a.cfg.PublicEnabled
		}
		if a.selfMon != nil {
			out["self_monitor_node_id"] = a.selfMon.NodeID()
		}
		writeJSONResp(w, out)

	case http.MethodPatch:
		var req struct {
			PanelTitle    *string `json:"panel_title"`
			MainCurrency  *string `json:"main_currency"`
			PublicEnabled *bool   `json:"public_enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.PanelTitle != nil {
			t := strings.TrimSpace(*req.PanelTitle)
			if t == "" {
				t = DefaultPanelTitle
			}
			if len([]rune(t)) > 60 {
				http.Error(w, "panel_title too long (max 60 chars)", http.StatusBadRequest)
				return
			}
			a.cfg.PanelTitle = t
		}
		if req.MainCurrency != nil {
			a.cfg.MainCurrency = strings.TrimSpace(*req.MainCurrency)
		}
		if req.PublicEnabled != nil {
			a.cfg.PublicEnabled = *req.PublicEnabled
		}
		if err := a.cfg.Save(); err != nil {
			httpError(w, err)
			return
		}
		a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "update_config", "config", "")
		writeJSONResp(w, map[string]any{
			"panel_title":    a.cfg.PanelTitle,
			"main_currency":  a.cfg.MainCurrency,
			"public_enabled": a.cfg.PublicEnabled,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleInstallAsset 提供两类公开下载,小鸡装机用:
//   /install-bk.sh           → 注入主控地址后的安装脚本
//   /install/bk-linux-amd64  → 被控二进制(从主控旁的目录读)
//   /install/bk-linux-arm64
func (a *API) handleInstallAsset(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 安装脚本(动态生成,注入主控自己的 URL)
	if path == "/install-bk.sh" || path == "/install/install-bk.sh" {
		a.serveInstallScript(w, r)
		return
	}

	// 二进制下载
	if strings.HasPrefix(path, "/install/") {
		name := strings.TrimPrefix(path, "/install/")
		// 只允许这两个文件名,防路径穿越
		if name != "bk-linux-amd64" && name != "bk-linux-arm64" {
			http.NotFound(w, r)
			return
		}
		// 从可执行文件同目录的 ./dist 或 /var/lib/zk/dist 找
		candidates := []string{
			filepath.Join(a.cfg.DataDir, "dist", name),
			filepath.Join("./dist", name),
			filepath.Join("/usr/local/share/zk/dist", name),
		}
		for _, p := range candidates {
			if f, err := os.Open(p); err == nil {
				defer f.Close()
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
				io.Copy(w, f)
				return
			}
		}
		http.Error(w, "binary not found on server; place it at "+candidates[0], http.StatusNotFound)
		return
	}

	http.NotFound(w, r)
}

// serveInstallScript 输出已注入主控 URL 的 install-bk.sh
// 调用方式: curl http://主控/install-bk.sh | sudo bash -s -- -s wss://主控 -t TOKEN
func (a *API) serveInstallScript(w http.ResponseWriter, r *http.Request) {
	httpScheme := "https"
	wsScheme := "wss"
	if r.TLS == nil {
		httpScheme = "http"
		wsScheme = "ws"
	}
	masterHTTP := httpScheme + "://" + r.Host
	masterWS := wsScheme + "://" + r.Host

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, installScriptTemplate, masterHTTP, masterWS)
}

// installScriptTemplate 是装被控的脚本模板,服务端注入两个 URL 后下发。
// %[1]s = http(s)://主控 (拉二进制用)
// %[2]s = ws(s)://主控   (被控连接默认值)
const installScriptTemplate = `#!/bin/bash
# install-bk.sh — 被控一键安装脚本(由主控动态生成)
# 主控地址: %[1]s
#
# 用法:
#   bash <(curl -fsSL %[1]s/install-bk.sh) -t TOKEN
#   bash <(curl -fsSL %[1]s/install-bk.sh) -s wss://别的主控 -t TOKEN
#   bash <(curl -fsSL %[1]s/install-bk.sh) --auto-discovery KEY

set -e

MASTER_HTTP="%[1]s"
SERVER="%[2]s"
TOKEN=""
AD_KEY=""

BIN_PATH="/usr/local/bin/bk"
CONFIG_DIR="/etc/bk"
LOG_DIR="/var/log/bk"
SERVICE_FILE="/etc/systemd/system/bk.service"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--server) SERVER="$2"; shift 2 ;;
    -t|--token)  TOKEN="$2"; shift 2 ;;
    --auto-discovery) AD_KEY="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $0 [-s wss://host] -t TOKEN"
      echo "       $0 [-s wss://host] --auto-discovery KEY"
      exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

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

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "❌ 不支持的架构: $ARCH" >&2; exit 1 ;;
esac

echo "▶ 正在安装 bk 被控 (linux-$ARCH) ..."
echo "  主控    : $SERVER"

TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

URL="$MASTER_HTTP/install/bk-linux-$ARCH"
echo "  下载    : $URL"
curl -fsSL -o "$TMP/bk" "$URL"
chmod +x "$TMP/bk"

systemctl stop bk 2>/dev/null || true
install -m 755 "$TMP/bk" "$BIN_PATH"
mkdir -p "$CONFIG_DIR" "$LOG_DIR"

cat > "$CONFIG_DIR/config.json" <<CFG
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "auto_discovery": "$AD_KEY",
  "interval": 2,
  "heartbeat": 30
}
CFG
chmod 600 "$CONFIG_DIR/config.json"

cat > "$SERVICE_FILE" <<UNIT
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
UNIT

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
echo "管理: bk / bk status / bk restart / bk uninstall"
echo "日志: journalctl -u bk -f"
`

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	userOK := constantTimeEqualString(strings.TrimSpace(req.Username), a.cfg.AdminUsername)
	passOK := constantTimeEqualString(req.Password, a.cfg.AdminPassword)
	if !userOK || !passOK {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 2FA 检查
	if enabled, _ := a.store.KVGet("totp_enabled"); enabled == "1" {
		secret, ok := a.store.KVGet("totp_secret")
		if !ok || secret == "" || !totpVerify(secret, req.Code) {
			http.Error(w, "验证码错误或未提供", http.StatusUnauthorized)
			return
		}
	}
	token, expiresAt, err := a.auth.create()
	if err != nil {
		httpError(w, err)
		return
	}
	a.auth.setCookie(w, token, expiresAt, r.TLS != nil)
	writeJSONResp(w, map[string]any{"ok": true, "expires_at": expiresAt.Unix()})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.auth.revoke(c.Value)
	}
	clearSessionCookie(w, r.TLS != nil)
	writeJSONResp(w, map[string]bool{"ok": true})
}

// handleWhoAmI 返回当前登录状态(始终可访问)
func (a *API) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSONResp(w, map[string]any{
		"logged_in": loggedIn(r),
		"username":  a.cfg.AdminUsername,
	})
}

// handleChangePassword 修改管理员密码,改后清空所有 session
func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !constantTimeEqualString(req.Old, a.cfg.AdminPassword) {
		http.Error(w, "旧密码不正确", http.StatusUnauthorized)
		return
	}
	if len(req.New) < 6 {
		http.Error(w, "新密码太短(至少 6 位)", http.StatusBadRequest)
		return
	}
	a.cfg.AdminPassword = req.New
	if err := a.cfg.Save(); err != nil {
		httpError(w, err)
		return
	}
	// 清空所有 session,强制重新登录
	a.auth.revokeAll()
	writeJSONResp(w, map[string]bool{"ok": true})
}

// === helpers ===

func writeJSONResp(w http.ResponseWriter, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf)
}

func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// === 通知渠道 ===

func (a *API) handleNotifiers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.store.ListNotifiers()
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSONResp(w, list)

	case http.MethodPost:
		var n Notifier
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if n.Name == "" {
			n.Name = "通知渠道"
		}
		if n.Type == "" {
			n.Type = "telegram"
		}
		id, err := a.store.CreateNotifier(&n)
		if err != nil {
			httpError(w, err)
			return
		}
		out, _ := a.store.GetNotifier(id)
		writeJSONResp(w, out)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// /api/notifiers/{id}        PATCH/DELETE
// /api/notifiers/{id}/test   POST  发一条测试消息
func (a *API) handleNotifierOps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/notifiers/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}

	switch sub {
	case "test":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		n, err := a.store.GetNotifier(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := TestNotifier(n); err != nil {
			http.Error(w, "send failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		writeJSONResp(w, map[string]bool{"ok": true})

	case "":
		switch r.Method {
		case http.MethodPatch:
			n, err := a.store.GetNotifier(id)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(n); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			n.ID = id
			if err := a.store.UpdateNotifier(n); err != nil {
				httpError(w, err)
				return
			}
			writeJSONResp(w, n)
		case http.MethodDelete:
			a.store.DeleteNotifier(id)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	default:
		http.NotFound(w, r)
	}
}

// === 告警规则 ===

func (a *API) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.store.ListAlertRules()
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSONResp(w, list)

	case http.MethodPost:
		var rule AlertRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if rule.Name == "" {
			rule.Name = "未命名规则"
		}
		if rule.Kind == "" {
			http.Error(w, "kind required", http.StatusBadRequest)
			return
		}
		id, err := a.store.CreateAlertRule(&rule)
		if err != nil {
			httpError(w, err)
			return
		}
		rule.ID = id
		writeJSONResp(w, rule)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) handleAlertRuleOps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/alerts/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}
	if sub == "test" {
		a.opAlertTest(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		// 拿到现有规则,再用 body 覆盖
		rules, err := a.store.ListAlertRules()
		if err != nil {
			httpError(w, err)
			return
		}
		var rule *AlertRule
		for _, x := range rules {
			if x.ID == id {
				rule = x
				break
			}
		}
		if rule == nil {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(rule); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		rule.ID = id
		if err := a.store.UpdateAlertRule(rule); err != nil {
			httpError(w, err)
			return
		}
		writeJSONResp(w, rule)

	case http.MethodDelete:
		a.store.DeleteAlertRule(id)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// === 历史曲线 ===

func (a *API) opHistory(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rangeStr := r.URL.Query().Get("range")
	if rangeStr == "" {
		rangeStr = "1h"
	}
	points, err := a.store.GetHistory(id, rangeStr)
	if err != nil {
		httpError(w, err)
		return
	}
	if points == nil {
		points = []HistoryPoint{}
	}
	writeJSONResp(w, map[string]any{
		"node_id": id,
		"range":   rangeStr,
		"items":   points,
	})
}

// === 全员卸载 ===

func (a *API) handleUninstallAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ids := a.hub.OnlineNodes()
	count := 0
	for _, nodeID := range ids {
		if a.selfMon != nil && nodeID == a.selfMon.NodeID() {
			continue
		}
		if a.hub.SendTo(nodeID, "uninstall", nil) == nil {
			count++
		}
	}
	a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "uninstall_all", "all",
		fmt.Sprintf("sent to %d nodes", count))
	writeJSONResp(w, map[string]any{"ok": true, "count": count})
}

// === 审计日志 ===

func (a *API) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, err := a.store.ListAudit(limit, offset)
	if err != nil {
		httpError(w, err)
		return
	}
	if entries == nil {
		entries = []*AuditEntry{}
	}
	writeJSONResp(w, map[string]any{"items": entries, "limit": limit, "offset": offset})
}

// === 备份/恢复 ===

func (a *API) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nodes, err := a.store.ListNodes()
	if err != nil {
		httpError(w, err)
		return
	}
	notifiers, err := a.store.ListNotifiers()
	if err != nil {
		httpError(w, err)
		return
	}
	rules, err := a.store.ListAlertRules()
	if err != nil {
		httpError(w, err)
		return
	}
	data := &BackupData{
		Version:      "v2.2",
		ExportedAt:   time.Now().Unix(),
		Nodes:        nodes,
		Notifiers:    notifiers,
		AlertRules:   rules,
		PanelTitle:   a.cfg.PanelTitle,
		MainCurrency: a.cfg.MainCurrency,
	}
	buf, err := json.Marshal(data)
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="mote-backup.json"`)
	w.Write(buf)
}

func (a *API) handleBackupImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data BackupData
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&data); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.store.ImportBackup(&data); err != nil {
		httpError(w, err)
		return
	}
	if data.PanelTitle != "" {
		a.cfg.PanelTitle = data.PanelTitle
	}
	if data.MainCurrency != "" {
		a.cfg.MainCurrency = data.MainCurrency
	}
	if err := a.cfg.Save(); err != nil {
		log.Printf("backup import: save config: %v", err)
	}
	a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "backup_import", "all", "")
	writeJSONResp(w, map[string]any{"ok": true})
}

// === 2FA ===

func (a *API) handle2FAStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	enabled, _ := a.store.KVGet("totp_enabled")
	writeJSONResp(w, map[string]any{"enabled": enabled == "1"})
}

func (a *API) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	secret := totpGenerateSecret()
	if err := a.store.KVSet("totp_secret_pending", secret); err != nil {
		httpError(w, err)
		return
	}
	issuer := "mote"
	account := a.cfg.AdminUsername
	uri := fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		issuer, account, secret, issuer)
	writeJSONResp(w, map[string]any{"secret": secret, "uri": uri})
}

func (a *API) handle2FAVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	secret, ok := a.store.KVGet("totp_secret_pending")
	if !ok || secret == "" {
		http.Error(w, "请先调用 /api/2fa/enable 生成密钥", http.StatusBadRequest)
		return
	}
	if !totpVerify(secret, req.Code) {
		http.Error(w, "验证码错误", http.StatusBadRequest)
		return
	}
	a.store.KVSet("totp_secret", secret)
	a.store.KVSet("totp_enabled", "1")
	a.store.KVDelete("totp_secret_pending")
	a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "enable_2fa", "config", "")
	writeJSONResp(w, map[string]any{"ok": true})
}

func (a *API) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if !constantTimeEqualString(req.Password, a.cfg.AdminPassword) {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}
	a.store.KVSet("totp_enabled", "0")
	a.store.KVDelete("totp_secret")
	a.store.LogAudit(a.cfg.AdminUsername, r.RemoteAddr, "disable_2fa", "config", "")
	writeJSONResp(w, map[string]any{"ok": true})
}

// === 告警规则测试触发 ===

func (a *API) opAlertTest(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rules, err := a.store.ListAlertRules()
	if err != nil {
		httpError(w, err)
		return
	}
	var rule *AlertRule
	for _, x := range rules {
		if x.ID == id {
			rule = x
			break
		}
	}
	if rule == nil {
		http.NotFound(w, r)
		return
	}
	var notifierIDs []int64
	if err := json.Unmarshal([]byte(rule.NotifierIDs), &notifierIDs); err != nil {
		http.Error(w, "通知渠道配置解析失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(notifierIDs) == 0 {
		http.Error(w, "该规则未配置通知渠道", http.StatusBadRequest)
		return
	}
	Dispatch(a.store, notifierIDs, &NotifyMessage{
		Title:    fmt.Sprintf("[测试] 规则「%s」", rule.Name),
		Body:     "这是一条测试告警消息,规则配置正确。",
		Level:    "info",
		NodeName: "测试节点",
		Timestamp: time.Now().Unix(),
	})
	writeJSONResp(w, map[string]any{"ok": true})
}
