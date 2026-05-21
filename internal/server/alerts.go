package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"mote/internal/shared"
)

// AlertEngine 在 scheduler tick 里被调用,扫描所有规则并触发通知
type AlertEngine struct {
	store *Store
	hub   *Hub // 用来判断节点是否在线
}

func NewAlertEngine(store *Store, hub *Hub) *AlertEngine {
	return &AlertEngine{store: store, hub: hub}
}

// Tick 评估所有规则,触发或恢复告警
func (e *AlertEngine) Tick() {
	rules, err := e.store.ListAlertRules()
	if err != nil {
		log.Printf("alert: list rules failed: %v", err)
		return
	}
	if len(rules) == 0 {
		return
	}
	nodes, err := e.store.ListNodes()
	if err != nil {
		log.Printf("alert: list nodes failed: %v", err)
		return
	}

	now := time.Now()
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		// 静默时段检查
		if inSilentWindow(now, rule.SilentFrom, rule.SilentTo) {
			continue
		}
		targets := filterTargets(rule.Target, nodes)
		for _, node := range targets {
			e.evaluateOne(rule, node, now)
		}
	}
}

func (e *AlertEngine) evaluateOne(rule *AlertRule, node *Node, now time.Time) {
	switch rule.Kind {
	case "offline":
		e.evalOffline(rule, node, now)
	case "online":
		e.evalOnline(rule, node, now)
	case "cpu":
		e.evalThreshold(rule, node, now, "cpu")
	case "mem":
		e.evalThreshold(rule, node, now, "mem")
	case "disk":
		e.evalThreshold(rule, node, now, "disk")
	case "load":
		e.evalThreshold(rule, node, now, "load")
	case "traffic":
		e.evalTraffic(rule, node, now)
	case "due":
		e.evalDue(rule, node, now)
	}
}

// === 离线/上线 ===

func (e *AlertEngine) evalOffline(rule *AlertRule, node *Node, now time.Time) {
	// 冷启动宽限:节点从未上报 metric,跳过离线判定
	if node.LastSeen == 0 {
		return
	}
	thresholdSec := int64(rule.Threshold)
	if thresholdSec <= 0 {
		thresholdSec = 180
	}
	online := e.hub.IsOnline(node.ID)
	stale := !online && now.Unix()-node.LastSeen > thresholdSec

	st, _ := e.store.GetAlertState(rule.ID, node.ID, "")
	if stale {
		// 首次发现:记录 first_breach,但不立刻发(看 duration)
		if st == nil {
			_ = e.store.UpsertAlertState(&AlertState{
				RuleID:      rule.ID,
				NodeID:      node.ID,
				FirstBreach: now.Unix(),
				Active:      false,
			})
			return
		}
		// duration 未到
		if rule.Duration > 0 && now.Unix()-st.FirstBreach < int64(rule.Duration) {
			return
		}
		// 冷却内不重发
		if st.Active && now.Unix()-st.LastAlerted < int64(rule.Cooldown) {
			return
		}
		byeReason := e.hub.LastByeReason(node.ID)
		body := fmt.Sprintf("已失联超过 %d 秒", thresholdSec)
		if byeReason == shared.ByeReasonShutdown || byeReason == shared.ByeReasonUninstall {
			body = "用户主动停止（" + byeReason + "）"
		}
		e.fire(rule, node, &NotifyMessage{
			Title:    "节点离线",
			Body:     body,
			Level:    "error",
			NodeName: node.Name,
		})
		st.Active = true
		st.LastAlerted = now.Unix()
		_ = e.store.UpsertAlertState(st)
	} else {
		// 恢复:若之前 active,发恢复通知然后清状态
		if st != nil && st.Active {
			e.fire(rule, node, &NotifyMessage{
				Title:    "节点恢复在线",
				Body:     "节点心跳已恢复",
				Level:    "ok",
				NodeName: node.Name,
			})
		}
		if st != nil {
			_ = e.store.ClearAlertState(rule.ID, node.ID, "")
		}
	}
}

