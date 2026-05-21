package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mote/internal/shared"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:    8192,
	WriteBufferSize:   8192,
	CheckOrigin:       func(r *http.Request) bool { return true },
	EnableCompression: true,
}

// adRateLimiter 限制单 IP 通过 AutoDiscovery 注册的速率
// 默认:每 IP 每分钟最多 5 次注册尝试
type adRateLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	window  time.Duration
	maxHits int
}

func newADRateLimiter() *adRateLimiter {
	return &adRateLimiter{
		hits:    make(map[string][]time.Time),
		window:  time.Minute,
		maxHits: 5,
	}
}

func (r *adRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-r.window)
	hits := r.hits[ip]
	fresh := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= r.maxHits {
		r.hits[ip] = fresh
		return false
	}
	fresh = append(fresh, now)
	r.hits[ip] = fresh
	if len(r.hits) > 10000 {
		for k, v := range r.hits {
			ok := false
			for _, t := range v {
				if t.After(cutoff) {
					ok = true
					break
				}
			}
			if !ok {
				delete(r.hits, k)
			}
		}
	}
	return true
}

type byeRecord struct {
	Reason    string
	ExpiresAt time.Time
}

// Hub 维护所有在线 Agent 连接
type Hub struct {
	store   *Store
	cfg     *Config
	mu      sync.RWMutex
	conns   map[int64]*AgentConn // nodeID → conn
	adLimit *adRateLimiter

	byeMu   sync.Mutex
	lastBye map[int64]byeRecord // 保存最近一次 bye reason,TTL 5 分钟
}

type AgentConn struct {
	NodeID    int64
	conn      *websocket.Conn
	writeMu   sync.Mutex
	send      chan []byte
	closeOnce sync.Once
	closed    chan struct{}
	hub       *Hub

	heartbeatSec int // 用于设置 read deadline
}

func NewHub(store *Store, cfg *Config) *Hub {
	return &Hub{
		store:   store,
		cfg:     cfg,
		conns:   make(map[int64]*AgentConn),
		adLimit: newADRateLimiter(),
		lastBye: make(map[int64]byeRecord),
	}
}

// recordBye 记录节点下线的原因,保留 5 分钟
func (h *Hub) recordBye(nodeID int64, reason string) {
	h.byeMu.Lock()
	h.lastBye[nodeID] = byeRecord{Reason: reason, ExpiresAt: time.Now().Add(5 * time.Minute)}
	h.byeMu.Unlock()
}

// LastByeReason 返回节点最近一次 bye 的原因(空字符串表示无记录或已过期)
func (h *Hub) LastByeReason(nodeID int64) string {
	h.byeMu.Lock()
	defer h.byeMu.Unlock()
	r, ok := h.lastBye[nodeID]
	if !ok || time.Now().After(r.ExpiresAt) {
		delete(h.lastBye, nodeID)
		return ""
	}
	return r.Reason
}

