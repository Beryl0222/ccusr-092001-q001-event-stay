package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeClock struct{ t string }

func (c *fakeClock) now() string { return c.t }

func newTestApp(t *testing.T, now string) (*App, *memStore) {
	t.Helper()
	store := newMemStore()
	clk := (&fakeClock{now}).now
	app, err := NewApp(store, clk)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app, store
}

// ok 返回的检查器签名恰好与各命令的 (IdemResult, error) 一致，
// 因此可以用 chk(a.X(...)) 直接消费多返回值。
func ok(t *testing.T) func(IdemResult, error) {
	t.Helper()
	return func(_ IdemResult, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("命令失败: %v", err)
		}
	}
}

// val 在需要命令返回值时使用：r := val(t)(a.X(...))。
func val(t *testing.T) func(IdemResult, error) IdemResult {
	return func(r IdemResult, err error) IdemResult {
		t.Helper()
		if err != nil {
			t.Fatalf("命令失败: %v", err)
		}
		return r
	}
}

// seedScenario 搭建一场跨日赛事的完整接待环境：乙等场馆、两条接驳、一家酒店、一条线路。
func seedScenario(t *testing.T, a *App) {
	t.Helper()
	chk := ok(t)
	chk(a.RegisterVenue(roleVenue, "", RegisterVenueCmd{ID: "V1", Name: "市体育中心", Grade: "乙", BaseCap: 1000}))
	chk(a.SetPartnerStatus(roleLodging, "", PartnerStatusCmd{ID: "H1", Name: "迎宾馆", Kind: kindLodging, Status: statusNormal, ReportBy: "2026-09-20"}))
	chk(a.SetWindow(roleTraffic, "", WindowCmd{Date: "2026-09-25", Start: "06:00:00", End: "23:00:00"}))
	chk(a.PlanShuttle(roleTraffic, "", ShuttleCmd{ID: "S1", Dir: dirInbound, From: "枢纽", To: "V1", Depart: "2026-09-25T17:00:00", Arrive: "2026-09-25T18:00:00", Cap: 60}))
	chk(a.PlanShuttle(roleTraffic, "", ShuttleCmd{ID: "S2", Dir: dirOutbound, From: "V1", To: "枢纽", Depart: "2026-09-25T21:30:00", Arrive: "2026-09-25T22:30:00", Cap: 60}))
	chk(a.PlanRoute(roleTourism, "", RouteCmd{ID: "R1", Name: "古城夜游", DailyCap: 20}))
	chk(a.ScheduleMatch(roleOperator, "", ScheduleMatchCmd{ID: "M1", Name: "联赛揭幕战", Venue: "V1", Start: "2026-09-25T19:00:00", End: "2026-09-25T21:00:00", Expected: 100}))
	chk(a.ReportCapacity(roleLodging, "", CapacityCmd{Partner: "H1", Date: "2026-09-25", Beds: 50}))
	chk(a.ReportCapacity(roleLodging, "", CapacityCmd{Partner: "H1", Date: "2026-09-26", Beds: 50}))
	chk(a.ReportCapacity(roleLodging, "", CapacityCmd{Partner: "H1", Date: "2026-09-27", Beds: 50}))
}

func intentA() IntentCmd {
	return IntentCmd{
		Ref: "A", MatchID: "M1", Revision: 1, People: 2,
		ArriveAt: "2026-09-25T16:30:00", ArriveBus: true,
		LeaveAt: "2026-09-25T23:00:00", LeaveBus: true,
		CheckIn: "2026-09-25", CheckOut: "2026-09-28",
		Routes: []RouteChoice{{Route: "R1", Date: "2026-09-26"}},
	}
}

// 承载等级系数：乙等 1000 座的有效容量为 850，丙等为 70%。
func TestVenueGradeFactor(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	chk := ok(t)
	chk(a.RegisterVenue(roleVenue, "", RegisterVenueCmd{ID: "V丙", Grade: "丙", BaseCap: 100}))
	v := a.state.venues["V丙"]
	if got := a.state.effectiveVenueCap(v, "2026-09-25T19:00:00"); got != 70 {
		t.Fatalf("丙等有效容量 = %d，期望 70", got)
	}
}

