package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			token TEXT UNIQUE NOT NULL,
			group_name TEXT DEFAULT '',
			tags TEXT DEFAULT '[]',
			os TEXT DEFAULT '',
			platform TEXT DEFAULT '',
			arch TEXT DEFAULT '',
			cpu_model TEXT DEFAULT '',
			cpu_cores INTEGER DEFAULT 0,
			mem_total INTEGER DEFAULT 0,
			disk_total INTEGER DEFAULT 0,
			agent_version TEXT DEFAULT '',
			boot_time INTEGER DEFAULT 0,
			created_at INTEGER NOT NULL,
			last_seen INTEGER DEFAULT 0,
			deleted_at INTEGER DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS node_meta (
			node_id INTEGER PRIMARY KEY,
			note TEXT DEFAULT '',
			note_visible INTEGER DEFAULT 1,
			price REAL DEFAULT 0,
			currency TEXT DEFAULT 'USD',
			cycle TEXT DEFAULT 'monthly',
			start_date INTEGER DEFAULT 0,
			next_due INTEGER DEFAULT 0,
			auto_renew INTEGER DEFAULT 1,
			traffic_limit INTEGER DEFAULT 0,
			traffic_type TEXT DEFAULT 'sum',
			traffic_reset_day INTEGER DEFAULT 1,
			traffic_reset_tz TEXT DEFAULT 'Asia/Shanghai',
			traffic_used_in INTEGER DEFAULT 0,
			traffic_used_out INTEGER DEFAULT 0,
			traffic_period_start INTEGER DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS metrics (
			node_id INTEGER NOT NULL,
			ts INTEGER NOT NULL,
			data BLOB NOT NULL,
			-- 以下为常用字段的列拷贝,方便未来分层降采样直接走 SQL 聚合,
			-- 不需要反序列化 data blob。这些值由 SaveMetric 同步写入。
			cpu REAL DEFAULT 0,
			mem_used INTEGER DEFAULT 0,
			mem_total INTEGER DEFAULT 0,
			load1 REAL DEFAULT 0,
			latency_ms REAL DEFAULT 0,
			loss_pct REAL DEFAULT 0,
			PRIMARY KEY (node_id, ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_ts ON metrics(ts)`,
		// 对旧库做 ALTER:列不存在时加,存在时报错忽略
		`ALTER TABLE metrics ADD COLUMN cpu REAL DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN mem_used INTEGER DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN mem_total INTEGER DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN load1 REAL DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN latency_ms REAL DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN loss_pct REAL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN machine_id TEXT DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN primary_mac TEXT DEFAULT ''`,
		// 流量列存（供聚合使用）
		`ALTER TABLE metrics ADD COLUMN net_in_d INTEGER DEFAULT 0`,
		`ALTER TABLE metrics ADD COLUMN net_out_d INTEGER DEFAULT 0`,
		// kv 表：通用键值存储（自监控节点 ID、TOTP secret 等）
		`CREATE TABLE IF NOT EXISTS kv (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT ''
		)`,
		// 1 分钟聚合（保留 24 小时）
		`CREATE TABLE IF NOT EXISTS metrics_1m (
			node_id INTEGER NOT NULL,
			ts      INTEGER NOT NULL,
			cpu     REAL    DEFAULT 0,
			mem_used    INTEGER DEFAULT 0,
			mem_total   INTEGER DEFAULT 0,
			load1   REAL    DEFAULT 0,
			net_in  INTEGER DEFAULT 0,
			net_out INTEGER DEFAULT 0,
			PRIMARY KEY (node_id, ts)
		)`,
		// 5 分钟聚合（保留 30 天）
		`CREATE TABLE IF NOT EXISTS metrics_5m (
			node_id INTEGER NOT NULL,
			ts      INTEGER NOT NULL,
			cpu     REAL    DEFAULT 0,
			mem_used    INTEGER DEFAULT 0,
			mem_total   INTEGER DEFAULT 0,
			load1   REAL    DEFAULT 0,
			net_in  INTEGER DEFAULT 0,
			net_out INTEGER DEFAULT 0,
			PRIMARY KEY (node_id, ts)
		)`,
		// 操作审计日志（保留 90 天）
		`CREATE TABLE IF NOT EXISTS audit_log (
			id     INTEGER PRIMARY KEY AUTOINCREMENT,
			ts     INTEGER NOT NULL,
			actor  TEXT NOT NULL DEFAULT '',
			ip     TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			target TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts)`,
		`CREATE TABLE IF NOT EXISTS traffic_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL,
			period_start INTEGER NOT NULL,
			period_end INTEGER NOT NULL,
			used_in INTEGER NOT NULL,
			used_out INTEGER NOT NULL
		)`,
		// 通知渠道(目前只 Telegram,留 type 字段方便后扩)
		`CREATE TABLE IF NOT EXISTS notifiers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'telegram',
			config TEXT NOT NULL DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			deleted_at INTEGER DEFAULT 0
		)`,
		// 告警规则
		`CREATE TABLE IF NOT EXISTS alert_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			kind TEXT NOT NULL,         -- offline / online / cpu / mem / disk / load / traffic / due
			target TEXT NOT NULL DEFAULT 'all',    -- all / node:1,2,3 / tag:foo
			threshold REAL DEFAULT 0,   -- 用法因 kind 而异
			duration INTEGER DEFAULT 0, -- 持续秒数
			extra TEXT DEFAULT '{}',    -- 续费天数列表等附加 JSON
			notifier_ids TEXT DEFAULT '[]',  -- JSON array of notifier id
			cooldown INTEGER DEFAULT 1800,   -- 同节点同规则告警间隔(秒)
			silent_from TEXT DEFAULT '',     -- HH:MM 静默起点(本地时区,空=不静默)
			silent_to   TEXT DEFAULT '',     -- HH:MM 静默终点
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			deleted_at INTEGER DEFAULT 0
		)`,
		// 告警状态(per rule per node):用于去抖、冷却、关联恢复
		`CREATE TABLE IF NOT EXISTS alert_state (
			rule_id INTEGER NOT NULL,
			node_id INTEGER NOT NULL,
			scope TEXT NOT NULL DEFAULT '',  -- 比如流量阶段 50/80/95/100;续费天数;不需要时留空
			first_breach INTEGER DEFAULT 0,  -- 首次越界时间(用于 duration 判定)
			last_alerted INTEGER DEFAULT 0,  -- 上次推送时间(用于 cooldown)
			active INTEGER DEFAULT 0,        -- 1=正在告警 0=已恢复
			PRIMARY KEY (rule_id, node_id, scope)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			// ALTER TABLE 在列已存在时会失败,这是正常情况(老库已经迁移过)。
			// 检查错误信息中是否包含 "duplicate column",是就忽略。
			if isDuplicateColumnErr(err) {
				continue
			}
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite 返回 "duplicate column name: xxx"
	return strings.Contains(err.Error(), "duplicate column")
}

