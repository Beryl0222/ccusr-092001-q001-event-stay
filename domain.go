// 领域模型与状态聚合。状态本身不落盘，全部由事件重放得到；
// 命令先做业务校验，再追加事件，最后把事件应用到内存状态。
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// addDay 在当地日期上加减天数。
func addDay(date string, n int) (string, error) {
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return "", fmt.Errorf("日期格式应为 2006-01-02：%q：%w", date, err)
	}
	return t.AddDate(0, 0, n).Format("2006-01-02"), nil
}

// 承载等级与安全系数：实际可调度的容量取基础容量乘系数后向下取整。
var gradeFactor = map[string]float64{
	"甲": 1.0,
	"乙": 0.85,
	"丙": 0.7,
}

const (
	statusNormal    = "正常"
	statusSuspended = "暂停"
	statusPending   = "待报送"
	statusCanceled  = "已取消"

	kindLodging = "住宿"
	kindScenic  = "景区"

	dirInbound  = "去程"
	dirOutbound = "回程"
	dirTransfer = "转场"

	reasonReschedule = "场次改期"
	reasonClosure    = "场地临时关闭"
	reasonLateReport = "合作方迟报"
	reasonPartner    = "合作方状态变更"
	reasonManual     = "人工重新评估"
)

// TimeRange 是左闭右开的当地时间区间。
type TimeRange struct {
	Start string `json:"开始"`
	End   string `json:"结束"`
}

func (r TimeRange) overlaps(o TimeRange) bool {
	return r.Start < o.End && o.Start < r.End
}

func (r TimeRange) contains(t string) bool {
	return r.Start <= t && t < r.End
}

type Venue struct {
	ID      string `json:"场馆"`
	Name    string `json:"名称"`
	Grade   string `json:"等级"`
	BaseCap int    `json:"基础容量"`
}

type Partner struct {
	ID       string `json:"合作方"`
	Name     string `json:"名称"`
	Kind     string `json:"类型"`
	Status   string `json:"状态"`
	ReportBy string `json:"报送截止"`
}

// Capacity 是合作方某一当地日的容量快照；迟报为 true 表示快照晚于报送截止。
type Capacity struct {
	Partner string `json:"合作方"`
	Date    string `json:"日期"`
	Beds    int    `json:"容量"`
	Late    bool   `json:"迟报"`
}

type Closure struct {
	ID    string `json:"编号"`
	Venue string `json:"场馆"`
	TimeRange
	Reason  string `json:"原因"`
	EndedAt string `json:"解除时间,omitempty"`
	Active  bool   `json:"生效中"`
}

type Match struct {
	ID       string `json:"场次"`
	Name     string `json:"名称"`
	Venue    string `json:"场馆"`
	Start    string `json:"开始"`
	End      string `json:"结束"`
	Expected int    `json:"预计人数"`
	Revision int    `json:"修订"`
	Status   string `json:"状态"`
}

type Shuttle struct {
	ID     string `json:"班次"`
	Dir    string `json:"方向"`
	From   string `json:"起点"`
	To     string `json:"终点"`
	Depart string `json:"出发"`
	Arrive string `json:"到达"`
	Cap    int    `json:"容量"`
	Status string `json:"状态"`
	Rev    int    `json:"修订"`
}

type Route struct {
	ID       string `json:"线路"`
	Name     string `json:"名称"`
	DailyCap int    `json:"日容量"`
}

// RouteChoice 是观众意向中的某条主题线路与游玩日期。
type RouteChoice struct {
	Route string `json:"线路"`
	Date  string `json:"日期"`
}