func (e *AlertEngine) evalOnline(rule *AlertRule, node *Node, now time.Time) {
	// "上线"规则:只在 last_seen 从 0 变为非 0 时通知一次(首次连接)。
	// 这是冷启动场景,用 scope="first" 永久标记。
	if node.LastSeen == 0 {
		return
	}
	st, _ := e.store.GetAlertState(rule.ID, node.ID, "first")
	if st != nil {
		return // 已经通知过
	}
	e.fire(rule, node, &NotifyMessage{
		Title:    "节点首次上线",
		Body:     fmt.Sprintf("节点 %q 已加入监控", node.Name),
		Level:    "ok",
		NodeName: node.Name,
	})
	_ = e.store.UpsertAlertState(&AlertState{
		RuleID:      rule.ID,
		NodeID:      node.ID,
		Scope:       "first",
		FirstBreach: now.Unix(),
		LastAlerted: now.Unix(),
		Active:      true,
	})
}

// === CPU/Mem/Disk/Load 通用阈值 ===

func (e *AlertEngine) evalThreshold(rule *AlertRule, node *Node, now time.Time, kind string) {
	data, _, err := e.store.GetLatestMetric(node.ID)
	if err != nil {
		return // 没数据,跳过
	}
	var m metricSnapshot
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}

	var current float64
	var unit string
	var detail string
	switch kind {
	case "cpu":
		current = m.CPU
		unit = "%"
		detail = fmt.Sprintf("当前 CPU %.1f%%", current)
	case "mem":
		if m.MemTotal > 0 {
			current = float64(m.MemUsed) / float64(m.MemTotal) * 100
			unit = "%"
			detail = fmt.Sprintf("内存 %.1f%% (%s/%s)", current,
				humanBytes(m.MemUsed), humanBytes(m.MemTotal))
		}
	case "disk":
		// 取最大占用率的挂载点
		var worst struct {
			mp  string
			pct float64
		}
		for _, d := range m.Disks {
			if d.Total == 0 {
				continue
			}
			p := float64(d.Used) / float64(d.Total) * 100
			if p > worst.pct {
				worst.pct = p
				worst.mp = d.Mountpoint
			}
		}
		current = worst.pct
		unit = "%"
		detail = fmt.Sprintf("挂载点 %s 使用率 %.1f%%", worst.mp, current)
	case "load":
		current = m.Load1
		detail = fmt.Sprintf("Load1 = %.2f", current)
	}

	st, _ := e.store.GetAlertState(rule.ID, node.ID, "")
	overThreshold := current >= rule.Threshold

	if overThreshold {
		if st == nil {
			_ = e.store.UpsertAlertState(&AlertState{
				RuleID:      rule.ID,
				NodeID:      node.ID,
				FirstBreach: now.Unix(),
			})
			return
		}
		if rule.Duration > 0 && now.Unix()-st.FirstBreach < int64(rule.Duration) {
			return
		}
		if st.Active && now.Unix()-st.LastAlerted < int64(rule.Cooldown) {
			return
		}
		title := map[string]string{
			"cpu":  "CPU 使用率告警",
			"mem":  "内存使用率告警",
			"disk": "磁盘使用率告警",
			"load": "负载告警",
		}[kind]
		body := detail
		if unit == "%" {
			body += fmt.Sprintf(" (阈值 %.0f%%)", rule.Threshold)
		} else {
			body += fmt.Sprintf(" (阈值 %.2f)", rule.Threshold)
		}
		if rule.Duration > 0 {
			body += fmt.Sprintf("\n已持续 %ds", now.Unix()-st.FirstBreach)
		}
		e.fire(rule, node, &NotifyMessage{
			Title: title, Body: body, Level: "warn", NodeName: node.Name,
		})
		st.Active = true
		st.LastAlerted = now.Unix()
		_ = e.store.UpsertAlertState(st)
	} else {
		if st != nil && st.Active {
			e.fire(rule, node, &NotifyMessage{
				Title:    map[string]string{"cpu": "CPU 恢复正常", "mem": "内存恢复正常", "disk": "磁盘恢复正常", "load": "负载恢复正常"}[kind],
				Body:     detail,
				Level:    "ok",
				NodeName: node.Name,
			})
		}
		if st != nil {
			_ = e.store.ClearAlertState(rule.ID, node.ID, "")
		}
	}
}

