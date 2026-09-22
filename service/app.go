// Package service 是赛事停留链路的应用服务：解析命令、校验领域规则、
// 追加事件并折演内存状态。赛程改期、临时关闭与合作方迟报会触发自动改线，
// 每次自动改线都生成带原因与影响范围的改线单，等待人工确认。
package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/beryl0222/event-stay-orchestrator/domain"
	"github.com/beryl0222/event-stay-orchestrator/internal/store"
)

// Clock 是可注入的时钟；测试中使用固定时钟以保证结果可复现。
type Clock func() time.Time

// App 持有事件日志与折演状态。所有写操作经命令串行化。
type App struct {
	mu    sync.Mutex
	log   *store.EventLog
	state *domain.State
	now   Clock
	newID func(prefix string) string
}

// New 打开日志并重放全部事件重建状态。
func New(log *store.EventLog, clock Clock) (*App, error) {
	if clock == nil {
		clock = func() time.Time { return time.Now().In(domain.Local) }
	}
	app := &App{
		log:   log,
		state: domain.NewState(),
		now:   clock,
		newID: randomID,
	}
	if err := log.Replay(func(e domain.Event) error { return app.state.Apply(e) }); err != nil {
		return nil, fmt.Errorf("状态重建失败: %w", err)
	}
	return app, nil
}

// SetIDGenerator 仅供测试替换为确定性 ID 生成器。
func (a *App) SetIDGenerator(f func(prefix string) string) { a.newID = f }

func randomID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// State 返回当前状态快照的只读使用入口（调用方不得修改）。
func (a *App) State() *domain.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// append 追加事件并立即折演，事件中的时间与操作人在此统一注入。
func (a *App) append(eventType, actor string, payload any) (domain.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, err
	}
	e := domain.Event{
		ID: a.newID("ev_"), Type: eventType, At: a.now().In(domain.Local),
		Actor: actor, Payload: raw,
	}
	saved, err := a.log.Append(e)
	if err != nil {
		return saved, err
	}
	if err := a.state.Apply(saved); err != nil {
		return saved, fmt.Errorf("事件折演失败（日志已持久化）: %w", err)
	}
	return saved, nil
}

// ----------------------------------------------------------------------------
// 基础资源命令
// ----------------------------------------------------------------------------

// RegisterVenue 登记场馆（承载等级决定座席上限）。
func (a *App) RegisterVenue(actor string, v domain.Venue) (*domain.Venue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, err := domain.ValidateVenue(v)
	if err != nil {
		return nil, err
	}
	if _, exists := a.state.Venues[v.ID]; exists {
		return nil, &domain.ValidationError{Msg: "场馆已存在: " + v.ID}
	}
	if _, err := a.append(domain.EvVenueRegistered, actor, domain.VenueRegistered{Venue: v}); err != nil {
		return nil, err
	}
	return &v, nil
}

// ScheduleSessionInput 赛程发布入参。
type ScheduleSessionInput struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	VenueID string `json:"venue_id"`
	Start   string `json:"start"`
	End     string `json:"end"`
}

// ScheduleSession 发布赛程。
func (a *App) ScheduleSession(actor string, in ScheduleSessionInput) (*domain.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	start, end, err := parseWindow(in.Start, in.End)
	if err != nil {
		return nil, err
	}
	if _, ok := a.state.Venues[in.VenueID]; !ok {
		return nil, &domain.ValidationError{Msg: "场馆不存在: " + in.VenueID}
	}
	if _, exists := a.state.Sessions[in.ID]; exists {
		return nil, &domain.ValidationError{Msg: "场次已存在: " + in.ID}
	}
	sess := domain.Session{ID: in.ID, Name: in.Name, VenueID: in.VenueID, Start: start, End: end}
	if _, err := a.append(domain.EvSessionScheduled, actor, domain.SessionScheduled{Session: sess}); err != nil {
		return nil, err
	}
	return a.state.Sessions[in.ID], nil
}

