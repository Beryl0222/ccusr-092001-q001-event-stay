package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/beryl0222/event-stay-orchestrator/domain"
	"github.com/beryl0222/event-stay-orchestrator/internal/store"
)

// fixture 构建一个带固定时钟与确定性 ID 的应用，预置基础资源。
type fixture struct {
	app   *App
	log   *store.EventLog
	clock *fixedClock
	dir   string
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	log, err := store.OpenEventLog(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &fixedClock{t: time.Date(2026, 9, 20, 10, 0, 0, 0, domain.Local)}
	app, err := New(log, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	// 确定性 ID：按序号递增。
	n := 0
	app.SetIDGenerator(func(prefix string) string {
		n++
		return prefix + itoaSafe(n)
	})
	f := &fixture{app: app, log: log, clock: clk, dir: dir}
	f.seed(t)
	return f
}

func itoaSafe(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (f *fixture) seed(t *testing.T) {
	venues := []domain.Venue{
		{ID: "v_a", Name: "甲馆", Tier: domain.TierA, Seats: 9000},
		{ID: "v_s", Name: "超级馆", Tier: domain.TierS, Seats: 18000},
		{ID: "v_b", Name: "乙馆", Tier: domain.TierB, Seats: 4000},
	}
	for _, v := range venues {
		if _, err := f.app.RegisterVenue("ops:tester", v); err != nil {
			t.Fatalf("登记场馆: %v", err)
		}
	}
	partners := []domain.Partner{
		{ID: "h1", Name: "酒店一", Kind: domain.PartnerHotel, Status: domain.PartnerActive},
		{ID: "h2", Name: "酒店二", Kind: domain.PartnerHotel, Status: domain.PartnerActive},
		{ID: "b1", Name: "接驳一", Kind: domain.PartnerShuttle, Status: domain.PartnerActive},
		{ID: "c1", Name: "文旅一", Kind: domain.PartnerAttraction, Status: domain.PartnerActive},
	}
	for _, p := range partners {
		if _, err := f.app.RegisterPartner("ops:tester", p); err != nil {
			t.Fatalf("登记合作方: %v", err)
		}
	}
	if _, err := f.app.ScheduleSession("ops:tester", ScheduleSessionInput{
		ID: "s1", Name: "揭幕战", VenueID: "v_a",
		Start: "2026-10-03T19:30:00+08:00", End: "2026-10-03T22:00:00+08:00",
	}); err != nil {
		t.Fatalf("发布赛程: %v", err)
	}
	runs := []domain.ParseRunInput{
		{ID: "rin1", PartnerID: "b1", VenueID: "v_a", Kind: "inbound",
			Depart: "2026-10-03T16:00:00+08:00", Arrive: "2026-10-03T17:00:00+08:00", Capacity: 40},
		{ID: "rout1", PartnerID: "b1", VenueID: "v_a", Kind: "outbound",
			Depart: "2026-10-03T22:30:00+08:00", Arrive: "2026-10-03T23:30:00+08:00", Capacity: 40},
	}
	for _, r := range runs {
		if _, err := f.app.ScheduleRun("transport:tester", r); err != nil {
			t.Fatalf("登记班次: %v", err)
		}
	}
	if _, err := f.app.RegisterRoute("culture:tester", domain.ThemeRoute{
		ID: "route1", Name: "江城夜游", PartnerID: "c1", DailyCapacity: 10,
	}); err != nil {
		t.Fatalf("登记线路: %v", err)
	}
	for _, night := range []string{"2026-10-02", "2026-10-03"} {
		if _, err := f.app.RecordSnapshot("h1", domain.SnapshotInput{
			PartnerID: "h1", Date: night, TotalRooms: 100, BookedRooms: 0,
		}); err != nil {
			t.Fatalf("报送容量: %v", err)
		}
	}
}

func baseIntent(token, key string) domain.IntentInput {
	return domain.IntentInput{
		VisitorToken: token, IdempotencyKey: key, SessionID: "s1", Pax: 2,
		ArriveFrom: "2026-10-02T14:00:00+08:00", ArriveTo: "2026-10-02T20:00:00+08:00",
		DepartFrom: "2026-10-03T22:00:00+08:00", DepartTo: "2026-10-04T01:00:00+08:00",
	}
}

// 幂等重放：相同键相同内容不产生新事件；同键不同内容被拒绝。
func TestSubmitIdempotency(t *testing.T) {
	f := newFixture(t)
	res1, err := f.app.SubmitIntent(baseIntent("alpha", "key-1"))
	if err != nil {
		t.Fatal(err)
	}
	seqAfterFirst := f.log.Seq()
	res2, err := f.app.SubmitIntent(baseIntent("alpha", "key-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Deduped {
		t.Fatal("相同键重放应幂等命中")
	}
	if f.log.Seq() != seqAfterFirst {
		t.Fatal("幂等重放不得追加事件")
	}
	_ = res1

	bad := baseIntent("alpha", "key-1")
	bad.Pax = 4
	if _, err := f.app.SubmitIntent(bad); err == nil {
		t.Fatal("同键不同内容应被拒绝")
	}
}

// 同一观众重复提交（新键）取代旧版本，旧版本保留且标记 superseded。
func TestSubmitReplaceSupersedes(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.SubmitIntent(baseIntent("alpha", "v1")); err != nil {
		t.Fatal(err)
	}
	in2 := baseIntent("alpha", "v2")
	in2.ArriveFrom = "2026-10-01T14:00:00+08:00" // 提前一天
	in2.ArriveTo = "2026-10-01T20:00:00+08:00"
	res, err := f.app.SubmitIntent(in2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced || res.Intent.Version != 2 {
		t.Fatalf("期望取代且版本 2，实际 replaced=%v version=%d", res.Replaced, res.Intent.Version)
	}
	got, err := f.app.Intent("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.IdempotencyKey != "v2" {
		t.Fatalf("当前意向应为 v2，实际 %s", got.IdempotencyKey)
	}
	vers := f.app.State().IntentVersions["alpha"]
	if !vers["v1"].Superseded {
		t.Fatal("v1 应保留并标记 superseded")
	}
}

// 赛程改期触发自动改线：原因、影响范围齐全，确认前状态为 proposed。
func TestMoveSessionCreatesReroute(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.SubmitIntent(baseIntent("alpha", "k1")); err != nil {
		t.Fatal(err)
	}
	// 先建立基线方案。
	if _, err := f.app.PlanSession("s1"); err != nil {
		t.Fatal(err)
	}
	rr, err := f.app.MoveSession("ops:tester", "s1", MoveSessionInput{
		VenueID: "v_s",
		Start:   "2026-10-04T19:30:00+08:00",
		End:     "2026-10-04T22:00:00+08:00",
		Reason:  "电视转播调整",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rr == nil {
		t.Fatal("改期应生成改线单")
	}
	if rr.Status != domain.RerouteProposed {
		t.Fatalf("期望 proposed，实际 %s", rr.Status)
	}
	if rr.Reason == "" || len(rr.Changes) == 0 {
		t.Fatal("改线原因与变更说明缺失")
	}
	if rr.Scope.VisitorCount != 1 {
		t.Fatalf("影响观众期望 1，实际 %d", rr.Scope.VisitorCount)
	}
	// 场次尚未换馆（确认前不动赛程）。
	if f.app.State().Sessions["s1"].VenueID != "v_a" {
		t.Fatal("人工确认前不得换馆")
	}
	if f.app.State().Sessions["s1"].Start.Day() != 3 {
		t.Fatal("人工确认前不得改期")
	}

	decided, err := f.app.DecideReroute("ops:chief", rr.ID, "confirmed", "按转播方案执行")
	if err != nil {
		t.Fatal(err)
	}
	if decided.Status != domain.RerouteConfirmed || decided.Decision == nil ||
		decided.Decision.By != "ops:chief" {
		t.Fatalf("确认记录不完整: %+v", decided)
	}
	sess := f.app.State().Sessions["s1"]
	if sess.VenueID != "v_s" || sess.Start.Day() != 4 {
		t.Fatalf("确认后赛程应已生效: %+v", sess)
	}
	// 重复确认应被拒绝。
	if _, err := f.app.DecideReroute("ops:chief", rr.ID, "confirmed", ""); err == nil {
		t.Fatal("已处理改线单不得重复确认")
	}
}

// 场地临时关闭：自动选择不低于原等级的替代场馆，确认后换馆。
func TestClosurePicksAlternative(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.SubmitIntent(baseIntent("alpha", "k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.PlanSession("s1"); err != nil {
		t.Fatal(err)
	}
	// 乙馆等级低于甲馆（B<A），不得入选；超级馆 S 应入选。
	reroutes, err := f.app.ScheduleClosure("venue:tester", ClosureInput{
		ID: "cl1", VenueID: "v_a",
		Start: "2026-10-03T18:00:00+08:00", End: "2026-10-03T23:59:00+08:00",
		Reason: "设备检修",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reroutes) != 1 {
		t.Fatalf("期望 1 张改线单，实际 %d", len(reroutes))
	}
	rr := reroutes[0]
	if rr.Target.VenueID != "v_s" {
		t.Fatalf("期望拟换超级馆，实际 %s", rr.Target.VenueID)
	}
	if _, err := f.app.DecideReroute("ops:chief", rr.ID, "confirmed", ""); err != nil {
		t.Fatal(err)
	}
	if f.app.State().Sessions["s1"].VenueID != "v_s" {
		t.Fatal("确认闭馆改线后应换至超级馆")
	}
}

// 合作方迟报：晚于时限返回 late=true 并触发复核；二次报送被拒。
func TestLateSnapshotTriggersReview(t *testing.T) {
	f := newFixture(t)
	// alpha 提前到 10-01：10-01 房夜 h1 未报送，基线方案存在住宿缺口。
	in := baseIntent("alpha", "k1")
	in.ArriveFrom = "2026-10-01T14:00:00+08:00"
	in.ArriveTo = "2026-10-01T20:00:00+08:00"
	if _, err := f.app.SubmitIntent(in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.PlanSession("s1"); err != nil {
		t.Fatal(err)
	}
	// 10-01 房夜时限为 09-30 18:00；拨到 19:00 报送即迟报，且补齐缺口改变方案。
	f.clock.t = time.Date(2026, 9, 30, 19, 0, 0, 0, domain.Local)
	snap, err := f.app.RecordSnapshot("h2", domain.SnapshotInput{
		PartnerID: "h2", Date: "2026-10-01", TotalRooms: 50, BookedRooms: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Late {
		t.Fatal("09-30 19:00 报送 10-01 房夜应判迟报")
	}
	if n := len(f.app.ReroutesOf("s1")); n != 1 {
		t.Fatalf("迟报应触发 1 张改线单，实际 %d", n)
	}
	if _, err := f.app.RecordSnapshot("h2", domain.SnapshotInput{
		PartnerID: "h2", Date: "2026-10-01", TotalRooms: 60,
	}); err == nil {
		t.Fatal("同房夜重复报送应被拒绝")
	}
}

// 推荐方案不超载：酒店容量不足时产生缺口而非超分。
func TestPlanRespectsCapacity(t *testing.T) {
	f := newFixture(t)
	// 40 人团队，酒店 10-02 夜仅报 100 间空房，但接驳 rin1 仅 40 座——恰好；
	// 再叠加第二个 40 人团队，接驳必然缺口，且不得让任一班次超载。
	if _, err := f.app.SubmitIntent(func() domain.IntentInput {
		in := baseIntent("g1", "k1")
		in.Pax = 40
		return in
	}()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SubmitIntent(func() domain.IntentInput {
		in := baseIntent("g2", "k2")
		in.Pax = 40
		return in
	}()); err != nil {
		t.Fatal(err)
	}
	p, err := f.app.PlanSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	load := map[string]int{}
	for _, it := range p.Items {
		if it.Kind == domain.AssignShuttleIn {
			load[it.Reference] += it.Pax
		}
	}
	if load["rin1"] > 40 {
		t.Fatalf("rin1 超载: %d", load["rin1"])
	}
	if len(p.Unmet) == 0 {
		t.Fatal("容量不足时应存在未满足项")
	}
}

// 相同事件序列重放 / 重启后，方案摘要与状态完全一致。
func TestReplayDeterminism(t *testing.T) {
	f := newFixture(t)
	if _, err := f.app.SubmitIntent(baseIntent("alpha", "k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.MoveSession("ops:tester", "s1", MoveSessionInput{
		Start: "2026-10-04T19:30:00+08:00", End: "2026-10-04T22:00:00+08:00",
		Reason: "转播调整",
	}); err != nil {
		t.Fatal(err)
	}
	seq := f.log.Seq()

	// 用同一日志文件重新打开应用（模拟重启），时钟不同也不影响历史结果。
	log2, err := store.OpenEventLog(filepath.Join(f.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	otherClock := func() time.Time {
		return time.Date(2030, 1, 1, 0, 0, 0, 0, domain.Local)
	}
	app2, err := New(log2, otherClock)
	if err != nil {
		t.Fatal(err)
	}
	if log2.Seq() != seq {
		t.Fatalf("重启后序号期望 %d，实际 %d", seq, log2.Seq())
	}
	p1, err := f.app.Proposal("s1")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := app2.Proposal("s1")
	if err != nil {
		t.Fatal(err)
	}
	if p1.Digest != p2.Digest {
		t.Fatalf("重启后方案摘要不一致: %s vs %s", p1.Digest, p2.Digest)
	}
	if len(app2.ReroutesOf("s1")) != len(f.app.ReroutesOf("s1")) {
		t.Fatal("重启后改线单数量不一致")
	}
}

// 缺口接口可直接回答某场次的接待缺口。
func TestGapReportEndToEnd(t *testing.T) {
	f := newFixture(t)
	in := baseIntent("alpha", "k1")
	in.Pax = 50 // rin1 仅 40 座，产生 10 人抵程缺口
	if _, err := f.app.SubmitIntent(in); err != nil {
		t.Fatal(err)
	}
	rep, err := f.app.GapReport("s1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, seg := range rep.Segments {
		if seg.Kind == domain.AssignShuttleIn && seg.Gap >= 10 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报告抵程接驳缺口 >=10: %+v", rep.Segments)
	}
}
