// Package domain 定义赛事停留链路协调的领域模型、事件折演与确定性规划算法。
//
// 所有跨日时间一律使用 Asia/Shanghai 本地时区（见 Local）；领域内不保存
// 姓名、证件号等实名信息，观众仅以一次性访客令牌 visitor_token 关联行程。
package domain

import (
	"encoding/json"
	"time"
)

// Local 是全系统统一使用的本地时区（UTC+8）。跨日时段一律按此时区切日。
var Local = time.FixedZone("UTC+8", 8*60*60)

// ----------------------------------------------------------------------------
// 基础约定：场馆承载等级
// ----------------------------------------------------------------------------

// CapacityTier 场馆承载等级。等级决定默认座席数，是场地替换的硬约束之一。
type CapacityTier string

const (
	TierS CapacityTier = "S" // 18000 座
	TierA CapacityTier = "A" // 9000 座
	TierB CapacityTier = "B" // 4000 座
)

// TierSeats 承载等级与默认座席数的约定。
var TierSeats = map[CapacityTier]int{
	TierS: 18000,
	TierA: 9000,
	TierB: 4000,
}

// ----------------------------------------------------------------------------
// 基础约定：合作方状态
// ----------------------------------------------------------------------------

type PartnerKind string

const (
	PartnerHotel      PartnerKind = "hotel"      // 住宿合作方
	PartnerShuttle    PartnerKind = "shuttle"    // 接驳合作方
	PartnerAttraction PartnerKind = "attraction" // 文旅（景区/线路）合作方
)

type PartnerStatus string

const (
	PartnerActive    PartnerStatus = "active"    // 正常合作
	PartnerSuspended PartnerStatus = "suspended" // 暂停合作，其资源不参与编排
)

// 基础约定：容量快照报送时限为入住日前一天 18:00（本地时间），晚于该时刻记为迟报。
const SnapshotDueHour = 18

// 基础约定：交通时窗。首班 06:00，末班发车 23:30；跨日班次到达截止次日 01:30。
const (
	FirstRunHour, FirstRunMin     = 6, 0
	LastDepartHour, LastDepartMin = 23, 30
	OvernightArriveHour           = 1
	OvernightArriveMin            = 30
)

// ----------------------------------------------------------------------------
// 实体
// ----------------------------------------------------------------------------

// Venue 场馆。
type Venue struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Tier  CapacityTier `json:"tier"`
	Seats int          `json:"seats"`
}

// Session 赛事场次。改期/换馆通过 Version 单调递增留痕。
type Session struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	VenueID string    `json:"venue_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Version int       `json:"version"`
}

// Closure 场馆临时关闭时段。
type Closure struct {
	ID      string    `json:"id"`
	VenueID string    `json:"venue_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Reason  string    `json:"reason"`
}

// Partner 合作方。
type Partner struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Kind   PartnerKind   `json:"kind"`
	Status PartnerStatus `json:"status"`
}

type RunKind string

const (
	RunInbound  RunKind = "inbound"  // 抵程：前往场馆
	RunOutbound RunKind = "outbound" // 返程：由场馆出发
)

// ShuttleRun 接驳班次。VenueID 对抵程为目的地、返程为出发地。
type ShuttleRun struct {
	ID        string    `json:"id"`
	PartnerID string    `json:"partner_id"`
	VenueID   string    `json:"venue_id"`
	Kind      RunKind   `json:"kind"`
	Depart    time.Time `json:"depart"`
	Arrive    time.Time `json:"arrive"`
	Capacity  int       `json:"capacity"`
	Active    bool      `json:"active"`
}

// HotelSnapshot 住宿容量快照（合作方按夜报送）。
type HotelSnapshot struct {
	PartnerID   string    `json:"partner_id"`
	Date        string    `json:"date"` // 入住夜，本地日期 YYYY-MM-DD
	TotalRooms  int       `json:"total_rooms"`
	BookedRooms int       `json:"booked_rooms"`
	RecordedAt  time.Time `json:"recorded_at"`
	ExpectedBy  time.Time `json:"expected_by"`
	Late        bool      `json:"late"`
}

// ThemeRoute 主题线路，按日放槽。
type ThemeRoute struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PartnerID     string `json:"partner_id"`
	DailyCapacity int    `json:"daily_capacity"` // 默认每日槽位
}

// Intent 观众行程意向（最小化字段：无姓名、无证件号）。
// 同一观众重复提交时以新版本取代旧版本，旧版本标记 Superseded。
type Intent struct {
	VisitorToken   string    `json:"visitor_token"`
	IdempotencyKey string    `json:"idempotency_key"`
	SessionID      string    `json:"session_id"`
	ArriveFrom     time.Time `json:"arrive_from"`
	ArriveTo       time.Time `json:"arrive_to"`
	DepartFrom     time.Time `json:"depart_from"`
	DepartTo       time.Time `json:"depart_to"`
	Pax            int       `json:"pax"` // 同行人数（含本人）
	Version        int       `json:"version"`
	SubmittedAt    time.Time `json:"submitted_at"`
	Superseded     bool      `json:"superseded"`
}

// AssignmentKind 停留链路段类型。
type AssignmentKind string

const (
	AssignHotel      AssignmentKind = "hotel"
	AssignShuttleIn  AssignmentKind = "shuttle_in"
	AssignShuttleOut AssignmentKind = "shuttle_out"
	AssignRoute      AssignmentKind = "route"
)

