package report

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"assistant-agent/internal/collect"
	"assistant-agent/internal/queue"
)

// 采集单次超时：内部探测已带各自子超时，此处兜底不超过一个上报周期量级。
const collectTimeout = 25 * time.Second

// DefaultBackoff 指数退避表：1s→2→4→8→16→32→60s 封顶（events.md §5）。
func DefaultBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	const max = 60 * time.Second
	if attempt > 10 {
		return max
	}
	d := time.Second << uint(attempt)
	if d > max {
		return max
	}
	return d
}

// Reporter 驱动采集循环与补传循环。
//
// 不变式：
//   - 每帧样本先入持久队列（绝不直接发），由补传循环按序送达——
//     中枢不可达天然转为积压补传，采集循环永不阻塞在网络上。
//   - 指标 seq / 事件 seq 由队列分配，重启续号。
type Reporter struct {
	prov collect.Provider
	q    *queue.Queue
	hc   *HubClient
	info AgentInfo
	log  *slog.Logger

	wake     chan struct{}
	interval atomic.Int64 // 当前上报间隔（time.Duration 纳秒），随 hub 响应动态调整

	now          func() time.Time
	backoffFn    func(attempt int) time.Duration
	probeEvery   time.Duration // 401 探测周期，默认 5min
	flushTimeout time.Duration // 退出 flush 上限
	maxReplay    int64         // 指标补传批数上限（>则丢最旧+overflow），默认 100
	maxEventSend int           // 单批事件上限，契约 100
}

// New 构造 Reporter。interval 为初始上报间隔。
func New(prov collect.Provider, q *queue.Queue, hc *HubClient, info AgentInfo, interval time.Duration, log *slog.Logger) *Reporter {
	r := &Reporter{
		prov:         prov,
		q:            q,
		hc:           hc,
		info:         info,
		log:          log,
		wake:         make(chan struct{}, 1),
		now:          time.Now,
		backoffFn:    DefaultBackoff,
		probeEvery:   5 * time.Minute,
		flushTimeout: 10 * time.Second,
		maxReplay:    100,
		maxEventSend: 100,
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	r.interval.Store(int64(interval))
	return r
}

// Interval 返回当前上报间隔（hub 可下调）。
func (r *Reporter) Interval() time.Duration {
	return time.Duration(r.interval.Load())
}

// Run 启动：发 agent_started → 采集循环 → 补传循环（阻塞至 ctx 取消 → flush）。
func (r *Reporter) Run(ctx context.Context) error {
	r.emitEvent(func(seq int64) Event {
		return AgentStartedEvent(seq, r.info.Version, r.info.Hostname, r.now())
	})
	go r.collectLoop(ctx)
	r.drainLoop(ctx)
	return nil
}

// signal 唤醒补传循环（非阻塞）。
func (r *Reporter) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// setIntervalSeconds 应用 hub 下发的间隔（契约范围 10..600 秒）。
func (r *Reporter) setIntervalSeconds(s int64) {
	if s < 10 {
		s = 10
	}
	if s > 600 {
		s = 600
	}
	r.interval.Store(int64(time.Duration(s) * time.Second))
}

// emitEvent 分配事件 seq、入队并唤醒补传。
func (r *Reporter) emitEvent(build func(seq int64) Event) {
	seq, err := r.q.NextEventSeq()
	if err != nil {
		r.log.Error("event seq alloc failed", "err", err)
		return
	}
	e := build(seq)
	b, err := MarshalEvent(e)
	if err != nil {
		r.log.Error("event marshal failed", "err", err)
		return
	}
	if _, err := r.q.EnqueueEvent(seq, b); err != nil {
		r.log.Error("event enqueue failed", "err", err)
		return
	}
	r.signal()
}

// emitSystemEventNoTrim 入队系统汇总事件（queue_overflow / 毒载荷通知），
// 不裁剪事件队列（汇总事件本身是丢弃凭证，不能为它再丢条目）。
func (r *Reporter) emitSystemEventNoTrim(build func(seq int64) Event) {
	seq, err := r.q.NextEventSeq()
	if err != nil {
		r.log.Error("event seq alloc failed", "err", err)
		return
	}
	b, err := MarshalEvent(build(seq))
	if err != nil {
		r.log.Error("event marshal failed", "err", err)
		return
	}
	if err := r.q.EnqueueEventNoTrim(seq, b); err != nil {
		r.log.Error("system event enqueue failed", "err", err)
	}
}

// collectLoop 每 interval 采集一帧 → 分配 seq → 入队 → 唤醒补传。
// 不直接 POST：保证顺序与"宁可入队不丢"的语义，也绝不阻塞采集。
func (r *Reporter) collectLoop(ctx context.Context) {
	timer := time.NewTimer(r.Interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.collectOnce(ctx)
			timer.Reset(r.Interval())
		}
	}
}