// === Nodes ===

type Node struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Token        string `json:"token,omitempty"`
	GroupName    string `json:"group"`
	Tags         []string `json:"tags"`
	OS           string `json:"os"`
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
	CPUModel     string `json:"cpu_model"`
	CPUCores     int    `json:"cpu_cores"`
	MemTotal     uint64 `json:"mem_total"`
	DiskTotal    uint64 `json:"disk_total"`
	AgentVersion string `json:"agent_version"`
	BootTime     int64  `json:"boot_time"`
	CreatedAt    int64  `json:"created_at"`
	LastSeen     int64  `json:"last_seen"`
}

func (s *Store) CreateNode(name string) (*Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := generateToken()
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO nodes(name, token, created_at) VALUES(?, ?, ?)`,
		name, token, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err = s.db.Exec(`INSERT INTO node_meta(node_id) VALUES(?)`, id); err != nil {
		return nil, err
	}
	if _, err = s.db.Exec(`UPDATE node_meta SET traffic_period_start=? WHERE node_id=?`, now, id); err != nil {
		return nil, err
	}
	return s.GetNode(id)
}

func (s *Store) GetNode(id int64) (*Node, error) {
	row := s.db.QueryRow(`SELECT id, name, token, group_name, tags, os, platform, arch,
		cpu_model, cpu_cores, mem_total, disk_total, agent_version, boot_time, created_at, last_seen
		FROM nodes WHERE id=? AND deleted_at=0`, id)
	return scanNode(row)
}

func (s *Store) GetNodeByToken(token string) (*Node, error) {
	row := s.db.QueryRow(`SELECT id, name, token, group_name, tags, os, platform, arch,
		cpu_model, cpu_cores, mem_total, disk_total, agent_version, boot_time, created_at, last_seen
		FROM nodes WHERE token=? AND deleted_at=0`, token)
	return scanNode(row)
}

func (s *Store) ListNodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT id, name, token, group_name, tags, os, platform, arch,
		cpu_model, cpu_cores, mem_total, disk_total, agent_version, boot_time, created_at, last_seen
		FROM nodes WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Store) UpdateNodeName(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE nodes SET name=? WHERE id=?`, name, id)
	return err
}