// clientIP 从 X-Real-IP / X-Forwarded-For / RemoteAddr 中提取客户端 IP。
// 注意 trim 空格,避免同一 IP 的不同空格写法被当成多个 IP(影响限流)。
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// 取第一个,trim 空格
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// HandleWS 是 /ws/agent 的 HTTP handler
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("ws upgrade:", err)
		return
	}

	// 等 hello
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}
	var env shared.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		conn.Close()
		return
	}
	if env.Type != shared.MsgTypeHello {
		conn.Close()
		return
	}
	var hello shared.HelloPayload
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		conn.Close()
		return
	}

	// 协议版本检查(0 视为旧 agent,放行)
	if hello.ProtocolVersion != 0 && hello.ProtocolVersion != shared.ProtocolVersion {
		log.Printf("ws: protocol version mismatch (agent=%d, server=%d), refusing",
			hello.ProtocolVersion, shared.ProtocolVersion)
		writeEnvelopeOnce(conn, shared.MsgTypeError, shared.ErrorPayload{
			Code:    "protocol_version_mismatch",
			Message: "please upgrade agent",
		})
		conn.Close()
		return
	}

	ip := clientIP(r)

	// === 鉴权 + 节点解析 ===
	var node *Node
	if hello.Token != "" {
		node, err = h.store.GetNodeByToken(hello.Token)
		if err != nil {
			log.Printf("ws: unknown token from %s", ip)
			conn.Close()
			return
		}
	} else if hello.AutoDiscoveryKey != "" && h.cfg.AutoDiscoveryKey != "" &&
		hello.AutoDiscoveryKey == h.cfg.AutoDiscoveryKey {
		// 速率限制:防止 AD key 泄露后被批量注册
		if !h.adLimit.Allow(ip) {
			log.Printf("ws: auto-discovery rate limited from %s", ip)
			conn.Close()
			return
		}
		// 原子地"按指纹查重 / 找不到就建",避免并发同指纹建两条
		name := hello.Hostname
		if name == "" {
			name = "unnamed"
		}
		if hello.MachineIDSuspect {
			log.Printf("ws: machine-id suspect from %s, dedup skipped for machine_id", ip)
		}
		n, created, ferr := h.store.FindOrCreateNodeByFingerprint(hello.MachineID, hello.PrimaryMAC, name, hello.MachineIDSuspect)
		if ferr != nil {
			log.Println("auto-create node failed:", ferr)
			conn.Close()
			return
		}
		node = n
		if created {
			log.Printf("auto-registered node #%d %q from %s", node.ID, name, ip)
		} else {
			log.Printf("auto-discovery: matched existing node #%d by fingerprint", node.ID)
		}
	} else {
		log.Printf("ws: auth failed from %s", ip)
		conn.Close()
		return
	}

	// 更新节点静态信息 + 指纹
	h.store.UpdateNodeInfo(node.ID,
		hello.OS, hello.Platform, hello.Arch, hello.CPUModel,
		hello.CPUCores, hello.MemTotal, hello.DiskTotal,
		hello.AgentVersion, hello.BootTime)
	h.store.UpdateNodeFingerprint(node.ID, hello.MachineID, hello.PrimaryMAC)

	// 发 hello_ack(下发主控配置的采集/心跳间隔)
	interval := h.cfg.AgentInterval
	heartbeat := h.cfg.AgentHeartbeat
	ack := shared.HelloAckPayload{
		ProtocolVersion: shared.ProtocolVersion,
		NodeID:          node.ID,
		NodeName:        node.Name,
		Interval:        interval,
		HeartbeatSec:    heartbeat,
	}
	if err := writeEnvelopeOnce(conn, shared.MsgTypeHelloAck, ack); err != nil {
		conn.Close()
		return
	}

	// 注册到 hub
	ac := &AgentConn{
		NodeID:       node.ID,
		conn:         conn,
		send:         make(chan []byte, 64),
		closed:       make(chan struct{}),
		hub:          h,
		heartbeatSec: heartbeat,
	}
	h.register(ac)
	defer h.unregister(ac)

	log.Printf("agent #%d %q connected from %s", node.ID, node.Name, ip)

	// 设置 read deadline 与 Pong handler。
	// 服务端不主动发底层 Ping(由 agent 主动发),只在收到 Ping/Pong/任何消息时刷新窗口。
	ac.setupKeepalive()

	// 下发探针配置(异步,避免阻塞握手)
	go h.sendProbeConfig(ac)

	// 启动 writer 和 reader
	go ac.writer()
	ac.reader()
}

func (h *Hub) register(c *AgentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.conns[c.NodeID]; ok {
		old.Close()
	}
	h.conns[c.NodeID] = c
}

func (h *Hub) unregister(c *AgentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[c.NodeID]; ok && cur == c {
		delete(h.conns, c.NodeID)
	}
	c.Close()
}

func (h *Hub) IsOnline(nodeID int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

func (h *Hub) OnlineNodes() []int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]int64, 0, len(h.conns))
	for id := range h.conns {
		out = append(out, id)
	}
	return out
}

// SendTo 给指定节点发指令。
// 返回 ErrNodeOffline 表示节点不在线;返回 ErrSendBufferFull 表示节点在线但发送缓冲已满。
func (h *Hub) SendTo(nodeID int64, msgType string, payload any) error {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	data, err := shared.MakeEnvelope(msgType, payload)
	if err != nil {
		return err
	}
	select {
	case c.send <- data:
		return nil
	default:
		return ErrSendBufferFull
	}
}