// 同一观众重复提交：修订号不递增必须冲突；递增后覆盖；幂等键重试返回首次结果。
func TestDuplicateIntentRevision(t *testing.T) {
	a, store := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk, get := ok(t), val(t)

	chk(a.SubmitIntent(roleOperator, "", intentA()))
	if _, err := a.SubmitIntent(roleOperator, "", intentA()); err == nil {
		t.Fatal("相同修订号重复提交应冲突")
	}

	upd := intentA()
	upd.Revision = 2
	upd.People = 3
	chk(a.SubmitIntent(roleOperator, "idem-42", upd))
	got := a.state.intents["A\x00M1"]
	if got.People != 3 || got.Revision != 2 {
		t.Fatalf("修订提交未覆盖：%+v", got)
	}
	intentEvents := 0
	for _, e := range store.Events() {
		if e.Type == evIntentReceived {
			intentEvents++
		}
	}
	if intentEvents != 2 {
		t.Fatalf("意向事件数 = %d，期望 2", intentEvents)
	}

	// 带相同幂等键重试：不再落事件，返回首次序号。
	r := get(a.SubmitIntent(roleOperator, "idem-42", upd))
	if !r.Replay {
		t.Fatal("幂等重试应被识别为重放")
	}
	intentEvents2 := 0
	for _, e := range store.Events() {
		if e.Type == evIntentReceived {
			intentEvents2++
		}
	}
	if intentEvents2 != 2 {
		t.Fatalf("幂等重试后意向事件数 = %d，期望仍为 2", intentEvents2)
	}
}

// 跨日行程按当地日期逐夜计需，25 入住 28 退房 = 25/26/27 三晚。
func TestCrossDayNights(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk := ok(t)
	chk(a.SubmitIntent(roleOperator, "", intentA()))

	rep, err := a.Gap("M1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lodging) != 3 {
		t.Fatalf("住宿夜数 = %d，期望 3：%+v", len(rep.Lodging), rep.Lodging)
	}
	for i, b := range rep.Lodging {
		want := []string{"2026-09-25", "2026-09-26", "2026-09-27"}[i]
		if b.Date != want || b.Demand != 2 || b.Gap != 0 {
			t.Fatalf("第 %d 夜统计错误：%+v", i, b)
		}
	}
}

// 推荐方案不超载：去程只有 3 座、两个团体各 2 人时，总分配不得超过容量，缺口显式列出。
func TestRecommendationNoOverload(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk := ok(t)
	chk(a.AdjustShuttle(roleTraffic, "", ShuttleCmd{ID: "S1", Cap: 3}))

	ia := intentA()
	ib := intentA()
	ib.Ref = "B"
	ib.ArriveAt = "2026-09-25T16:45:00"
	chk(a.SubmitIntent(roleOperator, "", ia))
	chk(a.SubmitIntent(roleOperator, "", ib))

	plan, err := a.Recommend("M1")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]int{}
	unmetSeats := 0
	for _, as := range plan.Assignments {
		for _, p := range as.Inbound {
			used[p.Shuttle] += p.Seats
		}
	}
	if used["S1"] != 3 {
		t.Fatalf("S1 分配 %d 座，期望 3（容量 3）", used["S1"])
	}
	for _, u := range plan.Unmet {
		if u.Segment == "去程接驳" {
			unmetSeats += u.People
		}
	}
	if unmetSeats != 1 {
		t.Fatalf("去程未满足 = %d，期望 1：%+v", unmetSeats, plan.Unmet)
	}
}

