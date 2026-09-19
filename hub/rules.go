package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ruleKey 标识规则引擎内的一条计时器（来源 + 规则 + 标签选择器）。
type ruleKey struct {
	sourceID string
	ruleID   string
	labelSel string
}

// ruleState 为 threshold 规则的计时状态（内存态；重启后重新计时）。
type ruleState struct {
	breach       bool
	breachSince  time.Time
	recovering   bool
	recoverSince time.Time
}

// ruleEngine 为中枢侧规则引擎（events.md §4）。
type ruleEngine struct {
	app    *app
	mu     sync.Mutex
	states map[ruleKey]*ruleState
}

func newRuleEngine(a *app) *ruleEngine {
	return &ruleEngine{app: a, states: map[ruleKey]*ruleState{}}
}

// evalSample 对每个最新样本执行全部启用规则（须在写事务内调用）。
// arrival 为到达时间；判定用 max(sample.ts, arrival) 防补传旧样本立刻触发。
func (re *ruleEngine) evalSample(tx *sql.Tx, src *sourceRow, doc *rulesDoc,
	sample *samplePayload, arrival time.Time, changes *[]*changeEntry) error {
	re.mu.Lock()
	defer re.mu.Unlock()

	sampleTS, _ := parseTS(sample.TS)
	evalNow := arrival
	if sampleTS.After(evalNow) {
		evalNow = sampleTS
	}

	// 前一个已应用样本（容器 / SMART / 池迁移判定）。
	var prev *samplePayload
	if src.PrevSample.Valid && src.PrevSample.String != "" {
		var p samplePayload
		if json.Unmarshal([]byte(src.PrevSample.String), &p) == nil {
			prev = &p
		}
	}

	for i := range doc.Rules {
		r := &doc.Rules[i]
		if !r.Enabled {
			continue
		}
		var err error
		switch r.Kind {
		case "threshold":
			err = re.evalThreshold(tx, src, r, sample, evalNow, changes)
		case "container_exit":
			err = re.evalContainers(tx, src, r, prev, sample, evalNow, changes)
		case "smart":
			err = re.evalSmart(tx, src, r, sample, evalNow, changes)
		case "pool":
			err = re.evalPool(tx, src, r, sample, evalNow, changes)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- threshold ----

// labelValue 为一次取数结果：标签选择器 + 数值。
type labelValue struct {
	sel string
	v   float64
}

// extractMetric 按规则从样本取数；返回存在的 (labelSel, value) 列表。
func extractMetric(r *rule, s *samplePayload) []labelValue {
	out := []labelValue{}
	switch r.Metric {
	case "cpu_percent":
		if s.CPUPercent != nil {
			out = append(out, labelValue{"-", *s.CPUPercent})
		}
	case "mem_percent":
		if s.MemPercent != nil {
			out = append(out, labelValue{"-", *s.MemPercent})
		}
	case "disk_percent":
		for _, d := range s.Disks {
			if d.Percent == nil {
				continue
			}
			sel := "mount=" + d.Mount
			if r.Label != nil && *r.Label != "*" && *r.Label != sel {
				continue
			}
			out = append(out, labelValue{sel, *d.Percent})
		}
	case "net_rx_bps":
		if s.Net != nil && s.Net.RxBps != nil {
			out = append(out, labelValue{"-", *s.Net.RxBps})
		}
	case "net_tx_bps":
		if s.Net != nil && s.Net.TxBps != nil {
			out = append(out, labelValue{"-", *s.Net.TxBps})
		}
	}
	return out
}

// violates 判定超限：op ∈ gt|gte|lt|lte（缺省 gt）。
func violates(op string, v, limit float64) bool {
	switch op {
	case "gte":
		return v >= limit
	case "lt":
		return v < limit
	case "lte":
		return v <= limit
	default: // gt
		return v > limit
	}
}

// recovers 判定跨过回差（与超限方向相反）。
func recovers(op string, v, limit float64) bool {
	switch op {
	case "lt", "lte":
		return v > limit
	default: // gt, gte
		return v < limit
	}
}

func (re *ruleEngine) evalThreshold(tx *sql.Tx, src *sourceRow, r *rule,
	s *samplePayload, evalNow time.Time, changes *[]*changeEntry) error {
	if r.Value == nil {
		return nil
	}
	limit := *r.Value
	recoverLimit := limit
	if r.RecoverValue != nil {
		recoverLimit = *r.RecoverValue
	}

	for _, lv := range extractMetric(r, s) {
		key := ruleKey{src.ID, r.ID, lv.sel}
		st := re.states[key]
		if st == nil {
			st = &ruleState{}
			re.states[key] = st
		}
		fk := fmt.Sprintf("rule:%s:%s", r.ID, lv.sel)

		if violates(r.Op, lv.v, limit) {
			if !st.breach {
				st.breach = true
				st.breachSince = evalNow
			}
			st.recovering = false
			if evalNow.Sub(st.breachSince) >= time.Duration(r.ForSeconds)*time.Second {
				// 窗口内所有样本均超限 → open（已开放则合并由状态机保证幂等？这里仅在未开放时发事件）。
				open, err := isFaultOpen(tx, src.ID, fk)
				if err != nil {
					return err
				}
				if !open {
					act := "open"
					title := fmt.Sprintf("%s：%s 持续超限", src.Name, thresholdTitle(r, lv.sel))
					body := fmt.Sprintf("%s=%.2f 超过 %.0f 已 %d 秒", lv.sel, lv.v, limit, r.ForSeconds)
					if err := re.emit(tx, src, "threshold", r.Severity, &fk, &act, title, &body, evalNow, changes); err != nil {
						return err
					}
				}
			}
		} else {
			st.breach = false
			// 回差恢复判定。
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if open {
				if recovers(r.Op, lv.v, recoverLimit) {
					if !st.recovering {
						st.recovering = true
						st.recoverSince = evalNow
					}
					if evalNow.Sub(st.recoverSince) >= time.Duration(r.RecoverForSeconds)*time.Second {
						act := "resolve"
						title := fmt.Sprintf("%s：%s 恢复正常", src.Name, thresholdTitle(r, lv.sel))
						body := fmt.Sprintf("%s=%.2f 回落至 %.0f 以下持续 %d 秒", lv.sel, lv.v, recoverLimit, r.RecoverForSeconds)
						if err := re.emit(tx, src, "threshold_recovered", "info", &fk, &act, title, &body, evalNow, changes); err != nil {
							return err
						}
						st.recovering = false
						*st = ruleState{}
					}
				} else {
					st.recovering = false
				}
			}
		}
	}
	return nil
}

func thresholdTitle(r *rule, sel string) string {
	name := r.Metric
	if sel != "-" && sel != "" {
		return name + "(" + sel + ")"
	}
	return name
}

// isFaultOpen 查询是否存在开放轮次。
func isFaultOpen(tx *sql.Tx, sourceID, faultKey string) (bool, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(1) FROM faults WHERE source_id=? AND fault_key=? AND state='open'`,
		sourceID, faultKey).Scan(&n)
	return n > 0, err
}

// emit 为规则引擎发事件的便捷封装。
func (re *ruleEngine) emit(tx *sql.Tx, src *sourceRow, kind, severity string,
	fk *string, act *string, title string, body *string, at time.Time, changes *[]*changeEntry) error {
	return re.app.emitHubEventTx(tx, src, kind, severity, fk, act, title, body, at, changes)
}

// ---- container_exit ----

func (re *ruleEngine) evalContainers(tx *sql.Tx, src *sourceRow, r *rule,
	prev, cur *samplePayload, evalNow time.Time, changes *[]*changeEntry) error {
	if cur.Containers == nil {
		return nil // 本批未采集容器（capabilities 指示不支持 / 缺席）
	}
	match := func(name string) bool {
		return r.Match == nil || *r.Match == "*" || *r.Match == name
	}
	prevByName := map[string]*containerPayload{}
	if prev != nil {
		for i := range prev.Containers {
			c := &prev.Containers[i]
			prevByName[c.Name] = c
		}
	}
	curByName := map[string]bool{}
	for i := range cur.Containers {
		c := &cur.Containers[i]
		curByName[c.Name] = true
		if !match(c.Name) {
			continue
		}
		p := prevByName[c.Name]
		fk := "container:" + c.Name
		if p == nil {
			continue // 新出现的容器：契约仅定义 running→非 running 迁移
		}
		switch {
		case p.State == "running" && c.State != "running":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if !open {
				act := "open"
				title := fmt.Sprintf("容器 %s 退出", c.Name)
				body := fmt.Sprintf("state=%s", c.State)
				if c.ExitCode != nil {
					body = fmt.Sprintf("state=%s exitCode=%d", c.State, *c.ExitCode)
				}
				sev := r.Severity
				if sev == "" {
					sev = "warning"
				}
				if err := re.emit(tx, src, "container_exit", sev, &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		case p.State != "running" && c.State == "running":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if open {
				act := "resolve"
				title := fmt.Sprintf("容器 %s 恢复运行", c.Name)
				body := "state=running"
				if err := re.emit(tx, src, "container_back", "info", &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		case p.State == "running" && c.State == "running":
			// restartCount 增长但恒 running → 仅已有开放故障时记一条 update。
			grew := c.RestartCount != nil && p.RestartCount != nil && *c.RestartCount > *p.RestartCount
			if grew {
				open, err := isFaultOpen(tx, src.ID, fk)
				if err != nil {
					return err
				}
				if open {
					act := "update"
					title := fmt.Sprintf("容器 %s 重启计数增加", c.Name)
					body := fmt.Sprintf("restartCount=%d", *c.RestartCount)
					if err := re.emit(tx, src, "container_exit", "warning", &fk, &act, title, &body, evalNow, changes); err != nil {
						return err
					}
				}
			}
		}
	}
	// 容器从列表消失：已有开放故障 → resolve。
	if prev != nil {
		for i := range prev.Containers {
			p := &prev.Containers[i]
			if curByName[p.Name] || !match(p.Name) {
				continue
			}
			fk := "container:" + p.Name
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if open {
				act := "resolve"
				title := fmt.Sprintf("容器 %s 已移除", p.Name)
				body := "容器已移除"
				if err := re.emit(tx, src, "container_back", "info", &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---- smart ----

func (re *ruleEngine) evalSmart(tx *sql.Tx, src *sourceRow, r *rule,
	cur *samplePayload, evalNow time.Time, changes *[]*changeEntry) error {
	if cur.NAS == nil {
		return nil
	}
	sev := r.Severity
	if sev == "" {
		sev = "critical"
	}
	for _, d := range cur.NAS.Disks {
		fk := "smart:" + d.Dev
		switch d.Smart {
		case "failing":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if !open {
				act := "open"
				title := fmt.Sprintf("磁盘 %s SMART 异常", d.Dev)
				body := "smart=failing"
				if err := re.emit(tx, src, "smart_failing", sev, &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		case "ok":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if open {
				act := "resolve"
				title := fmt.Sprintf("磁盘 %s SMART 恢复", d.Dev)
				body := "smart=ok"
				if err := re.emit(tx, src, "smart_back", "info", &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		default:
			// asleep / unsupported / failed / 未知：不迁移状态。
		}
	}
	return nil
}

// ---- pool ----

func (re *ruleEngine) evalPool(tx *sql.Tx, src *sourceRow, r *rule,
	cur *samplePayload, evalNow time.Time, changes *[]*changeEntry) error {
	if cur.NAS == nil {
		return nil
	}
	sev := r.Severity
	if sev == "" {
		sev = "critical"
	}
	for _, p := range cur.NAS.Pools {
		fk := "pool:" + p.Name
		switch p.State {
		case "degraded", "error":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if !open {
				act := "open"
				title := fmt.Sprintf("存储池 %s 异常", p.Name)
				body := "state=" + p.State
				if err := re.emit(tx, src, "pool_error", sev, &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		case "ok":
			open, err := isFaultOpen(tx, src.ID, fk)
			if err != nil {
				return err
			}
			if open {
				act := "resolve"
				title := fmt.Sprintf("存储池 %s 恢复", p.Name)
				body := "state=ok"
				if err := re.emit(tx, src, "pool_back", "info", &fk, &act, title, &body, evalNow, changes); err != nil {
					return err
				}
			}
		default:
			// unknown / 缺席：不迁移。
		}
	}
	return nil
}

// ---- heartbeat 监测 ----

// pauseSource 清空来源的规则计时状态（offline 后 threshold 计时暂停，
// 恢复后以最近样本重新计时）。
func (re *ruleEngine) pauseSource(sourceID string) {
	re.mu.Lock()
	defer re.mu.Unlock()
	for k := range re.states {
		if k.sourceID == sourceID {
			delete(re.states, k)
		}
	}
}

// checkHeartbeats 对 kind=device 且有 lastSeenAt 的启用来源判定失联；
// feedback 来源无周期上报预期，不做心跳判定（events.md §4）。
// 失联 → heartbeat_lost 开放故障（同一失联期只开一次）。
func (a *app) checkHeartbeats(now time.Time) {
	hbSec := a.st.heartbeatSeconds()
	sources, err := a.st.listSources()
	if err != nil {
		return
	}
	for _, src := range sources {
		if !src.Enabled || src.Kind != "device" || !src.LastSeenAt.Valid {
			continue
		}
		lastSeen, err := parseTS(src.LastSeenAt.String)
		if err != nil {
			continue
		}
		if now.Sub(lastSeen) <= time.Duration(hbSec)*time.Second {
			continue
		}
		open, err := a.st.faultOpen(src.ID, "heartbeat")
		if err != nil || open {
			continue
		}
		var changes []*changeEntry
		err = a.st.withTx(nil, func(tx *sql.Tx) error {
			// 事务内重读来源并复核全部条件（避免与接入并发竞态：
			// 预检后新接入已提交时不得再开失联故障）。
			fresh, err := loadSourceTx(tx, src.ID)
			if err != nil {
				return err
			}
			if !fresh.Enabled || fresh.Kind != "device" || fresh.DeletedAt.Valid ||
				!fresh.LastSeenAt.Valid {
				return nil
			}
			freshSeen, err := parseTS(fresh.LastSeenAt.String)
			if err != nil || now.Sub(freshSeen) <= time.Duration(hbSec)*time.Second {
				return nil
			}
			// 事务内再确认（避免与接入并发双开）。
			f, err := loadFaultTx(tx, fresh.ID, "heartbeat")
			if err != nil {
				return err
			}
			if f != nil && f.State == "open" {
				return nil
			}
			fk := "heartbeat"
			act := "open"
			title := fmt.Sprintf("%s 失联", fresh.Name)
			body := fmt.Sprintf("已连续 %d 秒无心跳上报", hbSec)
			if err := a.emitHubEventTx(tx, fresh, "heartbeat_lost", "critical", &fk, &act,
				title, &body, now, &changes); err != nil {
				return err
			}
			return a.emitSourceChangeTx(tx, fresh, &changes)
		})
		if err == nil {
			a.re.pauseSource(src.ID)
			a.st.publish(changes)
		}
	}
}

// heartbeatLoop 周期性执行心跳监测。
func (a *app) heartbeatLoop(ctxDone <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case now := <-t.C:
			a.checkHeartbeats(now.UTC())
		}
	}
}
