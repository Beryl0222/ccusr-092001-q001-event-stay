// HTTP 接口层：岗位 Bearer 令牌鉴权、幂等键透传、行程查询强制审计。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// 角色（岗位）沿用 domain.json 的参与方称谓；每个岗位只拿到最小必要作用域。
const (
	roleOperator = "赛事运营"
	roleTraffic  = "交通协调"
	roleVenue    = "场馆值守"
	roleLodging  = "住宿合作方"
	roleTourism  = "文旅调度"
)

// 作用域：行程查询单独成项，仅赛事运营持有。
const (
	scopeOps      = "运营指挥" // 赛程、改期决策、事件回放
	scopeTraffic  = "接驳编排"
	scopeVenue    = "场馆管控"
	scopeLodging  = "容量报送"
	scopeTourism  = "线路编排"
	scopeIntentRW = "行程读写"
	scopeIntentQ  = "行程查询"
	scopeRead     = "态势读取" // 缺口与推荐：各协同岗位均可看
)

type principal struct {
	Token string
	Role  string
}

type authorizer struct {
	tokens map[string]string // token -> 岗位
	scopes map[string][]string
}

func newAuthorizer(tokens map[string]string) *authorizer {
	return &authorizer{
		tokens: tokens,
		scopes: map[string][]string{
			roleOperator: {scopeOps, scopeIntentRW, scopeIntentQ, scopeRead},
			roleTraffic:  {scopeTraffic, scopeRead},
			roleVenue:    {scopeVenue, scopeRead},
			roleLodging:  {scopeLodging, scopeRead},
			roleTourism:  {scopeTourism, scopeRead},
		},
	}
}

// defaultTokens 是开箱联调的默认令牌；生产部署应通过 -auth 令牌文件替换。
func defaultTokens() map[string]string {
	return map[string]string{
		"ops-2026":     roleOperator,
		"traffic-2026": roleTraffic,
		"venue-2026":   roleVenue,
		"lodging-2026": roleLodging,
		"tourism-2026": roleTourism,
	}
}