// 赛程改期导致接驳失配时，必须生成带原因与影响范围的改线建议，并可人工确认留痕。
func TestRescheduleCreatesProposal(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk, get := ok(t), val(t)
	chk(a.SubmitIntent(roleOperator, "", intentA()))

	before, _ := a.Recommend("M1")
	if len(before.Assignments[0].Inbound) == 0 {
		t.Fatal("前置条件错误：改期前应有去程班次")
	}

	// 改到下午：原去程班次 18:00 到达，晚于新开赛 15:00，无法承接。
	r := get(a.Reschedule(roleOperator, "rc-1", RescheduleCmd{
		MatchID: "M1", Start: "2026-09-25T15:00:00", End: "2026-09-25T17:00:00",
		Reason: "电视转播调整",
	}))
	if r.ProposalID == "" {
		t.Fatal("改期后应产生改线建议")
	}
	pr, err := a.ProposalView(r.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Category != reasonReschedule || pr.Status != "待确认" {
		t.Fatalf("改线建议属性错误：%+v", pr)
	}
	if pr.Reason != "电视转播调整" || pr.TriggerSeq == 0 {
		t.Fatalf("原因或触发事件序号缺失：%+v", pr)
	}
	if len(pr.Scope.Audience) != 1 || pr.Scope.Audience[0] != "A" ||
		len(pr.Scope.Shuttles) != 2 || pr.Scope.Matches[0] != "M1" {
		t.Fatalf("影响范围错误：%+v", pr.Scope)
	}
	if pr.Before == nil || len(pr.Before.Assignments[0].Inbound) == 0 {
		t.Fatal("原方案快照缺失")
	}
	if len(pr.Suggested.Unmet) == 0 {
		t.Fatal("建议方案应列出接驳缺口")
	}

	// 人工确认留痕；重复确认冲突。
	chk(a.DecideProposal(roleOperator, "", DecisionCmd{ProposalID: pr.ID, Decision: "确认", Opinion: "已加开备用班次后执行"}))
	pr2, _ := a.ProposalView(pr.ID)
	if pr2.Status != "已确认" || pr2.ConfirmedBy != roleOperator || pr2.ConfirmOpinion == "" {
		t.Fatalf("确认记录缺失：%+v", pr2)
	}
	if _, err := a.DecideProposal(roleOperator, "", DecisionCmd{ProposalID: pr.ID, Decision: "驳回"}); err == nil {
		t.Fatal("重复决定应冲突")
	}
}

// 场地临时关闭：观赛容量归零、产生关闭类改线建议；解除后缺口恢复。
func TestClosureFlow(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk, get := ok(t), val(t)
	chk(a.SubmitIntent(roleOperator, "", intentA()))

	r := get(a.CloseVenue(roleVenue, "", ClosureCmd{
		ID: "C1", Venue: "V1", Start: "2026-09-25T18:00:00", End: "2026-09-25T22:00:00",
		Reason: "设备抢修",
	}))
	if r.ProposalID == "" {
		t.Fatal("关闭与赛时重叠时应产生改线建议")
	}
	pr, _ := a.ProposalView(r.ProposalID)
	if pr.Category != reasonClosure {
		t.Fatalf("原因类别 = %s", pr.Category)
	}
	rep, _ := a.Gap("M1")
	for _, b := range rep.Attendance {
		if b.Supply != 0 || b.Gap != b.Demand {
			t.Fatalf("关闭时段观赛缺口错误：%+v", b)
		}
	}

	chk(a.EndClosure(roleVenue, "", "C1"))
	rep2, _ := a.Gap("M1")
	for _, b := range rep2.Attendance {
		if b.Supply != 850 || b.Gap != 0 {
			t.Fatalf("解除关闭后容量未恢复：%+v", b)
		}
	}
}

// 合作方迟报：晚于报送截止的容量快照须标记迟报，并对受影响场次产生改线建议。
func TestLateCapacityReport(t *testing.T) {
	b, _ := newTestApp(t, "2026-09-24T09:00:00")
	chk, get := ok(t), val(t)
	chk(b.RegisterVenue(roleVenue, "", RegisterVenueCmd{ID: "V1", Grade: "甲", BaseCap: 1000}))
	chk(b.SetPartnerStatus(roleLodging, "", PartnerStatusCmd{ID: "H9", Kind: kindLodging, Status: statusNormal, ReportBy: "2026-09-20"}))
	chk(b.ScheduleMatch(roleOperator, "", ScheduleMatchCmd{ID: "M1", Venue: "V1", Start: "2026-09-25T19:00:00", End: "2026-09-25T21:00:00", Expected: 10}))
	ci := intentA()
	ci.ArriveBus = false
	ci.LeaveBus = false
	ci.Routes = nil
	chk(b.SubmitIntent(roleOperator, "", ci))

	plan, _ := b.Recommend("M1")
	lodgingGap := false
	for _, u := range plan.Unmet {
		if u.Segment == "住宿" {
			lodgingGap = true
		}
	}
	if !lodgingGap {
		t.Fatal("迟报前应有住宿缺口")
	}
	r := get(b.ReportCapacity(roleLodging, "", CapacityCmd{Partner: "H9", Date: "2026-09-25", Beds: 10}))
	if r.ProposalID == "" {
		t.Fatal("迟报补齐容量后应产生改线建议")
	}
	pr, _ := b.ProposalView(r.ProposalID)
	if pr.Category != reasonLateReport {
		t.Fatalf("原因类别 = %s，期望 %s", pr.Category, reasonLateReport)
	}
	// 只报了 25 日夜；26/27 夜仍缺，建议方案中应保留剩余住宿缺口。
	stillGap := false
	for _, u := range pr.Suggested.Unmet {
		if u.Segment == "住宿" {
			stillGap = true
		}
	}
	if !stillGap {
		t.Fatal("其余夜未报送，建议方案应保留住宿缺口")
	}
	snap := b.state.caps[capKey("H9", "2026-09-25")]
	if !snap.Late {
		t.Fatal("容量快照应标记为迟报")
	}
}

// 合作方暂停接待同样触发改线，且保留暂停前后方案对照。
func TestPartnerSuspension(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk, get := ok(t), val(t)
	ci := intentA()
	ci.ArriveBus = false
	ci.LeaveBus = false
	ci.Routes = nil
	chk(a.SubmitIntent(roleOperator, "", ci))

	r := get(a.SetPartnerStatus(roleLodging, "", PartnerStatusCmd{
		ID: "H1", Name: "迎宾馆", Kind: kindLodging, Status: statusSuspended, ReportBy: "2026-09-20",
	}))
	if r.ProposalID == "" {
		t.Fatal("暂停接待应产生改线建议")
	}
	pr, _ := a.ProposalView(r.ProposalID)
	if pr.Before == nil || pr.Category != reasonPartner {
		t.Fatalf("暂停改线建议错误：%+v", pr)
	}
	rep, _ := a.Gap("M1")
	if rep.Lodging[0].Supply != 0 || len(rep.Lodging[0].Suspended) != 1 {
		t.Fatalf("暂停后容量应归零并列出：%+v", rep.Lodging[0])
	}
}

// 相同命令序列在两个独立存储上重放，事件哈希与编排结果必须逐字节一致。
func TestDeterministicReplay(t *testing.T) {
	clk := (&fakeClock{"2026-09-19T10:00:00"}).now
	build := func(t *testing.T) *App {
		st := newMemStore()
		a, err := NewApp(st, clk)
		if err != nil {
			t.Fatal(err)
		}
		seedScenario(t, a)
		ok := ok(t)
		ok(a.SubmitIntent(roleOperator, "", intentA()))
		return a
	}
	a1 := build(t)
	a2 := build(t)

	e1, _ := json.Marshal(a1.store.Events())
	e2, _ := json.Marshal(a2.store.Events())
	if !bytes.Equal(e1, e2) {
		t.Fatal("同一命令序列产生的事件流不一致")
	}
	p1, _ := a1.Recommend("M1")
	p2, _ := a2.Recommend("M1")
	g1, _ := a1.Gap("M1")
	g2, _ := a2.Gap("M1")
	j1, _ := json.Marshal([]any{p1, g1, a1.Proposals()})
	j2, _ := json.Marshal([]any{p2, g2, a2.Proposals()})
	if !bytes.Equal(j1, j2) {
		t.Fatal("同一事件序列重放后的方案/缺口/建议不一致")
	}
}

// 服务重启：文件事件日志重放后状态与重启前一致，哈希链通过校验。
func TestRestartFromFileLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eventlog.jsonl")
	chk, get := ok(t), val(t)

	clk := (&fakeClock{"2026-09-19T10:00:00"}).now
	fs, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewApp(fs, clk)
	if err != nil {
		t.Fatal(err)
	}
	seedScenario(t, a)
	chk(a.SubmitIntent(roleOperator, "", intentA()))
	get(a.Reschedule(roleOperator, "", RescheduleCmd{
		MatchID: "M1", Start: "2026-09-25T15:00:00", End: "2026-09-25T17:00:00",
	}))
	p1, _ := a.Recommend("M1")
	before, _ := json.Marshal([]any{a.Proposals(), p1})

	// 重新打开日志，模拟进程重启。
	fs2, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := NewApp(fs2, clk)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := a2.Recommend("M1")
	after, _ := json.Marshal([]any{a2.Proposals(), p2})
	if !bytes.Equal(before, after) {
		t.Fatal("重启重放后结果不一致")
	}
}

