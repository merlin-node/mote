package server

import (
	"log"
	"time"
)

// ComputeNextDue 给定起租时间和周期,计算下次到期时间
func ComputeNextDue(startUnix int64, cycle string, nowUnix int64) int64 {
	if cycle == "lifetime" || cycle == "" {
		return 0
	}
	start := time.Unix(startUnix, 0)
	now := time.Unix(nowUnix, 0)
	next := start
	step := func(t time.Time) time.Time {
		switch cycle {
		case "monthly":
			return addMonths(t, 1)
		case "quarterly":
			return addMonths(t, 3)
		case "semiannually":
			return addMonths(t, 6)
		case "yearly":
			return addMonths(t, 12)
		case "biennially":
			return addMonths(t, 24)
		case "once":
			return t.AddDate(100, 0, 0) // 一次性,远未来
		}
		return addMonths(t, 1)
	}
	// 滚动到未来第一个截止时间
	for !next.After(now) {
		next = step(next)
	}
	return next.Unix()
}

// addMonths 安全地按月加,处理月底边界
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	hh, mm, ss := t.Clock()
	loc := t.Location()
	totalMonths := int(m) + n - 1
	newYear := y + totalMonths/12
	newMonth := time.Month(totalMonths%12 + 1)
	// 计算新月份的最后一天
	lastDay := time.Date(newYear, newMonth+1, 0, 0, 0, 0, 0, loc).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(newYear, newMonth, d, hh, mm, ss, 0, loc)
}

// Scheduler 后台定时任务
type Scheduler struct {
	store  *Store
	alerts *AlertEngine
	stop   chan struct{}
}

func NewScheduler(store *Store, hub *Hub) *Scheduler {
	return &Scheduler{
		store:  store,
		alerts: NewAlertEngine(store, hub),
		stop:   make(chan struct{}),
	}
}

func (s *Scheduler) Start() {
	go s.loop()
}

func (s *Scheduler) Stop() {
	close(s.stop)
}

func (s *Scheduler) loop() {
	// 启动后立即跑一次
	s.tick()
	s.alertTick()
	s.store.AggregateMetrics(600)
	slow := time.NewTicker(10 * time.Minute)
	fast := time.NewTicker(1 * time.Minute) // 告警评估 + 聚合
	defer slow.Stop()
	defer fast.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-slow.C:
			s.tick()
		case <-fast.C:
			s.alertTick()
			s.store.AggregateMetrics(600)
		}
	}
}

func (s *Scheduler) alertTick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("alert tick panic: %v", r)
		}
	}()
	s.alerts.Tick()
}

func (s *Scheduler) tick() {
	now := time.Now().Unix()

	// 1) 流量周期重置
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	for _, n := range nodes {
		meta, err := s.store.GetMeta(n.ID)
		if err != nil || meta.TrafficResetDay <= 0 {
			continue
		}
		loc, err := time.LoadLocation(meta.TrafficResetTZ)
		if err != nil {
			loc = time.UTC
		}
		nowInLoc := time.Unix(now, 0).In(loc)
		if meta.TrafficPeriodStart <= 0 {
			periodStart := currentPeriodStart(nowInLoc, meta.TrafficResetDay, loc)
			if err := s.store.SetTrafficPeriodStart(n.ID, periodStart.Unix()); err != nil {
				log.Printf("init traffic period for node #%d failed: %v", n.ID, err)
			}
			meta.TrafficPeriodStart = periodStart.Unix()
		}
		// 当前周期应当在哪一天结束?
		periodEnd := nextResetTime(time.Unix(meta.TrafficPeriodStart, 0).In(loc),
			meta.TrafficResetDay, loc)
		if now >= periodEnd.Unix() {
			// 触发重置
			log.Printf("traffic reset for node #%d", n.ID)
			s.store.ResetTraffic(n.ID, periodEnd.Unix(), periodEnd.Unix())
		}

		// 2) 自动续费(到期且 auto_renew 时,把 next_due 滚到下一周期)
		if meta.AutoRenew && meta.NextDue > 0 && meta.NextDue < now &&
			meta.Cycle != "lifetime" && meta.Cycle != "once" {
			meta.NextDue = ComputeNextDue(meta.StartDate, meta.Cycle, now)
			s.store.UpdateMeta(meta)
		}
	}

	// 3) 清理过期 metrics(保留 30 天)
	cutoff := now - 30*24*3600
	s.store.CleanupOldMetrics(cutoff)

	// 4) 清理过期 alert_state 流量/续费 scope(保留 90 天)
	s.store.CleanupAlertStateTraffic(90 * 24 * 3600)

	// 5) 清理过期聚合数据和审计日志
	s.store.CleanupAggregated(now)
	s.store.CleanupAudit(now - 90*24*3600)
}

// nextResetTime 给定当前周期起点,返回下一个重置时刻(指定日的 00:00)
func nextResetTime(periodStart time.Time, resetDay int, loc *time.Location) time.Time {
	// 找到 periodStart 之后第一个 reset_day 当天 00:00
	t := time.Date(periodStart.Year(), periodStart.Month(), 1, 0, 0, 0, 0, loc)
	// 在 periodStart 的下个月或同月找
	for {
		lastDay := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, loc).Day()
		day := resetDay
		if day > lastDay {
			day = lastDay
		}
		candidate := time.Date(t.Year(), t.Month(), day, 0, 0, 0, 0, loc)
		if candidate.After(periodStart) {
			return candidate
		}
		t = t.AddDate(0, 1, 0)
	}
}

func currentPeriodStart(now time.Time, resetDay int, loc *time.Location) time.Time {
	lastDay := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	day := resetDay
	if day > lastDay {
		day = lastDay
	}
	candidate := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, loc)
	if candidate.After(now) {
		prevMonth := now.AddDate(0, -1, 0)
		lastDay = time.Date(prevMonth.Year(), prevMonth.Month()+1, 0, 0, 0, 0, 0, loc).Day()
		day = resetDay
		if day > lastDay {
			day = lastDay
		}
		candidate = time.Date(prevMonth.Year(), prevMonth.Month(), day, 0, 0, 0, 0, loc)
	}
	return candidate
}
