// 应用服务：命令校验、追加事件、维护重放状态，并在扰动发生时自动产出改线建议。
// 所有写命令在同一把锁内串行执行，保证“同一事件序列 → 同一结果”。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// App 是领域应用服务，持有事件存储与重放得到的状态。
type App struct {
	store Store
	mu    sync.Mutex
	state *State
	clock Clock
	// idemAnchor 记录幂等键首次出现的事件序号；idemProposal 记录该命令产出的首条改线建议。
	idemAnchor   map[string]int64
	idemProposal map[string]string
}

func NewApp(store Store, clock Clock) (*App, error) {
	if clock == nil {
		clock = defaultClock
	}
	a := &App{
		store:        store,
		state:        newState(),
		clock:        clock,
		idemAnchor:   map[string]int64{},
		idemProposal: map[string]string{},
	}
	for _, e := range store.Events() {
		if err := a.state.apply(e); err != nil {
			return nil, fmt.Errorf("重放事件 %d 失败：%w", e.Seq, err)
		}
		if e.Idem != "" {
			if _, ok := a.idemAnchor[e.Idem]; !ok {
				a.idemAnchor[e.Idem] = e.Seq
			}
			if e.Type == evRerouteProposed {
				var pr Proposal
				if err := json.Unmarshal(e.Payload, &pr); err == nil {
					if _, ok := a.idemProposal[e.Idem]; !ok {
						a.idemProposal[e.Idem] = pr.ID
					}
				}
			}
		}
	}
	return a, nil
}

// IdemResult 是幂等重试时返回的首次执行结果。
type IdemResult struct {
	Replay     bool   `json:"幂等重放"`
	AnchorSeq  int64  `json:"首次事件序号"`
	ProposalID string `json:"改线建议,omitempty"`
}

// begin 检查幂等键；已见过则返回首次结果，调用方必须直接结束命令。
func (a *App) begin(idem string) (IdemResult, bool) {
	if idem != "" {
		if seq, ok := a.idemAnchor[idem]; ok {
			return IdemResult{Replay: true, AnchorSeq: seq, ProposalID: a.idemProposal[idem]}, true
		}
	}
	return IdemResult{}, false
}

func (a *App) appendEvent(typ, actor, idem string, payload any) (Event, error) {
	e, err := a.store.Append(typ, actor, idem, payload, a.clock)
	if err != nil {
		return Event{}, err
	}
	if err := a.state.apply(e); err != nil {
		return Event{}, fmt.Errorf("应用事件失败：%w", err)
	}
	if idem != "" {
		if _, ok := a.idemAnchor[idem]; !ok {
			a.idemAnchor[idem] = e.Seq
		}
	}
	return e, nil
}

// ---------- 基础资料命令 ----------

type RegisterVenueCmd struct {
	ID      string `json:"场馆"`
	Name    string `json:"名称"`
	Grade   string `json:"等级"`
	BaseCap int    `json:"基础容量"`
}