func (r *Reporter) collectOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	res, err := r.prov.Collect(cctx)
	if err != nil {
		r.log.Warn("collect failed", "err", err)
		return // 采不到就缺席整帧，绝不伪造
	}
	seq, err := r.q.NextMetricsSeq()
	if err != nil {
		r.log.Error("metrics seq alloc failed", "err", err)
		return
	}
	body, err := BuildMetrics(seq, r.info, res, r.now())
	if err != nil {
		r.log.Error("metrics marshal failed", "err", err)
		return
	}
	dropped, err := r.q.EnqueueMetrics(seq, body)
	if err != nil {
		r.log.Error("metrics enqueue failed", "err", err)
		return
	}
	if dropped > 0 {
		r.log.Warn("metrics queue overflow, dropped oldest batches", "dropped", dropped)
	}
	r.signal()
}

// drainLoop 补传状态机：
//   - 空闲：阻塞在 wake；
//   - 可重试失败：指数退避 1s→60s；
//   - 401：停发 + 每 probeEvery（5min）探测一次。
func (r *Reporter) drainLoop(ctx context.Context) {
	var (
		attempt      int
		unauthorized bool
		nextTry      time.Time
	)
	for {
		now := r.now()
		if now.Before(nextTry) {
			timer := time.NewTimer(nextTry.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				r.finalFlush()
				return
			case <-r.wake:
				timer.Stop()
				continue
			case <-timer.C:
				continue
			}
		}
		if !r.anyPending() {
			select {
			case <-ctx.Done():
				r.finalFlush()
				return
			case <-r.wake:
				continue
			}
		}
		if unauthorized {
			r.probeUnauthorized(ctx, &attempt, &unauthorized, &nextTry)
			continue
		}
		err := r.drainOnce(ctx)
		if err == nil {
			attempt = 0
			nextTry = time.Time{}
			continue
		}
		switch Classify(err) {
		case ErrUnauthorized:
			unauthorized = true
			nextTry = r.now().Add(r.probeEvery)
			r.log.Warn("hub 401 unauthorized: pause sending, probe every "+r.probeEvery.String(), "err", err)
		case ErrPoison:
			attempt = 0 // drainOnce 内部已处理；防御性兜底
		default:
			delay := r.backoffFn(attempt)
			if se, ok := err.(*SendError); ok && se.RetryAfter > 0 {
				delay = se.RetryAfter
			}
			attempt++
			nextTry = r.now().Add(delay)
			r.log.Warn("ingest failed, queued for retry", "retry_in", delay.String(), "err", err)
		}
	}
}

// anyPending 判断队列是否有待发送内容。
func (r *Reporter) anyPending() bool {
	m, err1 := r.q.MetricsPending()
	e, err2 := r.q.EventsPending()
	if err1 != nil || err2 != nil {
		return true // 读不出来按有待发处理，走 drainOnce 的错误路径退避
	}
	return m+e > 0
}

// drainOnce 尽力把两队发空：先转 overflow 汇总事件，再按序发指标、事件。
// 返回 nil = 全发完；返回 error = 中途失败（调用方按分类退避）。
func (r *Reporter) drainOnce(ctx context.Context) error {
	if err := r.flushOverflowEvents(); err != nil {
		return err
	}

	// 指标批次：积压 >maxReplay → 丢最旧只补最近 maxReplay 批（events.md §5）。
	for {
		pending, err := r.q.MetricsPending()
		if err != nil {
			return err
		}
		if pending == 0 {
			break
		}
		if pending > r.maxReplay {
			dropped, err := r.q.DropOldestMetrics(pending - r.maxReplay)
			if err != nil {
				return err
			}
			if dropped > 0 {
				r.log.Warn("metrics backlog trimmed, keeping latest batches",
					"kept", r.maxReplay, "dropped", dropped)
				if err := r.flushOverflowEvents(); err != nil {
					return err
				}
			}
			continue
		}
		rows, err := r.q.PeekMetrics(1)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		row := rows[0]
		ack, serr := r.hc.PostMetrics(ctx, row.Payload)
		if serr != nil {
			if Classify(serr) == ErrPoison {
				r.dropPoisonMetrics(row, serr)
				continue
			}
			return serr
		}
		if err := r.q.DeleteMetrics([]int64{row.ID}); err != nil {
			return err
		}
		if ack.ReportIntervalSeconds > 0 {
			r.setIntervalSeconds(int64(ack.ReportIntervalSeconds))
		}
	}

	// 事件：≤100 条一批。
	for {
		rows, err := r.q.PeekEvents(r.maxEventSend)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		raws := make([]json.RawMessage, 0, len(rows))
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			raws = append(raws, json.RawMessage(row.Payload))
			ids = append(ids, row.ID)
		}
		body, err := BuildEvents(raws, r.now())
		if err != nil {
			return err
		}
		ack, serr := r.hc.PostEvents(ctx, body)
		if serr != nil {
			if Classify(serr) == ErrPoison {
				r.dropPoisonEvents(rows, serr)
				continue
			}
			return serr
		}
		for _, rej := range ack.Rejected {
			r.log.Warn("event rejected by hub", "eventId", rej.EventID, "code", rej.Code, "msg", rej.Message)
		}
		if err := r.q.DeleteEvents(ids); err != nil {
			return err
		}
	}
	return nil
}

