package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// TrafficWindow 交通时窗约定（本地时间）：
// 首班 06:00；末班发车 23:30；跨日班次到达截止次日 01:30。
func TrafficWindow(onDate time.Time) (first, lastDepart, arriveDeadline time.Time) {
	d := onDate.In(Local)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, Local)
	first = day.Add(FirstRunHour*time.Hour + FirstRunMin*time.Minute)
	lastDepart = day.Add(LastDepartHour*time.Hour + LastDepartMin*time.Minute)
	arriveDeadline = day.Add(24*time.Hour + OvernightArriveHour*time.Hour + OvernightArriveMin*time.Minute)
	return
}

// IsWithinTrafficWindow 校验班次是否落在交通时窗内（含跨日到达截止）。
func IsWithinTrafficWindow(r *ShuttleRun) bool {
	depart := r.Depart.In(Local)
	first, last, arriveDeadline := TrafficWindow(depart)
	if depart.Before(first) || depart.After(last) {
		return false
	}
	if r.Arrive.Before(r.Depart) {
		return false // 到达不得早于发车
	}
	// 跨日班次必须在次日 01:30 前到达。
	return !r.Arrive.In(Local).After(arriveDeadline)
}

// ----------------------------------------------------------------------------
// 接待缺口报告
// ----------------------------------------------------------------------------

// GapSegment 描述某时段/某资源的接待缺口。
type GapSegment struct {
	Kind        AssignmentKind `json:"kind"`
	Reference   string         `json:"reference,omitempty"` // 班次/线路/酒店 ID
	Name        string         `json:"name,omitempty"`
	WindowStart time.Time      `json:"window_start,omitempty"`
	WindowEnd   time.Time      `json:"window_end,omitempty"`
	Date        string         `json:"date,omitempty"`
	Demand      int            `json:"demand"`
	Supply      int            `json:"supply"`
	Gap         int            `json:"gap"`
	// DataStatus: ok 正常；missing 合作方未报容量；late 合作方迟报。
	DataStatus string `json:"data_status"`
}

// GapReport 场次各时段接待缺口。
type GapReport struct {
	SessionID   string       `json:"session_id"`
	VenueID     string       `json:"venue_id"`
	GeneratedAt time.Time    `json:"generated_at"`
	Segments    []GapSegment `json:"segments"`
	TotalGap    int          `json:"total_gap"`
}

// hourFloor 截断到本地整点。
func hourFloor(t time.Time) time.Time {
	t = t.In(Local)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, Local)
}

// hourlyBuckets 在 [from,to] 内生成整点边界（含两端整点）。
func hourlyBounds(from, to time.Time) []time.Time {
	start := hourFloor(from)
	end := hourFloor(to)
	var out []time.Time
	for cur := start; !cur.After(end); cur = cur.Add(time.Hour) {
		out = append(out, cur)
	}
	return out
}