// === 流量 ===

// 触发点固定 50/80/95/100,每个点只发一次,周期重置后再走一轮
func (e *AlertEngine) evalTraffic(rule *AlertRule, node *Node, now time.Time) {
	meta, err := e.store.GetMeta(node.ID)
	if err != nil || meta.TrafficLimit == 0 {
		return
	}
	// 算用量
	var used uint64
	switch meta.TrafficType {
	case "in":
		used = meta.TrafficUsedIn
	case "out":
		used = meta.TrafficUsedOut
	case "max":
		if meta.TrafficUsedIn > meta.TrafficUsedOut {
			used = meta.TrafficUsedIn
		} else {
			used = meta.TrafficUsedOut
		}
	default:
		used = meta.TrafficUsedIn + meta.TrafficUsedOut
	}
	pct := float64(used) / float64(meta.TrafficLimit) * 100

	// 用 period_start 作为 scope 前缀,周期一变 scope 就变,自动重置
	periodTag := fmt.Sprintf("p%d-", meta.TrafficPeriodStart)
	stages := []int{50, 80, 95, 100}
	for _, stage := range stages {
		if pct < float64(stage) {
			continue
		}
		scope := periodTag + strconv.Itoa(stage)
		if st, _ := e.store.GetAlertState(rule.ID, node.ID, scope); st != nil {
			continue // 这个阶段在本周期内已触发
		}
		var level string
		if stage >= 100 {
			level = "error"
		} else if stage >= 80 {
			level = "warn"
		} else {
			level = "info"
		}
		body := fmt.Sprintf("用量已达 %d%% (%s / %s)",
			stage, humanBytes(used), humanBytes(meta.TrafficLimit))
		e.fire(rule, node, &NotifyMessage{
			Title:    fmt.Sprintf("流量 %d%% 阈值", stage),
			Body:     body,
			Level:    level,
			NodeName: node.Name,
		})
		_ = e.store.UpsertAlertState(&AlertState{
			RuleID:      rule.ID,
			NodeID:      node.ID,
			Scope:       scope,
			FirstBreach: now.Unix(),
			LastAlerted: now.Unix(),
			Active:      true,
		})
	}
}

// === 续费到期 ===

type dueExtra struct {
	Days []int `json:"days"`
}

func (e *AlertEngine) evalDue(rule *AlertRule, node *Node, now time.Time) {
	meta, err := e.store.GetMeta(node.ID)
	if err != nil || meta.NextDue == 0 {
		return
	}
	days := []int{7, 3, 1}
	var ex dueExtra
	if rule.Extra != "" {
		_ = json.Unmarshal([]byte(rule.Extra), &ex)
	}
	if len(ex.Days) > 0 {
		days = ex.Days
	}
	// 距离到期还有几天(向下取整,负数表示已过期)
	daysLeft := int((meta.NextDue - now.Unix()) / 86400)

	// 用 next_due 作为 scope 前缀,确保下次续费后重新走一轮
	periodTag := fmt.Sprintf("due%d-", meta.NextDue)
	for _, d := range days {
		// 只在 daysLeft <= d 且没发过这个档位时发
		if daysLeft > d {
			continue
		}
		scope := periodTag + strconv.Itoa(d)
		if st, _ := e.store.GetAlertState(rule.ID, node.ID, scope); st != nil {
			continue
		}
		level := "info"
		if d <= 1 {
			level = "warn"
		}
		body := fmt.Sprintf("距到期还有 %d 天\n到期时间: %s",
			daysLeft, time.Unix(meta.NextDue, 0).Format("2006-01-02"))
		if meta.Price > 0 {
			body += fmt.Sprintf("\n金额: %.2f %s / %s", meta.Price, meta.Currency, cycleLabel(meta.Cycle))
		}
		e.fire(rule, node, &NotifyMessage{
			Title:    fmt.Sprintf("续费提醒 (剩 %d 天)", daysLeft),
			Body:     body,
			Level:    level,
			NodeName: node.Name,
		})
		_ = e.store.UpsertAlertState(&AlertState{
			RuleID:      rule.ID,
			NodeID:      node.ID,
			Scope:       scope,
			FirstBreach: now.Unix(),
			LastAlerted: now.Unix(),
			Active:      true,
		})
	}
}