// flushOverflowEvents 把累计溢出记录转成 queue_overflow 事件（不入裁剪队列）。
func (r *Reporter) flushOverflowEvents() error {
	for _, kind := range []string{queue.KindMetrics, queue.KindEvents} {
		ov, ok, err := r.q.TakeOverflow(kind)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		r.log.Warn("queue overflow occurred", "kind", kind, "dropped", ov.Dropped,
			"first", ov.FirstAt.UTC().Format(time.RFC3339), "last", ov.LastAt.UTC().Format(time.RFC3339))
		r.emitSystemEventNoTrim(func(seq int64) Event {
			return QueueOverflowEvent(seq, kind, ov.Dropped, ov.FirstAt, ov.LastAt, r.now())
		})
	}
	return nil
}

// dropPoisonMetrics 丢弃被中枢判为非法的指标批次（400/413），记 custom 事件。
func (r *Reporter) dropPoisonMetrics(row queue.MetricsRow, serr error) {
	r.log.Warn("metrics batch rejected by hub, dropping", "seq", row.Seq, "err", serr)
	if err := r.q.DeleteMetrics([]int64{row.ID}); err != nil {
		r.log.Error("drop poison metrics failed", "err", err)
	}
	r.emitSystemEventNoTrim(func(seq int64) Event {
		return PayloadDroppedEvent(seq, "metrics seq="+strconv.FormatInt(row.Seq, 10), serr.Error(), r.now())
	})
}

// dropPoisonEvents 丢弃被中枢整批拒绝的事件（400/413），记 custom 事件。
func (r *Reporter) dropPoisonEvents(rows []queue.EventRow, serr error) {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	r.log.Warn("events batch rejected by hub, dropping", "count", len(rows), "err", serr)
	if err := r.q.DeleteEvents(ids); err != nil {
		r.log.Error("drop poison events failed", "err", err)
	}
	r.emitSystemEventNoTrim(func(seq int64) Event {
		return PayloadDroppedEvent(seq, "events count="+strconv.Itoa(len(rows)), serr.Error(), r.now())
	})
}

// probeUnauthorized 401 状态下每 probeEvery 探测一次队首：
//   - 成功 → 密钥恢复（该条已送达，删除）→ 回到正常补传；
//   - 仍 401 → 继续停发；
//   - 其他失败 → 转普通退避。
func (r *Reporter) probeUnauthorized(ctx context.Context, attempt *int, unauthorized *bool, nextTry *time.Time) {
	rows, _ := r.q.PeekMetrics(1)
	if len(rows) > 0 {
		ack, err := r.hc.PostMetrics(ctx, rows[0].Payload)
		if err == nil {
			if ack.ReportIntervalSeconds > 0 {
				r.setIntervalSeconds(int64(ack.ReportIntervalSeconds))
			}
			if derr := r.q.DeleteMetrics([]int64{rows[0].ID}); derr != nil {
				r.log.Error("probe delete failed", "err", derr)
			}
			*unauthorized = false
			*attempt = 0
			r.log.Info("hub key accepted again, resuming ingest")
			return
		}
		r.handleProbeErr(err, attempt, unauthorized, nextTry)
		return
	}
	evs, _ := r.q.PeekEvents(1)
	if len(evs) == 0 {
		*unauthorized = false // 无可探对象，回到常态（下个入队会再触发判定）
		return
	}
	body, berr := BuildEvents([]json.RawMessage{evs[0].Payload}, r.now())
	if berr != nil {
		r.handleProbeErr(berr, attempt, unauthorized, nextTry)
		return
	}
	_, err := r.hc.PostEvents(ctx, body)
	if err == nil {
		if derr := r.q.DeleteEvents([]int64{evs[0].ID}); derr != nil {
			r.log.Error("probe delete failed", "err", derr)
		}
		*unauthorized = false
		*attempt = 0
		r.log.Info("hub key accepted again, resuming ingest")
		return
	}
	r.handleProbeErr(err, attempt, unauthorized, nextTry)
}

func (r *Reporter) handleProbeErr(err error, attempt *int, unauthorized *bool, nextTry *time.Time) {
	if Classify(err) == ErrUnauthorized {
		*nextTry = r.now().Add(r.probeEvery)
		return
	}
	*unauthorized = false
	delay := r.backoffFn(*attempt)
	*attempt++
	*nextTry = r.now().Add(delay)
}

// finalFlush 退出前尽力发空队列（独立 ctx + 上限）。
func (r *Reporter) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), r.flushTimeout)
	defer cancel()
	r.log.Info("agent stopping, flushing queue")
	if err := r.drainOnce(ctx); err != nil {
		r.log.Warn("final flush incomplete", "err", err)
	}
}
