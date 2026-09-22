// 确定性编排：纯函数读取聚合状态，输出接待缺口报告与不超载推荐方案。
// 所有候选资源按标识排序、按固定次序贪心匹配，同一状态必然得到同一结果。
// 缺口报告是推荐方案的聚合视图：两者共用同一次分配，绝不会出现“方案满足而报告报缺口”。
package main

import (
	"fmt"
	"sort"
	"time"
)

// GapBucket 是某一时段/日期的单项供需对比；已承接表示该时段需求已被资源覆盖的数量。
type GapBucket struct {
	Slot    string `json:"时段"`
	Demand  int    `json:"需求"`
	Covered int    `json:"已承接,omitempty"`
	Supply  int    `json:"有效供给"`
	Gap     int    `json:"缺口"`
}

// LodgingBucket 额外标出未报送容量的合作方，迟报与漏报在报告中可见。
type LodgingBucket struct {
	Date       string   `json:"日期"`
	Demand     int      `json:"需求"`
	Covered    int      `json:"已承接"`
	Supply     int      `json:"已报容量"`
	Unreported []string `json:"未报送合作方,omitempty"`
	Suspended  []string `json:"暂停合作方,omitempty"`
	Gap        int      `json:"缺口"`
}

type RouteBucket struct {
	Date    string `json:"日期"`
	Route   string `json:"线路"`
	Demand  int    `json:"需求"`
	Covered int    `json:"已承接"`
	Supply  int    `json:"日容量"`
	Gap     int    `json:"缺口"`
}

// GapReport 回答“某场赛事各时段的接待缺口”。
type GapReport struct {
	MatchID     string          `json:"场次"`
	MatchName   string          `json:"名称"`
	Venue       string          `json:"场馆"`
	VenueCap    int             `json:"观赛有效容量,omitempty"`
	VenueClosed bool            `json:"场馆关闭中,omitempty"`
	Attendance  []GapBucket     `json:"观赛时段"`
	Inbound     []GapBucket     `json:"去程接驳"`
	Outbound    []GapBucket     `json:"回程接驳"`
	Lodging     []LodgingBucket `json:"住宿夜"`
	Routes      []RouteBucket   `json:"主题线路"`
}

func gapOf(demand, supply int) int {
	if demand > supply {
		return demand - supply
	}
	return 0
}

// withinWindow 判断时刻是否落在该日交通时窗内；未设时窗视为当日不限。
func (s *State) withinWindow(t string) bool {
	w, ok := s.windows[dayBucket(t)]
	if !ok {
		return true
	}
	return w.contains(t)
}

