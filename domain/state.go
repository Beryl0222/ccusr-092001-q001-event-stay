package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// State 是事件日志折演后的全部领域状态。所有读取与规划都基于 State 快照，
// 快照本身不持久化，可由事件日志在任意时刻完整重建。
type State struct {
	Seq int64

	Venues     map[string]*Venue
	Sessions   map[string]*Session
	Closures   map[string]*Closure
	Partners   map[string]*Partner
	Runs       map[string]*ShuttleRun
	Snapshots  map[string]HotelSnapshot // key: partnerID + "|" + date
	Routes     map[string]*ThemeRoute
	RouteCaps  map[string]int // key: routeID + "|" + date，按日覆盖
	Intents    map[string]*Intent
	Proposals  map[string]*Proposal // 各场次当前方案
	Reroutes   map[string]*Reroute
	ReroutesOf map[string][]*Reroute // sessionID -> 改线单（按创建顺序）

	// IntentLatest 与 IntentVersions 支撑“同一观众重复提交”的幂等与版本链。
	IntentLatest   map[string]string             // visitorToken -> 当前意向对应的 idempotencyKey
	IntentVersions map[string]map[string]*Intent // visitorToken -> key -> intent
}

// NewState 返回空状态。
func NewState() *State {
	return &State{
		Venues:         map[string]*Venue{},
		Sessions:       map[string]*Session{},
		Closures:       map[string]*Closure{},
		Partners:       map[string]*Partner{},
		Runs:           map[string]*ShuttleRun{},
		Snapshots:      map[string]HotelSnapshot{},
		Routes:         map[string]*ThemeRoute{},
		RouteCaps:      map[string]int{},
		Intents:        map[string]*Intent{},
		Proposals:      map[string]*Proposal{},
		Reroutes:       map[string]*Reroute{},
		ReroutesOf:     map[string][]*Reroute{},
		IntentLatest:   map[string]string{},
		IntentVersions: map[string]map[string]*Intent{},
	}
}