// Intent 是最小化保存的观众行程意向：只存化名标识、人数与时间/服务选择，
// 不存姓名、证件号、联系方式等任何实名信息。
type Intent struct {
	Ref         string        `json:"观众标识"`
	MatchID     string        `json:"场次"`
	Revision    int           `json:"修订号"`
	People      int           `json:"人数"`
	ArriveAt    string        `json:"到达时间"`
	ArriveBus   bool          `json:"到达需接驳"`
	LeaveAt     string        `json:"离开时间"`
	LeaveBus    bool          `json:"离开需接驳"`
	CheckIn     string        `json:"入住日期"`
	CheckOut    string        `json:"退房日期"`
	Routes      []RouteChoice `json:"线路"`
	SubmittedAt string        `json:"提交时间"`
}

func (i Intent) key() string { return i.Ref + "\x00" + i.MatchID }

// nights 返回跨日过夜的当地日期列表（入住当晚 至 退房前一晚）。
func (i Intent) nights() []string {
	var out []string
	d := i.CheckIn
	for d != "" && d < i.CheckOut {
		out = append(out, d)
		next, err := addDay(d, 1)
		if err != nil {
			break
		}
		d = next
	}
	return out
}

// ShuttlePick 记录观众团体占用的单个班次及座位数（团体可能被拆到多个班次）。
type ShuttlePick struct {
	Shuttle string `json:"班次"`
	Seats   int    `json:"座位"`
}

// Assignment 是方案中针对单个观众团体的一段编排。
type Assignment struct {
	Ref      string            `json:"观众标识"`
	People   int               `json:"人数"`
	Inbound  []ShuttlePick     `json:"去程班次,omitempty"`
	Outbound []ShuttlePick     `json:"回程班次,omitempty"`
	Lodging  []NightAssignment `json:"过夜,omitempty"`
	Routes   []RouteAssignment `json:"线路,omitempty"`
}

type NightAssignment struct {
	Date    string `json:"日期"`
	Partner string `json:"住宿合作方"`
	Beds    int    `json:"床位数"`
}

type RouteAssignment struct {
	Date   string `json:"日期"`
	Route  string `json:"线路"`
	People int    `json:"人数"`
}

// Unmet 记录因容量不足无法满足的需求，方案宁可显式留缺口也不允许超载。
type Unmet struct {
	Ref     string `json:"观众标识"`
	Segment string `json:"环节"`
	Detail  string `json:"说明"`
	People  int    `json:"人数"`
}

// Plan 是一次确定性编排的完整结果。小写字段为内部占用量，不参与 JSON 序列化。
type Plan struct {
	MatchID     string           `json:"场次"`
	GeneratedAt string           `json:"生成时间"`
	Attendance  []AttendanceSlot `json:"观赛保障"`
	Assignments []Assignment     `json:"编排"`
	Unmet       []Unmet          `json:"未满足"`

	shuttleUsed map[string]int // 班次 -> 已占座位
	bedsUsed    map[string]int // 合作方|日期 -> 已占床位
	routeUsed   map[string]int // 日期|线路 -> 已占名额
}

// AttendanceSlot 是开赛时段内某一小时的观赛容量供需。
type AttendanceSlot struct {
	Slot   string `json:"时段"`
	Demand int    `json:"需求"`
	Supply int    `json:"有效供给"`
	Gap    int    `json:"缺口"`
}

// ImpactScope 描述一次扰动波及的资源与观众（确定性排序）。
type ImpactScope struct {
	Matches  []string `json:"场次"`
	Audience []string `json:"观众"`
	Shuttles []string `json:"接驳班次"`
	Partners []string `json:"合作方"`
	Venues   []string `json:"场馆"`
}

// Proposal 是自动改线建议：原因、影响范围、原方案、建议方案与人工确认状态齐备。
type Proposal struct {
	ID             string      `json:"编号"`
	TriggerSeq     int64       `json:"触发事件序号"`
	Category       string      `json:"原因类别"`
	Reason         string      `json:"原因"`
	Scope          ImpactScope `json:"影响范围"`
	Before         *Plan       `json:"原方案,omitempty"`
	Suggested      Plan        `json:"建议方案"`
	Status         string      `json:"状态"` // 待确认 / 已确认 / 已驳回
	ConfirmedBy    string      `json:"确认人,omitempty"`
	ConfirmOpinion string      `json:"确认意见,omitempty"`
	DecidedAt      string      `json:"决定时间,omitempty"`
	CreatedAt      string      `json:"创建时间"`
}