// UpdateNodeInfo 由 hello 触发更新静态信息
func (s *Store) UpdateNodeInfo(id int64, os_, platform, arch, cpuModel string,
	cpuCores int, memTotal, diskTotal uint64, agentVer string, bootTime int64) error {
	_, err := s.db.Exec(`UPDATE nodes SET os=?, platform=?, arch=?, cpu_model=?,
		cpu_cores=?, mem_total=?, disk_total=?, agent_version=?, boot_time=?
		WHERE id=?`, os_, platform, arch, cpuModel, cpuCores, memTotal, diskTotal,
		agentVer, bootTime, id)
	return err
}

// UpdateNodeFingerprint 写入/覆盖机器指纹。仅当传入值非空时才写,避免清空旧值。
func (s *Store) UpdateNodeFingerprint(id int64, machineID, primaryMAC string) error {
	if machineID == "" && primaryMAC == "" {
		return nil
	}
	if machineID != "" && primaryMAC != "" {
		_, err := s.db.Exec(`UPDATE nodes SET machine_id=?, primary_mac=? WHERE id=?`,
			machineID, primaryMAC, id)
		return err
	}
	if machineID != "" {
		_, err := s.db.Exec(`UPDATE nodes SET machine_id=? WHERE id=?`, machineID, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE nodes SET primary_mac=? WHERE id=?`, primaryMAC, id)
	return err
}

// FindNodeByFingerprint 尝试根据机器指纹找到现有节点(用于 AD 自动注册去重)。
// 优先 machine_id 精确匹配;次选 primary_mac 精确匹配。两者都为空返回 nil。
func (s *Store) FindNodeByFingerprint(machineID, primaryMAC string) (*Node, error) {
	if machineID != "" {
		row := s.db.QueryRow(`SELECT id, name, token, group_name, tags, os, platform, arch,
			cpu_model, cpu_cores, mem_total, disk_total, agent_version, boot_time, created_at, last_seen
			FROM nodes WHERE machine_id=? AND deleted_at=0 LIMIT 1`, machineID)
		if n, err := scanNode(row); err == nil {
			return n, nil
		}
	}
	if primaryMAC != "" {
		row := s.db.QueryRow(`SELECT id, name, token, group_name, tags, os, platform, arch,
			cpu_model, cpu_cores, mem_total, disk_total, agent_version, boot_time, created_at, last_seen
			FROM nodes WHERE primary_mac=? AND deleted_at=0 LIMIT 1`, primaryMAC)
		if n, err := scanNode(row); err == nil {
			return n, nil
		}
	}
	return nil, sql.ErrNoRows
}

// FindOrCreateNodeByFingerprint 在一次互斥操作内完成"按指纹查 + 找不到就建",
// 避免并发 AD 注册时同一指纹被建两次。返回值 created=true 表示这次新建了。
// suspect=true 时跳过 machine_id 匹配(容器/克隆 VM 场景),仅靠 primary_mac 去重。
func (s *Store) FindOrCreateNodeByFingerprint(machineID, primaryMAC, name string, suspect bool) (node *Node, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// suspect 模式下不用 machine_id 查重,避免把多台相同 ID 的机器混为一谈
	lookupMachineID := machineID
	if suspect {
		lookupMachineID = ""
	}
	if existing, ferr := s.FindNodeByFingerprint(lookupMachineID, primaryMAC); ferr == nil && existing != nil {
		return existing, false, nil
	}
	// 新建
	token := generateToken()
	now := time.Now().Unix()
	res, ierr := s.db.Exec(
		`INSERT INTO nodes(name, token, machine_id, primary_mac, created_at) VALUES(?, ?, ?, ?, ?)`,
		name, token, machineID, primaryMAC, now)
	if ierr != nil {
		return nil, false, ierr
	}
	id, _ := res.LastInsertId()
	if _, err := s.db.Exec(`INSERT INTO node_meta(node_id) VALUES(?)`, id); err != nil {
		return nil, false, err
	}
	if _, err := s.db.Exec(`UPDATE node_meta SET traffic_period_start=? WHERE node_id=?`, now, id); err != nil {
		return nil, false, err
	}
	n, gerr := s.GetNode(id)
	if gerr != nil {
		return nil, false, gerr
	}
	return n, true, nil
}

func (s *Store) UpdateLastSeen(id int64, ts int64) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen=? WHERE id=?`, ts, id)
	return err
}

func (s *Store) DeleteNode(id int64) error {
	_, err := s.db.Exec(`UPDATE nodes SET deleted_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var tagsJSON string
	err := row.Scan(&n.ID, &n.Name, &n.Token, &n.GroupName, &tagsJSON,
		&n.OS, &n.Platform, &n.Arch, &n.CPUModel, &n.CPUCores,
		&n.MemTotal, &n.DiskTotal, &n.AgentVersion, &n.BootTime,
		&n.CreatedAt, &n.LastSeen)
	if err != nil {
		return nil, err
	}
	if tagsJSON != "" {
		json.Unmarshal([]byte(tagsJSON), &n.Tags)
	}
	if n.Tags == nil {
		n.Tags = []string{}
	}
	return &n, nil
}

// === Meta ===

type NodeMeta struct {
	NodeID             int64   `json:"node_id"`
	Note               string  `json:"note"`
	NoteVisible        bool    `json:"note_visible"`
	Price              float64 `json:"price"`
	Currency           string  `json:"currency"`
	Cycle              string  `json:"cycle"`
	StartDate          int64   `json:"start_date"`
	NextDue            int64   `json:"next_due"`
	AutoRenew          bool    `json:"auto_renew"`
	TrafficLimit       uint64  `json:"traffic_limit"`
	TrafficType        string  `json:"traffic_type"`
	TrafficResetDay    int     `json:"traffic_reset_day"`
	TrafficResetTZ     string  `json:"traffic_reset_tz"`
	TrafficUsedIn      uint64  `json:"traffic_used_in"`
	TrafficUsedOut     uint64  `json:"traffic_used_out"`
	TrafficPeriodStart int64   `json:"traffic_period_start"`
}

func (s *Store) GetMeta(nodeID int64) (*NodeMeta, error) {
	row := s.db.QueryRow(`SELECT node_id, note, note_visible, price, currency, cycle,
		start_date, next_due, auto_renew, traffic_limit, traffic_type, traffic_reset_day,
		traffic_reset_tz, traffic_used_in, traffic_used_out, traffic_period_start
		FROM node_meta WHERE node_id=?`, nodeID)
	var m NodeMeta
	var nv, ar int
	err := row.Scan(&m.NodeID, &m.Note, &nv, &m.Price, &m.Currency, &m.Cycle,
		&m.StartDate, &m.NextDue, &ar, &m.TrafficLimit, &m.TrafficType,
		&m.TrafficResetDay, &m.TrafficResetTZ, &m.TrafficUsedIn,
		&m.TrafficUsedOut, &m.TrafficPeriodStart)
	if err != nil {
		return nil, err
	}
	m.NoteVisible = nv != 0
	m.AutoRenew = ar != 0
	return &m, nil
}

func (s *Store) UpdateMeta(m *NodeMeta) error {
	_, err := s.db.Exec(`UPDATE node_meta SET note=?, note_visible=?, price=?,
		currency=?, cycle=?, start_date=?, next_due=?, auto_renew=?,
		traffic_limit=?, traffic_type=?, traffic_reset_day=?, traffic_reset_tz=?,
		traffic_period_start=CASE WHEN traffic_period_start<=0 THEN ? ELSE traffic_period_start END
		WHERE node_id=?`,
		m.Note, boolToInt(m.NoteVisible), m.Price, m.Currency, m.Cycle,
		m.StartDate, m.NextDue, boolToInt(m.AutoRenew),
		m.TrafficLimit, m.TrafficType, m.TrafficResetDay, m.TrafficResetTZ,
		time.Now().Unix(), m.NodeID)
	return err
}

// AddTraffic 累加流量(由 metric 上报触发)
func (s *Store) AddTraffic(nodeID int64, deltaIn, deltaOut uint64) error {
	_, err := s.db.Exec(`UPDATE node_meta
		SET traffic_used_in = traffic_used_in + ?,
		    traffic_used_out = traffic_used_out + ?
		WHERE node_id=?`, deltaIn, deltaOut, nodeID)
	return err
}

// ResetTraffic 周期重置(归档历史 + 清零)
func (s *Store) ResetTraffic(nodeID int64, periodStart, periodEnd int64) error {
	m, err := s.GetMeta(nodeID)
	if err != nil {
		return err
	}
	if m.TrafficUsedIn > 0 || m.TrafficUsedOut > 0 {
		_, err := s.db.Exec(`INSERT INTO traffic_history(node_id, period_start, period_end, used_in, used_out)
			VALUES(?,?,?,?,?)`, nodeID, m.TrafficPeriodStart, periodEnd, m.TrafficUsedIn, m.TrafficUsedOut)
		if err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE node_meta SET traffic_used_in=0, traffic_used_out=0,
		traffic_period_start=? WHERE node_id=?`, periodStart, nodeID)
	return err
}

func (s *Store) SetTrafficPeriodStart(nodeID, periodStart int64) error {
	_, err := s.db.Exec(`UPDATE node_meta SET traffic_period_start=? WHERE node_id=?`, periodStart, nodeID)
	return err
}

// === Metrics ===

// SaveMetric 把一条指标写入 metrics 表。
// 既保留完整 JSON blob(供前端展示和未来扩展),也同步写常用聚合列(供 SQL 聚合使用)。
func (s *Store) SaveMetric(nodeID int64, m *MetricForStore) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO metrics
		(node_id, ts, data, cpu, mem_used, mem_total, load1, latency_ms, loss_pct, net_in_d, net_out_d)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, m.Timestamp, m.Raw,
		m.CPU, m.MemUsed, m.MemTotal, m.Load1, m.LatencyMS, m.LossPct,
		m.NetInDelta, m.NetOutDelta)
	return err
}

