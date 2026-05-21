package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"mote/internal/shared"
)

// 一次连接如果存活超过 stableThreshold,认为是健康连接,重置 failCount。
// 否则失败计数继续累加,触发指数退避。
const stableThreshold = 60 * time.Second

// metric 重连补传 ring buffer 的最大条数。每条 metric ~1KB,
// 60 条 ~60KB 内存,代价低,可覆盖几分钟离线后的补传。
const metricBufferCap = 60

// Run 阻塞运行 Agent 主循环。
// 收到 SIGTERM/SIGINT 时尝试 graceful shutdown,然后返回。
func Run(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	probe := newProbeTracker()
	collector := NewCollector(cfg, probe)

	// 监听信号
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	go func() {
		<-sigCh
		log.Println("signal received, shutting down")
		rootCancel()
	}()

	// metric 缓冲(在 Agent 进程生命周期内持续存在,跨连接保留)
	buf := newMetricBuffer(metricBufferCap)

	failCount := 0
	for {
		if rootCtx.Err() != nil {
			return nil
		}

		start := time.Now()
		err := runOnce(rootCtx, cfg, collector, probe, buf)
		alive := time.Since(start)

		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("connection ended: %v (alive=%s)", err, alive.Round(time.Second))
			failCount++
		} else if err == nil {
			// 正常退出(收到 uninstall),不再循环
			return nil
		}

		// 健康连接(存活超过阈值)后再断,认为是临时网络抖动,重置计数
		if alive >= stableThreshold {
			if failCount > 0 {
				log.Printf("connection was healthy (%s), resetting fail counter", alive.Round(time.Second))
			}
			failCount = 0
		}

		if rootCtx.Err() != nil {
			return nil
		}

		// 退避策略:
		// - 第一次失败立即重连(网络抖动通常瞬时恢复)
		// - 之后指数退避,上限 5 分钟
		// - 连续失败 1000 次(约半小时以上)后切到 1 小时低频
		var wait time.Duration
		switch {
		case failCount == 1:
			wait = 0
		case failCount > 1000:
			wait = time.Hour
		default:
			wait = backoff(failCount)
		}
		if wait > 0 {
			log.Printf("retry in %v (fail #%d)", wait, failCount)
		}
		select {
		case <-time.After(wait):
		case <-rootCtx.Done():
			return nil
		}
	}
}

// backoff 计算第 n 次失败的等待时间。n>=1。
// 序列: 1s, 2s, 4s, 8s, 16s, 32s, 64s, ... 直到 5 分钟封顶。
// 加上 0~25% 的随机抖动避免雪崩。
func backoff(n int) time.Duration {
	shift := n - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 8 {
		shift = 8
	}
	base := time.Second * time.Duration(1<<shift)
	if base > 5*time.Minute {
		base = 5 * time.Minute
	}
	maxJitter := int64(base / 4)
	if maxJitter <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(maxJitter))
}