// Decision 记录在“方案确认”事件中，重放后仍可追溯人工处置。
type Decision struct {
	ProposalID string `json:"改线建议"`
	Decision   string `json:"决定"` // 确认 / 驳回
	Operator   string `json:"确认人"`
	Opinion    string `json:"意见"`
	At         string `json:"时间"`
}

// State 是事件重放后的完整聚合状态。
type State struct {
	venues      map[string]*Venue
	venueIDs    []string
	partners    map[string]*Partner
	partnerIDs  []string
	caps        map[string]Capacity // key: 合作方|日期
	capDates    map[string][]string // partner -> dates
	windows     map[string]TimeRange
	windowDays  []string
	closures    map[string]*Closure
	closureIDs  []string
	matches     map[string]*Match
	shuttles    map[string]*Shuttle
	shuttleIDs  []string
	routes      map[string]*Route
	routeIDs    []string
	intents     map[string]*Intent
	intentKeys  []string
	proposals   map[string]*Proposal
	proposalIDs []string
	decisions   map[string]Decision
}

func newState() *State {
	return &State{
		venues:    map[string]*Venue{},
		partners:  map[string]*Partner{},
		caps:      map[string]Capacity{},
		capDates:  map[string][]string{},
		windows:   map[string]TimeRange{},
		closures:  map[string]*Closure{},
		matches:   map[string]*Match{},
		shuttles:  map[string]*Shuttle{},
		routes:    map[string]*Route{},
		intents:   map[string]*Intent{},
		proposals: map[string]*Proposal{},
		decisions: map[string]Decision{},
	}
}

func capKey(p, d string) string { return p + "|" + d }

// effectiveVenueCap 返回场馆在时刻 t 的有效容量；关闭时窗内为 0。
func (s *State) effectiveVenueCap(v *Venue, t string) int {
	for _, c := range s.closures {
		if c.Active && c.Venue == v.ID && c.contains(t) {
			return 0
		}
	}
	f := gradeFactor[v.Grade]
	return int(float64(v.BaseCap) * f)
}

// partnerCap 返回某合作方某日的可用床位数；暂停营业视为 0，未报送返回 -1。
func (s *State) partnerCap(pid, date string) (int, bool) {
	c, ok := s.caps[capKey(pid, date)]
	if !ok {
		return 0, false
	}
	if s.partners[pid] != nil && s.partners[pid].Status == statusSuspended {
		return 0, true
	}
	return c.Beds, true
}