func (s *State) sortedShuttles() []Shuttle {
	out := make([]Shuttle, 0, len(s.shuttleIDs))
	for _, id := range s.shuttleIDs {
		out = append(out, *s.shuttles[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *State) sortedLodgingPartners() []*Partner {
	var out []*Partner
	for _, id := range s.partnerIDs {
		if p := s.partners[id]; p.Kind == kindLodging {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// eligibleShuttles 返回场次可用的去程/回程班次（取消与时窗外剔除）。
func (s *State) eligibleShuttles(m *Match) (inbound, outbound []Shuttle) {
	for _, t := range s.sortedShuttles() {
		if t.Status == statusCanceled || !s.withinWindow(t.Depart) {
			continue
		}
		if t.Dir == dirInbound && t.To == m.Venue {
			inbound = append(inbound, t)
		}
		if t.Dir == dirOutbound && t.From == m.Venue {
			outbound = append(outbound, t)
		}
	}
	return inbound, outbound
}

// GeneratePlan 依据当前状态为单场赛生产编排：任何环节容量不足都记入未满足，绝不超载。
func GeneratePlan(s *State, matchID string) (Plan, error) {
	m := s.matches[matchID]
	if m == nil {
		return Plan{}, fmt.Errorf("%w: 场次 %s", ErrNotFound, matchID)
	}
	plan := Plan{
		MatchID:     matchID,
		Attendance:  []AttendanceSlot{},
		Assignments: []Assignment{},
		Unmet:       []Unmet{},
		shuttleUsed: map[string]int{},
		bedsUsed:    map[string]int{},
		routeUsed:   map[string]int{},
	}
	intents := s.intentsForMatch(matchID)

	// 观赛保障：按小时展开场次时段；临时关闭使有效容量归零，缺口进入未满足。
	registered := 0
	for _, it := range intents {
		registered += it.People
	}
	attDemand := m.Expected
	if registered > attDemand {
		attDemand = registered
	}
	v := s.venues[m.Venue]
	for h := hourBucket(m.Start); h != "" && h < hourBucket(m.End); h = nextHour(h) {
		supply := 0
		if v != nil {
			supply = s.effectiveVenueCap(v, h+":00:00")
		}
		slot := AttendanceSlot{Slot: h, Demand: attDemand, Supply: supply, Gap: gapOf(attDemand, supply)}
		plan.Attendance = append(plan.Attendance, slot)
	}
	for _, slot := range plan.Attendance {
		if slot.Gap > 0 {
			plan.Unmet = append(plan.Unmet, Unmet{
				Ref: "-", Segment: "观赛",
				Detail: slot.Slot + " 场馆有效容量不足（含临时关闭）", People: slot.Gap,
			})
		}
	}

	inbound, outbound := s.eligibleShuttles(m)

	addUnmet := func(ref, seg, detail string, people int) {
		if people <= 0 {
			return
		}
		plan.Unmet = append(plan.Unmet, Unmet{Ref: ref, Segment: seg, Detail: detail, People: people})
	}

	for _, it := range intents {
		a := Assignment{Ref: it.Ref, People: it.People}

		if it.ArriveBus {
			left := it.People
			for _, t := range inbound {
				if left == 0 {
					break
				}
				// 班次不得早于观众到达、必须在开赛前送到。
				if t.Depart < it.ArriveAt || t.Arrive > m.Start {
					continue
				}
				seats := t.Cap - plan.shuttleUsed[t.ID]
				if seats <= 0 {
					continue
				}
				take := minInt(left, seats)
				plan.shuttleUsed[t.ID] += take
				left -= take
				a.Inbound = append(a.Inbound, ShuttlePick{Shuttle: t.ID, Seats: take})
			}
			addUnmet(it.Ref, "去程接驳", "到达 "+it.ArriveAt+" 至开赛前无足够班次", left)
		}

		if it.LeaveBus {
			left := it.People
			for _, t := range outbound {
				if left == 0 {
					break
				}
				// 班次不得早于散场、必须在观众离开前送达。
				if t.Depart < m.End || t.Arrive > it.LeaveAt {
					continue
				}
				seats := t.Cap - plan.shuttleUsed[t.ID]
				if seats <= 0 {
					continue
				}
				take := minInt(left, seats)
				plan.shuttleUsed[t.ID] += take
				left -= take
				a.Outbound = append(a.Outbound, ShuttlePick{Shuttle: t.ID, Seats: take})
			}
			addUnmet(it.Ref, "回程接驳", "终场后至离开 "+it.LeaveAt+" 无足够班次", left)
		}

		for _, night := range it.nights() {
			left := it.People
			for _, p := range s.sortedLodgingPartners() {
				if left == 0 {
					break
				}
				if p.Status == statusSuspended {
					continue
				}
				cap, reported := s.partnerCap(p.ID, night)
				if !reported {
					continue
				}
				k := capKey(p.ID, night)
				take := minInt(left, cap-plan.bedsUsed[k])
				if take <= 0 {
					continue
				}
				plan.bedsUsed[k] += take
				left -= take
				a.Lodging = append(a.Lodging, NightAssignment{Date: night, Partner: p.ID, Beds: take})
			}
			addUnmet(it.Ref, "住宿", night+" 夜床位不足", left)
		}

		for _, rc := range it.Routes {
			k := rc.Date + "|" + rc.Route
			cap := 0
			if r := s.routes[rc.Route]; r != nil {
				cap = r.DailyCap
			}
			if cap-plan.routeUsed[k] >= it.People {
				plan.routeUsed[k] += it.People
				a.Routes = append(a.Routes, RouteAssignment{Date: rc.Date, Route: rc.Route, People: it.People})
			} else {
				addUnmet(it.Ref, "主题线路", rc.Date+" "+rc.Route+" 名额不足", it.People)
			}
		}

		plan.Assignments = append(plan.Assignments, a)
	}
	sort.Slice(plan.Unmet, func(i, j int) bool {
		if plan.Unmet[i].Ref != plan.Unmet[j].Ref {
			return plan.Unmet[i].Ref < plan.Unmet[j].Ref
		}
		return plan.Unmet[i].Segment < plan.Unmet[j].Segment
	})
	return plan, nil
}

// GapAnalysis 与 GeneratePlan 使用同一次分配：先算方案，再把方案的实际承接结果按时段聚合。
func GapAnalysis(s *State, matchID string) (GapReport, error) {
	m := s.matches[matchID]
	if m == nil {
		return GapReport{}, fmt.Errorf("%w: 场次 %s", ErrNotFound, matchID)
	}
	plan, err := GeneratePlan(s, matchID)
	if err != nil {
		return GapReport{}, err
	}
	intents := s.intentsForMatch(matchID)
	rep := GapReport{
		MatchID: m.ID, MatchName: m.Name, Venue: m.Venue,
		Attendance: []GapBucket{}, Inbound: []GapBucket{}, Outbound: []GapBucket{},
		Lodging: []LodgingBucket{}, Routes: []RouteBucket{},
	}

	for _, slot := range plan.Attendance {
		if slot.Supply == 0 {
			rep.VenueClosed = true
		}
		rep.VenueCap = slot.Supply
		rep.Attendance = append(rep.Attendance, GapBucket{
			Slot: slot.Slot, Demand: slot.Demand,
			Covered: slot.Demand - slot.Gap, Supply: slot.Supply, Gap: slot.Gap,
		})
	}

	// 接驳：需求按观众的期望到达/离开小时归桶；已承接按实际分到的座位归入同一期望桶；
	// 供给按班次实际到达/出发小时归桶。承接可能晚于期望时刻，具体时刻见推荐方案。
	inDemand, inCovered := map[string]int{}, map[string]int{}
	outDemand, outCovered := map[string]int{}, map[string]int{}
	for i, it := range intents {
		if it.ArriveBus {
			h := hourBucket(it.ArriveAt)
			inDemand[h] += it.People
			for _, p := range plan.Assignments[i].Inbound {
				inCovered[h] += p.Seats
			}
		}
		if it.LeaveBus {
			h := hourBucket(it.LeaveAt)
			outDemand[h] += it.People
			for _, p := range plan.Assignments[i].Outbound {
				outCovered[h] += p.Seats
			}
		}
	}
	inSupply, outSupply := map[string]int{}, map[string]int{}
	inb, outb := s.eligibleShuttles(m)
	for _, t := range inb {
		inSupply[hourBucket(t.Arrive)] += t.Cap
	}
	for _, t := range outb {
		outSupply[hourBucket(t.Depart)] += t.Cap
	}
	rep.Inbound = mergeGapBuckets(inDemand, inCovered, inSupply)
	rep.Outbound = mergeGapBuckets(outDemand, outCovered, outSupply)

	// 住宿：按夜统计，已承接取自方案实际分房，未报送与暂停的合作方单列。
	nightDemand := map[string]int{}
	for _, it := range intents {
		for _, n := range it.nights() {
			nightDemand[n] += it.People
		}
	}
	for _, night := range sortedStringKeys(nightDemand) {
		b := LodgingBucket{Date: night, Demand: nightDemand[night]}
		covered := 0
		for _, p := range s.sortedLodgingPartners() {
			if p.Status == statusSuspended {
				b.Suspended = append(b.Suspended, p.ID)
				continue
			}
			c, reported := s.partnerCap(p.ID, night)
			if !reported {
				b.Unreported = append(b.Unreported, p.ID)
				continue
			}
			b.Supply += c
			covered += plan.bedsUsed[capKey(p.ID, night)]
		}
		b.Covered = covered
		b.Gap = b.Demand - covered
		rep.Lodging = append(rep.Lodging, b)
	}

	// 主题线路：按日期与线路统计，已承接取自方案实际占位。
	routeDemand := map[string]int{}
	for _, it := range intents {
		for _, rc := range it.Routes {
			routeDemand[rc.Date+"|"+rc.Route] += it.People
		}
	}
	for _, k := range sortedStringKeys(routeDemand) {
		date, rid := splitKey(k)
		supply := 0
		if r := s.routes[rid]; r != nil {
			supply = r.DailyCap
		}
		d := routeDemand[k]
		covered := minInt(d, plan.routeUsed[k])
		rep.Routes = append(rep.Routes, RouteBucket{
			Date: date, Route: rid, Demand: d, Covered: covered,
			Supply: supply, Gap: d - covered,
		})
	}
	return rep, nil
}

// mergeGapBuckets 合并期望桶（需求/已承接/缺口）与资源桶（供给），按小时排序。
func mergeGapBuckets(demand, covered, supply map[string]int) []GapBucket {
	keys := map[string]struct{}{}
	for k := range demand {
		keys[k] = struct{}{}
	}
	for k := range supply {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := make([]GapBucket, 0, len(sorted))
	for _, k := range sorted {
		d, c := demand[k], covered[k]
		out = append(out, GapBucket{
			Slot: k, Demand: d, Covered: c, Supply: supply[k], Gap: d - c,
		})
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sortedStringKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

func nextHour(h string) string {
	t, err := time.ParseInLocation("2006-01-02T15", h, time.Local)
	if err != nil {
		return ""
	}
	return t.Add(time.Hour).Format("2006-01-02T15")
}