// Assignment 停留链路上的一段占位（酒店房夜 / 接驳座位 / 线路槽位）。
type Assignment struct {
	VisitorToken string         `json:"visitor_token"`
	Pax          int            `json:"pax"`
	Kind         AssignmentKind `json:"kind"`
	PartnerID    string         `json:"partner_id"`
	Reference    string         `json:"reference"` // 班次ID 或 线路ID
	Date         string         `json:"date"`      // 房夜/出游日（本地日期）
	Detail       string         `json:"detail"`
}

// UnmetNeed 规划中无法满足的需求，即接待缺口的逐人明细。
type UnmetNeed struct {
	VisitorToken string         `json:"visitor_token"`
	Pax          int            `json:"pax"`
	Kind         AssignmentKind `json:"kind"`
	Detail       string         `json:"detail"`
}

// SessionTarget 规划目标场次窗口。改线待确认期间，目标窗口可能与当前场次不同。
type SessionTarget struct {
	VenueID string    `json:"venue_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// Proposal 某场次的完整编排方案（含占位与未满足项），每次重算整体替换。
type Proposal struct {
	SessionID   string        `json:"session_id"`
	RerouteID   string        `json:"reroute_id,omitempty"`
	Target      SessionTarget `json:"target"`
	Items       []Assignment  `json:"items"`
	Unmet       []UnmetNeed   `json:"unmet"`
	Digest      string        `json:"digest"`
	GeneratedAt time.Time     `json:"generated_at"`
	Confirmed   bool          `json:"confirmed"`
	Supersedes  string        `json:"supersedes,omitempty"`
}

// RerouteStatus 改线单状态。
type RerouteStatus string

const (
	RerouteProposed  RerouteStatus = "proposed" // 待人工确认
	RerouteConfirmed RerouteStatus = "confirmed"
	RerouteRejected  RerouteStatus = "rejected"
)

// RerouteScope 改线影响范围。
type RerouteScope struct {
	VisitorCount   int      `json:"visitor_count"`
	Visitors       []string `json:"visitors"`
	AffectedHotels []string `json:"affected_hotels"`
	AffectedRuns   []string `json:"affected_runs"`
	AffectedRoutes []string `json:"affected_routes"`
	Shortfall      int      `json:"shortfall"`
}

// Decision 人工确认记录。
type Decision struct {
	Action string    `json:"action"` // confirmed / rejected
	By     string    `json:"by"`     // 岗位标识
	At     time.Time `json:"at"`
	Note   string    `json:"note"`
}

// Reroute 自动改线记录：原因、影响范围、目标窗口与人工确认结论全程留痕。
type Reroute struct {
	ID         string        `json:"id"`
	SessionID  string        `json:"session_id"`
	Reason     string        `json:"reason"`
	TriggerSeq int64         `json:"trigger_seq"`
	Scope      RerouteScope  `json:"scope"`
	Digest     string        `json:"digest"`
	Status     RerouteStatus `json:"status"`
	CreatedAt  time.Time     `json:"created_at"`
	Target     SessionTarget `json:"target"`
	Changes    []string      `json:"changes"`
	Decision   *Decision     `json:"decision,omitempty"`
}

// ----------------------------------------------------------------------------
// 事件信封与载荷
// ----------------------------------------------------------------------------

// Event 追加日志中的不可变事件。
type Event struct {
	Seq     int64           `json:"seq"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// 事件类型。
const (
	EvVenueRegistered      = "venue.registered"
	EvSessionScheduled     = "session.scheduled"
	EvSessionMoved         = "session.moved" // 改期或换馆生效
	EvClosureScheduled     = "closure.scheduled"
	EvClosureCancelled     = "closure.cancelled"
	EvPartnerRegistered    = "partner.registered"
	EvPartnerStatusChanged = "partner.status_changed"
	EvRunScheduled         = "run.scheduled"
	EvRunCancelled         = "run.cancelled"
	EvSnapshotRecorded     = "snapshot.recorded"
	EvRouteRegistered      = "route.registered"
	EvRouteCapacitySet     = "route.capacity_set"
	EvIntentSubmitted      = "intent.submitted"
	EvProposalPublished    = "proposal.published"
	EvRerouteProposed      = "reroute.proposed"
	EvRerouteDecided       = "reroute.decided"
)

type VenueRegistered struct{ Venue Venue }
type SessionScheduled struct{ Session Session }
type SessionMoved struct {
	SessionID   string    `json:"session_id"`
	FromVenueID string    `json:"from_venue_id"`
	ToVenueID   string    `json:"to_venue_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Reason      string    `json:"reason"`
	Version     int       `json:"version"`
}
type ClosureScheduled struct{ Closure Closure }
type ClosureCancelled struct {
	ClosureID string `json:"closure_id"`
}
type PartnerRegistered struct{ Partner Partner }
type PartnerStatusChanged struct {
	PartnerID string        `json:"partner_id"`
	Status    PartnerStatus `json:"status"`
	Reason    string        `json:"reason"`
}
type RunScheduled struct{ Run ShuttleRun }
type RunCancelled struct {
	RunID string `json:"run_id"`
}
type SnapshotRecorded struct{ Snapshot HotelSnapshot }
type RouteRegistered struct{ Route ThemeRoute }
type RouteCapacitySet struct {
	RouteID  string `json:"route_id"`
	Date     string `json:"date"`
	Capacity int    `json:"capacity"`
}
type IntentSubmitted struct{ Intent Intent }
type ProposalPublished struct{ Proposal Proposal }
type RerouteProposedEvent struct{ Reroute Reroute }
type RerouteDecided struct {
	RerouteID string    `json:"reroute_id"`
	Action    string    `json:"action"`
	By        string    `json:"by"`
	Note      string    `json:"note"`
	At        time.Time `json:"at"`
}