// === AgentConn methods ===

func (c *AgentConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.conn.Close()
	})
}

// setupKeepalive 配置 read deadline 与 Pong handler。
// 任何收到的消息(包括底层 Ping/Pong)都会刷新 read deadline。
func (c *AgentConn) setupKeepalive() {
	hb := c.heartbeatSec
	if hb <= 0 {
		hb = 30
	}
	window := time.Duration(float64(hb)*2.5) * time.Second
	if window < 30*time.Second {
		window = 30 * time.Second
	}
	reset := func() {
		c.conn.SetReadDeadline(time.Now().Add(window))
	}
	reset()
	c.conn.SetPongHandler(func(string) error {
		reset()
		return nil
	})
	defaultPing := c.conn.PingHandler()
	c.conn.SetPingHandler(func(s string) error {
		reset()
		return defaultPing(s) // gorilla 默认会自动回 Pong
	})
}

func (c *AgentConn) reader() {
	defer c.Close()
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var env shared.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Type {
		case shared.MsgTypePing:
			// 应用层 ping 兼容(老 agent 可能用)
			c.sendRaw(shared.MsgTypePong, nil)
		case shared.MsgTypePong:
			// 应用层 pong,忽略
		case shared.MsgTypeMetric:
			c.handleMetric(env.Payload)
		case shared.MsgTypeMetricBatch:
			c.handleMetricBatch(env.Payload)
		case shared.MsgTypeBye:
			var bye shared.ByePayload
			if len(env.Payload) > 0 {
				_ = json.Unmarshal(env.Payload, &bye)
			}
			reason := bye.Reason
			if reason == "" {
				reason = "unknown"
			}
			log.Printf("agent #%d sent bye (reason=%s)", c.NodeID, reason)
			c.hub.recordBye(c.NodeID, reason)
			return
		case shared.MsgTypeProbeResult:
			c.handleProbeResult(env.Payload)
		}
	}
}