// MetricForStore 是 store 写入 metrics 表所需的最小字段集。
// 调用方(hub)用 shared.MetricPayload 填充。
type MetricForStore struct {
	Timestamp   int64
	Raw         []byte // 完整 JSON
	CPU         float64
	MemUsed     uint64
	MemTotal    uint64
	Load1       float64
	LatencyMS   float64
	LossPct     float64
	NetInDelta  uint64
	NetOutDelta uint64
}

// GetLatestMetric 返回某节点最新的指标 JSON
type ProbeHistoryPoint struct {
	Timestamp int64   `json:"t"`
	LatencyMS float64 `json:"latency_ms"`
	LossPct   float64 `json:"loss_pct"`
}

func (s *Store) GetLatestMetric(nodeID int64) ([]byte, int64, error) {
	row := s.db.QueryRow(`SELECT data, ts FROM metrics WHERE node_id=? ORDER BY ts DESC LIMIT 1`, nodeID)
	var data []byte
	var ts int64
	err := row.Scan(&data, &ts)
	return data, ts, err
}

// CleanupOldMetrics 删除指定时间之前的 metrics
func (s *Store) CleanupOldMetrics(before int64) error {
	_, err := s.db.Exec(`DELETE FROM metrics WHERE ts < ?`, before)
	return err
}

