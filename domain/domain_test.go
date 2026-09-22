package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.In(Local)
}

// raw 将事件载荷序列化为 RawMessage。
func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// 交通时窗：06:00 首班、23:30 末班发车、跨日到达次日 01:30 截止。
func TestTrafficWindow(t *testing.T) {
	cases := []struct {
		name   string
		run    ShuttleRun
		expect bool
	}{
		{"日间班次", ShuttleRun{Depart: ts("2026-10-03T10:00:00+08:00"), Arrive: ts("2026-10-03T11:00:00+08:00")}, true},
		{"首班准点", ShuttleRun{Depart: ts("2026-10-03T06:00:00+08:00"), Arrive: ts("2026-10-03T07:00:00+08:00")}, true},
		{"早于首班", ShuttleRun{Depart: ts("2026-10-03T05:30:00+08:00"), Arrive: ts("2026-10-03T06:30:00+08:00")}, false},
		{"末班准点发车跨日", ShuttleRun{Depart: ts("2026-10-03T23:30:00+08:00"), Arrive: ts("2026-10-04T00:30:00+08:00")}, true},
		{"晚于末班发车", ShuttleRun{Depart: ts("2026-10-03T23:45:00+08:00"), Arrive: ts("2026-10-04T00:45:00+08:00")}, false},
		{"跨日超过到达截止", ShuttleRun{Depart: ts("2026-10-03T23:00:00+08:00"), Arrive: ts("2026-10-04T02:00:00+08:00")}, false},
		{"到达早于发车", ShuttleRun{Depart: ts("2026-10-03T10:00:00+08:00"), Arrive: ts("2026-10-03T09:00:00+08:00")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsWithinTrafficWindow(&c.run); got != c.expect {
				t.Fatalf("期望 %v，实际 %v", c.expect, got)
			}
		})
	}
}