func (a *App) RegisterVenue(actor, idem string, c RegisterVenueCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("场馆", c.ID); err != nil {
		return IdemResult{}, err
	}
	if _, ok := gradeFactor[c.Grade]; !ok {
		return IdemResult{}, fmt.Errorf("承载等级必须为 甲/乙/丙，收到 %q", c.Grade)
	}
	if c.BaseCap <= 0 {
		return IdemResult{}, fmt.Errorf("基础容量必须为正数")
	}
	if _, exists := a.state.venues[c.ID]; exists {
		return IdemResult{}, fmt.Errorf("%w: 场馆 %s 已登记", ErrConflict, c.ID)
	}
	if _, err := a.appendEvent(evVenueRegistered, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

type PartnerStatusCmd struct {
	ID       string `json:"合作方"`
	Name     string `json:"名称"`
	Kind     string `json:"类型"`
	Status   string `json:"状态"`
	ReportBy string `json:"报送截止"`
}

func (a *App) SetPartnerStatus(actor, idem string, c PartnerStatusCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("合作方", c.ID); err != nil {
		return IdemResult{}, err
	}
	if c.Kind != kindLodging && c.Kind != kindScenic {
		return IdemResult{}, fmt.Errorf("合作方类型必须为 住宿/景区")
	}
	if c.Status != statusNormal && c.Status != statusSuspended {
		return IdemResult{}, fmt.Errorf("合作方状态必须为 正常/暂停")
	}
	if c.ReportBy != "" {
		if _, err := parseCivil(c.ReportBy + "T00:00:00"); err != nil {
			return IdemResult{}, err
		}
	}
	prev := a.state.partners[c.ID]
	willSuspend := prev != nil && prev.Status != statusSuspended && c.Status == statusSuspended

	// 暂停前先固定受影响场次与旧方案快照。
	var before map[string]*Plan
	if willSuspend {
		set := map[string]bool{}
		for _, k := range a.state.intentKeys {
			it := a.state.intents[k]
			for _, n := range it.nights() {
				if cap, ok := a.state.caps[capKey(c.ID, n)]; ok && cap.Beds > 0 {
					set[it.MatchID] = true
				}
			}
		}
		before = map[string]*Plan{}
		for _, mid := range sortedMatchSet(set) {
			pl, err := GeneratePlan(a.state, mid)
			if err != nil {
				return IdemResult{}, err
			}
			before[mid] = &pl
		}
	}

	if _, err := a.appendEvent(evPartnerStatus, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	// 合作方由正常转暂停，已编排的接待能力需要重估。
	var pid string
	if willSuspend {
		p, err := a.rerouteForPartnerSuspension(c.ID, "合作方 "+c.ID+" 暂停接待", before)
		if err != nil {
			return IdemResult{}, err
		}
		pid = firstProposal(p)
	}
	return a.result(idem, pid), nil
}

type CapacityCmd struct {
	Partner string `json:"合作方"`
	Date    string `json:"日期"`
	Beds    int    `json:"容量"`
}

// ReportCapacity 登记容量快照；晚于合作方报送截止即标记迟报并触发受影响场次的改线。
func (a *App) ReportCapacity(actor, idem string, c CapacityCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	p := a.state.partners[c.Partner]
	if p == nil {
		return IdemResult{}, fmt.Errorf("%w: 合作方 %s", ErrNotFound, c.Partner)
	}
	if _, err := parseCivil(c.Date + "T00:00:00"); err != nil {
		return IdemResult{}, err
	}
	if c.Beds < 0 {
		return IdemResult{}, fmt.Errorf("容量不能为负")
	}
	late := p.ReportBy != "" && dayBucket(a.clock()) > p.ReportBy

	// 迟报前先固定受影响场次与旧方案快照，事件应用后再做差异比较。
	var affected []string
	var before map[string]*Plan
	if late {
		affected = a.matchesStayingNight(c.Date)
		before = map[string]*Plan{}
		for _, mid := range affected {
			pl, err := GeneratePlan(a.state, mid)
			if err != nil {
				return IdemResult{}, err
			}
			before[mid] = &pl
		}
	}

	snap := Capacity{Partner: c.Partner, Date: c.Date, Beds: c.Beds, Late: late}
	trigger, err := a.appendEvent(evCapacitySnapshot, actor, idem, snap)
	if err != nil {
		return IdemResult{}, err
	}
	var pid string
	if late {
		reason := "合作方 " + c.Partner + " 于 " + c.Date + " 容量迟报（报送截止 " + p.ReportBy + "）"
		p, err := a.rerouteForLateCapacity(trigger.Seq, snap, reason, before)
		if err != nil {
			return IdemResult{}, err
		}
		pid = firstProposal(p)
	}
	return a.result(idem, pid), nil
}

type WindowCmd struct {
	Date  string `json:"日期"`
	Start string `json:"开始"`
	End   string `json:"结束"`
}

func (a *App) SetWindow(actor, idem string, c WindowCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if _, err := parseCivil(c.Date + "T00:00:00"); err != nil {
		return IdemResult{}, err
	}
	st, err1 := parseCivil(c.Date + "T" + c.Start)
	en, err2 := parseCivil(c.Date + "T" + c.End)
	if err1 != nil || err2 != nil {
		return IdemResult{}, fmt.Errorf("交通时窗时间格式应为 HH:MM:SS")
	}
	if !st.Before(en) {
		return IdemResult{}, fmt.Errorf("交通时窗开始必须早于结束")
	}
	payload := struct {
		Date string `json:"日期"`
		TimeRange
	}{c.Date, TimeRange{Start: st.Format(civilFmt), End: en.Format(civilFmt)}}
	if _, err := a.appendEvent(evWindowSet, actor, idem, payload); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

const civilFmt = "2006-01-02T15:04:05"

type ShuttleCmd struct {
	ID     string `json:"班次"`
	Dir    string `json:"方向"`
	From   string `json:"起点"`
	To     string `json:"终点"`
	Depart string `json:"出发"`
	Arrive string `json:"到达"`
	Cap    int    `json:"容量"`
}

func validShuttleTimes(c ShuttleCmd) error {
	d, err1 := parseCivil(c.Depart)
	ar, err2 := parseCivil(c.Arrive)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("班次时间格式应为 2006-01-02T15:04:05")
	}
	if !d.Before(ar) {
		return fmt.Errorf("出发必须早于到达")
	}
	return nil
}

func (a *App) PlanShuttle(actor, idem string, c ShuttleCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("班次", c.ID); err != nil {
		return IdemResult{}, err
	}
	if c.Dir != dirInbound && c.Dir != dirOutbound && c.Dir != dirTransfer {
		return IdemResult{}, fmt.Errorf("班次方向必须为 去程/回程/转场")
	}
	if err := validShuttleTimes(c); err != nil {
		return IdemResult{}, err
	}
	if c.Cap < 0 {
		return IdemResult{}, fmt.Errorf("班次容量不能为负")
	}
	if _, exists := a.state.shuttles[c.ID]; exists {
		return IdemResult{}, fmt.Errorf("%w: 班次 %s 已存在", ErrConflict, c.ID)
	}
	if _, err := a.appendEvent(evShuttlePlanned, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

// AdjustShuttle 修改既定时班次（时刻/容量），未提供的字段沿用现值。
func (a *App) AdjustShuttle(actor, idem string, c ShuttleCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	cur := a.state.shuttles[c.ID]
	if cur == nil {
		return IdemResult{}, fmt.Errorf("%w: 班次 %s", ErrNotFound, c.ID)
	}
	if c.Dir != "" && c.Dir != dirInbound && c.Dir != dirOutbound && c.Dir != dirTransfer {
		return IdemResult{}, fmt.Errorf("班次方向必须为 去程/回程/转场")
	}
	if c.Dir == "" {
		c.Dir = cur.Dir
	}
	if c.From == "" {
		c.From = cur.From
	}
	if c.To == "" {
		c.To = cur.To
	}
	if c.Depart == "" {
		c.Depart = cur.Depart
	}
	if c.Arrive == "" {
		c.Arrive = cur.Arrive
	}
	if c.Cap == 0 {
		c.Cap = cur.Cap
	}
	if err := validShuttleTimes(c); err != nil {
		return IdemResult{}, err
	}
	if _, err := a.appendEvent(evShuttleAdjusted, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

type RouteCmd struct {
	ID       string `json:"线路"`
	Name     string `json:"名称"`
	DailyCap int    `json:"日容量"`
}

func (a *App) PlanRoute(actor, idem string, c RouteCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("线路", c.ID); err != nil {
		return IdemResult{}, err
	}
	if c.DailyCap < 0 {
		return IdemResult{}, fmt.Errorf("线路日容量不能为负")
	}
	if _, err := a.appendEvent(evRoutePlanned, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

// ---------- 赛程命令 ----------

type ScheduleMatchCmd struct {
	ID       string `json:"场次"`
	Name     string `json:"名称"`
	Venue    string `json:"场馆"`
	Start    string `json:"开始"`
	End      string `json:"结束"`
	Expected int    `json:"预计人数"`
}

func (a *App) ScheduleMatch(actor, idem string, c ScheduleMatchCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("场次", c.ID); err != nil {
		return IdemResult{}, err
	}
	if a.state.venues[c.Venue] == nil {
		return IdemResult{}, fmt.Errorf("%w: 场馆 %s", ErrNotFound, c.Venue)
	}
	st, err1 := parseCivil(c.Start)
	en, err2 := parseCivil(c.End)
	if err1 != nil || err2 != nil {
		return IdemResult{}, fmt.Errorf("场次时间格式应为 2006-01-02T15:04:05")
	}
	if !st.Before(en) {
		return IdemResult{}, fmt.Errorf("开赛必须早于散场")
	}
	if c.Expected < 0 {
		return IdemResult{}, fmt.Errorf("预计人数不能为负")
	}
	if _, exists := a.state.matches[c.ID]; exists {
		return IdemResult{}, fmt.Errorf("%w: 场次 %s 已发布", ErrConflict, c.ID)
	}
	if _, err := a.appendEvent(evMatchScheduled, actor, idem, c); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

type RescheduleCmd struct {
	MatchID string `json:"场次"`
	Start   string `json:"新开始"`
	End     string `json:"新结束"`
	Reason  string `json:"原因"`
}

// Reschedule 执行赛程改期：先留存改期前方案快照，改期后重算，方案发生变化即产生改线建议。
func (a *App) Reschedule(actor, idem string, c RescheduleCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	m := a.state.matches[c.MatchID]
	if m == nil {
		return IdemResult{}, fmt.Errorf("%w: 场次 %s", ErrNotFound, c.MatchID)
	}
	st, err1 := parseCivil(c.Start)
	en, err2 := parseCivil(c.End)
	if err1 != nil || err2 != nil {
		return IdemResult{}, fmt.Errorf("场次时间格式应为 2006-01-02T15:04:05")
	}
	if !st.Before(en) {
		return IdemResult{}, fmt.Errorf("开赛必须早于散场")
	}
	before, err := GeneratePlan(a.state, c.MatchID)
	if err != nil {
		return IdemResult{}, err
	}
	trigger, err := a.appendEvent(evMatchRescheduled, actor, idem, c)
	if err != nil {
		return IdemResult{}, err
	}
	after, err := GeneratePlan(a.state, c.MatchID)
	if err != nil {
		return IdemResult{}, err
	}
	var pid string
	if planSignature(&before) != planSignature(&after) {
		reason := c.Reason
		if strings.TrimSpace(reason) == "" {
			reason = "场次 " + c.MatchID + " 改期至 " + c.Start
		}
		scope := a.scopeForMatch(c.MatchID)
		bp := before
		p, err := a.propose(trigger.Seq, c.MatchID, reasonReschedule, reason, scope, &bp, after, idem)
		if err != nil {
			return IdemResult{}, err
		}
		pid = p
	}
	return a.result(idem, pid), nil
}

type ClosureCmd struct {
	ID     string `json:"编号"`
	Venue  string `json:"场馆"`
	Start  string `json:"开始"`
	End    string `json:"结束"`
	Reason string `json:"原因"`
}

// CloseVenue 登记场地临时关闭，对时窗内使用该场馆的所有场次逐一评估改线。
func (a *App) CloseVenue(actor, idem string, c ClosureCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("编号", c.ID); err != nil {
		return IdemResult{}, err
	}
	if a.state.venues[c.Venue] == nil {
		return IdemResult{}, fmt.Errorf("%w: 场馆 %s", ErrNotFound, c.Venue)
	}
	st, e1 := parseCivil(c.Start)
	en, e2 := parseCivil(c.End)
	if e1 != nil || e2 != nil {
		return IdemResult{}, fmt.Errorf("关闭时间格式应为 2006-01-02T15:04:05")
	}
	if !st.Before(en) {
		return IdemResult{}, fmt.Errorf("关闭开始必须早于结束")
	}
	if _, exists := a.state.closures[c.ID]; exists {
		return IdemResult{}, fmt.Errorf("%w: 关闭单 %s 已存在", ErrConflict, c.ID)
	}
	win := TimeRange{Start: c.Start, End: c.End}
	closure := Closure{ID: c.ID, Venue: c.Venue, TimeRange: win, Reason: c.Reason, Active: true}

	// 关闭事件应用前固定受影响场次及旧方案。
	var matchIDs []string
	for _, m := range a.state.matches {
		if m.Venue == c.Venue && m.Status == statusNormal &&
			win.overlaps(TimeRange{Start: m.Start, End: m.End}) {
			matchIDs = append(matchIDs, m.ID)
		}
	}
	sort.Strings(matchIDs)
	before := map[string]*Plan{}
	for _, mid := range matchIDs {
		pl, err := GeneratePlan(a.state, mid)
		if err != nil {
			return IdemResult{}, err
		}
		before[mid] = &pl
	}

	trigger, err := a.appendEvent(evClosureStarted, actor, idem, closure)
	if err != nil {
		return IdemResult{}, err
	}
	reason := c.Reason
	if strings.TrimSpace(reason) == "" {
		reason = "场馆 " + c.Venue + " 于 " + c.Start + " 至 " + c.End + " 临时关闭"
	}
	var pids []string
	for _, mid := range matchIDs {
		after, err := GeneratePlan(a.state, mid)
		if err != nil {
			return IdemResult{}, err
		}
		if planSignature(before[mid]) != planSignature(&after) {
			scope := a.scopeForMatch(mid)
			scope.Venues = dedupeSorted(append(scope.Venues, c.Venue))
			p, err := a.propose(trigger.Seq, mid, reasonClosure, reason, scope, before[mid], after, idem)
			if err != nil {
				return IdemResult{}, err
			}
			pids = append(pids, p)
		}
	}
	return a.result(idem, firstProposal(pids)), nil
}

// EndClosure 解除场地关闭。
func (a *App) EndClosure(actor, idem, closureID string) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	c := a.state.closures[closureID]
	if c == nil {
		return IdemResult{}, fmt.Errorf("%w: 关闭单 %s", ErrNotFound, closureID)
	}
	if !c.Active {
		return IdemResult{}, fmt.Errorf("%w: 关闭单 %s 已解除", ErrConflict, closureID)
	}
	payload := struct {
		ID  string `json:"编号"`
		End string `json:"解除时间"`
	}{closureID, a.clock()}
	if _, err := a.appendEvent(evClosureEnded, actor, idem, payload); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

// ---------- 观众行程意向 ----------

type IntentCmd struct {
	Ref       string        `json:"观众标识"`
	MatchID   string        `json:"场次"`
	Revision  int           `json:"修订号"`
	People    int           `json:"人数"`
	ArriveAt  string        `json:"到达时间"`
	ArriveBus bool          `json:"到达需接驳"`
	LeaveAt   string        `json:"离开时间"`
	LeaveBus  bool          `json:"离开需接驳"`
	CheckIn   string        `json:"入住日期"`
	CheckOut  string        `json:"退房日期"`
	Routes    []RouteChoice `json:"线路"`
}

// SubmitIntent 接收最小化行程意向；同一观众同一场次的重复提交必须带递增修订号。
func (a *App) SubmitIntent(actor, idem string, c IntentCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	if err := requireText("观众标识", c.Ref); err != nil {
		return IdemResult{}, err
	}
	m := a.state.matches[c.MatchID]
	if m == nil {
		return IdemResult{}, fmt.Errorf("%w: 场次 %s", ErrNotFound, c.MatchID)
	}
	if c.Revision < 1 {
		return IdemResult{}, fmt.Errorf("修订号必须从 1 开始")
	}
	if c.People <= 0 {
		return IdemResult{}, fmt.Errorf("人数必须为正数")
	}
	arrive, err1 := parseCivil(c.ArriveAt)
	leave, err2 := parseCivil(c.LeaveAt)
	if err1 != nil || err2 != nil {
		return IdemResult{}, fmt.Errorf("到达/离开时间格式应为 2006-01-02T15:04:05")
	}
	if !arrive.Before(leave) {
		return IdemResult{}, fmt.Errorf("到达时间必须早于离开时间")
	}
	if c.CheckIn != "" || c.CheckOut != "" {
		in, e1 := parseCivil(c.CheckIn + "T00:00:00")
		out, e2 := parseCivil(c.CheckOut + "T00:00:00")
		if e1 != nil || e2 != nil {
			return IdemResult{}, fmt.Errorf("入住/退房日期格式应为 2006-01-02")
		}
		if !in.Before(out) {
			return IdemResult{}, fmt.Errorf("入住日期必须早于退房日期（跨日住宿至少 1 晚）")
		}
	}
	for _, rc := range c.Routes {
		if a.state.routes[rc.Route] == nil {
			return IdemResult{}, fmt.Errorf("%w: 线路 %s", ErrNotFound, rc.Route)
		}
		if _, err := parseCivil(rc.Date + "T00:00:00"); err != nil {
			return IdemResult{}, fmt.Errorf("线路日期格式应为 2006-01-02")
		}
	}
	if prev := a.state.intents[c.Ref+"\x00"+c.MatchID]; prev != nil {
		if c.Revision <= prev.Revision {
			return IdemResult{}, fmt.Errorf("%w: 观众 %s 对场次 %s 的最新修订号为 %d，重复提交必须递增",
				ErrConflict, c.Ref, c.MatchID, prev.Revision)
		}
	}
	in := Intent{
		Ref: c.Ref, MatchID: c.MatchID, Revision: c.Revision, People: c.People,
		ArriveAt: c.ArriveAt, ArriveBus: c.ArriveBus,
		LeaveAt: c.LeaveAt, LeaveBus: c.LeaveBus,
		CheckIn: c.CheckIn, CheckOut: c.CheckOut, Routes: c.Routes,
		SubmittedAt: a.clock(),
	}
	if in.Routes == nil {
		in.Routes = []RouteChoice{}
	}
	if _, err := a.appendEvent(evIntentReceived, actor, idem, in); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

// ---------- 方案确认 ----------

type DecisionCmd struct {
	ProposalID string `json:"改线建议"`
	Decision   string `json:"决定"`
	Opinion    string `json:"意见"`
}

// DecideProposal 记录人工对自动改线建议的确认或驳回；重复决定一律冲突。
func (a *App) DecideProposal(actor, idem string, c DecisionCmd) (IdemResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, dup := a.begin(idem); dup {
		return r, nil
	}
	pr := a.state.proposals[c.ProposalID]
	if pr == nil {
		return IdemResult{}, fmt.Errorf("%w: 改线建议 %s", ErrNotFound, c.ProposalID)
	}
	if c.Decision != "确认" && c.Decision != "驳回" {
		return IdemResult{}, fmt.Errorf("决定必须为 确认/驳回")
	}
	if pr.Status != "待确认" {
		return IdemResult{}, fmt.Errorf("%w: 改线建议 %s 已由 %s %s", ErrConflict, c.ProposalID, pr.ConfirmedBy, pr.Status)
	}
	d := Decision{ProposalID: c.ProposalID, Decision: c.Decision, Operator: actor, Opinion: c.Opinion, At: a.clock()}
	if _, err := a.appendEvent(evPlanConfirmed, actor, idem, d); err != nil {
		return IdemResult{}, err
	}
	return a.result(idem, ""), nil
}

// ---------- 查询 ----------

func (a *App) Gap(matchID string) (GapReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return GapAnalysis(a.state, matchID)
}

func (a *App) Recommend(matchID string) (Plan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	plan, err := GeneratePlan(a.state, matchID)
	if err != nil {
		return Plan{}, err
	}
	plan.GeneratedAt = a.clock()
	return plan, nil
}

func (a *App) ProposalView(id string) (Proposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pr := a.state.proposals[id]
	if pr == nil {
		return Proposal{}, fmt.Errorf("%w: 改线建议 %s", ErrNotFound, id)
	}
	return *pr, nil
}

func (a *App) Proposals() []Proposal {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Proposal, 0, len(a.state.proposalIDs))
	for _, id := range a.state.proposalIDs {
		out = append(out, *a.state.proposals[id])
	}
	return out
}

// IntentsForMatch 返回场次下的观众意向（最小化字段）；调用方必须已完成岗位鉴权。
func (a *App) IntentsForMatch(matchID string) ([]Intent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.matches[matchID] == nil {
		return nil, fmt.Errorf("%w: 场次 %s", ErrNotFound, matchID)
	}
	return a.state.intentsForMatch(matchID), nil
}

// AuditAccess 为个人行程查询追加审计事件。
func (a *App) AuditAccess(actor, action, condition string, count int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload := struct {
		Actor     string `json:"岗位"`
		Action    string `json:"动作"`
		Condition string `json:"查询条件"`
		Count     int    `json:"返回条数"`
	}{actor, action, condition, count}
	_, err := a.appendEvent(evAccessAudited, actor, "", payload)
	return err
}

// ---------- 自动改线内部逻辑 ----------

// planSignature 忽略生成时间戳，对编排内容做稳定摘要，用于判断方案是否真的变化。
func planSignature(p *Plan) string {
	if p == nil {
		return ""
	}
	core := struct {
		Attendance  []AttendanceSlot `json:"观赛保障"`
		Assignments []Assignment     `json:"编排"`
		Unmet       []Unmet          `json:"未满足"`
	}{p.Attendance, p.Assignments, p.Unmet}
	raw, _ := json.Marshal(core)
	sum := sha256.Sum256(canonicalPayload(raw))
	return hex.EncodeToString(sum[:])
}

func (a *App) propose(triggerSeq int64, matchID, category, reason string, scope ImpactScope, before *Plan, after Plan, idemCmd string) (string, error) {
	n := len(a.state.proposalIDs) + 1
	id := fmt.Sprintf("chg-%06d", n)
	after.GeneratedAt = a.clock()
	pr := Proposal{
		ID: id, TriggerSeq: triggerSeq, Category: category, Reason: reason,
		Scope: scope, Before: before, Suggested: after,
		Status: "待确认", CreatedAt: a.clock(),
	}
	if _, err := a.appendEvent(evRerouteProposed, "系统自动", idemCmd, pr); err != nil {
		return "", err
	}
	if idemCmd != "" {
		if _, ok := a.idemProposal[idemCmd]; !ok {
			a.idemProposal[idemCmd] = id
		}
	}
	return id, nil
}

// result 汇总命令的写入结果；pid 为该命令产生的首条改线建议。
func (a *App) result(idem, pid string) IdemResult {
	seq := int64(len(a.store.Events()))
	if idem != "" {
		seq = a.idemAnchor[idem]
		pid = a.idemProposal[idem]
	}
	return IdemResult{AnchorSeq: seq, ProposalID: pid}
}

func (a *App) scopeForMatch(matchID string) ImpactScope {
	scope := ImpactScope{Matches: []string{matchID}, Audience: []string{}, Shuttles: []string{}, Partners: []string{}, Venues: []string{}}
	for _, it := range a.state.intentsForMatch(matchID) {
		scope.Audience = append(scope.Audience, it.Ref)
	}
	m := a.state.matches[matchID]
	for _, id := range a.state.shuttleIDs {
		t := a.state.shuttles[id]
		if t.From == m.Venue || t.To == m.Venue {
			scope.Shuttles = append(scope.Shuttles, id)
		}
	}
	scope.Audience = dedupeSorted(scope.Audience)
	scope.Shuttles = dedupeSorted(scope.Shuttles)
	return scope
}

// matchesStayingNight 返回有观众在指定夜住宿的场次（确定性排序）。
func (a *App) matchesStayingNight(night string) []string {
	set := map[string]bool{}
	for _, k := range a.state.intentKeys {
		it := a.state.intents[k]
		for _, n := range it.nights() {
			if n == night {
				set[it.MatchID] = true
			}
		}
	}
	return sortedMatchSet(set)
}

// rerouteForLateCapacity 用迟报前固定的 before 方案与新容量下的重算结果比较。
func (a *App) rerouteForLateCapacity(triggerSeq int64, snap Capacity, reason string, before map[string]*Plan) ([]string, error) {
	var pids []string
	for _, mid := range sortedKeysOf(before) {
		after, err := GeneratePlan(a.state, mid)
		if err != nil {
			return nil, err
		}
		if planSignature(before[mid]) != planSignature(&after) {
			scope := a.scopeForMatch(mid)
			scope.Partners = dedupeSorted(append(scope.Partners, snap.Partner))
			p, err := a.propose(triggerSeq, mid, reasonLateReport, reason, scope, before[mid], after, "")
			if err != nil {
				return nil, err
			}
			pids = append(pids, p)
		}
	}
	return pids, nil
}

func (a *App) rerouteForPartnerSuspension(pid, reason string, before map[string]*Plan) ([]string, error) {
	trigger := int64(len(a.store.Events()))
	var pids []string
	for _, mid := range sortedKeysOf(before) {
		after, err := GeneratePlan(a.state, mid)
		if err != nil {
			return nil, err
		}
		scope := a.scopeForMatch(mid)
		scope.Partners = dedupeSorted(append(scope.Partners, pid))
		p, err := a.propose(trigger, mid, reasonPartner, reason, scope, before[mid], after, "")
		if err != nil {
			return nil, err
		}
		pids = append(pids, p)
	}
	return pids, nil
}

func firstProposal(pids []string) string {
	if len(pids) > 0 {
		return pids[0]
	}
	return ""
}

func sortedKeysOf(m map[string]*Plan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedMatchSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	sort.Strings(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
