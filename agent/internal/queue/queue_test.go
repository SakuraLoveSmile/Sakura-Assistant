package queue

import (
	"fmt"
	"testing"
	"time"
)

func openTest(t *testing.T, dir string, maxBatches, maxEvents int) *Queue {
	t.Helper()
	q, err := Open(dir, maxBatches, maxEvents)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	return q
}

// seq 持久化：重启（关库再开）后续号不清零，且两流独立计数。
func TestSeqPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 1000)
	for i := int64(1); i <= 3; i++ {
		s, err := q.NextMetricsSeq()
		if err != nil || s != i {
			t.Fatalf("metrics seq = %d, %v; want %d", s, err, i)
		}
	}
	es, err := q.NextEventSeq()
	if err != nil || es != 1 {
		t.Fatalf("events seq = %d, %v; want 1 (独立计数)", es, err)
	}
	q.Close()

	q2 := openTest(t, dir, 500, 1000)
	defer q2.Close()
	s, err := q2.NextMetricsSeq()
	if err != nil || s != 4 {
		t.Fatalf("reopened metrics seq = %d, %v; want 4", s, err)
	}
	es, err = q2.NextEventSeq()
	if err != nil || es != 2 {
		t.Fatalf("reopened events seq = %d, %v; want 2", es, err)
	}
}