func (c *AgentConn) writer() {
	for {
		select {
		case <-c.closed:
			return
		case msg := <-c.send:
			c.writeMu.Lock()
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := c.conn.WriteMessage(websocket.TextMessage, msg)
			c.writeMu.Unlock()
			if err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *AgentConn) sendRaw(msgType string, payload any) {
	data, err := shared.MakeEnvelope(msgType, payload)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	c.conn.WriteMessage(websocket.TextMessage, data)
}

// applyMetric 把一条 metric 持久化:写 metrics 表(含列存)+ 更新 last_seen + 累加流量。
// raw 是这条 metric 的原始 JSON 字节(直接存进 data 列,避免再 marshal 一遍)。
func (c *AgentConn) applyMetric(m *shared.MetricPayload, raw []byte) {
	if m.Timestamp == 0 {
		m.Timestamp = time.Now().Unix()
	}
	c.hub.store.SaveMetric(c.NodeID, &MetricForStore{
		Timestamp:   m.Timestamp,
		Raw:         raw,
		CPU:         m.CPUUsage,
		MemUsed:     m.MemUsed,
		MemTotal:    m.MemTotal,
		Load1:       m.Load1,
		LatencyMS:   m.LatencyMS,
		LossPct:     m.LossPct,
		NetInDelta:  m.NetInDelta,
		NetOutDelta: m.NetOutDelta,
	})
	c.hub.store.UpdateLastSeen(c.NodeID, m.Timestamp)
	if m.NetInDelta > 0 || m.NetOutDelta > 0 {
		c.hub.store.AddTraffic(c.NodeID, m.NetInDelta, m.NetOutDelta)
	}
}

func (c *AgentConn) handleMetric(payload json.RawMessage) {
	var m shared.MetricPayload
	if err := json.Unmarshal(payload, &m); err != nil {
		log.Printf("warn: bad metric from node #%d: %v", c.NodeID, err)
		return
	}
	c.applyMetric(&m, []byte(payload))
}

func (c *AgentConn) handleMetricBatch(payload json.RawMessage) {
	var batch shared.MetricBatchPayload
	if err := json.Unmarshal(payload, &batch); err != nil {
		log.Printf("warn: bad metric_batch from node #%d: %v", c.NodeID, err)
		return
	}
	// 补传可能与新 metric 时间戳乱序,排序后再写入避免覆盖更新的数据
	sort.SliceStable(batch.Items, func(i, j int) bool {
		return batch.Items[i].Timestamp < batch.Items[j].Timestamp
	})
	log.Printf("node #%d replayed %d buffered metrics", c.NodeID, len(batch.Items))
	for i := range batch.Items {
		// 对补传的每条 metric 重新序列化一次拿到 data blob。
		// 这是冷路径(只在重连时触发),开销可以接受。
		raw, err := json.Marshal(&batch.Items[i])
		if err != nil {
			continue
		}
		c.applyMetric(&batch.Items[i], raw)
	}
}

// sendProbeConfig 把分配给该节点的探针目标下发给被控
func (h *Hub) sendProbeConfig(ac *AgentConn) {
	targets, err := h.store.ListProbeTargets()
	if err != nil {
		return
	}
	var assigned []shared.ProbeTargetItem
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		var nodeIDs []int64
		json.Unmarshal([]byte(t.NodeIDs), &nodeIDs) //nolint:errcheck
		for _, nid := range nodeIDs {
			if nid == ac.NodeID {
				assigned = append(assigned, shared.ProbeTargetItem{
					ID: t.ID, Name: t.Name, IP: t.IP, Port: t.Port, Proto: t.Proto,
				})
				break
			}
		}
	}
	if len(assigned) == 0 {
		return
	}
	ac.sendRaw(shared.MsgTypeProbeConfig, shared.ProbeConfigPayload{Targets: assigned})
}

// PushProbeConfig 当探针目标变更时, 向已在线的被分配节点推送最新配置
func (h *Hub) PushProbeConfig(nodeID int64) {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	go h.sendProbeConfig(ac)
}

func (c *AgentConn) handleProbeResult(payload json.RawMessage) {
	var res shared.ProbeResultPayload
	if err := json.Unmarshal(payload, &res); err != nil {
		log.Printf("warn: bad probe_result from node #%d: %v", c.NodeID, err)
		return
	}
	if res.Timestamp == 0 {
		res.Timestamp = time.Now().Unix()
	}
	for _, item := range res.Items {
		_ = c.hub.store.SaveProbeResult(&ProbeResultRow{
			TargetID:  item.TargetID,
			NodeID:    c.NodeID,
			TS:        res.Timestamp,
			Proto:     item.Proto,
			LatencyMS: item.LatencyMS,
			Success:   item.Success,
		})
	}
}

// BroadcastMigration 向所有在线节点广播迁移种子。
// 针对每个节点用其自身 token 做 HMAC key，保证签名不可伪造。
// 返回成功发出的节点数和各节点的错误描述。
func (h *Hub) BroadcastMigration(newServer, newToken string) (sent int, errs []string) {
	h.mu.RLock()
	nodeIDs := make([]int64, 0, len(h.conns))
	for id := range h.conns {
		nodeIDs = append(nodeIDs, id)
	}
	h.mu.RUnlock()

	for _, nodeID := range nodeIDs {
		node, err := h.store.GetNode(nodeID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("node#%d: get token: %v", nodeID, err))
			continue
		}
		mac := hmac.New(sha256.New, []byte(node.Token))
		mac.Write([]byte(newServer + "\n" + newToken))
		sig := hex.EncodeToString(mac.Sum(nil))
		p := shared.MigrationPayload{NewServer: newServer, NewToken: newToken, HMAC: sig}
		if err := h.SendTo(nodeID, shared.MsgTypeMigration, p); err != nil {
			errs = append(errs, fmt.Sprintf("node#%d: %v", nodeID, err))
		} else {
			sent++
		}
	}
	return
}

// writeEnvelopeOnce 不加锁的单次写入(仅握手阶段使用)
func writeEnvelopeOnce(conn *websocket.Conn, msgType string, payload any) error {
	data, err := shared.MakeEnvelope(msgType, payload)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}
