package server

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"mote/internal/agent"
)

const kvKeySelfNodeID = "self_monitor_node_id"

// SelfMonitor 让主控自身也作为一个被监控节点上报指标,直接写库不走 WebSocket。
type SelfMonitor struct {
	store     *Store
	cfg       *Config
	collector *agent.Collector
	nodeID    int64
	stop      chan struct{}
}

func NewSelfMonitor(store *Store, cfg *Config) *SelfMonitor {
	return &SelfMonitor{
		store: store,
		cfg:   cfg,
		stop:  make(chan struct{}),
	}
}

func (sm *SelfMonitor) Start() {
	nodeID, err := sm.resolveNode()
	if err != nil {
		log.Printf("self_monitor: resolve node failed: %v", err)
		return
	}
	sm.nodeID = nodeID

	agentCfg := agent.NewConfig("", "", "", "")
	sm.collector = agent.NewCollectorSimple(agentCfg)

	go sm.loop()
}

func (sm *SelfMonitor) Stop() {
	close(sm.stop)
}

// NodeID 返回自监控节点的 ID（供前端识别，隐藏卸载/reconfig 按钮）
func (sm *SelfMonitor) NodeID() int64 { return sm.nodeID }

func (sm *SelfMonitor) loop() {
	interval := sm.cfg.AgentInterval
	if interval <= 0 {
		interval = 2
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-sm.stop:
			return
		case <-ticker.C:
			sm.collect()
		}
	}
}

func (sm *SelfMonitor) collect() {
	m := sm.collector.Collect()
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	sm.store.SaveMetric(sm.nodeID, &MetricForStore{
		Timestamp:   now,
		Raw:         raw,
		CPU:         m.CPUUsage,
		MemUsed:     m.MemUsed,
		MemTotal:    m.MemTotal,
		Load1:       m.Load1,
		NetInDelta:  m.NetInDelta,
		NetOutDelta: m.NetOutDelta,
	})
	sm.store.UpdateLastSeen(sm.nodeID, now)
	if m.NetInDelta > 0 || m.NetOutDelta > 0 {
		sm.store.AddTraffic(sm.nodeID, m.NetInDelta, m.NetOutDelta)
	}
}

func (sm *SelfMonitor) resolveNode() (int64, error) {
	if v, ok := sm.store.KVGet(kvKeySelfNodeID); ok && v != "" {
		var id int64
		if _, err := fmt.Sscan(v, &id); err == nil && id > 0 {
			if n, err := sm.store.GetNode(id); err == nil && n != nil {
				return id, nil
			}
		}
	}
	// 新建自监控节点
	n, err := sm.store.CreateNode("localhost (主控)")
	if err != nil {
		return 0, err
	}
	if err := sm.store.KVSet(kvKeySelfNodeID, fmt.Sprintf("%d", n.ID)); err != nil {
		log.Printf("self_monitor: save node id to kv failed: %v", err)
	}
	log.Printf("self_monitor: created self node #%d", n.ID)
	return n.ID, nil
}