func TestSnapshotDueAndLate(t *testing.T) {
	// 入住日 10-03 的报送时限为 10-02 18:00。
	due, err := SnapshotDue("2026-10-03")
	if err != nil {
		t.Fatal(err)
	}
	want := ts("2026-10-02T18:00:00+08:00")
	if !due.Equal(want) {
		t.Fatalf("时限期望 %v，实际 %v", want, due)
	}
	snap, err := ParseSnapshot(SnapshotInput{PartnerID: "h1", Date: "2026-10-03", TotalRooms: 10},
		ts("2026-10-02T19:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Late {
		t.Fatal("19:00 报送应判定为迟报")
	}
	snap2, _ := ParseSnapshot(SnapshotInput{PartnerID: "h1", Date: "2026-10-03", TotalRooms: 10},
		ts("2026-10-02T17:59:00+08:00"))
	if snap2.Late {
		t.Fatal("17:59 报送不应判定为迟报")
	}
}

// buildPlannedState 构造含两场两馆、酒店/接驳/线路资源的基础状态并放入意向。
func buildPlannedState(t *testing.T) (*State, SessionTarget) {
	t.Helper()
	s := NewState()
	mustApply := func(e Event) {
		if err := s.Apply(e); err != nil {
			t.Fatalf("折演失败: %v", err)
		}
	}
	venueA := Venue{ID: "v_a", Name: "甲馆", Tier: TierA, Seats: 9000}
	venueS := Venue{ID: "v_s", Name: "超级馆", Tier: TierS, Seats: 18000}
	mustApply(Event{Seq: 1, Type: EvVenueRegistered, Payload: raw(VenueRegistered{Venue: venueA})})
	mustApply(Event{Seq: 2, Type: EvVenueRegistered, Payload: raw(VenueRegistered{Venue: venueS})})
	mustApply(Event{Seq: 3, Type: EvPartnerRegistered, Payload: raw(PartnerRegistered{Partner: Partner{ID: "h1", Name: "酒店一", Kind: PartnerHotel, Status: PartnerActive}})})
	mustApply(Event{Seq: 4, Type: EvPartnerRegistered, Payload: raw(PartnerRegistered{Partner: Partner{ID: "b1", Name: "接驳一", Kind: PartnerShuttle, Status: PartnerActive}})})
	mustApply(Event{Seq: 5, Type: EvPartnerRegistered, Payload: raw(PartnerRegistered{Partner: Partner{ID: "c1", Name: "文旅一", Kind: PartnerAttraction, Status: PartnerActive}})})

	start, end := ts("2026-10-03T19:30:00+08:00"), ts("2026-10-03T22:00:00+08:00")
	mustApply(Event{Seq: 6, Type: EvSessionScheduled, Payload: raw(SessionScheduled{Session: Session{ID: "s1", VenueID: "v_a", Start: start, End: end}})})

	runs := []ShuttleRun{
		{ID: "rin0", PartnerID: "b1", VenueID: "v_a", Kind: RunInbound, Depart: ts("2026-10-01T16:00:00+08:00"), Arrive: ts("2026-10-01T17:00:00+08:00"), Capacity: 40, Active: true},
		{ID: "rin1", PartnerID: "b1", VenueID: "v_a", Kind: RunInbound, Depart: ts("2026-10-03T16:00:00+08:00"), Arrive: ts("2026-10-03T17:00:00+08:00"), Capacity: 40, Active: true},
		{ID: "rin2", PartnerID: "b1", VenueID: "v_a", Kind: RunInbound, Depart: ts("2026-10-03T18:00:00+08:00"), Arrive: ts("2026-10-03T19:00:00+08:00"), Capacity: 1, Active: true},
		{ID: "rout1", PartnerID: "b1", VenueID: "v_a", Kind: RunOutbound, Depart: ts("2026-10-03T22:30:00+08:00"), Arrive: ts("2026-10-03T23:30:00+08:00"), Capacity: 40, Active: true},
		{ID: "rout2", PartnerID: "b1", VenueID: "v_a", Kind: RunOutbound, Depart: ts("2026-10-03T23:30:00+08:00"), Arrive: ts("2026-10-04T00:30:00+08:00"), Capacity: 40, Active: true},
	}
	seq := int64(7)
	for _, r := range runs {
		mustApply(Event{Seq: seq, Type: EvRunScheduled, Payload: raw(RunScheduled{Run: r})})
		seq++
	}
	// 房夜容量：仅 2 间房，制造不超载场景。
	for _, night := range []string{"2026-10-01", "2026-10-02", "2026-10-03"} {
		mustApply(Event{Seq: seq, Type: EvSnapshotRecorded, Payload: raw(SnapshotRecorded{Snapshot: HotelSnapshot{
			PartnerID: "h1", Date: night, TotalRooms: 2, BookedRooms: 0,
		}})})
		seq++
	}
	mustApply(Event{Seq: seq, Type: EvRouteRegistered, Payload: raw(RouteRegistered{Route: ThemeRoute{ID: "route1", Name: "江城夜游", PartnerID: "c1", DailyCapacity: 1}})})
	seq++

	// 观众 alpha 提前两天到（10-01），bravo 比赛日当天到。
	intents := []Intent{
		{VisitorToken: "alpha", IdempotencyKey: "k1", SessionID: "s1", Pax: 2,
			ArriveFrom: ts("2026-10-01T15:00:00+08:00"), ArriveTo: ts("2026-10-01T20:00:00+08:00"),
			DepartFrom: ts("2026-10-03T22:00:00+08:00"), DepartTo: ts("2026-10-04T01:00:00+08:00")},
		{VisitorToken: "bravo", IdempotencyKey: "k2", SessionID: "s1", Pax: 2,
			ArriveFrom: ts("2026-10-03T16:30:00+08:00"), ArriveTo: ts("2026-10-03T18:30:00+08:00"),
			DepartFrom: ts("2026-10-03T22:00:00+08:00"), DepartTo: ts("2026-10-04T01:00:00+08:00")},
	}
	for _, in := range intents {
		mustApply(Event{Seq: seq, Type: EvIntentSubmitted, Payload: raw(IntentSubmitted{Intent: in})})
		seq++
	}
	return s, SessionTarget{VenueID: "v_a", Start: start, End: end}
}

func countKind(items []Assignment, kind AssignmentKind) int {
	n := 0
	for _, it := range items {
		if it.Kind == kind {
			n++
		}
	}
	return n
}

func unmetKinds(p *Proposal) map[AssignmentKind]int {
	m := map[AssignmentKind]int{}
	for _, u := range p.Unmet {
		m[u.Kind]++
	}
	return m
}

// 不超载编排：rin2 仅 1 座，bravo 2 人不能挤入；线路每日 1 槽，alpha 的出游日只一人有位。
func TestPlanNoOverload(t *testing.T) {
	s, target := buildPlannedState(t)
	p, err := Plan(s, "s1", target, ts("2026-10-01T12:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	// 接驳：alpha 乘 rin1（40座），bravo 当天到，rin2 只有 1 座不能整团乘，rin1 在其抵城前发车，
	// 故 bravo 抵程无可用班次 → 1 条抵程缺口。
	if countKind(p.Items, AssignShuttleIn) != 1 {
		t.Fatalf("期望 1 条抵程占位，实际 %d（%+v）", countKind(p.Items, AssignShuttleIn), p.Items)
	}
	if unmetKinds(p)[AssignShuttleIn] != 1 {
		t.Fatalf("期望 1 条抵程缺口，实际 %+v", p.Unmet)
	}
	// 返程两团均乘 rout1（22:30，40 座足够）。
	if countKind(p.Items, AssignShuttleOut) != 2 {
		t.Fatalf("期望 2 条返程占位，实际 %d", countKind(p.Items, AssignShuttleOut))
	}
	// 酒店：alpha 住 10-01/02/03 三夜；bravo 当天到不住宿。每晚 2 间房足够 1 间需求。
	hotelNights := map[string]int{}
	for _, it := range p.Items {
		if it.Kind == AssignHotel {
			hotelNights[it.Date]++
		}
	}
	if len(hotelNights) != 3 {
		t.Fatalf("期望 3 个房夜占位，实际 %v", hotelNights)
	}
	// 线路：alpha 出游日为 10-02，仅 1 槽可入；bravo 无整日在城内，不产生线路需求。
	if countKind(p.Items, AssignRoute) != 1 {
		t.Fatalf("期望 1 条线路占位，实际 %d", countKind(p.Items, AssignRoute))
	}
	if p.Digest == "" {
		t.Fatal("方案摘要缺失")
	}
}

// 相同输入必须得到相同方案摘要（确定性）。
func TestPlanDeterministicDigest(t *testing.T) {
	s1, tgt1 := buildPlannedState(t)
	s2, tgt2 := buildPlannedState(t)
	at := ts("2026-10-01T12:00:00+08:00")
	p1, err := Plan(s1, "s1", tgt1, at)
	if err != nil {
		t.Fatal(err)
	}
	// 生成时刻不同也不得影响摘要。
	p2, err := Plan(s2, "s1", tgt2, ts("2026-10-02T08:30:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if p1.Digest != p2.Digest {
		t.Fatalf("相同输入摘要不一致: %s vs %s", p1.Digest, p2.Digest)
	}
}

// 闭馆期间全员座席缺口。
func TestPlanClosedVenue(t *testing.T) {
	s, target := buildPlannedState(t)
	if err := s.Apply(Event{Seq: 99, Type: EvClosureScheduled, Payload: raw(ClosureScheduled{
		Closure: Closure{ID: "cl1", VenueID: "v_a", Start: ts("2026-10-03T18:00:00+08:00"),
			End: ts("2026-10-03T23:00:00+08:00"), Reason: "设备检修"},
	})}); err != nil {
		t.Fatal(err)
	}
	p, err := Plan(s, "s1", target, ts("2026-10-01T12:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if unmetKinds(p)["venue"] != 2 {
		t.Fatalf("期望闭馆产生 2 条座席缺口，实际 %+v", p.Unmet)
	}
}

// 缺口报告：未报房夜容量标记 missing；暂停合作方不参与供给。
func TestGapReportStatuses(t *testing.T) {
	s, _ := buildPlannedState(t)
	// 新观众 10-05 才走，而 10-04 房夜无人报送。
	in := Intent{VisitorToken: "charlie", IdempotencyKey: "k3", SessionID: "s1", Pax: 1,
		ArriveFrom: ts("2026-10-01T15:00:00+08:00"), ArriveTo: ts("2026-10-01T20:00:00+08:00"),
		DepartFrom: ts("2026-10-05T08:00:00+08:00"), DepartTo: ts("2026-10-05T12:00:00+08:00")}
	if err := s.Apply(Event{Seq: 100, Type: EvIntentSubmitted, Payload: raw(IntentSubmitted{Intent: in})}); err != nil {
		t.Fatal(err)
	}
	rep, err := GapReportFor(s, "s1", ts("2026-10-01T12:00:00+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	var sawMissing bool
	for _, seg := range rep.Segments {
		if seg.Kind == AssignHotel && seg.Date == "2026-10-04" && seg.DataStatus == "missing" && seg.Gap == 1 {
			sawMissing = true
		}
	}
	if !sawMissing {
		t.Fatalf("期望 10-04 房夜标记 missing 且缺口 1，实际 %+v", rep.Segments)
	}
	if rep.TotalGap <= 0 {
		t.Fatal("总缺口应为正")
	}
}

// 跨日房夜与出游日切分按本地时区。
func TestStayNightsAndActivityDays(t *testing.T) {
	in := &Intent{
		ArriveFrom: ts("2026-10-01T23:30:00+08:00"), ArriveTo: ts("2026-10-02T00:30:00+08:00"),
		DepartFrom: ts("2026-10-05T22:00:00+08:00"), DepartTo: ts("2026-10-06T01:00:00+08:00"),
	}
	nights := stayNights(in)
	want := []string{"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05"}
	if len(nights) != len(want) {
		t.Fatalf("房夜期望 %v，实际 %v", want, nights)
	}
	for i := range want {
		if nights[i] != want[i] {
			t.Fatalf("房夜期望 %v，实际 %v", want, nights)
		}
	}
	days := activityDays(in, "2026-10-03")
	// 抵达当日 10-01、比赛日 10-03、离开日 10-06 均不安排；10-02、10-04、10-05 可安排。
	wantDays := []string{"2026-10-02", "2026-10-04", "2026-10-05"}
	if len(days) != len(wantDays) {
		t.Fatalf("出游日期望 %v，实际 %v", wantDays, days)
	}
	for i := range wantDays {
		if days[i] != wantDays[i] {
			t.Fatalf("出游日期望 %v，实际 %v", wantDays, days)
		}
	}
}

// 承载等级校验：B 级馆不得登记 9000 座。
func TestValidateVenueTier(t *testing.T) {
	if _, err := ValidateVenue(Venue{ID: "v", Tier: TierB, Seats: 9000}); err == nil {
		t.Fatal("B 级馆 9000 座应被拒绝")
	}
	v, err := ValidateVenue(Venue{ID: "v", Tier: TierB})
	if err != nil {
		t.Fatal(err)
	}
	if v.Seats != TierSeats[TierB] {
		t.Fatalf("默认座席期望 %d，实际 %d", TierSeats[TierB], v.Seats)
	}
}
