// Package shared 定义主控与被控之间的协议数据结构
package shared

import "encoding/json"

const ProtocolVersion = 1

// WebSocket 消息类型常量
const (
	MsgTypeHello       = "hello"        // 被控 → 主控:首次连接,自报家门
	MsgTypeHelloAck    = "hello_ack"    // 主控 → 被控:接受连接,下发节点 ID
	MsgTypeMetric      = "metric"       // 被控 → 主控:监控数据
	MsgTypeMetricBatch = "metric_batch" // 被控 → 主控:批量补传(重连后)
	MsgTypePing        = "ping"         // 双向心跳(应用层,作为底层 WS Ping 的补充)
	MsgTypePong        = "pong"         // 双向心跳响应
	MsgTypeReconfig    = "reconfig"     // 主控 → 被控:重新加载配置
	MsgTypeUninstall   = "uninstall"    // 主控 → 被控:卸载自己
	MsgTypeMigration   = "migration"    // 主控 → 被控:下发迁移种子(MVP 暂不实现)
	MsgTypeBye         = "bye"          // 被控 → 主控:主动下线通知(graceful)
	MsgTypeError       = "error"        // 通用错误响应
	MsgTypeProbeConfig = "probe_config" // 主控 → 被控:下发探针目标列表
	MsgTypeProbeResult = "probe_result" // 被控 → 主控:探针结果上报
)

// Envelope 是所有 WS 消息的统一外层。
// Payload 使用 json.RawMessage,避免 "marshal→unmarshal" 两次反序列化。
// 调用方拿到 envelope 后,自己用 json.Unmarshal(env.Payload, &具体类型) 一次到位。
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MakeEnvelope 是构造带 payload 的 envelope 的便捷函数。
func MakeEnvelope(msgType string, payload any) ([]byte, error) {
	if payload == nil {
		return json.Marshal(Envelope{Type: msgType})
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Type: msgType, Payload: p})
}

// HelloPayload 被控首次连接时上报的元信息
type HelloPayload struct {
	ProtocolVersion  int    `json:"protocol_version"`
	Token            string `json:"token,omitempty"`
	AutoDiscoveryKey string `json:"ad_key,omitempty"`

	// 机器指纹(用于 AD 自动注册时去重,避免反复 reinstall 产生重复节点)
	MachineID        string `json:"machine_id,omitempty"`
	PrimaryMAC       string `json:"primary_mac,omitempty"`
	MachineIDSuspect bool   `json:"machine_id_suspect,omitempty"` // 疑似容器/克隆 VM 的不可靠 ID

	Hostname      string `json:"hostname"`
	OS            string `json:"os"`       // linux/windows/darwin
	Platform      string `json:"platform"` // ubuntu/debian/centos...
	PlatformVer   string `json:"platform_version"`
	KernelVersion string `json:"kernel"`
	Arch          string `json:"arch"` // amd64/arm64
	CPUModel      string `json:"cpu_model"`
	CPUCores      int    `json:"cpu_cores"`
	MemTotal      uint64 `json:"mem_total"`
	DiskTotal     uint64 `json:"disk_total"`
	AgentVersion  string `json:"agent_version"`
	BootTime      int64  `json:"boot_time"`
}

// HelloAckPayload 主控接受连接后下发
type HelloAckPayload struct {
	ProtocolVersion int    `json:"protocol_version"`
	NodeID          int64  `json:"node_id"`
	NodeName        string `json:"node_name"`
	Interval        int    `json:"interval"`  // 采集间隔秒;0 表示让 agent 用本地配置
	HeartbeatSec    int    `json:"heartbeat"` // 心跳间隔秒;0 表示让 agent 用本地配置
}

// MetricPayload 被控上报的监控数据
type MetricPayload struct {
	Timestamp int64 `json:"t"`

	// CPU
	CPUUsage   float64   `json:"cpu"`
	CPUPerCore []float64 `json:"cpu_per,omitempty"`
	Load1      float64   `json:"load1"`
	Load5      float64   `json:"load5"`
	Load15     float64   `json:"load15"`

	// 内存
	MemUsed   uint64 `json:"mem_u"`
	MemTotal  uint64 `json:"mem_t"`
	SwapUsed  uint64 `json:"swap_u"`
	SwapTotal uint64 `json:"swap_t"`

	// 磁盘
	Disks          []DiskInfo `json:"disks"`
	DisksTruncated bool       `json:"disks_truncated,omitempty"`

	// 网络(增量)
	NetInDelta  uint64 `json:"net_in_d"`  // 自上次上报以来的入站字节数
	NetOutDelta uint64 `json:"net_out_d"` // 出站
	NetInSpeed  uint64 `json:"net_in_s"`  // 当前速率(字节/秒)
	NetOutSpeed uint64 `json:"net_out_s"`
	LatencyMS   float64 `json:"latency_ms"`
	LossPct     float64 `json:"loss_pct"`

	// 其他
	ProcessCount int    `json:"proc"`
	TCPConn      int    `json:"tcp"`
	UDPConn      int    `json:"udp"`
	Uptime       uint64 `json:"uptime"`
}

// MetricBatchPayload 重连后补传的若干条 metric。
type MetricBatchPayload struct {
	Items []MetricPayload `json:"items"`
}

type DiskInfo struct {
	Mountpoint string `json:"mp"`
	Used       uint64 `json:"u"`
	Total      uint64 `json:"t"`
}

// ReconfigPayload 主控下发的配置变更
type ReconfigPayload struct {
	Interval     int `json:"interval,omitempty"`
	HeartbeatSec int `json:"heartbeat,omitempty"`
}

// ByePayload agent 主动下线时携带的原因
type ByePayload struct {
	Reason string `json:"reason,omitempty"`
}

const (
	ByeReasonShutdown  = "shutdown"  // systemctl stop / SIGTERM
	ByeReasonUninstall = "uninstall" // 收到 uninstall 指令
	ByeReasonReconfig  = "reconfig"  // reconfig 后重启
)

// ErrorPayload 错误信息
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ProbeTargetItem 主控下发给被控的单条探针目标
type ProbeTargetItem struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	IP    string `json:"ip"`
	Port  int    `json:"port,omitempty"` // TCP 端口,0 表示不做 TCP 探针
	Proto string `json:"proto"`           // "icmp" / "tcp" / "both"
}

// ProbeConfigPayload 主控 → 被控:推送探针目标列表
type ProbeConfigPayload struct {
	Targets []ProbeTargetItem `json:"targets"`
}

// ProbeResultItem 单条探针测量结果
type ProbeResultItem struct {
	TargetID  int64   `json:"target_id"`
	Proto     string  `json:"proto"` // "icmp" or "tcp"
	LatencyMS float64 `json:"latency_ms"`
	Success   bool    `json:"success"`
}

// ProbeResultPayload 被控 → 主控:一批探针结果
type ProbeResultPayload struct {
	Timestamp int64             `json:"t"`
	Items     []ProbeResultItem `json:"items"`
}
