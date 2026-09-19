package main

import (
	"testing"
	"time"
)

// setRules 直接 PUT 规则集。
func setRules(t *testing.T, base, token string, rules []map[string]any, hbSec int64) {
	t.Helper()
	code, body := doJSON(t, "GET", base+"/api/v1/rules", token, nil)
	if code != 200 {
		t.Fatalf("get rules: %d", code)
	}
	ver := int64(body["version"].(float64))
	req := map[string]any{"expectedVersion": ver, "rules": rules}
	if hbSec > 0 {
		req["heartbeatSeconds"] = hbSec
	}
	code, body = doJSON(t, "PUT", base+"/api/v1/rules", token, req)
	if code != 200 {
		t.Fatalf("put rules: %d %v", code, body)
	}
}

// sampleAt 构造带特定 ts 的样本。
func sampleAt(ts time.Time, extra map[string]any) map[string]any {
	s := map[string]any{"ts": fmtTS(ts)}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// TestThresholdWindow 阈值窗口：forSeconds 内所有样本超限才 open。
func TestThresholdWindow(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	setRules(t, srv.URL, token, []map[string]any{
		{"id": "cpu", "kind": "threshold", "metric": "cpu_percent", "op": "gt",
			"value": 90, "forSeconds": 180, "recoverValue": 75, "recoverForSeconds": 60,
			"severity": "warning", "enabled": true},
	}, 0)

	base := nowUTC()
	fk := "rule:cpu:-"

	// t0：首次超限 → 计时开始，未 open。
	ingestMetrics(t, srv.URL, key, 1, sampleAt(base, map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatalf("premature open: %+v", f)
	}
	// t0+100s：仍超限但未满窗口 → 不 open。
	ingestMetrics(t, srv.URL, key, 2, sampleAt(base.Add(100*time.Second), map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatalf("early open: %+v", f)
	}
	// t0+50s 回落一次 → 计时重置。
	// 改为：t0+200s 仍超限 → 满窗口 open。
	ingestMetrics(t, srv.URL, key, 3, sampleAt(base.Add(200*time.Second), map[string]any{"cpuPercent": 91.0}))
	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" {
		t.Fatalf("should open: %+v", f)
	}
	if f.Severity != "warning" {
		t.Fatalf("sev: %s", f.Severity)
	}
	var kind string
	if err := a.st.db.QueryRow(
		`SELECT kind FROM messages WHERE fault_id=?`, f.ID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "threshold" {
		t.Fatalf("msg kind: %s", kind)
	}
}

// TestThresholdBreachReset 窗口内回落 → 计时重置。
func TestThresholdBreachReset(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	setRules(t, srv.URL, token, []map[string]any{
		{"id": "cpu", "kind": "threshold", "metric": "cpu_percent", "op": "gt",
			"value": 90, "forSeconds": 100, "recoverValue": 80, "recoverForSeconds": 60,
			"severity": "warning", "enabled": true},
	}, 0)
	base := nowUTC().Add(-time.Minute)
	fk := "rule:cpu:-"

	ingestMetrics(t, srv.URL, key, 1, sampleAt(base, map[string]any{"cpuPercent": 95.0}))
	// +50s 回落 → 重置。
	ingestMetrics(t, srv.URL, key, 2, sampleAt(base.Add(50*time.Second), map[string]any{"cpuPercent": 10.0}))
	// +160s 再次超限 → 新计时，只有 0s 累积。
	ingestMetrics(t, srv.URL, key, 3, sampleAt(base.Add(160*time.Second), map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatalf("should not open (timer reset): %+v", f)
	}
	// +270s 超限 → 110s 累积 → open。
	ingestMetrics(t, srv.URL, key, 4, sampleAt(base.Add(270*time.Second), map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f == nil || f.State != "open" {
		t.Fatalf("should open: %+v", f)
	}
}

// TestThresholdAbsentHold 窗口内样本缺席：计时器保持不重置。
func TestThresholdAbsentHold(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	setRules(t, srv.URL, token, []map[string]any{
		{"id": "cpu", "kind": "threshold", "metric": "cpu_percent", "op": "gt",
			"value": 90, "forSeconds": 100, "recoverValue": 80, "recoverForSeconds": 60,
			"severity": "warning", "enabled": true},
	}, 0)
	base := nowUTC()
	fk := "rule:cpu:-"

	ingestMetrics(t, srv.URL, key, 1, sampleAt(base, map[string]any{"cpuPercent": 95.0}))
	// +50s 样本无 cpuPercent（缺席）→ 保持不重置。
	ingestMetrics(t, srv.URL, key, 2, sampleAt(base.Add(50*time.Second), map[string]any{"memPercent": 10.0}))
	// +120s 再超限 → 距首次 120s ≥ 100s → open（缺席未重置）。
	ingestMetrics(t, srv.URL, key, 3, sampleAt(base.Add(120*time.Second), map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f == nil || f.State != "open" {
		t.Fatalf("absent should hold timer: %+v", f)
	}
}

// TestThresholdRecovery 回差恢复：跨过 recoverValue 连续 recoverForSeconds → resolve。
func TestThresholdRecovery(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	setRules(t, srv.URL, token, []map[string]any{
		{"id": "cpu", "kind": "threshold", "metric": "cpu_percent", "op": "gt",
			"value": 90, "forSeconds": 60, "recoverValue": 75, "recoverForSeconds": 60,
			"severity": "warning", "enabled": true},
	}, 0)
	base := nowUTC()
	fk := "rule:cpu:-"

	ingestMetrics(t, srv.URL, key, 1, sampleAt(base, map[string]any{"cpuPercent": 95.0}))
	ingestMetrics(t, srv.URL, key, 2, sampleAt(base.Add(70*time.Second), map[string]any{"cpuPercent": 95.0}))
	if f := queryFault(t, a, srcID, fk); f == nil {
		t.Fatal("should be open")
	}
	// +80s：回差区间内（85 > recoverValue 75，且不超限）→ 不恢复。
	ingestMetrics(t, srv.URL, key, 3, sampleAt(base.Add(80*time.Second), map[string]any{"cpuPercent": 85.0}))
	if f := queryFault(t, a, srcID, fk); f.State != "open" {
		t.Fatalf("hysteresis should hold open")
	}
	// +90s：跨过回差（70 < 75）→ 恢复计时开始。
	ingestMetrics(t, srv.URL, key, 4, sampleAt(base.Add(90*time.Second), map[string]any{"cpuPercent": 70.0}))
	if f := queryFault(t, a, srcID, fk); f.State != "open" {
		t.Fatalf("recover window not met")
	}
	// +100s：回到 85（区间）→ 恢复计时中断。
	ingestMetrics(t, srv.URL, key, 5, sampleAt(base.Add(100*time.Second), map[string]any{"cpuPercent": 85.0}))
	// +170s：再次跨过回差 → 重新计时 0s。
	ingestMetrics(t, srv.URL, key, 6, sampleAt(base.Add(170*time.Second), map[string]any{"cpuPercent": 70.0}))
	if f := queryFault(t, a, srcID, fk); f.State != "open" {
		t.Fatalf("recover timer should have reset")
	}
	// +240s：连续 70s 低于 75 → resolve。
	ingestMetrics(t, srv.URL, key, 7, sampleAt(base.Add(240*time.Second), map[string]any{"cpuPercent": 70.0}))
	f := queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("should recover: %+v", f)
	}
}

// TestDiskThresholdLabel disk_percent 标签选择器：每个 mount 独立计时。
func TestDiskThresholdLabel(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	setRules(t, srv.URL, token, []map[string]any{
		{"id": "disk", "kind": "threshold", "metric": "disk_percent", "label": "*", "op": "gt",
			"value": 90, "forSeconds": 60, "recoverValue": 85, "recoverForSeconds": 60,
			"severity": "warning", "enabled": true},
	}, 0)
	base := nowUTC()

	disks := func(root, vol float64) []map[string]any {
		return []map[string]any{
			{"mount": "/", "percent": root},
			{"mount": "/vol1", "percent": vol},
		}
	}
	ingestMetrics(t, srv.URL, key, 1, sampleAt(base, map[string]any{"disks": disks(50, 95)}))
	ingestMetrics(t, srv.URL, key, 2, sampleAt(base.Add(70*time.Second), map[string]any{"disks": disks(50, 95)}))
	if f := queryFault(t, a, srcID, "rule:disk:mount=/vol1"); f == nil || f.State != "open" {
		t.Fatal("vol1 should open")
	}
	if f := queryFault(t, a, srcID, "rule:disk:mount=/"); f != nil {
		t.Fatal("root should not open")
	}
}

// TestContainerTransitions 容器 running→exited open、回 running resolve、消失 resolve。
func TestContainerTransitions(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)
	fk := "container:web"
	rc := 0

	run := map[string]any{"name": "web", "state": "running", "restartCount": rc}
	// 首拍建立基线（running）。
	ingestMetrics(t, srv.URL, key, 1, sampleAt(nowUTC(), map[string]any{"containers": []any{run}}))
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatal("baseline should not open")
	}
	// running → exited → open。
	ingestMetrics(t, srv.URL, key, 2, sampleAt(nowUTC(), map[string]any{
		"containers": []any{map[string]any{"name": "web", "state": "exited", "exitCode": 137}},
	}))
	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" || f.Severity != "warning" {
		t.Fatalf("container exit: %+v", f)
	}
	var kind string
	a.st.db.QueryRow(`SELECT kind FROM messages WHERE fault_id=?`, f.ID).Scan(&kind)
	if kind != "container_exit" {
		t.Fatalf("kind: %s", kind)
	}
	// 保持 exited → 不重复发事件（eventCount 仍 1）。
	ingestMetrics(t, srv.URL, key, 3, sampleAt(nowUTC(), map[string]any{
		"containers": []any{map[string]any{"name": "web", "state": "exited", "exitCode": 137}},
	}))
	f = queryFault(t, a, srcID, fk)
	if f.EventCount != 1 {
		t.Fatalf("no repeat: %d", f.EventCount)
	}
	// 回 running → resolve。
	ingestMetrics(t, srv.URL, key, 4, sampleAt(nowUTC(), map[string]any{"containers": []any{run}}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("should resolve: %+v", f)
	}
	// 再 exited → 新 incident。
	ingestMetrics(t, srv.URL, key, 5, sampleAt(nowUTC(), map[string]any{
		"containers": []any{map[string]any{"name": "web", "state": "exited"}},
	}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "open" || f.Incident != 2 {
		t.Fatalf("second incident: %+v", f)
	}
	// 容器消失 → resolve（容器已移除）。
	ingestMetrics(t, srv.URL, key, 6, sampleAt(nowUTC(), map[string]any{"containers": []any{}}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("vanished should resolve: %+v", f)
	}
}

// TestSmartTransitions smart：failing→open、ok→resolve、asleep 不迁移。
func TestSmartTransitions(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)
	fk := "smart:sda"

	nas := func(smart string) map[string]any {
		return map[string]any{"disks": []any{map[string]any{"dev": "sda", "smart": smart}}}
	}
	ingestMetrics(t, srv.URL, key, 1, sampleAt(nowUTC(), map[string]any{"nas": nas("ok")}))
	ingestMetrics(t, srv.URL, key, 2, sampleAt(nowUTC(), map[string]any{"nas": nas("asleep")}))
	if f := queryFault(t, a, srcID, fk); f != nil {
		t.Fatal("asleep should not open")
	}
	ingestMetrics(t, srv.URL, key, 3, sampleAt(nowUTC(), map[string]any{"nas": nas("failing")}))
	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" || f.Severity != "critical" {
		t.Fatalf("failing: %+v", f)
	}
	// asleep：不迁移 → 仍 open。
	ingestMetrics(t, srv.URL, key, 4, sampleAt(nowUTC(), map[string]any{"nas": nas("asleep")}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "open" {
		t.Fatal("asleep must not migrate state")
	}
	// ok → resolve。
	ingestMetrics(t, srv.URL, key, 5, sampleAt(nowUTC(), map[string]any{"nas": nas("ok")}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("ok should resolve: %+v", f)
	}
}

// TestPoolTransitions pool：degraded→open、ok→resolve、unknown 不迁移。
func TestPoolTransitions(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)
	fk := "pool:storage1"

	pool := func(state string) map[string]any {
		return map[string]any{"pools": []any{map[string]any{"name": "storage1", "state": state}}}
	}
	ingestMetrics(t, srv.URL, key, 1, sampleAt(nowUTC(), map[string]any{"nas": pool("ok")}))
	ingestMetrics(t, srv.URL, key, 2, sampleAt(nowUTC(), map[string]any{"nas": pool("degraded")}))
	f := queryFault(t, a, srcID, fk)
	if f == nil || f.State != "open" || f.Severity != "critical" {
		t.Fatalf("degraded: %+v", f)
	}
	ingestMetrics(t, srv.URL, key, 3, sampleAt(nowUTC(), map[string]any{"nas": pool("unknown")}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "open" {
		t.Fatal("unknown must not migrate")
	}
	ingestMetrics(t, srv.URL, key, 4, sampleAt(nowUTC(), map[string]any{"nas": pool("ok")}))
	f = queryFault(t, a, srcID, fk)
	if f.State != "resolved" {
		t.Fatalf("ok should resolve: %+v", f)
	}
}

// TestHeartbeat 失联开故障、任何接入恢复。
func TestHeartbeat(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "nas", "device", nil)

	ingestMetrics(t, srv.URL, key, 1, sampleAt(nowUTC(), nil))
	a.checkHeartbeats(nowUTC())
	if f := queryFault(t, a, srcID, "heartbeat"); f != nil {
		t.Fatal("should not open yet")
	}
	// 模拟 200s 后（>180 默认）。
	a.checkHeartbeats(nowUTC().Add(200 * time.Second))
	f := queryFault(t, a, srcID, "heartbeat")
	if f == nil || f.State != "open" || f.Severity != "critical" {
		t.Fatalf("heartbeat lost: %+v", f)
	}
	// 同一失联期只开一次。
	a.checkHeartbeats(nowUTC().Add(400 * time.Second))
	f = queryFault(t, a, srcID, "heartbeat")
	if f.EventCount != 1 {
		t.Fatalf("dup heartbeat: %d", f.EventCount)
	}
	// 任何接入恢复（这里是 events 接入也能恢复）。
	code, _ := ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("e1", 1, "custom", nil)})
	if code != 200 {
		t.Fatal("ingest events")
	}
	f = queryFault(t, a, srcID, "heartbeat")
	if f.State != "resolved" {
		t.Fatalf("should recover on ingest: %+v", f)
	}
	// 再次失联 → 新轮次。
	a.checkHeartbeats(nowUTC().Add(600 * time.Second))
	f = queryFault(t, a, srcID, "heartbeat")
	if f.State != "open" || f.Incident != 2 {
		t.Fatalf("second loss: %+v", f)
	}
}

// TestHeartbeatSkipsFeedback kind=feedback 来源无周期心跳预期，
// checkHeartbeats 不得对其开失联故障（events.md §4）。
func TestHeartbeatSkipsFeedback(t *testing.T) {
	a := newTestApp(t)
	srv := testServer(t, a)
	token := login(t, srv.URL)
	srcID, key := createSource(t, srv.URL, token, "fb", "feedback", nil)

	// feedback 接入同样刷新 lastSeenAt，但超期不得开 fault。
	ingestEvents(t, srv.URL, key, []map[string]any{mkEvent("f1", 1, "custom", nil)})
	a.checkHeartbeats(nowUTC().Add(300 * time.Second))
	if f := queryFault(t, a, srcID, "heartbeat"); f != nil {
		t.Fatalf("feedback source must not get heartbeat fault: %+v", f)
	}
}