func (az *authorizer) authenticate(r *http.Request) (principal, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return principal{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	role, ok := az.tokens[token]
	if !ok {
		return principal{}, false
	}
	return principal{Token: token, Role: role}, true
}

func (az *authorizer) allows(p principal, scope string) bool {
	for _, s := range az.scopes[p.Role] {
		if s == scope {
			return true
		}
	}
	return false
}

type apiError struct {
	Error string `json:"错误"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type server struct {
	app *App
	az  *authorizer
}

func newMux(app *App, az *authorizer) http.Handler {
	s := &server{app: app, az: az}
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, health())
	})

	// 基础资料
	mux.HandleFunc("POST /v1/venues", s.require(scopeVenue, s.registerVenue))
	mux.HandleFunc("POST /v1/partners", s.require(scopeLodging, s.setPartner))
	mux.HandleFunc("POST /v1/capacities", s.require(scopeLodging, s.reportCapacity))
	mux.HandleFunc("POST /v1/windows", s.require(scopeTraffic, s.setWindow))
	mux.HandleFunc("POST /v1/shuttles", s.require(scopeTraffic, s.planShuttle))
	mux.HandleFunc("POST /v1/shuttles/{id}/adjust", s.require(scopeTraffic, s.adjustShuttle))
	mux.HandleFunc("POST /v1/routes", s.require(scopeTourism, s.planRoute))

	// 赛程与场地
	mux.HandleFunc("POST /v1/matches", s.require(scopeOps, s.scheduleMatch))
	mux.HandleFunc("POST /v1/matches/{id}/reschedule", s.require(scopeOps, s.reschedule))
	mux.HandleFunc("POST /v1/closures", s.require(scopeVenue, s.closeVenue))
	mux.HandleFunc("POST /v1/closures/{id}/end", s.require(scopeVenue, s.endClosure))

	// 观众行程：提交与查询分离，查询需专门授权并写审计。
	mux.HandleFunc("POST /v1/intents", s.require(scopeIntentRW, s.submitIntent))
	mux.HandleFunc("GET /v1/intents", s.require(scopeIntentQ, s.listIntents))

	// 缺口、推荐、改线建议：缺口为聚合数据（不含任何观众标识），协同岗位可读；
	// 推荐方案逐条展开观众编排，属于个人行程，仅运营指挥可读。
	mux.HandleFunc("GET /v1/matches/{id}/gaps", s.require(scopeRead, s.gaps))
	mux.HandleFunc("GET /v1/matches/{id}/recommendation", s.require(scopeOps, s.recommendation))
	mux.HandleFunc("GET /v1/proposals", s.require(scopeOps, s.listProposals))
	mux.HandleFunc("GET /v1/proposals/{id}", s.require(scopeOps, s.getProposal))
	mux.HandleFunc("POST /v1/proposals/{id}/decision", s.require(scopeOps, s.decideProposal))

	// 事件日志（重放/巡检）：含化名行程，仅运营指挥可查。
	mux.HandleFunc("GET /v1/events", s.require(scopeOps, s.listEvents))

	return mux
}

// require 完成令牌与作用域校验后才进入处理函数。
func (s *server) require(scope string, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.az.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "缺少或无效的 Bearer 令牌")
			return
		}
		if !s.az.allows(p, scope) {
			writeErr(w, http.StatusForbidden, "岗位 "+p.Role+" 无权执行该操作（需要作用域 "+scope+"）")
			return
		}
		h(w, r, p)
	}
}

func idemKey(r *http.Request) string { return r.Header.Get("Idempotency-Key") }

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return false
	}
	return true
}

func (s *server) registerVenue(w http.ResponseWriter, r *http.Request, p principal) {
	var c RegisterVenueCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.RegisterVenue(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) setPartner(w http.ResponseWriter, r *http.Request, p principal) {
	var c PartnerStatusCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.SetPartnerStatus(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) reportCapacity(w http.ResponseWriter, r *http.Request, p principal) {
	var c CapacityCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.ReportCapacity(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) setWindow(w http.ResponseWriter, r *http.Request, p principal) {
	var c WindowCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.SetWindow(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) planShuttle(w http.ResponseWriter, r *http.Request, p principal) {
	var c ShuttleCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.PlanShuttle(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) adjustShuttle(w http.ResponseWriter, r *http.Request, p principal) {
	var c ShuttleCmd
	if !decode(w, r, &c) {
		return
	}
	c.ID = r.PathValue("id")
	res, err := s.app.AdjustShuttle(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) planRoute(w http.ResponseWriter, r *http.Request, p principal) {
	var c RouteCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.PlanRoute(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) scheduleMatch(w http.ResponseWriter, r *http.Request, p principal) {
	var c ScheduleMatchCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.ScheduleMatch(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) reschedule(w http.ResponseWriter, r *http.Request, p principal) {
	var c RescheduleCmd
	if !decode(w, r, &c) {
		return
	}
	c.MatchID = r.PathValue("id")
	res, err := s.app.Reschedule(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) closeVenue(w http.ResponseWriter, r *http.Request, p principal) {
	var c ClosureCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.CloseVenue(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) endClosure(w http.ResponseWriter, r *http.Request, p principal) {
	res, err := s.app.EndClosure(p.Role, idemKey(r), r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) submitIntent(w http.ResponseWriter, r *http.Request, p principal) {
	var c IntentCmd
	if !decode(w, r, &c) {
		return
	}
	res, err := s.app.SubmitIntent(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listIntents 返回个人行程数据：除作用域校验外，每次查询都追加查询审计事件。
func (s *server) listIntents(w http.ResponseWriter, r *http.Request, p principal) {
	matchID := r.URL.Query().Get("match")
	if matchID == "" {
		writeErr(w, http.StatusBadRequest, "必须提供 match 查询参数")
		return
	}
	intents, err := s.app.IntentsForMatch(matchID)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	condition := "场次=" + matchID
	if ref := r.URL.Query().Get("观众标识"); ref != "" {
		condition += "；观众标识=" + ref
		var filtered []Intent
		for _, it := range intents {
			if it.Ref == ref {
				filtered = append(filtered, it)
			}
		}
		intents = filtered
	}
	// 返回前写审计，保证“谁在何时因何条件查了多少条个人行程”可回放。
	if err := s.app.AuditAccess(p.Role, "查询场次行程", condition, len(intents)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"场次": matchID, "行程意向": intents})
}

func (s *server) gaps(w http.ResponseWriter, r *http.Request, _ principal) {
	rep, err := s.app.Gap(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *server) recommendation(w http.ResponseWriter, r *http.Request, _ principal) {
	plan, err := s.app.Recommend(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *server) listProposals(w http.ResponseWriter, _ *http.Request, _ principal) {
	writeJSON(w, http.StatusOK, map[string]any{"改线建议": s.app.Proposals()})
}

func (s *server) getProposal(w http.ResponseWriter, r *http.Request, _ principal) {
	pr, err := s.app.ProposalView(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (s *server) decideProposal(w http.ResponseWriter, r *http.Request, p principal) {
	var c DecisionCmd
	if !decode(w, r, &c) {
		return
	}
	c.ProposalID = r.PathValue("id")
	res, err := s.app.DecideProposal(p.Role, idemKey(r), c)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) listEvents(w http.ResponseWriter, _ *http.Request, _ principal) {
	writeJSON(w, http.StatusOK, map[string]any{"事件": s.app.store.Events()})
}