// 事件日志被篡改时，重放必须因哈希链校验失败而中止。
func TestTamperedLogRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eventlog.jsonl")
	fs, err := openFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewApp(fs, (&fakeClock{"2026-09-19T10:00:00"}).now)
	if err != nil {
		t.Fatal(err)
	}
	ok(t)(a.RegisterVenue(roleVenue, "", RegisterVenueCmd{ID: "V1", Grade: "甲", BaseCap: 100}))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	ev["type"] = "伪造事件"
	bad, _ := json.Marshal(ev)
	lines[0] = string(bad)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openFileStore(path); err == nil {
		t.Fatal("被篡改的事件日志应无法通过哈希链校验")
	}
}

// 鉴权与审计：无令牌 401、无作用域岗位 403、行程查询仅运营可查且必留审计事件。
func TestHTTPAuthAndAudit(t *testing.T) {
	a, _ := newTestApp(t, "2026-09-19T10:00:00")
	seedScenario(t, a)
	chk := ok(t)
	chk(a.SubmitIntent(roleOperator, "", intentA()))

	srv := httptest.NewServer(newMux(a, newAuthorizer(defaultTokens())))
	defer srv.Close()

	// 无令牌。
	resp, err := http.Get(srv.URL + "/v1/intents?match=M1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌状态 = %d，期望 401", resp.StatusCode)
	}
	resp.Body.Close()

	// 住宿合作方查询个人行程：越权 403。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/intents?match=M1", nil)
	req.Header.Set("Authorization", "Bearer lodging-2026")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("越权状态 = %d，期望 403", resp.StatusCode)
	}
	resp.Body.Close()

	// 运营查询成功。
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/intents?match=M1", nil)
	req.Header.Set("Authorization", "Bearer ops-2026")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("授权查询状态 = %d", resp.StatusCode)
	}
	var out struct {
		List []Intent `json:"行程意向"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.List) != 1 || out.List[0].Ref != "A" {
		t.Fatalf("行程返回错误：%+v", out.List)
	}

	// 审计事件已入日志。
	audits := 0
	for _, e := range a.store.Events() {
		if e.Type == evAccessAudited {
			audits++
		}
	}
	if audits != 1 {
		t.Fatalf("审计事件数 = %d，期望 1", audits)
	}

	// 交通岗位可读缺口，但不能改赛程。
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/matches/M1/gaps", nil)
	req.Header.Set("Authorization", "Bearer traffic-2026")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("协同岗位读缺口状态 = %d，期望 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 推荐方案含逐人编排，协同岗位不可读。
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/matches/M1/recommendation", nil)
	req.Header.Set("Authorization", "Bearer traffic-2026")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("协同岗位读推荐方案状态 = %d，期望 403", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/matches/M1/reschedule",
		bytes.NewReader([]byte(`{"新开始":"2026-09-25T15:00:00","新结束":"2026-09-25T17:00:00"}`)))
	req.Header.Set("Authorization", "Bearer traffic-2026")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("越权改期状态 = %d，期望 403", resp.StatusCode)
	}
	resp.Body.Close()
}