// Apply 将单个事件折演进状态。该函数是纯函数式更新：相同事件序列必然得到相同状态。
func (s *State) Apply(e Event) error {
	s.Seq = e.Seq
	switch e.Type {
	case EvVenueRegistered:
		var p VenueRegistered
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if _, ok := s.Venues[p.Venue.ID]; ok {
			return fmt.Errorf("场馆已存在: %s", p.Venue.ID)
		}
		v := p.Venue
		s.Venues[v.ID] = &v

	case EvSessionScheduled:
		var p SessionScheduled
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if _, ok := s.Sessions[p.Session.ID]; ok {
			return fmt.Errorf("场次已存在: %s", p.Session.ID)
		}
		sess := p.Session
		sess.Version = 1
		s.Sessions[sess.ID] = &sess

	case EvSessionMoved:
		var p SessionMoved
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		sess, ok := s.Sessions[p.SessionID]
		if !ok {
			return fmt.Errorf("场次不存在: %s", p.SessionID)
		}
		sess.VenueID = p.ToVenueID
		sess.Start = p.Start
		sess.End = p.End
		sess.Version = p.Version

	case EvClosureScheduled:
		var p ClosureScheduled
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		c := p.Closure
		s.Closures[c.ID] = &c

	case EvClosureCancelled:
		var p ClosureCancelled
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		delete(s.Closures, p.ClosureID)

	case EvPartnerRegistered:
		var p PartnerRegistered
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if _, ok := s.Partners[p.Partner.ID]; ok {
			return fmt.Errorf("合作方已存在: %s", p.Partner.ID)
		}
		partner := p.Partner
		s.Partners[partner.ID] = &partner

	case EvPartnerStatusChanged:
		var p PartnerStatusChanged
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		partner, ok := s.Partners[p.PartnerID]
		if !ok {
			return fmt.Errorf("合作方不存在: %s", p.PartnerID)
		}
		partner.Status = p.Status

	case EvRunScheduled:
		var p RunScheduled
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		r := p.Run
		s.Runs[r.ID] = &r

	case EvRunCancelled:
		var p RunCancelled
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if r, ok := s.Runs[p.RunID]; ok {
			r.Active = false
		}

	case EvSnapshotRecorded:
		var p SnapshotRecorded
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		key := p.Snapshot.PartnerID + "|" + p.Snapshot.Date
		// 同一房夜仅以首次报送为准（迟报不覆盖既有容量口径）。
		if _, exists := s.Snapshots[key]; !exists {
			s.Snapshots[key] = p.Snapshot
		}

	case EvRouteRegistered:
		var p RouteRegistered
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		r := p.Route
		s.Routes[r.ID] = &r

	case EvRouteCapacitySet:
		var p RouteCapacitySet
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		s.RouteCaps[p.RouteID+"|"+p.Date] = p.Capacity

	case EvIntentSubmitted:
		var p IntentSubmitted
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		in := p.Intent
		// 新版本取代旧版本：旧版本标记 Superseded，但保留可追溯。
		if priorKey, ok := s.IntentLatest[in.VisitorToken]; ok {
			if prior := s.Intents[priorKey+"|"+in.VisitorToken]; prior != nil {
				prior.Superseded = true
			}
		}
		s.IntentLatest[in.VisitorToken] = in.IdempotencyKey
		if s.IntentVersions[in.VisitorToken] == nil {
			s.IntentVersions[in.VisitorToken] = map[string]*Intent{}
		}
		cp := in
		s.IntentVersions[in.VisitorToken][in.IdempotencyKey] = &cp
		s.Intents[in.IdempotencyKey+"|"+in.VisitorToken] = &cp

	case EvProposalPublished:
		var p ProposalPublished
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		pr := p.Proposal
		s.Proposals[pr.SessionID] = &pr

	case EvRerouteProposed:
		var p RerouteProposedEvent
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		r := p.Reroute
		s.Reroutes[r.ID] = &r
		s.ReroutesOf[r.SessionID] = append(s.ReroutesOf[r.SessionID], &r)

	case EvRerouteDecided:
		var p RerouteDecided
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		r, ok := s.Reroutes[p.RerouteID]
		if !ok {
			return fmt.Errorf("改线单不存在: %s", p.RerouteID)
		}
		r.Status = RerouteStatus(p.Action)
		r.Decision = &Decision{Action: p.Action, By: p.By, At: p.At, Note: p.Note}
		if p.Action == string(RerouteConfirmed) {
			if prop, ok := s.Proposals[r.SessionID]; ok && prop.RerouteID == r.ID {
				prop.Confirmed = true
			}
		}

	default:
		return fmt.Errorf("未知事件类型: %s", e.Type)
	}
	return nil
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("空事件载荷")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("事件载荷解析失败: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// 查询辅助
// ----------------------------------------------------------------------------

// ActiveIntents 返回某场次全部未被取代的意向，按访客令牌排序保证确定性。
func (s *State) ActiveIntents(sessionID string) []*Intent {
	var out []*Intent
	for token, vers := range s.IntentVersions {
		latest := s.IntentLatest[token]
		in := vers[latest]
		if in == nil || in.Superseded || in.SessionID != sessionID {
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VisitorToken < out[j].VisitorToken })
	return out
}

// ActiveRuns 返回某场馆当前有效的班次，按发车时刻排序。
func (s *State) ActiveRuns(venueID string) []*ShuttleRun {
	var out []*ShuttleRun
	for _, r := range s.Runs {
		if r.Active && r.VenueID == venueID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Depart.Equal(out[j].Depart) {
			return out[i].ID < out[j].ID
		}
		return out[i].Depart.Before(out[j].Depart)
	})
	return out
}

// VenueClosedAt 报告场馆在 t 时刻是否处于临时关闭。
func (s *State) VenueClosedAt(venueID string, t time.Time) (bool, string) {
	for _, c := range s.Closures {
		if c.VenueID == venueID && !t.Before(c.Start) && t.Before(c.End) {
			return true, c.Reason
		}
	}
	return false, ""
}

// PartnerActive 报告合作方是否可参与编排。
func (s *State) PartnerActive(id string) bool {
	p, ok := s.Partners[id]
	return ok && p.Status == PartnerActive
}

// RouteCapacity 返回线路某日槽位（按日覆盖优先，否则用默认容量）。
func (s *State) RouteCapacity(routeID, date string) (int, bool) {
	if c, ok := s.RouteCaps[routeID+"|"+date]; ok {
		return c, true
	}
	if r, ok := s.Routes[routeID]; ok {
		return r.DailyCapacity, true
	}
	return 0, false
}

// SnapshotDue 返回某入住夜容量快照的报送时限（入住日前一天 18:00 本地）。
func SnapshotDue(roomNight string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", roomNight, Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("入住夜日期格式应为 YYYY-MM-DD: %w", err)
	}
	return time.Date(d.Year(), d.Month(), d.Day()-1, SnapshotDueHour, 0, 0, 0, Local), nil
}

// LocalDate 返回本地日期串。
func LocalDate(t time.Time) string {
	return t.In(Local).Format("2006-01-02")
}