// runOnce 建立一次 WS 连接并跑完它的生命周期。
// 返回 nil 表示收到 uninstall/正常退出,不应再重连;
// 返回非 nil 表示需要重连。
func runOnce(rootCtx context.Context, cfg *Config, collector *Collector, probe *probeTracker, buf *metricBuffer) error {
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		ReadBufferSize:    8192,
		WriteBufferSize:   8192,
		EnableCompression: !cfg.DisableCompression,
	}
	header := http.Header{}
	header.Set("User-Agent", "bk-agent/"+shared.Version)

	log.Printf("connecting to %s", cfg.Server)
	dialCtx, dialCancel := context.WithTimeout(rootCtx, 20*time.Second)
	conn, _, err := dialer.DialContext(dialCtx, cfg.Server+"/ws/agent", header)
	dialCancel()
	if err != nil {
		return err
	}

	// 用 defer 确保任何路径下连接都被关闭
	closed := false
	closeConn := func() {
		if !closed {
			closed = true
			conn.Close()
		}
	}
	defer closeConn()

	// 发 hello
	hello := CollectHello(cfg)
	if err := writeEnvelope(conn, shared.MsgTypeHello, hello, 10*time.Second); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// 等 hello_ack
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	var env shared.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse envelope: %w", err)
	}
	if env.Type != shared.MsgTypeHelloAck {
		return fmt.Errorf("expected hello_ack, got %q", env.Type)
	}
	var ack shared.HelloAckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		log.Printf("warn: parse hello_ack payload: %v", err)
		// 不致命:走 fallback
	}
	if ack.ProtocolVersion != 0 && ack.ProtocolVersion != shared.ProtocolVersion {
		return fmt.Errorf("protocol version mismatch: server=%d agent=%d",
			ack.ProtocolVersion, shared.ProtocolVersion)
	}
	interval := ack.Interval
	if interval <= 0 {
		interval = cfg.Interval
	}
	heartbeat := ack.HeartbeatSec
	if heartbeat <= 0 {
		heartbeat = cfg.Heartbeat
	}
	log.Printf("connected as node #%d (%s), interval=%ds, heartbeat=%ds",
		ack.NodeID, ack.NodeName, interval, heartbeat)

	// 持久化主控下发的 interval/heartbeat(如果不同于本地)
	if ack.Interval > 0 || ack.HeartbeatSec > 0 {
		cfg.SetRuntime(ack.Interval, ack.HeartbeatSec)
	}

	// === 配置 WS 心跳:基于底层 PingMessage,而不是 JSON 业务 ping。 ===
	// read deadline 设为心跳间隔的 2.5 倍,任何收到的消息(含 Pong)都会刷新。
	readWindow := time.Duration(float64(heartbeat) * 2.5 * float64(time.Second))
	if readWindow < 30*time.Second {
		readWindow = 30 * time.Second
	}
	resetReadDeadline := func() {
		conn.SetReadDeadline(time.Now().Add(readWindow))
	}
	resetReadDeadline()
	conn.SetPongHandler(func(string) error {
		if probe != nil {
			probe.markPong(time.Now())
		}
		resetReadDeadline()
		return nil
	})
	// 收到对端 Ping(标准 WS 控制帧)时,gorilla 默认会自动回 Pong,不需要我们处理。
	// 但我们额外用 SetPingHandler 刷新一下 read deadline。
	defaultPing := conn.PingHandler()
	conn.SetPingHandler(func(s string) error {
		resetReadDeadline()
		return defaultPing(s)
	})

	connCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()
	var wg sync.WaitGroup
	writeMu := &sync.Mutex{}

	// reconfig 通过 channel 通知 ticker 协程
	metricReconfig := make(chan int, 4)
	heartbeatReconfig := make(chan int, 4)

	// === 启动前:补传上次连接没发出去的 metric ===
	if pending := buf.drain(); len(pending) > 0 {
		log.Printf("replaying %d buffered metrics", len(pending))
		batch := shared.MetricBatchPayload{Items: pending}
		if err := writeEnvelopeSafe(writeMu, conn, shared.MsgTypeMetricBatch, batch, 15*time.Second); err != nil {
			log.Printf("replay batch failed: %v (will re-buffer)", err)
			buf.pushMany(pending)
			cancel()
		}
	}

	// === 采集协程 ===
	wg.Add(1)
	go func() {
		defer wg.Done()
		curInterval := interval
		ticker := time.NewTicker(time.Duration(curInterval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case newInt := <-metricReconfig:
				if newInt > 0 && newInt != curInterval {
					curInterval = newInt
					ticker.Stop()
					ticker = time.NewTicker(time.Duration(curInterval) * time.Second)
					log.Printf("metric ticker restarted: interval=%ds", curInterval)
				}
			case <-ticker.C:
				m := collector.Collect()
				if err := writeEnvelopeSafe(writeMu, conn, shared.MsgTypeMetric, m, 10*time.Second); err != nil {
					log.Printf("write metric error: %v (buffering)", err)
					buf.push(*m)
					cancel()
					return
				}
			}
		}
	}()

	// === 心跳协程:发送底层 WS Ping 控制帧 ===
	wg.Add(1)
	go func() {
		defer wg.Done()
		curHB := heartbeat
		ticker := time.NewTicker(time.Duration(curHB) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case newHB := <-heartbeatReconfig:
				if newHB > 0 && newHB != curHB {
					curHB = newHB
					ticker.Stop()
					ticker = time.NewTicker(time.Duration(curHB) * time.Second)
					// read deadline 窗口也跟着调整
					readWindow = time.Duration(float64(curHB) * 2.5 * float64(time.Second))
					if readWindow < 30*time.Second {
						readWindow = 30 * time.Second
					}
					log.Printf("heartbeat ticker restarted: %ds", curHB)
				}
			case <-ticker.C:
				if probe != nil {
					probe.markSent(time.Now())
				}
				writeMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
					log.Printf("ws ping error: %v", err)
					cancel()
					return
				}
			}
		}
	}()

	// === 监听 rootCtx 取消(SIGTERM)→ 发 bye 并退出 ===
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-rootCtx.Done():
			log.Println("graceful shutdown: sending bye")
			_ = writeEnvelopeSafe(writeMu, conn, shared.MsgTypeBye, shared.ByePayload{Reason: shared.ByeReasonShutdown}, 3*time.Second)
			// 主动发 close frame
			writeMu.Lock()
			conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
				time.Now().Add(3*time.Second))
			writeMu.Unlock()
			cancel()
		case <-connCtx.Done():
			// 由其他原因 cancel 触发,无需操作
		}
	}()

	// === 读循环 ===
	var readErr error
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		// 任何应用层消息也刷新 read deadline
		resetReadDeadline()

		var e shared.Envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			log.Printf("warn: bad envelope: %v", err)
			continue
		}
		switch e.Type {
		case shared.MsgTypePong:
			// 应用层 pong,忽略
		case shared.MsgTypePing:
			// 应用层 ping,回应用层 pong(兼容老服务端)
			_ = writeEnvelopeSafe(writeMu, conn, shared.MsgTypePong, nil, 5*time.Second)
		case shared.MsgTypeReconfig:
			handleReconfig(cfg, e.Payload, metricReconfig, heartbeatReconfig)
		case shared.MsgTypeUninstall:
			log.Println("received uninstall command, executing...")
			_ = writeEnvelopeSafe(writeMu, conn, shared.MsgTypeBye, shared.ByePayload{Reason: shared.ByeReasonUninstall}, 3*time.Second)
			cancel()
			wg.Wait()
			closeConn()
			triggerUninstall()
			return nil
		}
	}

	cancel()
	wg.Wait()

	// 返回 nil 表示 rootCtx 取消(graceful 退出),否则返回错误
	if rootCtx.Err() != nil {
		return nil
	}
	return readErr
}