// GapReportFor 计算场次在当前排期下各时段的接待缺口（纯函数，无副作用）。
// 累计口径：每小时边界统计“届时必须到达/期望已离开”的人数与可提供的座位数。
func GapReportFor(s *State, sessionID string, at time.Time) (*GapReport, error) {
	sess, ok := s.Sessions[sessionID]
	if !ok {
		return nil, &NotFoundError{What: "场次", ID: sessionID}
	}
	intents := s.ActiveIntents(sessionID)
	report := &GapReport{SessionID: sessionID, VenueID: sess.VenueID, GeneratedAt: at.In(Local)}

	// 1) 场馆座席缺口。
	totalPax := 0
	for _, in := range intents {
		totalPax += in.Pax
	}
	venue := s.Venues[sess.VenueID]
	seats := 0
	if venue != nil {
		seats = venue.Seats
	}
	if closed, reason := s.VenueClosedAt(sess.VenueID, sess.Start); closed {
		report.Segments = append(report.Segments, GapSegment{
			Kind: "venue", Reference: sess.VenueID, Name: venueName(venue),
			WindowStart: sess.Start, WindowEnd: sess.End,
			Demand: totalPax, Supply: 0, Gap: totalPax,
			DataStatus: "closed:" + reason,
		})
	} else if totalPax > 0 {
		report.Segments = append(report.Segments, GapSegment{
			Kind: "venue", Reference: sess.VenueID, Name: venueName(venue),
			WindowStart: sess.Start, WindowEnd: sess.End,
			Demand: totalPax, Supply: seats, Gap: maxInt(0, totalPax-seats),
			DataStatus: "ok",
		})
	}

	// 2) 抵程接驳：按小时累计。
	report.Segments = append(report.Segments, shuttleGaps(s, sess, intents, RunInbound)...)
	// 3) 返程接驳：按小时累计。
	report.Segments = append(report.Segments, shuttleGaps(s, sess, intents, RunOutbound)...)
	// 4) 住宿：按房夜汇总。
	report.Segments = append(report.Segments, hotelGaps(s, intents)...)
	// 5) 主题线路：按日出游槽位。
	report.Segments = append(report.Segments, routeGaps(s, sess, intents)...)

	// 总缺口口径：场馆按 1 段；接驳同方向逐小时累计取峰值（避免同一缺口跨小时重复）；
	// 酒店与线路每房夜/出游日各一段，直接累加。
	peak := map[AssignmentKind]int{}
	for _, seg := range report.Segments {
		switch seg.Kind {
		case AssignShuttleIn, AssignShuttleOut:
			if seg.Gap > peak[seg.Kind] {
				peak[seg.Kind] = seg.Gap
			}
		default:
			report.TotalGap += seg.Gap
		}
	}
	report.TotalGap += peak[AssignShuttleIn] + peak[AssignShuttleOut]
	return report, nil
}

func venueName(v *Venue) string {
	if v == nil {
		return ""
	}
	return v.Name
}

func shuttleGaps(s *State, sess *Session, intents []*Intent, kind RunKind) []GapSegment {
	var runs []*ShuttleRun
	for _, r := range s.ActiveRuns(sess.VenueID) {
		if r.Kind != kind || !s.PartnerActive(r.PartnerID) {
			continue
		}
		runs = append(runs, r)
	}
	if len(runs) == 0 && len(intents) == 0 {
		return nil
	}
	var from, to time.Time
	for i, in := range intents {
		var f, t time.Time
		if kind == RunInbound {
			f, t = in.ArriveFrom, in.ArriveTo
		} else {
			f, t = in.DepartFrom, in.DepartTo
		}
		if i == 0 || f.Before(from) {
			from = f
		}
		if i == 0 || t.After(to) {
			to = t
		}
	}
	if len(intents) == 0 {
		return nil
	}
	var segs []GapSegment
	var lastDemand, lastSupply int
	for _, bound := range hourlyBounds(from, to) {
		demand := 0
		for _, in := range intents {
			if kind == RunInbound && !in.ArriveTo.After(bound) {
				demand += in.Pax
			}
			if kind == RunOutbound && !in.DepartTo.After(bound) {
				demand += in.Pax
			}
		}
		supply := 0
		for _, r := range runs {
			if kind == RunInbound && !r.Arrive.After(bound) {
				supply += r.Capacity
			}
			if kind == RunOutbound && !r.Depart.After(bound) {
				supply += r.Capacity
			}
		}
		// 累计口径下供需均未变化的相邻小时段不重复列出。
		if demand == lastDemand && supply == lastSupply {
			continue
		}
		lastDemand, lastSupply = demand, supply
		if demand == 0 && supply == 0 {
			continue
		}
		segKind := AssignShuttleIn
		if kind == RunOutbound {
			segKind = AssignShuttleOut
		}
		segs = append(segs, GapSegment{
			Kind: segKind, WindowStart: bound.Add(-time.Hour), WindowEnd: bound,
			Demand: demand, Supply: supply, Gap: maxInt(0, demand-supply), DataStatus: "ok",
		})
	}
	return segs
}

