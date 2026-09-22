package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beryl0222/event-stay-orchestrator/domain"
	"github.com/beryl0222/event-stay-orchestrator/internal/auth"
	"github.com/beryl0222/event-stay-orchestrator/internal/store"
	"github.com/beryl0222/event-stay-orchestrator/service"
)

const tokensJSON = `{
  "tokens": [
    {"token": "t-ops", "name": "值班长", "role": "operations"},
    {"token": "t-transport", "name": "交通员", "role": "transport"},
    {"token": "t-venue", "name": "场馆员", "role": "venue"},
    {"token": "t-culture", "name": "文旅员", "role": "culture"},
    {"token": "t-hotel", "name": "酒店", "role": "partner", "partner_id": "p_h1", "kind": "hotel"},
    {"token": "t-shuttle", "name": "接驳", "role": "partner", "partner_id": "p_b1", "kind": "shuttle"},
    {"token": "t-other-shuttle", "name": "他司接驳", "role": "partner", "partner_id": "p_b2", "kind": "shuttle"}
  ]
}`

type testEnv struct {
	handler http.Handler
	app     *service.App
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	log, err := store.OpenEventLog(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 9, 20, 10, 0, 0, 0, domain.Local) }
	app, err := service.New(log, clock)
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(tokenPath, []byte(tokensJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := auth.LoadTokens(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{handler: NewServer(app, res).Handler(), app: app}
}

func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func seedWorld(t *testing.T, env *testEnv) {
	must := func(rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("播种失败: code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	must(do(t, env.handler, "POST", "/v1/venues", "t-ops", domain.Venue{
		ID: "v_a", Name: "甲馆", Tier: domain.TierA, Seats: 9000,
	}), http.StatusCreated)
	must(do(t, env.handler, "POST", "/v1/sessions", "t-ops", service.ScheduleSessionInput{
		ID: "s1", Name: "揭幕战", VenueID: "v_a",
		Start: "2026-10-03T19:30:00+08:00", End: "2026-10-03T22:00:00+08:00",
	}), http.StatusCreated)
	must(do(t, env.handler, "POST", "/v1/partners", "t-ops", domain.Partner{
		ID: "p_h1", Name: "酒店一", Kind: domain.PartnerHotel, Status: domain.PartnerActive,
	}), http.StatusCreated)
	must(do(t, env.handler, "POST", "/v1/partners", "t-ops", domain.Partner{
		ID: "p_b1", Name: "接驳一", Kind: domain.PartnerShuttle, Status: domain.PartnerActive,
	}), http.StatusCreated)
	must(do(t, env.handler, "POST", "/v1/partners", "t-ops", domain.Partner{
		ID: "p_c1", Name: "文旅一", Kind: domain.PartnerAttraction, Status: domain.PartnerActive,
	}), http.StatusCreated)
}

// 健康检查与观众意向无需岗位令牌。
func TestPublicEndpoints(t *testing.T) {
	env := newTestEnv(t)
	if rr := do(t, env.handler, "GET", "/health", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("health 期望 200，实际 %d", rr.Code)
	}
	seedWorld(t, env)
	body := domain.IntentInput{
		VisitorToken: "alpha", IdempotencyKey: "k1", SessionID: "s1", Pax: 2,
		ArriveFrom: "2026-10-02T14:00:00+08:00", ArriveTo: "2026-10-02T20:00:00+08:00",
		DepartFrom: "2026-10-03T22:00:00+08:00", DepartTo: "2026-10-04T01:00:00+08:00",
	}
	if rr := do(t, env.handler, "POST", "/v1/intents", "", body); rr.Code != http.StatusCreated {
		t.Fatalf("意向提交期望 201，实际 %d: %s", rr.Code, rr.Body.String())
	}
	// 幂等重放返回 200 且不产生新数据。
	if rr := do(t, env.handler, "POST", "/v1/intents", "", body); rr.Code != http.StatusOK {
		t.Fatalf("幂等重放期望 200，实际 %d", rr.Code)
	}
}

// 无令牌与越权访问受保护接口被拒。
func TestAuthRequired(t *testing.T) {
	env := newTestEnv(t)
	if rr := do(t, env.handler, "GET", "/v1/sessions/s1/gaps", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌期望 401，实际 %d", rr.Code)
	}
	if rr := do(t, env.handler, "GET", "/v1/sessions/s1/gaps", "badtoken", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("坏令牌期望 401，实际 %d", rr.Code)
	}
}

// 岗位权限矩阵：文旅不能改期；交通不能确认改线；场馆员不能登记班次。
func TestRoleMatrix(t *testing.T) {
	env := newTestEnv(t)
	seedWorld(t, env)
	moveBody := service.MoveSessionInput{Start: "2026-10-04T19:30:00+08:00",
		End: "2026-10-04T22:00:00+08:00", Reason: "转播"}
	if rr := do(t, env.handler, "POST", "/v1/sessions/s1/move", "t-culture", moveBody); rr.Code != http.StatusForbidden {
		t.Fatalf("文旅改期期望 403，实际 %d", rr.Code)
	}
	runBody := domain.ParseRunInput{ID: "r1", PartnerID: "p_b1", VenueID: "v_a", Kind: "inbound",
		Depart: "2026-10-03T16:00:00+08:00", Arrive: "2026-10-03T17:00:00+08:00", Capacity: 40}
	if rr := do(t, env.handler, "POST", "/v1/runs", "t-venue", runBody); rr.Code != http.StatusForbidden {
		t.Fatalf("场馆员登记班次期望 403，实际 %d", rr.Code)
	}
	if rr := do(t, env.handler, "POST", "/v1/reroutes/rr_x/decision", "t-transport",
		map[string]string{"action": "confirmed"}); rr.Code != http.StatusForbidden {
		t.Fatalf("交通确认改线期望 403，实际 %d", rr.Code)
	}
}

// 合作方归属隔离：接驳公司只能登记/取消本公司班次；酒店不能登记班次。
func TestPartnerOwnership(t *testing.T) {
	env := newTestEnv(t)
	seedWorld(t, env)
	// 酒店令牌登记接驳班次 → 403。
	runBody := domain.ParseRunInput{ID: "r1", PartnerID: "p_b1", VenueID: "v_a", Kind: "inbound",
		Depart: "2026-10-03T16:00:00+08:00", Arrive: "2026-10-03T17:00:00+08:00", Capacity: 40}
	if rr := do(t, env.handler, "POST", "/v1/runs", "t-hotel", runBody); rr.Code != http.StatusForbidden {
		t.Fatalf("酒店登记班次期望 403，实际 %d", rr.Code)
	}
	// 他司令牌以 p_b1 名义登记 → 403。
	if rr := do(t, env.handler, "POST", "/v1/runs", "t-other-shuttle", runBody); rr.Code != http.StatusForbidden {
		t.Fatalf("他司代登记期望 403，实际 %d", rr.Code)
	}
	// 本公司登记 → 201。
	if rr := do(t, env.handler, "POST", "/v1/runs", "t-shuttle", runBody); rr.Code != http.StatusCreated {
		t.Fatalf("本公司登记班次期望 201，实际 %d: %s", rr.Code, rr.Body.String())
	}
	// 他司取消本公司班次 → 403。
	if rr := do(t, env.handler, "POST", "/v1/runs/r1/cancel", "t-other-shuttle",
		map[string]string{"reason": "误操作"}); rr.Code != http.StatusForbidden {
		t.Fatalf("他司取消期望 403，实际 %d", rr.Code)
	}
	// 酒店只能向自己报送容量。
	snap := domain.SnapshotInput{PartnerID: "p_h1", Date: "2026-10-02", TotalRooms: 10}
	if rr := do(t, env.handler, "POST", "/v1/snapshots", "t-shuttle", snap); rr.Code != http.StatusForbidden {
		t.Fatalf("接驳公司报送客房期望 403，实际 %d", rr.Code)
	}
	if rr := do(t, env.handler, "POST", "/v1/snapshots", "t-hotel", snap); rr.Code != http.StatusCreated {
		t.Fatalf("本酒店报送期望 201，实际 %d: %s", rr.Code, rr.Body.String())
	}
}

// 个人行程查询仅向有权限的岗位开放：运营 200，交通 403。
func TestItineraryPrivacy(t *testing.T) {
	env := newTestEnv(t)
	seedWorld(t, env)
	do(t, env.handler, "POST", "/v1/intents", "", domain.IntentInput{
		VisitorToken: "alpha", IdempotencyKey: "k1", SessionID: "s1", Pax: 2,
		ArriveFrom: "2026-10-02T14:00:00+08:00", ArriveTo: "2026-10-02T20:00:00+08:00",
		DepartFrom: "2026-10-03T22:00:00+08:00", DepartTo: "2026-10-04T01:00:00+08:00",
	})
	if rr := do(t, env.handler, "GET", "/v1/visitors/alpha/intent", "t-transport", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("交通岗位查询个人行程期望 403，实际 %d", rr.Code)
	}
	rr := do(t, env.handler, "GET", "/v1/visitors/alpha/intent", "t-ops", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("运营查询个人行程期望 200，实际 %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.Intent
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.VisitorToken != "alpha" {
		t.Fatalf("返回意向异常: %+v", got)
	}
}

// 端到端：改期 → 运营确认 → 赛程生效；缺口与方案接口可回答。
func TestEndToEndMoveAndConfirm(t *testing.T) {
	env := newTestEnv(t)
	seedWorld(t, env)
	do(t, env.handler, "POST", "/v1/runs", "t-shuttle", domain.ParseRunInput{
		ID: "rin1", PartnerID: "p_b1", VenueID: "v_a", Kind: "inbound",
		Depart: "2026-10-03T16:00:00+08:00", Arrive: "2026-10-03T17:00:00+08:00", Capacity: 40,
	})
	do(t, env.handler, "POST", "/v1/intents", "", domain.IntentInput{
		VisitorToken: "alpha", IdempotencyKey: "k1", SessionID: "s1", Pax: 2,
		ArriveFrom: "2026-10-02T14:00:00+08:00", ArriveTo: "2026-10-02T20:00:00+08:00",
		DepartFrom: "2026-10-03T22:00:00+08:00", DepartTo: "2026-10-04T01:00:00+08:00",
	})
	if rr := do(t, env.handler, "POST", "/v1/sessions/s1/plan", "t-transport", nil); rr.Code != http.StatusOK {
		t.Fatalf("交通生成方案期望 200，实际 %d: %s", rr.Code, rr.Body.String())
	}
	rr := do(t, env.handler, "POST", "/v1/sessions/s1/move", "t-ops", service.MoveSessionInput{
		Start: "2026-10-04T19:30:00+08:00", End: "2026-10-04T22:00:00+08:00", Reason: "电视转播",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("改期期望 200，实际 %d: %s", rr.Code, rr.Body.String())
	}
	var moved struct {
		Reroute *domain.Reroute `json:"reroute"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &moved); err != nil || moved.Reroute == nil {
		t.Fatalf("改期响应缺少改线单: %s", rr.Body.String())
	}
	rrID := moved.Reroute.ID
	// 确认前查询改线单为 proposed。
	if rr := do(t, env.handler, "GET", "/v1/reroutes/"+rrID, "t-ops", nil); rr.Code != http.StatusOK {
		t.Fatalf("查询改线单失败: %d", rr.Code)
	}
	// 运营确认。
	if rr := do(t, env.handler, "POST", "/v1/reroutes/"+rrID+"/decision", "t-ops",
		map[string]string{"action": "confirmed", "note": "执行"}); rr.Code != http.StatusOK {
		t.Fatalf("确认期望 200，实际 %d: %s", rr.Code, rr.Body.String())
	}
	// 改线记录包含原因、影响范围与确认结论。
	got, err := env.app.Reroute(rrID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision == nil || got.Decision.By == "" || got.Reason == "" ||
		len(got.Scope.Visitors) == 0 {
		t.Fatalf("改线留痕不完整: %+v", got)
	}
	// 缺口接口可用。
	if rr := do(t, env.handler, "GET", "/v1/sessions/s1/gaps", "t-culture", nil); rr.Code != http.StatusOK {
		t.Fatalf("文旅查询缺口期望 200，实际 %d", rr.Code)
	}
	// 审计事件流仅运营可见。
	if rr := do(t, env.handler, "GET", "/v1/events", "t-transport", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("交通访问审计流期望 403，实际 %d", rr.Code)
	}
	if rr := do(t, env.handler, "GET", "/v1/events", "t-ops", nil); rr.Code != http.StatusOK {
		t.Fatalf("运营访问审计流期望 200，实际 %d", rr.Code)
	}
}