func (s *Store) QueryProbeHistory(nodeID, since int64, stepSec, limit int) ([]ProbeHistoryPoint, error) {
	if stepSec <= 0 {
		stepSec = 60
	}
	if limit <= 0 {
		limit = 360
	}
	rows, err := s.db.Query(`SELECT bucket_ts, avg_latency_ms, avg_loss_pct FROM (
			SELECT
				(ts / ?) * ? AS bucket_ts,
				AVG(CASE WHEN latency_ms > 0 THEN latency_ms END) AS avg_latency_ms,
				AVG(loss_pct) AS avg_loss_pct
			FROM metrics
			WHERE node_id=? AND ts>=?
			GROUP BY bucket_ts
			ORDER BY bucket_ts DESC
			LIMIT ?
		) ORDER BY bucket_ts ASC`, stepSec, stepSec, nodeID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProbeHistoryPoint
	for rows.Next() {
		var p ProbeHistoryPoint
		var latency sql.NullFloat64
		var loss sql.NullFloat64
		if err := rows.Scan(&p.Timestamp, &latency, &loss); err != nil {
			return nil, err
		}
		if latency.Valid {
			p.LatencyMS = latency.Float64
		}
		if loss.Valid {
			p.LossPct = loss.Float64
		}
		out = append(out, p)
	}
	return out, nil
}

// === Helpers ===

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func secureRand(max int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var (
	ErrNotFound       = errors.New("not found")
	ErrNodeOffline    = errors.New("node offline")
	ErrSendBufferFull = errors.New("send buffer full")
)

// === Notifiers ===

type Notifier struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`     // "telegram"
	Config    string `json:"config"`   // JSON 字符串,如 {"bot_token":"...","chat_id":"..."}
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) CreateNotifier(n *Notifier) (int64, error) {
	if n.Type == "" {
		n.Type = "telegram"
	}
	if n.Config == "" {
		n.Config = "{}"
	}
	res, err := s.db.Exec(`INSERT INTO notifiers(name, type, config, enabled, created_at)
		VALUES(?,?,?,?,?)`, n.Name, n.Type, n.Config, boolToInt(n.Enabled), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *Store) UpdateNotifier(n *Notifier) error {
	_, err := s.db.Exec(`UPDATE notifiers SET name=?, type=?, config=?, enabled=?
		WHERE id=? AND deleted_at=0`,
		n.Name, n.Type, n.Config, boolToInt(n.Enabled), n.ID)
	return err
}

func (s *Store) DeleteNotifier(id int64) error {
	_, err := s.db.Exec(`UPDATE notifiers SET deleted_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) ListNotifiers() ([]*Notifier, error) {
	rows, err := s.db.Query(`SELECT id, name, type, config, enabled, created_at
		FROM notifiers WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Notifier
	for rows.Next() {
		n := &Notifier{}
		var en int
		if err := rows.Scan(&n.ID, &n.Name, &n.Type, &n.Config, &en, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Enabled = en != 0
		out = append(out, n)
	}
	return out, nil
}

func (s *Store) GetNotifier(id int64) (*Notifier, error) {
	row := s.db.QueryRow(`SELECT id, name, type, config, enabled, created_at
		FROM notifiers WHERE id=? AND deleted_at=0`, id)
	n := &Notifier{}
	var en int
	if err := row.Scan(&n.ID, &n.Name, &n.Type, &n.Config, &en, &n.CreatedAt); err != nil {
		return nil, err
	}
	n.Enabled = en != 0
	return n, nil
}

// === Alert Rules ===

type AlertRule struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`         // offline/online/cpu/mem/disk/load/traffic/due
	Target      string  `json:"target"`       // all / node:1,2 / tag:xxx
	Threshold   float64 `json:"threshold"`
	Duration    int     `json:"duration"`     // 秒
	Extra       string  `json:"extra"`        // JSON,如 {"days":[7,3,1]}
	NotifierIDs string  `json:"notifier_ids"` // JSON,如 [1,2]
	Cooldown    int     `json:"cooldown"`     // 秒
	SilentFrom  string  `json:"silent_from"`  // HH:MM
	SilentTo    string  `json:"silent_to"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   int64   `json:"created_at"`
}

func (s *Store) CreateAlertRule(r *AlertRule) (int64, error) {
	if r.NotifierIDs == "" {
		r.NotifierIDs = "[]"
	}
	if r.Extra == "" {
		r.Extra = "{}"
	}
	if r.Target == "" {
		r.Target = "all"
	}
	if r.Cooldown <= 0 {
		r.Cooldown = 1800
	}
	res, err := s.db.Exec(`INSERT INTO alert_rules(name, kind, target, threshold, duration,
		extra, notifier_ids, cooldown, silent_from, silent_to, enabled, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Name, r.Kind, r.Target, r.Threshold, r.Duration,
		r.Extra, r.NotifierIDs, r.Cooldown, r.SilentFrom, r.SilentTo,
		boolToInt(r.Enabled), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *Store) UpdateAlertRule(r *AlertRule) error {
	_, err := s.db.Exec(`UPDATE alert_rules SET name=?, kind=?, target=?, threshold=?,
		duration=?, extra=?, notifier_ids=?, cooldown=?, silent_from=?, silent_to=?, enabled=?
		WHERE id=? AND deleted_at=0`,
		r.Name, r.Kind, r.Target, r.Threshold, r.Duration,
		r.Extra, r.NotifierIDs, r.Cooldown, r.SilentFrom, r.SilentTo,
		boolToInt(r.Enabled), r.ID)
	return err
}

func (s *Store) DeleteAlertRule(id int64) error {
	_, err := s.db.Exec(`UPDATE alert_rules SET deleted_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) ListAlertRules() ([]*AlertRule, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, target, threshold, duration,
		extra, notifier_ids, cooldown, silent_from, silent_to, enabled, created_at
		FROM alert_rules WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AlertRule
	for rows.Next() {
		r := &AlertRule{}
		var en int
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Target, &r.Threshold, &r.Duration,
			&r.Extra, &r.NotifierIDs, &r.Cooldown, &r.SilentFrom, &r.SilentTo,
			&en, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Enabled = en != 0
		out = append(out, r)
	}
	return out, nil
}

// === Alert State (内部状态机) ===

type AlertState struct {
	RuleID      int64
	NodeID      int64
	Scope       string // 比如续费 "7"/"3"/"1",流量 "50"/"80"
	FirstBreach int64
	LastAlerted int64
	Active      bool
}

func (s *Store) GetAlertState(ruleID, nodeID int64, scope string) (*AlertState, error) {
	row := s.db.QueryRow(`SELECT rule_id, node_id, scope, first_breach, last_alerted, active
		FROM alert_state WHERE rule_id=? AND node_id=? AND scope=?`, ruleID, nodeID, scope)
	st := &AlertState{}
	var act int
	if err := row.Scan(&st.RuleID, &st.NodeID, &st.Scope, &st.FirstBreach, &st.LastAlerted, &act); err != nil {
		return nil, err
	}
	st.Active = act != 0
	return st, nil
}

func (s *Store) UpsertAlertState(st *AlertState) error {
	_, err := s.db.Exec(`INSERT INTO alert_state(rule_id, node_id, scope, first_breach, last_alerted, active)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(rule_id, node_id, scope) DO UPDATE SET
			first_breach=excluded.first_breach,
			last_alerted=excluded.last_alerted,
			active=excluded.active`,
		st.RuleID, st.NodeID, st.Scope, st.FirstBreach, st.LastAlerted, boolToInt(st.Active))
	return err
}

func (s *Store) ClearAlertState(ruleID, nodeID int64, scope string) error {
	_, err := s.db.Exec(`DELETE FROM alert_state WHERE rule_id=? AND node_id=? AND scope=?`,
		ruleID, nodeID, scope)
	return err
}

// === KV Store ===

func (s *Store) KVGet(k string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) KVSet(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) KVDelete(k string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE k=?`, k)
	return err
}

// === 分层降采样 ===

// HistoryPoint 是历史曲线的一个数据点
type HistoryPoint struct {
	Timestamp int64   `json:"t"`
	CPU       float64 `json:"cpu"`
	MemUsed   uint64  `json:"mem_u"`
	MemTotal  uint64  `json:"mem_t"`
	Load1     float64 `json:"load1"`
	NetIn     uint64  `json:"net_in"`
	NetOut    uint64  `json:"net_out"`
}

// AggregateMetrics 把 metrics 原始数据聚合到 metrics_1m/metrics_5m。
// 幂等：INSERT OR REPLACE，每分钟调用一次即可。
// windowSec 表示覆盖最近多少秒（通常传 600，覆盖最近 10 分钟）。
func (s *Store) AggregateMetrics(windowSec int64) error {
	now := time.Now().Unix()
	from := now - windowSec
	// 1 分钟桶
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO metrics_1m(node_id, ts, cpu, mem_used, mem_total, load1, net_in, net_out)
		SELECT node_id,
		       (ts / 60) * 60 AS bucket,
		       AVG(cpu),
		       CAST(AVG(mem_used) AS INTEGER),
		       CAST(AVG(mem_total) AS INTEGER),
		       AVG(load1),
		       SUM(net_in_d),
		       SUM(net_out_d)
		FROM metrics
		WHERE ts >= ? AND ts < (SELECT ((MAX(ts)/60)*60) FROM metrics WHERE ts >= ?)
		GROUP BY node_id, bucket`, from, from)
	if err != nil {
		return fmt.Errorf("aggregate 1m: %w", err)
	}
	// 5 分钟桶
	_, err = s.db.Exec(`
		INSERT OR REPLACE INTO metrics_5m(node_id, ts, cpu, mem_used, mem_total, load1, net_in, net_out)
		SELECT node_id,
		       (ts / 300) * 300 AS bucket,
		       AVG(cpu),
		       CAST(AVG(mem_used) AS INTEGER),
		       CAST(AVG(mem_total) AS INTEGER),
		       AVG(load1),
		       SUM(net_in_d),
		       SUM(net_out_d)
		FROM metrics
		WHERE ts >= ? AND ts < (SELECT ((MAX(ts)/300)*300) FROM metrics WHERE ts >= ?)
		GROUP BY node_id, bucket`, from, from)
	if err != nil {
		return fmt.Errorf("aggregate 5m: %w", err)
	}
	return nil
}

// CleanupAggregated 删除过期聚合数据
func (s *Store) CleanupAggregated(now int64) error {
	_, err := s.db.Exec(`DELETE FROM metrics_1m WHERE ts < ?`, now-24*3600)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM metrics_5m WHERE ts < ?`, now-30*24*3600)
	return err
}

// GetHistory 按时间范围返回历史曲线点
func (s *Store) GetHistory(nodeID int64, rangeStr string) ([]HistoryPoint, error) {
	now := time.Now().Unix()
	var from int64
	var table, orderBy string
	switch rangeStr {
	case "1h":
		from = now - 3600
		table = "metrics"
		orderBy = "ts"
	case "24h":
		from = now - 24*3600
		table = "metrics_1m"
		orderBy = "ts"
	case "7d":
		from = now - 7*24*3600
		table = "metrics_5m"
		orderBy = "ts"
	default: // 30d
		from = now - 30*24*3600
		table = "metrics_5m"
		orderBy = "ts"
	}

	var rows *sql.Rows
	var err error
	if table == "metrics" {
		rows, err = s.db.Query(
			`SELECT ts, cpu, mem_used, mem_total, load1, net_in_d, net_out_d
			 FROM metrics WHERE node_id=? AND ts>=? ORDER BY `+orderBy,
			nodeID, from)
	} else {
		rows, err = s.db.Query(
			`SELECT ts, cpu, mem_used, mem_total, load1, net_in, net_out
			 FROM `+table+` WHERE node_id=? AND ts>=? ORDER BY `+orderBy,
			nodeID, from)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryPoint
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.Timestamp, &p.CPU, &p.MemUsed, &p.MemTotal, &p.Load1, &p.NetIn, &p.NetOut); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// === UpdateNodeTags ===

func (s *Store) UpdateNodeTags(id int64, tags []string) error {
	b, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE nodes SET tags=? WHERE id=?`, string(b), id)
	return err
}

// === 审计日志 ===

type AuditEntry struct {
	ID     int64  `json:"id"`
	Ts     int64  `json:"ts"`
	Actor  string `json:"actor"`
	IP     string `json:"ip"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

func (s *Store) LogAudit(actor, ip, action, target, detail string) {
	if _, err := s.db.Exec(`INSERT INTO audit_log(ts, actor, ip, action, target, detail) VALUES(?,?,?,?,?,?)`,
		time.Now().Unix(), actor, ip, action, target, detail); err != nil {
		log.Printf("audit log: %v", err)
	}
}

func (s *Store) ListAudit(limit, offset int) ([]*AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, ts, actor, ip, action, target, detail FROM audit_log ORDER BY ts DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.ID, &e.Ts, &e.Actor, &e.IP, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Store) CleanupAudit(before int64) error {
	_, err := s.db.Exec(`DELETE FROM audit_log WHERE ts < ?`, before)
	return err
}

// CleanupAlertStateTraffic 删除流量/续费类的旧 scope 记录,避免无限积累。
// scope 以 'p' 开头的是流量周期标记,以 'due' 开头的是续费提醒标记。
// maxAge 为秒数,last_alerted 超过此时间的记录将被删除。
func (s *Store) CleanupAlertStateTraffic(maxAge int64) error {
	cutoff := time.Now().Unix() - maxAge
	_, err := s.db.Exec(`DELETE FROM alert_state
		WHERE last_alerted < ? AND (scope LIKE 'p%' OR scope LIKE 'due%')`,
		cutoff)
	return err
}

// === Backup ===

// BackupData 是导出/导入的完整配置快照
type BackupData struct {
	Version      string       `json:"version"`
	ExportedAt   int64        `json:"exported_at"`
	Nodes        []*Node      `json:"nodes"`
	Notifiers    []*Notifier  `json:"notifiers"`
	AlertRules   []*AlertRule `json:"alert_rules"`
	PanelTitle   string       `json:"panel_title"`
	MainCurrency string       `json:"main_currency"`
}

// ImportBackup 从备份数据恢复节点/通知渠道/告警规则。
// 节点按 token 做 upsert:token 存在则更新,否则新建。
// 通知渠道和告警规则先软删除全部,再重建。
func (s *Store) ImportBackup(data *BackupData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 节点: 按 token upsert
	for _, n := range data.Nodes {
		if n.Token == "" {
			continue
		}
		tagsJSON, _ := json.Marshal(n.Tags)
		if n.Tags == nil {
			tagsJSON = []byte("[]")
		}
		res, err := tx.Exec(
			`UPDATE nodes SET name=?, group_name=?, tags=?, deleted_at=0 WHERE token=?`,
			n.Name, n.GroupName, string(tagsJSON), n.Token)
		if err != nil {
			return err
		}
		rows, _ := res.RowsAffected()
		if rows == 0 {
			ca := n.CreatedAt
			if ca == 0 {
				ca = time.Now().Unix()
			}
			_, err = tx.Exec(
				`INSERT OR IGNORE INTO nodes(name, token, group_name, tags, created_at) VALUES(?,?,?,?,?)`,
				n.Name, n.Token, n.GroupName, string(tagsJSON), ca)
			if err != nil {
				return err
			}
		}
	}

	now := time.Now().Unix()

	// 通知渠道: 软删全部,重建
	if _, err := tx.Exec(`UPDATE notifiers SET deleted_at=? WHERE deleted_at=0`, now); err != nil {
		return err
	}
	for _, n := range data.Notifiers {
		if n.Config == "" {
			n.Config = "{}"
		}
		if n.Type == "" {
			n.Type = "telegram"
		}
		_, err := tx.Exec(
			`INSERT INTO notifiers(name, type, config, enabled, created_at) VALUES(?,?,?,?,?)`,
			n.Name, n.Type, n.Config, boolToInt(n.Enabled), now)
		if err != nil {
			return err
		}
	}

	// 告警规则: 软删全部,重建
	if _, err := tx.Exec(`UPDATE alert_rules SET deleted_at=? WHERE deleted_at=0`, now); err != nil {
		return err
	}
	for _, r := range data.AlertRules {
		if r.NotifierIDs == "" {
			r.NotifierIDs = "[]"
		}
		if r.Extra == "" {
			r.Extra = "{}"
		}
		if r.Target == "" {
			r.Target = "all"
		}
		if r.Cooldown <= 0 {
			r.Cooldown = 1800
		}
		_, err := tx.Exec(
			`INSERT INTO alert_rules(name, kind, target, threshold, duration, extra, notifier_ids, cooldown, silent_from, silent_to, enabled, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.Name, r.Kind, r.Target, r.Threshold, r.Duration,
			r.Extra, r.NotifierIDs, r.Cooldown, r.SilentFrom, r.SilentTo,
			boolToInt(r.Enabled), now)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