// RegisterPartner 登记合作方。
func (a *App) RegisterPartner(actor string, p domain.Partner) (*domain.Partner, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.ID == "" {
		return nil, &domain.ValidationError{Msg: "缺少合作方标识"}
	}
	if p.Kind != domain.PartnerHotel && p.Kind != domain.PartnerShuttle && p.Kind != domain.PartnerAttraction {
		return nil, &domain.ValidationError{Msg: "合作方类型须为 hotel/shuttle/attraction"}
	}
	if p.Status == "" {
		p.Status = domain.PartnerActive
	}
	if p.Status != domain.PartnerActive && p.Status != domain.PartnerSuspended {
		return nil, &domain.ValidationError{Msg: "合作方状态须为 active/suspended"}
	}
	if _, exists := a.state.Partners[p.ID]; exists {
		return nil, &domain.ValidationError{Msg: "合作方已存在: " + p.ID}
	}
	if _, err := a.append(domain.EvPartnerRegistered, actor, domain.PartnerRegistered{Partner: p}); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPartnerStatus 暂停或恢复合作方；暂停后其资源不参与编排。
func (a *App) SetPartnerStatus(actor, partnerID string, status domain.PartnerStatus, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Partners[partnerID]; !ok {
		return &domain.NotFoundError{What: "合作方", ID: partnerID}
	}
	if status != domain.PartnerActive && status != domain.PartnerSuspended {
		return &domain.ValidationError{Msg: "合作方状态须为 active/suspended"}
	}
	_, err := a.append(domain.EvPartnerStatusChanged, actor,
		domain.PartnerStatusChanged{PartnerID: partnerID, Status: status, Reason: reason})
	return err
}

// ScheduleRun 登记接驳班次（须符合交通时窗）。
func (a *App) ScheduleRun(actor string, in domain.ParseRunInput) (*domain.ShuttleRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := domain.ParseRun(in)
	if err != nil {
		return nil, err
	}
	if _, ok := a.state.Partners[r.PartnerID]; !ok {
		return nil, &domain.ValidationError{Msg: "合作方不存在: " + r.PartnerID}
	}
	if a.state.Partners[r.PartnerID].Kind != domain.PartnerShuttle {
		return nil, &domain.ValidationError{Msg: "仅接驳合作方可登记班次"}
	}
	if _, ok := a.state.Venues[r.VenueID]; !ok {
		return nil, &domain.ValidationError{Msg: "场馆不存在: " + r.VenueID}
	}
	if _, exists := a.state.Runs[r.ID]; exists {
		return nil, &domain.ValidationError{Msg: "班次已存在: " + r.ID}
	}
	if _, err := a.append(domain.EvRunScheduled, actor, domain.RunScheduled{Run: r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// CancelRun 取消班次并为受影响场次触发自动改线。
func (a *App) CancelRun(actor, runID, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.state.Runs[runID]
	if !ok {
		return &domain.NotFoundError{What: "班次", ID: runID}
	}
	if !r.Active {
		return &domain.ValidationError{Msg: "班次已取消: " + runID}
	}
	if _, err := a.append(domain.EvRunCancelled, actor, domain.RunCancelled{RunID: runID}); err != nil {
		return err
	}
	return a.autoRerouteVenue(r.VenueID, "接驳班次 "+runID+" 取消："+reason, actor)
}

// RecordSnapshot 记录住宿容量快照；迟报（晚于时限）会标记并触发相关场次复核。
func (a *App) RecordSnapshot(actor string, in domain.SnapshotInput) (*domain.HotelSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Partners[in.PartnerID]; !ok {
		return nil, &domain.ValidationError{Msg: "合作方不存在: " + in.PartnerID}
	}
	if a.state.Partners[in.PartnerID].Kind != domain.PartnerHotel {
		return nil, &domain.ValidationError{Msg: "仅住宿合作方可报送客房容量"}
	}
	snap, err := domain.ParseSnapshot(in, a.now())
	if err != nil {
		return nil, err
	}
	key := in.PartnerID + "|" + in.Date
	if _, exists := a.state.Snapshots[key]; exists {
		return nil, &domain.ValidationError{Msg: "该房夜容量已报送，口径以首次报送为准"}
	}
	if _, err := a.append(domain.EvSnapshotRecorded, actor, domain.SnapshotRecorded{Snapshot: snap}); err != nil {
		return nil, err
	}
	if snap.Late {
		if err := a.autoRerouteForNight(in.Date, in.PartnerID, actor); err != nil {
			return nil, err
		}
	}
	cp := a.state.Snapshots[key]
	return &cp, nil
}

// RegisterRoute 登记主题线路。
func (a *App) RegisterRoute(actor string, r domain.ThemeRoute) (*domain.ThemeRoute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.ID == "" || r.DailyCapacity <= 0 {
		return nil, &domain.ValidationError{Msg: "线路标识与每日容量必填且容量大于 0"}
	}
	if _, ok := a.state.Partners[r.PartnerID]; !ok {
		return nil, &domain.ValidationError{Msg: "合作方不存在: " + r.PartnerID}
	}
	if a.state.Partners[r.PartnerID].Kind != domain.PartnerAttraction {
		return nil, &domain.ValidationError{Msg: "仅文旅合作方可登记主题线路"}
	}
	if _, err := a.append(domain.EvRouteRegistered, actor, domain.RouteRegistered{Route: r}); err != nil {
		return nil, err
	}
	return &r, nil
}

// SetRouteCapacity 调整线路某日槽位。
func (a *App) SetRouteCapacity(actor, routeID, date string, capacity int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Routes[routeID]; !ok {
		return &domain.NotFoundError{What: "主题线路", ID: routeID}
	}
	if capacity < 0 {
		return &domain.ValidationError{Msg: "容量不得为负"}
	}
	if _, err := time.ParseInLocation("2006-01-02", date, domain.Local); err != nil {
		return &domain.ValidationError{Msg: "日期格式应为 YYYY-MM-DD"}
	}
	_, err := a.append(domain.EvRouteCapacitySet, actor,
		domain.RouteCapacitySet{RouteID: routeID, Date: date, Capacity: capacity})
	return err
}

// ----------------------------------------------------------------------------
// 观众意向（最小化字段 + 幂等 + 重复提交取代）
// ----------------------------------------------------------------------------

// SubmitIntentResult 说明提交处理结果。
type SubmitIntentResult struct {
	Intent   *domain.Intent `json:"intent"`
	Deduped  bool           `json:"deduped"`            // 幂等命中，未产生新事件
	Replaced bool           `json:"replaced,omitempty"` // 取代了该观众的旧意向
}

// SubmitIntent 接收观众行程意向。
// 同一观众携带相同幂等键重放：直接返回既有版本（deduped）；
// 携带新键：视为修订，旧版本保留但标记 superseded。
func (a *App) SubmitIntent(in domain.IntentInput) (*SubmitIntentResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	intent, err := domain.ParseIntent(in)
	if err != nil {
		return nil, err
	}
	if _, ok := a.state.Sessions[intent.SessionID]; !ok {
		return nil, &domain.ValidationError{Msg: "场次不存在: " + intent.SessionID}
	}
	// 幂等：同一访客同一键，内容必须一致，否则视为重复提交冲突。
	if vers := a.state.IntentVersions[intent.VisitorToken]; vers != nil {
		if prior, ok := vers[intent.IdempotencyKey]; ok {
			if sameIntent(*prior, intent) {
				return &SubmitIntentResult{Intent: prior, Deduped: true}, nil
			}
			return nil, &domain.ValidationError{Msg: "同一幂等键重复提交且内容不一致"}
		}
	}
	intent.Version = 1
	if _, has := a.state.IntentLatest[intent.VisitorToken]; has {
		intent.Version = a.state.IntentVersions[intent.VisitorToken][a.state.IntentLatest[intent.VisitorToken]].Version + 1
	}
	intent.SubmittedAt = a.now().In(domain.Local)
	if _, err := a.append(domain.EvIntentSubmitted, "visitor:"+intent.VisitorToken,
		domain.IntentSubmitted{Intent: intent}); err != nil {
		return nil, err
	}
	// 新意向可能改变接待缺口与既有方案，触发该场次复核。
	err = a.autoRerouteVenue(a.state.Sessions[intent.SessionID].VenueID,
		"观众 "+intent.VisitorToken+" 更新行程意向", "visitor:"+intent.VisitorToken)
	return &SubmitIntentResult{
		Intent:   a.state.Intents[intent.IdempotencyKey+"|"+intent.VisitorToken],
		Replaced: intent.Version > 1,
	}, err
}

func sameIntent(a, b domain.Intent) bool {
	return a.SessionID == b.SessionID && a.Pax == b.Pax &&
		a.ArriveFrom.Equal(b.ArriveFrom) && a.ArriveTo.Equal(b.ArriveTo) &&
		a.DepartFrom.Equal(b.DepartFrom) && a.DepartTo.Equal(b.DepartTo)
}

// ----------------------------------------------------------------------------
// 赛程改期与临时关闭
// ----------------------------------------------------------------------------

// MoveSessionInput 赛程改期入参。
type MoveSessionInput struct {
	VenueID string `json:"venue_id"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Reason  string `json:"reason"`
}

// MoveSession 执行赛程改期/换馆，并对停留链路自动改线（待人工确认）。
func (a *App) MoveSession(actor, sessionID string, in MoveSessionInput) (*domain.Reroute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.state.Sessions[sessionID]
	if !ok {
		return nil, &domain.NotFoundError{What: "场次", ID: sessionID}
	}
	if in.Reason == "" {
		return nil, &domain.ValidationError{Msg: "改期必须说明原因"}
	}
	targetVenue := in.VenueID
	if targetVenue == "" {
		targetVenue = sess.VenueID
	}
	if _, ok := a.state.Venues[targetVenue]; !ok {
		return nil, &domain.ValidationError{Msg: "场馆不存在: " + targetVenue}
	}
	start, end, err := parseWindow(in.Start, in.End)
	if err != nil {
		return nil, err
	}
	if sess.VenueID == targetVenue && sess.Start.Equal(start) && sess.End.Equal(end) {
		return nil, &domain.ValidationError{Msg: "新排期与现行排期一致"}
	}
	// 改期/换馆不立即落入赛程：以目标窗口重算停留链路并立改线单，待人工确认后生效。
	// 即使尚无既有方案或链路内容未变，改期本身也必须确认，故强制立单。
	target := domain.SessionTarget{VenueID: targetVenue, Start: start, End: end}
	return a.proposeRerouteForce(sessionID, target, "赛程改期："+in.Reason, actor)
}

// ClosureInput 临时关闭入参。
type ClosureInput struct {
	ID      string `json:"id"`
	VenueID string `json:"venue_id"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Reason  string `json:"reason"`
}

// ScheduleClosure 登记场馆临时关闭，并为受影响场次自动生成改线提议：
// 优先选择承载等级不低于原馆、容量足够的替代场馆；确认后才正式换馆。
func (a *App) ScheduleClosure(actor string, in ClosureInput) ([]*domain.Reroute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	start, end, err := parseWindow(in.Start, in.End)
	if err != nil {
		return nil, err
	}
	if in.Reason == "" {
		return nil, &domain.ValidationError{Msg: "临时关闭必须说明原因"}
	}
	if _, ok := a.state.Venues[in.VenueID]; !ok {
		return nil, &domain.ValidationError{Msg: "场馆不存在: " + in.VenueID}
	}
	if in.ID == "" {
		in.ID = a.newID("cl_")
	}
	if _, exists := a.state.Closures[in.ID]; exists {
		return nil, &domain.ValidationError{Msg: "关闭单已存在: " + in.ID}
	}
	closure := domain.Closure{ID: in.ID, VenueID: in.VenueID, Start: start, End: end, Reason: in.Reason}
	if _, err := a.append(domain.EvClosureScheduled, actor, domain.ClosureScheduled{Closure: closure}); err != nil {
		return nil, err
	}
	var reroutes []*domain.Reroute
	for _, sessID := range a.sortedSessionIDs() {
		sess := a.state.Sessions[sessID]
		if sess.VenueID != in.VenueID || !windowsOverlap(sess.Start, sess.End, start, end) {
			continue
		}
		if a.state.Proposals[sessID] == nil && len(a.state.ActiveIntents(sessID)) == 0 {
			continue // 尚无接待链路，无需此刻立改线单
		}
		target := domain.SessionTarget{VenueID: in.VenueID, Start: sess.Start, End: sess.End}
		changes := []string{}
		if alt := a.pickAlternativeVenue(sess); alt != nil {
			target.VenueID = alt.ID
			changes = append(changes, fmt.Sprintf("拟换馆 %s→%s（%s）", sess.VenueID, alt.ID, alt.Name))
		} else {
			changes = append(changes, "无满足承载等级的替代场馆，存在座席缺口")
		}
		// 闭馆处置必须由人工确认，即使新链路内容恰好未变也强制立单。
		rr, err := a.proposeRerouteWith(sessID, target, "场馆临时关闭："+in.Reason, actor, changes, true)
		if err != nil {
			return nil, err
		}
		if rr != nil {
			reroutes = append(reroutes, rr)
		}
	}
	return reroutes, nil
}

// CancelClosure 解除临时关闭。
func (a *App) CancelClosure(actor, closureID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.state.Closures[closureID]; !ok {
		return &domain.NotFoundError{What: "关闭单", ID: closureID}
	}
	_, err := a.append(domain.EvClosureCancelled, actor, domain.ClosureCancelled{ClosureID: closureID})
	return err
}

// tierRank 承载等级序（S 最高）。
func tierRank(t domain.CapacityTier) int {
	switch t {
	case domain.TierS:
		return 3
	case domain.TierA:
		return 2
	default:
		return 1
	}
}

// pickAlternativeVenue 按确定性规则选择替代场馆：
// 承载等级不低于原馆、窗口内不闭馆、座席足够；同等级优先、座席适度、ID 最小。
func (a *App) pickAlternativeVenue(sess *domain.Session) *domain.Venue {
	cur := a.state.Venues[sess.VenueID]
	demand := 0
	for _, in := range a.state.ActiveIntents(sess.ID) {
		demand += in.Pax
	}
	var candidates []*domain.Venue
	for _, v := range a.state.Venues {
		if v.ID == sess.VenueID {
			continue
		}
		if cur != nil && tierRank(v.Tier) < tierRank(cur.Tier) {
			continue
		}
		if v.Seats < demand {
			continue
		}
		if closed, _ := a.state.VenueClosedAt(v.ID, sess.Start); closed {
			continue
		}
		candidates = append(candidates, v)
	}
	sortVenues(candidates, cur)
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

// ----------------------------------------------------------------------------
// 方案生成、自动改线与人工确认
// ----------------------------------------------------------------------------

// PlanSession 为场次生成当前排期下的推荐方案（显式触发）。
func (a *App) PlanSession(sessionID string) (*domain.Proposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.state.Sessions[sessionID]
	if !ok {
		return nil, &domain.NotFoundError{What: "场次", ID: sessionID}
	}
	target := domain.SessionTarget{VenueID: sess.VenueID, Start: sess.Start, End: sess.End}
	prop, err := domain.Plan(a.state, sessionID, target, a.now())
	if err != nil {
		return nil, err
	}
	if old := a.state.Proposals[sessionID]; old != nil {
		prop.Supersedes = old.Digest
	}
	if _, err := a.append(domain.EvProposalPublished, "planner",
		domain.ProposalPublished{Proposal: *prop}); err != nil {
		return nil, err
	}
	return prop, nil
}

// proposeReroute 按目标窗口重算并（在方案确有变化时）生成待确认改线单。
func (a *App) proposeReroute(sessionID string, target domain.SessionTarget, reason, actor string) (*domain.Reroute, error) {
	return a.proposeRerouteWith(sessionID, target, reason, actor, nil, false)
}

// proposeRerouteForce 无论链路是否变化都生成改线单（赛程改期本身需要人工确认）。
func (a *App) proposeRerouteForce(sessionID string, target domain.SessionTarget, reason, actor string) (*domain.Reroute, error) {
	return a.proposeRerouteWith(sessionID, target, reason, actor, nil, true)
}

func (a *App) proposeRerouteWith(sessionID string, target domain.SessionTarget,
	reason, actor string, changes []string, force bool) (*domain.Reroute, error) {
	old := a.state.Proposals[sessionID]
	prop, err := domain.Plan(a.state, sessionID, target, a.now())
	if err != nil {
		return nil, err
	}
	// 既无既有方案也不强制立单：首版编排只发布方案。
	if old == nil && !force {
		if _, err := a.append(domain.EvProposalPublished, actor,
			domain.ProposalPublished{Proposal: *prop}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	// 非强制场景下，链路内容未变则不立单。
	if old != nil && !force && old.Digest == prop.Digest {
		return nil, nil
	}
	rrID := a.newID("rr_")
	prop.RerouteID = rrID
	if old != nil {
		prop.Supersedes = old.Digest
	}
	scope := domain.AffectedScope(old, prop)

	sess := a.state.Sessions[sessionID]
	if len(changes) == 0 {
		if sess.VenueID != target.VenueID {
			changes = append(changes, fmt.Sprintf("换馆 %s→%s", sess.VenueID, target.VenueID))
		}
		if !sess.Start.Equal(target.Start) || !sess.End.Equal(target.End) {
			changes = append(changes, fmt.Sprintf("时间调整为 %s~%s",
				target.Start.In(domain.Local).Format("01-02 15:04"),
				target.End.In(domain.Local).Format("01-02 15:04")))
		}
	}
	changes = append(changes,
		fmt.Sprintf("影响观众 %d 人、酒店 %d 家、班次 %d 个、线路 %d 条，未满足项 %d 项",
			scope.VisitorCount, len(scope.AffectedHotels), len(scope.AffectedRuns),
			len(scope.AffectedRoutes), scope.Shortfall))

	rr := domain.Reroute{
		ID: rrID, SessionID: sessionID, Reason: reason, TriggerSeq: a.state.Seq + 1,
		Scope: scope, Digest: prop.Digest, Status: domain.RerouteProposed,
		CreatedAt: a.now().In(domain.Local), Target: target, Changes: changes,
	}
	if _, err := a.append(domain.EvProposalPublished, actor,
		domain.ProposalPublished{Proposal: *prop}); err != nil {
		return nil, err
	}
	if _, err := a.append(domain.EvRerouteProposed, actor,
		domain.RerouteProposedEvent{Reroute: rr}); err != nil {
		return nil, err
	}
	return &rr, nil
}

// autoRerouteVenue 对使用该场馆且已有意向/方案的场次按现行排期复核。
func (a *App) autoRerouteVenue(venueID, reason, actor string) error {
	for _, sessID := range a.sortedSessionIDs() {
		sess := a.state.Sessions[sessID]
		if sess.VenueID != venueID {
			continue
		}
		if len(a.state.ActiveIntents(sessID)) == 0 {
			continue
		}
		target := domain.SessionTarget{VenueID: sess.VenueID, Start: sess.Start, End: sess.End}
		if _, err := a.proposeReroute(sessID, target, reason, actor); err != nil {
			return err
		}
	}
	return nil
}

// autoRerouteForNight 因某房夜迟报而复核相关场次。
func (a *App) autoRerouteForNight(date, partnerID, actor string) error {
	for _, sessID := range a.sortedSessionIDs() {
		for _, in := range a.state.ActiveIntents(sessID) {
			for _, n := range nightsOf(in) {
				if n == date {
					sess := a.state.Sessions[sessID]
					target := domain.SessionTarget{VenueID: sess.VenueID, Start: sess.Start, End: sess.End}
					if _, err := a.proposeReroute(sessID, target,
						fmt.Sprintf("住宿合作方 %s 迟报 %s 房夜容量快照", partnerID, date), actor); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return nil
}

func nightsOf(in *domain.Intent) []string {
	cur := domain.LocalDate(in.ArriveFrom)
	end := domain.LocalDate(in.DepartTo)
	var out []string
	for cur != end && len(out) < 64 {
		out = append(out, cur)
		d, _ := time.ParseInLocation("2006-01-02", cur, domain.Local)
		cur = d.AddDate(0, 0, 1).Format("2006-01-02")
	}
	return out
}

// DecideReroute 人工确认/驳回改线。闭馆改线若含拟换馆，确认时正式生效换馆。
func (a *App) DecideReroute(actor, rerouteID, action, note string) (*domain.Reroute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rr, ok := a.state.Reroutes[rerouteID]
	if !ok {
		return nil, &domain.NotFoundError{What: "改线单", ID: rerouteID}
	}
	if rr.Status != domain.RerouteProposed {
		return nil, &domain.ValidationError{Msg: "改线单已处理: " + string(rr.Status)}
	}
	if action != string(domain.RerouteConfirmed) && action != string(domain.RerouteRejected) {
		return nil, &domain.ValidationError{Msg: "处理结论须为 confirmed 或 rejected"}
	}
	if action == string(domain.RerouteConfirmed) {
		// 改期/换馆在确认时才落入赛程（目标窗口与现行赛程任一不同即落事件）。
		sess := a.state.Sessions[rr.SessionID]
		if sess.VenueID != rr.Target.VenueID || !sess.Start.Equal(rr.Target.Start) ||
			!sess.End.Equal(rr.Target.End) {
			if _, err := a.append(domain.EvSessionMoved, actor, domain.SessionMoved{
				SessionID: rr.SessionID, FromVenueID: sess.VenueID, ToVenueID: rr.Target.VenueID,
				Start: rr.Target.Start, End: rr.Target.End,
				Reason: "确认改线单 " + rr.ID + "：" + rr.Reason, Version: sess.Version + 1,
			}); err != nil {
				return nil, err
			}
		}
	}
	if _, err := a.append(domain.EvRerouteDecided, actor, domain.RerouteDecided{
		RerouteID: rr.ID, Action: action, By: actor, Note: note, At: a.now().In(domain.Local),
	}); err != nil {
		return nil, err
	}
	return a.state.Reroutes[rr.ID], nil
}

// ----------------------------------------------------------------------------
// 查询
// ----------------------------------------------------------------------------

// GapReport 返回场次各时段接待缺口。
func (a *App) GapReport(sessionID string) (*domain.GapReport, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return domain.GapReportFor(a.state, sessionID, a.now())
}

// Proposal 返回场次当前推荐方案。
func (a *App) Proposal(sessionID string) (*domain.Proposal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.state.Proposals[sessionID]
	if !ok {
		return nil, &domain.NotFoundError{What: "方案", ID: sessionID}
	}
	return p, nil
}

// ReroutesOf 返回场次的全部改线记录（含原因、影响范围与人工确认）。
func (a *App) ReroutesOf(sessionID string) []*domain.Reroute {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.state.ReroutesOf[sessionID]
	cp := make([]*domain.Reroute, len(out))
	copy(cp, out)
	return cp
}

// Reroute 按 ID 取回改线单。
func (a *App) Reroute(id string) (*domain.Reroute, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.state.Reroutes[id]
	if !ok {
		return nil, &domain.NotFoundError{What: "改线单", ID: id}
	}
	return r, nil
}

// Intent 取回观众当前行程意向（个人行程，调用方须做岗位鉴权）。
func (a *App) Intent(token string) (*domain.Intent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key, ok := a.state.IntentLatest[token]
	if !ok {
		return nil, &domain.NotFoundError{What: "行程意向", ID: token}
	}
	return a.state.Intents[key+"|"+token], nil
}

// ReplayEvents 按序号输出事件流水（审计接口使用），不触碰内存状态。
func (a *App) ReplayEvents(handle func(domain.Event) error) error {
	return a.log.Replay(handle)
}

// Run 按 ID 取回班次。
func (a *App) Run(id string) (*domain.ShuttleRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.state.Runs[id]
	if !ok {
		return nil, &domain.NotFoundError{What: "班次", ID: id}
	}
	return r, nil
}

// RunBelongsTo 报告班次是否归属某合作方。
func (a *App) RunBelongsTo(runID, partnerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.state.Runs[runID]
	return ok && r.PartnerID == partnerID
}

// RouteBelongsTo 报告线路是否归属某合作方。
func (a *App) RouteBelongsTo(routeID, partnerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.state.Routes[routeID]
	return ok && r.PartnerID == partnerID
}