// stayNights 返回意向覆盖的房夜（本地日期，左闭右开：到达夜住、离开日不住）。
func stayNights(in *Intent) []string {
	cur := hourDate(in.ArriveFrom)
	end := hourDate(in.DepartTo)
	var nights []string
	for cur != end {
		nights = append(nights, cur)
		d, _ := time.ParseInLocation("2006-01-02", cur, Local)
		cur = d.AddDate(0, 0, 1).Format("2006-01-02")
		if len(nights) > 64 { // 防御性上限
			break
		}
	}
	return nights
}

func hourDate(t time.Time) string { return t.In(Local).Format("2006-01-02") }

func hotelGaps(s *State, intents []*Intent) []GapSegment {
	demand := map[string]int{} // date -> 房间需求
	for _, in := range intents {
		rooms := (in.Pax + 1) / 2 // 每间房最多 2 人
		for _, n := range stayNights(in) {
			demand[n] += rooms
		}
	}
	dates := make([]string, 0, len(demand))
	for d := range demand {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	var segs []GapSegment
	for _, d := range dates {
		supply, late := 0, false
		reported := false
		for _, p := range sortedPartners(s, PartnerHotel) {
			snap, ok := s.Snapshots[p.ID+"|"+d]
			if !ok {
				continue
			}
			reported = true
			supply += maxInt(0, snap.TotalRooms-snap.BookedRooms)
			if snap.Late {
				late = true
			}
		}
		status := "ok"
		gap := maxInt(0, demand[d]-supply)
		if !reported {
			status = "missing" // 合作方未报该房夜容量
			gap = demand[d]
		} else if late {
			status = "late" // 存在迟报，容量口径可能继续变化
		}
		segs = append(segs, GapSegment{
			Kind: AssignHotel, Date: d, Demand: demand[d], Supply: supply,
			Gap: gap, DataStatus: status,
		})
	}
	return segs
}

func routeGaps(s *State, sess *Session, intents []*Intent) []GapSegment {
	eventDate := hourDate(sess.Start)
	demand := map[string]int{} // 出游日 -> 团队数（每团 1 个槽位）
	for _, in := range intents {
		for _, d := range activityDays(in, eventDate) {
			demand[d]++
		}
	}
	dates := make([]string, 0, len(demand))
	for d := range demand {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	var segs []GapSegment
	for _, d := range dates {
		supply := 0
		for _, r := range sortedRoutes(s) {
			if !s.PartnerActive(r.PartnerID) {
				continue
			}
			if cap, ok := s.RouteCapacity(r.ID, d); ok {
				supply += cap
			}
		}
		segs = append(segs, GapSegment{
			Kind: AssignRoute, Date: d, Demand: demand[d], Supply: supply,
			Gap: maxInt(0, demand[d]-supply), DataStatus: "ok",
		})
	}
	return segs
}

// activityDays 返回意向中可安排主题线路的出游日：提前到达日与比赛日之间、
// 比赛日与返程日之间的整日（不含抵达当日、比赛当日与返程当日）。
func activityDays(in *Intent, eventDate string) []string {
	arrive := hourDate(in.ArriveFrom)
	depart := hourDate(in.DepartTo)
	var days []string
	cur := arrive
	for i := 0; i < 64; i++ {
		d, _ := time.ParseInLocation("2006-01-02", cur, Local)
		cur = d.AddDate(0, 0, 1).Format("2006-01-02")
		if cur >= depart {
			break
		}
		if cur != eventDate {
			days = append(days, cur)
		}
	}
	return days
}

func sortedPartners(s *State, kind PartnerKind) []*Partner {
	var out []*Partner
	for _, p := range s.Partners {
		if p.Kind == kind && p.Status == PartnerActive {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedRoutes(s *State) []*ThemeRoute {
	var out []*ThemeRoute
	for _, r := range s.Routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ----------------------------------------------------------------------------
// 不超载编排
// ----------------------------------------------------------------------------

// usage 在一次编排内累计各资源占用，保证任何分配都不超过容量。
type usage struct {
	runSeats map[string]int
	rooms    map[string]int // partnerID|date
	slots    map[string]int // routeID|date
}

// Plan 依据给定目标窗口为场次生成不超载的推荐方案（纯函数）。
// target 通常取自场次当前排期；改线评估时传入候选窗口。
// 观众按访客令牌排序处理，所有资源平局均按 ID 决断，因此结果确定。
func Plan(s *State, sessionID string, target SessionTarget, at time.Time) (*Proposal, error) {
	sess, ok := s.Sessions[sessionID]
	if !ok {
		return nil, &NotFoundError{What: "场次", ID: sessionID}
	}
	intents := s.ActiveIntents(sessionID)
	u := &usage{
		runSeats: map[string]int{},
		rooms:    map[string]int{},
		slots:    map[string]int{},
	}
	proposal := &Proposal{SessionID: sessionID, Target: target, GeneratedAt: at.In(Local)}

	// 场馆座席占用：按观众顺序计入，超出部分记缺口。
	venue := s.Venues[target.VenueID]
	venueSeats := 0
	if venue != nil {
		venueSeats = venue.Seats
	}
	seated := 0
	if closed, reason := s.VenueClosedAt(target.VenueID, target.Start); closed {
		for _, in := range intents {
			proposal.Unmet = append(proposal.Unmet, UnmetNeed{
				VisitorToken: in.VisitorToken, Pax: in.Pax, Kind: "venue",
				Detail: "场馆临时关闭: " + reason,
			})
		}
	} else {
		for _, in := range intents {
			take := minInt(venueSeats-seated, in.Pax) // 该团队能入座的人数
			if take < 0 {
				take = 0
			}
			seated += take
			if in.Pax-take > 0 {
				proposal.Unmet = append(proposal.Unmet, UnmetNeed{
					VisitorToken: in.VisitorToken, Pax: in.Pax - take, Kind: "venue",
					Detail: "超出场馆承载",
				})
			}
		}
	}

	inbound := feasibleRuns(s, target, RunInbound)
	outbound := feasibleRuns(s, target, RunOutbound)

	prevHotel := map[string]string{} // 访客上一夜入住的酒店，优先续住
	for _, in := range intents {
		token := in.VisitorToken

		// 抵程接驳。
		if !assignShuttle(s, proposal, u, in, inbound, true, sess, target) {
			proposal.Unmet = append(proposal.Unmet, UnmetNeed{
				VisitorToken: token, Pax: in.Pax, Kind: AssignShuttleIn,
				Detail: "交通时窗内无可用抵程班次",
			})
		}
		// 返程接驳。
		if !assignShuttle(s, proposal, u, in, outbound, false, sess, target) {
			proposal.Unmet = append(proposal.Unmet, UnmetNeed{
				VisitorToken: token, Pax: in.Pax, Kind: AssignShuttleOut,
				Detail: "交通时窗内无可用返程班次",
			})
		}
		// 住宿：逐房夜分房，优先同一家酒店续住。
		rooms := (in.Pax + 1) / 2
		for _, night := range stayNights(in) {
			hotelID := assignHotel(s, proposal, u, in, night, rooms, prevHotel[token])
			if hotelID == "" {
				status := ""
				if !anySnapshot(s, night) {
					status = "（合作方未报容量）"
				}
				proposal.Unmet = append(proposal.Unmet, UnmetNeed{
					VisitorToken: token, Pax: in.Pax, Kind: AssignHotel,
					Detail: "房夜 " + night + " 无可用客房" + status,
				})
				continue
			}
			prevHotel[token] = hotelID
		}
		// 主题线路。
		for _, day := range activityDays(in, hourDate(target.Start)) {
			if routeID := assignRoute(s, proposal, u, in, day); routeID == "" {
				proposal.Unmet = append(proposal.Unmet, UnmetNeed{
					VisitorToken: token, Pax: in.Pax, Kind: AssignRoute,
					Detail: "出游日 " + day + " 无可用线路槽位",
				})
			}
		}
	}

	// 占位与缺口均已按确定顺序产生；摘要覆盖编排内容而不含生成时刻。
	proposal.Digest = DigestProposal(sessionID, target, proposal.Items, proposal.Unmet)
	return proposal, nil
}

func feasibleRuns(s *State, target SessionTarget, kind RunKind) []*ShuttleRun {
	var out []*ShuttleRun
	for _, r := range s.ActiveRuns(target.VenueID) {
		if r.Kind != kind || !s.PartnerActive(r.PartnerID) {
			continue
		}
		if !IsWithinTrafficWindow(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func assignShuttle(s *State, p *Proposal, u *usage, in *Intent, runs []*ShuttleRun,
	inbound bool, sess *Session, target SessionTarget) bool {
	for _, r := range runs {
		ok := false
		if inbound {
			// 发车不早于观众抵城、到达不晚于观众可接受时刻且不晚于开赛。
			ok = !r.Depart.Before(in.ArriveFrom) && !r.Arrive.After(in.ArriveTo) &&
				!r.Arrive.After(target.Start)
		} else {
			// 发车不早于散场与观众可返程时刻，且不晚于观众最晚返程时刻。
			ok = !r.Depart.Before(target.End) && !r.Depart.Before(in.DepartFrom) &&
				!r.Depart.After(in.DepartTo)
		}
		if !ok || u.runSeats[r.ID]+in.Pax > r.Capacity {
			continue
		}
		u.runSeats[r.ID] += in.Pax
		kind := AssignShuttleIn
		if !inbound {
			kind = AssignShuttleOut
		}
		p.Items = append(p.Items, Assignment{
			VisitorToken: in.VisitorToken, Pax: in.Pax, Kind: kind,
			PartnerID: r.PartnerID, Reference: r.ID,
			Detail: r.Depart.In(Local).Format("01-02 15:04") + "→" +
				r.Arrive.In(Local).Format("01-02 15:04"),
		})
		return true
	}
	return false
}

func assignHotel(s *State, p *Proposal, u *usage, in *Intent, night string,
	rooms int, preferred string) string {
	candidates := sortedPartners(s, PartnerHotel)
	ordered := make([]*Partner, 0, len(candidates))
	for _, c := range candidates { // 续住优先，其余按 ID。
		if c.ID == preferred {
			ordered = append(ordered, c)
		}
	}
	for _, c := range candidates {
		if c.ID != preferred {
			ordered = append(ordered, c)
		}
	}
	for _, hotel := range ordered {
		snap, ok := s.Snapshots[hotel.ID+"|"+night]
		if !ok {
			continue
		}
		key := hotel.ID + "|" + night
		free := maxInt(0, snap.TotalRooms-snap.BookedRooms) - u.rooms[key]
		if free < rooms {
			continue
		}
		u.rooms[key] += rooms
		p.Items = append(p.Items, Assignment{
			VisitorToken: in.VisitorToken, Pax: in.Pax, Kind: AssignHotel,
			PartnerID: hotel.ID, Date: night,
			Detail: hotel.Name + " " + night + " 房x" + itoa(rooms) +
				lateMark(snap),
		})
		return hotel.ID
	}
	return ""
}

func lateMark(snap HotelSnapshot) string {
	if snap.Late {
		return "（迟报）"
	}
	return ""
}

func anySnapshot(s *State, night string) bool {
	for _, p := range s.Partners {
		if p.Kind == PartnerHotel {
			if _, ok := s.Snapshots[p.ID+"|"+night]; ok {
				return true
			}
		}
	}
	return false
}

func assignRoute(s *State, p *Proposal, u *usage, in *Intent, day string) string {
	for _, route := range sortedRoutes(s) {
		if !s.PartnerActive(route.PartnerID) {
			continue
		}
		cap, ok := s.RouteCapacity(route.ID, day)
		if !ok {
			continue
		}
		key := route.ID + "|" + day
		if u.slots[key] >= cap {
			continue
		}
		u.slots[key]++
		p.Items = append(p.Items, Assignment{
			VisitorToken: in.VisitorToken, Pax: in.Pax, Kind: AssignRoute,
			PartnerID: route.PartnerID, Reference: route.ID, Date: day,
			Detail: route.Name,
		})
		return route.ID
	}
	return ""
}

// digestBody 是参与摘要的稳定结构（时间一律 RFC3339，切片顺序即编排顺序）。
type digestBody struct {
	SessionID string        `json:"session_id"`
	Target    SessionTarget `json:"target"`
	Items     []Assignment  `json:"items"`
	Unmet     []UnmetNeed   `json:"unmet"`
}

// DigestProposal 计算编排内容的确定性摘要：相同输入必得相同摘要。
func DigestProposal(sessionID string, target SessionTarget, items []Assignment, unmet []UnmetNeed) string {
	body := digestBody{
		SessionID: sessionID,
		Target: SessionTarget{
			VenueID: target.VenueID,
			Start:   target.Start.In(Local),
			End:     target.End.In(Local),
		},
		Items: items,
		Unmet: unmet,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func collectResource(it Assignment, hotels, runs, routes map[string]struct{}) {
	switch it.Kind {
	case AssignHotel:
		hotels[it.PartnerID] = struct{}{}
	case AssignShuttleIn, AssignShuttleOut:
		runs[it.Reference] = struct{}{}
	case AssignRoute:
		routes[it.Reference] = struct{}{}
	}
}

// AffectedScope 对比旧方案与新方案，给出改线影响范围。
func AffectedScope(oldP, newP *Proposal) RerouteScope {
	tokens := map[string]struct{}{}
	hotels, runs, routes := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}

	oldRes := map[string]Assignment{}
	if oldP != nil {
		for _, it := range oldP.Items {
			oldRes[it.VisitorToken+"|"+string(it.Kind)+"|"+it.Date+"|"+it.Reference] = it
		}
	}
	newRes := map[string]Assignment{}
	for _, it := range newP.Items {
		newRes[it.VisitorToken+"|"+string(it.Kind)+"|"+it.Date+"|"+it.Reference] = it
	}
	for key, it := range newRes {
		if _, same := oldRes[key]; same {
			continue
		}
		tokens[it.VisitorToken] = struct{}{}
		collectResource(it, hotels, runs, routes)
	}
	for key, it := range oldRes {
		if _, keep := newRes[key]; !keep {
			tokens[it.VisitorToken] = struct{}{}
			collectResource(it, hotels, runs, routes) // 被移除的旧资源同样受影响
		}
	}
	oldUnmet := map[string]bool{}
	newUnmet := map[string]bool{}
	unmetKey := func(u UnmetNeed) string { return u.VisitorToken + "|" + string(u.Kind) + "|" + u.Detail }
	if oldP != nil {
		for _, u := range oldP.Unmet {
			oldUnmet[unmetKey(u)] = true
		}
	}
	for _, u := range newP.Unmet {
		k := unmetKey(u)
		newUnmet[k] = true
		if !oldUnmet[k] {
			tokens[u.VisitorToken] = struct{}{} // 新增缺口
		}
	}
	for k := range oldUnmet {
		if !newUnmet[k] {
			token := k[:strings.IndexByte(k, '|')]
			tokens[token] = struct{}{} // 原缺口消失（链路已变）
		}
	}
	scope := RerouteScope{
		VisitorCount:   len(tokens),
		Shortfall:      len(newP.Unmet),
		Visitors:       []string{},
		AffectedHotels: []string{},
		AffectedRuns:   []string{},
		AffectedRoutes: []string{},
	}
	for t := range tokens {
		scope.Visitors = append(scope.Visitors, t)
	}
	sort.Strings(scope.Visitors)
	for id := range hotels {
		scope.AffectedHotels = append(scope.AffectedHotels, id)
	}
	for id := range runs {
		scope.AffectedRuns = append(scope.AffectedRuns, id)
	}
	for id := range routes {
		scope.AffectedRoutes = append(scope.AffectedRoutes, id)
	}
	sort.Strings(scope.AffectedHotels)
	sort.Strings(scope.AffectedRuns)
	sort.Strings(scope.AffectedRoutes)
	return scope
}

// itoa 避免引入 strconv 的局部小工具。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