// 队列内容持久化：入队 → 关库 → 重开 → 数据仍在，按入队序读出。
func TestQueuedRowsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 1000)
	for i := int64(1); i <= 3; i++ {
		if _, err := q.EnqueueMetrics(i, []byte(fmt.Sprintf(`{"seq":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.EnqueueEvent(7, []byte(`{"seq":7}`)); err != nil {
		t.Fatal(err)
	}
	q.Close()

	q2 := openTest(t, dir, 500, 1000)
	defer q2.Close()
	rows, err := q2.PeekMetrics(10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("metrics rows = %v, %v; want 3", rows, err)
	}
	for i, r := range rows {
		want := int64(i + 1)
		if r.Seq != want {
			t.Fatalf("row %d seq = %d; want %d（按序）", i, r.Seq, want)
		}
	}
	evs, err := q2.PeekEvents(10)
	if err != nil || len(evs) != 1 || evs[0].Seq != 7 {
		t.Fatalf("event rows = %+v, %v", evs, err)
	}
}

// 溢出丢最旧 + overflow 记录（条数与时间窗）。
func TestOverflowDropsOldestAndRecords(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 3, 1000) // 上限 3 批
	defer q.Close()

	for i := int64(1); i <= 5; i++ {
		dropped, err := q.EnqueueMetrics(i, []byte(fmt.Sprintf("p%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if i <= 3 && dropped != 0 {
			t.Fatalf("enqueue %d dropped = %d; want 0", i, dropped)
		}
		if i > 3 && dropped != 1 {
			t.Fatalf("enqueue %d dropped = %d; want 1", i, dropped)
		}
	}

	rows, err := q.PeekMetrics(10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %v, %v; want 3", rows, err)
	}
	if rows[0].Seq != 3 || rows[2].Seq != 5 {
		t.Fatalf("remaining seqs = %d..%d; want 3..5（丢最旧）", rows[0].Seq, rows[2].Seq)
	}

	ov, ok, err := q.TakeOverflow(KindMetrics)
	if err != nil || !ok {
		t.Fatalf("overflow record missing: %v %v", ov, err)
	}
	if ov.Dropped != 2 {
		t.Fatalf("overflow dropped = %d; want 2", ov.Dropped)
	}
	if ov.FirstAt.After(ov.LastAt) || ov.FirstAt.IsZero() {
		t.Fatalf("bad overflow window: %v .. %v", ov.FirstAt, ov.LastAt)
	}

	// 取出后即清除。
	if _, ok, _ := q.TakeOverflow(KindMetrics); ok {
		t.Fatal("overflow should be cleared after take")
	}
}

// 事件队列独立上限与 overflow。
func TestEventsOverflow(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 2)
	defer q.Close()
	for i := int64(1); i <= 4; i++ {
		if _, err := q.EnqueueEvent(i, []byte("e")); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := q.PeekEvents(10)
	if len(evs) != 2 || evs[0].Seq != 3 {
		t.Fatalf("events = %+v; want seqs 3,4", evs)
	}
	ov, ok, _ := q.TakeOverflow(KindEvents)
	if !ok || ov.Dropped != 2 {
		t.Fatalf("events overflow = %+v ok=%v", ov, ok)
	}
}

// DropOldestMetrics：指标补传降采样，丢最旧 n 条 + overflow。
func TestDropOldestMetrics(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 1000)
	defer q.Close()
	for i := int64(1); i <= 6; i++ {
		if _, err := q.EnqueueMetrics(i, []byte("p")); err != nil {
			t.Fatal(err)
		}
	}
	dropped, err := q.DropOldestMetrics(4)
	if err != nil || dropped != 4 {
		t.Fatalf("dropped = %d, %v; want 4", dropped, err)
	}
	rows, _ := q.PeekMetrics(10)
	if len(rows) != 2 || rows[0].Seq != 5 {
		t.Fatalf("rows = %+v; want seqs 5,6", rows)
	}
	ov, ok, _ := q.TakeOverflow(KindMetrics)
	if !ok || ov.Dropped != 4 {
		t.Fatalf("overflow = %+v ok=%v; want dropped 4", ov, ok)
	}
}

// DeleteMetrics / DeleteEvents 按 id 删除。
func TestDeleteRows(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 1000)
	defer q.Close()
	for i := int64(1); i <= 3; i++ {
		q.EnqueueMetrics(i, []byte("p"))
		q.EnqueueEvent(i, []byte("e"))
	}
	rows, _ := q.PeekMetrics(10)
	if err := q.DeleteMetrics([]int64{rows[0].ID, rows[2].ID}); err != nil {
		t.Fatal(err)
	}
	left, _ := q.PeekMetrics(10)
	if len(left) != 1 || left[0].Seq != 2 {
		t.Fatalf("left = %+v; want seq 2", left)
	}
	evs, _ := q.PeekEvents(10)
	if err := q.DeleteEvents([]int64{evs[0].ID}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.EventsPending(); n != 2 {
		t.Fatalf("events pending = %d; want 2", n)
	}
}

// EnqueueEventNoTrim：系统事件不裁剪队列（溢出汇总自身不再丢条目）。
func TestEnqueueEventNoTrim(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 500, 2)
	defer q.Close()
	q.EnqueueEvent(1, []byte("a"))
	q.EnqueueEvent(2, []byte("b"))
	if err := q.EnqueueEventNoTrim(3, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.EventsPending(); n != 3 {
		t.Fatalf("pending = %d; want 3（NoTrim 不裁剪）", n)
	}
	// 但后续普通入队仍按上限裁剪。
	q.EnqueueEvent(4, []byte("d"))
	if n, _ := q.EventsPending(); n != 2 {
		t.Fatalf("pending = %d; want 2", n)
	}
}

// 溢出时间窗覆盖多次丢弃（min/max 合并）。
func TestOverflowWindowAccumulates(t *testing.T) {
	dir := t.TempDir()
	q := openTest(t, dir, 1, 1000)
	defer q.Close()
	q.now = func() time.Time { return time.Unix(100, 0) }
	q.EnqueueMetrics(1, []byte("a"))
	q.now = func() time.Time { return time.Unix(200, 0) }
	q.EnqueueMetrics(2, []byte("b"))
	q.now = func() time.Time { return time.Unix(300, 0) }
	q.EnqueueMetrics(3, []byte("c"))
	ov, ok, _ := q.TakeOverflow(KindMetrics)
	if !ok || ov.Dropped != 2 {
		t.Fatalf("dropped = %d; want 2", ov.Dropped)
	}
	if ov.FirstAt.Unix() != 100 || ov.LastAt.Unix() != 200 {
		t.Fatalf("window = %v..%v; want 100..200", ov.FirstAt.Unix(), ov.LastAt.Unix())
	}
}