// handleReconfig 解析主控下发的配置,持久化到 cfg,并通过 channel 通知 ticker 协程
func handleReconfig(cfg *Config, payload json.RawMessage, metricCh, hbCh chan<- int) {
	var rc shared.ReconfigPayload
	if err := json.Unmarshal(payload, &rc); err != nil {
		log.Printf("warn: parse reconfig: %v", err)
		return
	}
	cfg.SetRuntime(rc.Interval, rc.HeartbeatSec)
	if rc.Interval > 0 {
		select {
		case metricCh <- rc.Interval:
			log.Printf("reconfig: interval=%ds", rc.Interval)
		default:
			log.Printf("reconfig: metric channel full, dropped")
		}
	}
	if rc.HeartbeatSec > 0 {
		select {
		case hbCh <- rc.HeartbeatSec:
			log.Printf("reconfig: heartbeat=%ds", rc.HeartbeatSec)
		default:
			log.Printf("reconfig: heartbeat channel full, dropped")
		}
	}
}

// writeEnvelope 不加锁的写入(仅在握手阶段使用,此时没有并发 writer)
func writeEnvelope(conn *websocket.Conn, msgType string, payload any, timeout time.Duration) error {
	data, err := shared.MakeEnvelope(msgType, payload)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// writeEnvelopeSafe 并发安全的写入
func writeEnvelopeSafe(mu *sync.Mutex, conn *websocket.Conn, msgType string, payload any, timeout time.Duration) error {
	data, err := shared.MakeEnvelope(msgType, payload)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// metricBuffer 是一个并发安全的环形缓冲,
// 当 WS 写失败时把 metric 暂存,重连成功后批量补传。
// 满了之后丢弃最旧的(FIFO 覆盖)。
type metricBuffer struct {
	mu   sync.Mutex
	cap  int
	data []shared.MetricPayload
}

func newMetricBuffer(cap int) *metricBuffer {
	return &metricBuffer{cap: cap}
}

func (b *metricBuffer) push(m shared.MetricPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) >= b.cap {
		// 丢最旧的
		b.data = b.data[1:]
	}
	b.data = append(b.data, m)
}

func (b *metricBuffer) pushMany(ms []shared.MetricPayload) {
	for _, m := range ms {
		b.push(m)
	}
}

// drain 取出所有缓冲条目并清空。
func (b *metricBuffer) drain() []shared.MetricPayload {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) == 0 {
		return nil
	}
	out := b.data
	b.data = nil
	return out
}