// intentsForMatch 按观众标识确定性排序返回场次下的最新意向。
func (s *State) intentsForMatch(matchID string) []Intent {
	var out []Intent
	for _, it := range s.intents {
		if it.MatchID == matchID {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// apply 把单个事件作用到状态上；重放与实时追加走同一条路径。
func (s *State) apply(e Event) error {
	switch e.Type {
	case evVenueRegistered:
		var v Venue
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return err
		}
		if _, ok := s.venues[v.ID]; !ok {
			s.venueIDs = append(s.venueIDs, v.ID)
		}
		s.venues[v.ID] = &v

	case evPartnerStatus:
		var p Partner
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		if _, ok := s.partners[p.ID]; !ok {
			s.partnerIDs = append(s.partnerIDs, p.ID)
		}
		s.partners[p.ID] = &p

	case evCapacitySnapshot:
		var c Capacity
		if err := json.Unmarshal(e.Payload, &c); err != nil {
			return err
		}
		k := capKey(c.Partner, c.Date)
		if _, ok := s.caps[k]; !ok {
			s.capDates[c.Partner] = append(s.capDates[c.Partner], c.Date)
			sort.Strings(s.capDates[c.Partner])
		}
		s.caps[k] = c

	case evWindowSet:
		var w struct {
			Date string `json:"日期"`
			TimeRange
		}
		if err := json.Unmarshal(e.Payload, &w); err != nil {
			return err
		}
		if _, ok := s.windows[w.Date]; !ok {
			s.windowDays = append(s.windowDays, w.Date)
			sort.Strings(s.windowDays)
		}
		s.windows[w.Date] = w.TimeRange

	case evShuttlePlanned:
		var t Shuttle
		if err := json.Unmarshal(e.Payload, &t); err != nil {
			return err
		}
		if _, ok := s.shuttles[t.ID]; !ok {
			s.shuttleIDs = append(s.shuttleIDs, t.ID)
		}
		t.Status = statusNormal
		s.shuttles[t.ID] = &t

	case evShuttleAdjusted:
		var t Shuttle
		if err := json.Unmarshal(e.Payload, &t); err != nil {
			return err
		}
		if cur, ok := s.shuttles[t.ID]; ok {
			t.Status = cur.Status
			s.shuttles[t.ID] = &t
		}

	case evRoutePlanned:
		var r Route
		if err := json.Unmarshal(e.Payload, &r); err != nil {
			return err
		}
		if _, ok := s.routes[r.ID]; !ok {
			s.routeIDs = append(s.routeIDs, r.ID)
		}
		s.routes[r.ID] = &r

	case evMatchScheduled:
		var m Match
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			return err
		}
		m.Revision = 1
		m.Status = statusNormal
		s.matches[m.ID] = &m

	case evMatchRescheduled:
		var p struct {
			MatchID string `json:"场次"`
			Start   string `json:"新开始"`
			End     string `json:"新结束"`
			Reason  string `json:"原因"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		if m := s.matches[p.MatchID]; m != nil {
			m.Start = p.Start
			m.End = p.End
			m.Revision++
		}

	case evClosureStarted:
		var c Closure
		if err := json.Unmarshal(e.Payload, &c); err != nil {
			return err
		}
		c.Active = true
		s.closures[c.ID] = &c
		s.closureIDs = append(s.closureIDs, c.ID)

	case evClosureEnded:
		var p struct {
			ID  string `json:"编号"`
			End string `json:"解除时间"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		if c := s.closures[p.ID]; c != nil {
			c.Active = false
			c.EndedAt = p.End
		}

	case evIntentReceived:
		var in Intent
		if err := json.Unmarshal(e.Payload, &in); err != nil {
			return err
		}
		k := in.key()
		if _, ok := s.intents[k]; !ok {
			s.intentKeys = append(s.intentKeys, k)
		}
		s.intents[k] = &in

	case evRerouteProposed:
		var pr Proposal
		if err := json.Unmarshal(e.Payload, &pr); err != nil {
			return err
		}
		if _, ok := s.proposals[pr.ID]; !ok {
			s.proposalIDs = append(s.proposalIDs, pr.ID)
		}
		s.proposals[pr.ID] = &pr

	case evPlanConfirmed:
		var d Decision
		if err := json.Unmarshal(e.Payload, &d); err != nil {
			return err
		}
		s.decisions[d.ProposalID] = d
		if pr := s.proposals[d.ProposalID]; pr != nil {
			if d.Decision == "确认" {
				pr.Status = "已确认"
			} else {
				pr.Status = "已驳回"
			}
			pr.ConfirmedBy = d.Operator
			pr.ConfirmOpinion = d.Opinion
			pr.DecidedAt = d.At
		}

	case evAccessAudited:
		// 审计事件只留存，不改变聚合状态。

	default:
		return fmt.Errorf("未知事件类型 %q", e.Type)
	}
	return nil
}

// requireText 保证关键字段非空且不含分隔字符，避免日志键被污染。
func requireText(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s不能为空", field)
	}
	if strings.ContainsAny(v, "|\x00") {
		return fmt.Errorf("%s含非法字符", field)
	}
	return nil
}