// === 工具 ===

func (e *AlertEngine) fire(rule *AlertRule, node *Node, msg *NotifyMessage) {
	msg.Timestamp = time.Now().Unix()
	var ids []int64
	if rule.NotifierIDs != "" {
		_ = json.Unmarshal([]byte(rule.NotifierIDs), &ids)
	}
	if len(ids) == 0 {
		return // 规则没绑通知渠道
	}
	Dispatch(e.store, ids, msg)
}

type metricSnapshot struct {
	CPU       float64 `json:"cpu"`
	Load1     float64 `json:"load1"`
	MemUsed   uint64  `json:"mem_u"`
	MemTotal  uint64  `json:"mem_t"`
	Disks     []struct {
		Mountpoint string `json:"mp"`
		Used       uint64 `json:"u"`
		Total      uint64 `json:"t"`
	} `json:"disks"`
}

func filterTargets(target string, all []*Node) []*Node {
	if target == "" || target == "all" {
		return all
	}
	if strings.HasPrefix(target, "node:") {
		idsStr := strings.TrimPrefix(target, "node:")
		ids := map[int64]bool{}
		for _, s := range strings.Split(idsStr, ",") {
			if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				ids[v] = true
			}
		}
		var out []*Node
		for _, n := range all {
			if ids[n.ID] {
				out = append(out, n)
			}
		}
		return out
	}
	if strings.HasPrefix(target, "tag:") {
		tag := strings.TrimPrefix(target, "tag:")
		var out []*Node
		for _, n := range all {
			for _, t := range n.Tags {
				if t == tag {
					out = append(out, n)
					break
				}
			}
		}
		return out
	}
	return all
}

func inSilentWindow(now time.Time, from, to string) bool {
	if from == "" || to == "" {
		return false
	}
	fh, fm, ok := parseHM(from)
	if !ok {
		return false
	}
	th, tm, ok := parseHM(to)
	if !ok {
		return false
	}
	curMin := now.Hour()*60 + now.Minute()
	fromMin := fh*60 + fm
	toMin := th*60 + tm
	if fromMin == toMin {
		return false
	}
	if fromMin < toMin {
		return curMin >= fromMin && curMin < toMin
	}
	// 跨天(如 22:00 - 08:00)
	return curMin >= fromMin || curMin < toMin
}

func parseHM(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func humanBytes(n uint64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"K", "M", "G", "T", "P"}
	v := float64(n) / k
	u := 0
	for v >= k && u < len(units)-1 {
		v /= k
		u++
	}
	if v < 10 {
		return fmt.Sprintf("%.2f%sB", v, units[u])
	}
	return fmt.Sprintf("%.1f%sB", v, units[u])
}

func cycleLabel(c string) string {
	return map[string]string{
		"monthly": "月", "quarterly": "季", "semiannually": "半年",
		"yearly": "年", "biennially": "两年", "once": "一次性", "lifetime": "终身",
	}[c]
}
