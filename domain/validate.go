package domain

import (
	"time"
)

// IntentInput 是观众提交行程意向的入参（最小化：不含姓名、证件号）。
type IntentInput struct {
	VisitorToken   string `json:"visitor_token"`
	IdempotencyKey string `json:"idempotency_key"`
	SessionID      string `json:"session_id"`
	ArriveFrom     string `json:"arrive_from"`
	ArriveTo       string `json:"arrive_to"`
	DepartFrom     string `json:"depart_from"`
	DepartTo       string `json:"depart_to"`
	Pax            int    `json:"pax"`
}

// ParseIntent 校验并解析意向输入，所有时间统一折算到本地时区。
func ParseIntent(in IntentInput) (Intent, error) {
	if in.VisitorToken == "" {
		return Intent{}, validationError("缺少访客令牌")
	}
	if in.IdempotencyKey == "" {
		return Intent{}, validationError("缺少幂等键")
	}
	if in.SessionID == "" {
		return Intent{}, validationError("缺少场次标识")
	}
	if in.Pax <= 0 {
		return Intent{}, validationError("同行人数须大于 0")
	}
	if in.Pax > 50 {
		return Intent{}, validationError("单次意向同行人数上限 50")
	}
	af, err := parseLocal(in.ArriveFrom)
	if err != nil {
		return Intent{}, validationError("抵城时间格式无效: %v", err)
	}
	at, err := parseLocal(in.ArriveTo)
	if err != nil {
		return Intent{}, validationError("最晚到达时间格式无效: %v", err)
	}
	df, err := parseLocal(in.DepartFrom)
	if err != nil {
		return Intent{}, validationError("最早返程时间格式无效: %v", err)
	}
	dt, err := parseLocal(in.DepartTo)
	if err != nil {
		return Intent{}, validationError("最晚返程时间格式无效: %v", err)
	}
	if af.After(at) {
		return Intent{}, validationError("抵城区间起止倒置")
	}
	if df.After(dt) {
		return Intent{}, validationError("返程区间起止倒置")
	}
	if at.After(df) {
		return Intent{}, validationError("到达与返程区间不构成停留链路")
	}
	return Intent{
		VisitorToken:   in.VisitorToken,
		IdempotencyKey: in.IdempotencyKey,
		SessionID:      in.SessionID,
		ArriveFrom:     af, ArriveTo: at, DepartFrom: df, DepartTo: dt,
		Pax: in.Pax,
	}, nil
}

func parseLocal(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, validationError("缺少时间")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.In(Local), nil
}

// ParseRunInput 是接驳班次登记入参。
type ParseRunInput struct {
	ID        string `json:"id"`
	PartnerID string `json:"partner_id"`
	VenueID   string `json:"venue_id"`
	Kind      string `json:"kind"`
	Depart    string `json:"depart"`
	Arrive    string `json:"arrive"`
	Capacity  int    `json:"capacity"`
}

// ParseRun 校验班次：合作方类型、方向、交通时窗与跨日到达截止。
func ParseRun(in ParseRunInput) (ShuttleRun, error) {
	kind := RunKind(in.Kind)
	if kind != RunInbound && kind != RunOutbound {
		return ShuttleRun{}, validationError("班次方向须为 inbound 或 outbound")
	}
	if in.Capacity <= 0 {
		return ShuttleRun{}, validationError("班次容量须大于 0")
	}
	depart, err := parseLocal(in.Depart)
	if err != nil {
		return ShuttleRun{}, validationError("发车时间无效: %v", err)
	}
	arrive, err := parseLocal(in.Arrive)
	if err != nil {
		return ShuttleRun{}, validationError("到达时间无效: %v", err)
	}
	r := ShuttleRun{
		ID: in.ID, PartnerID: in.PartnerID, VenueID: in.VenueID, Kind: kind,
		Depart: depart, Arrive: arrive, Capacity: in.Capacity, Active: true,
	}
	if !IsWithinTrafficWindow(&r) {
		return ShuttleRun{}, validationError("班次超出交通时窗（首班 06:00、末班发车 23:30、跨日到达截止次日 01:30）")
	}
	return r, nil
}

// ValidateVenue 校验场馆承载等级与座席口径，并按等级回填默认座席。
func ValidateVenue(v Venue) (Venue, error) {
	if v.ID == "" {
		return v, validationError("缺少场馆标识")
	}
	seats, ok := TierSeats[v.Tier]
	if !ok {
		return v, validationError("承载等级须为 S/A/B")
	}
	if v.Seats == 0 {
		v.Seats = seats
	}
	if v.Seats > seats {
		return v, validationError("%s 级场馆座席不得超过 %d", v.Tier, seats)
	}
	return v, nil
}

// SnapshotInput 容量快照报送入参。
type SnapshotInput struct {
	PartnerID   string `json:"partner_id"`
	Date        string `json:"date"`
	TotalRooms  int    `json:"total_rooms"`
	BookedRooms int    `json:"booked_rooms"`
}

// ParseSnapshot 校验快照并判定迟报：晚于入住日前一天 18:00 记为迟报。
func ParseSnapshot(in SnapshotInput, recordedAt time.Time) (HotelSnapshot, error) {
	if in.Date == "" {
		return HotelSnapshot{}, validationError("缺少入住夜日期")
	}
	due, err := SnapshotDue(in.Date)
	if err != nil {
		return HotelSnapshot{}, err
	}
	if in.TotalRooms < 0 || in.BookedRooms < 0 || in.BookedRooms > in.TotalRooms {
		return HotelSnapshot{}, validationError("客房数量口径无效")
	}
	at := recordedAt.In(Local)
	return HotelSnapshot{
		PartnerID: in.PartnerID, Date: in.Date,
		TotalRooms: in.TotalRooms, BookedRooms: in.BookedRooms,
		RecordedAt: at, ExpectedBy: due, Late: at.After(due),
	}, nil
}
