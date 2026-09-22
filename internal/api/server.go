// Package api 提供赛事停留链路协调服务的 HTTP 接口与岗位鉴权边界。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/beryl0222/event-stay-orchestrator/domain"
	"github.com/beryl0222/event-stay-orchestrator/internal/auth"
	"github.com/beryl0222/event-stay-orchestrator/service"
)

type ctxKey string

const identityKey ctxKey = "identity"

// Server 聚合应用服务与令牌解析器。
type Server struct {
	app *service.App
	res *auth.Resolver
}

// NewServer 创建 API 服务。res 允许为空（此时除公开接口外一律 401）。
func NewServer(app *service.App, res *auth.Resolver) *Server {
	return &Server{app: app, res: res}
}

// Handler 装配全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "event-stay-orchestrator"})
	})

	// 观众侧：行程意向提交（仅写入本人最小化字段，无需岗位令牌）。
	mux.HandleFunc("POST /v1/intents", s.submitIntent)

	// 基础资源（赛事运营）。
	mux.HandleFunc("POST /v1/venues", s.require(auth.PermScheduleWrite, "", s.registerVenue))
	mux.HandleFunc("POST /v1/sessions", s.require(auth.PermScheduleWrite, "", s.scheduleSession))
	mux.HandleFunc("POST /v1/partners", s.require(auth.PermScheduleWrite, "", s.registerPartner))
	mux.HandleFunc("POST /v1/partners/{id}/status", s.require(auth.PermScheduleWrite, "", s.setPartnerStatus))
	mux.HandleFunc("POST /v1/sessions/{id}/move", s.require(auth.PermSessionMove, "", s.moveSession))
	mux.HandleFunc("POST /v1/closures", s.require(auth.PermClosureWrite, "", s.scheduleClosure))
	mux.HandleFunc("POST /v1/closures/{id}/cancel", s.require(auth.PermClosureWrite, "", s.cancelClosure))

	// 交通协调 / 接驳合作方。
	mux.HandleFunc("POST /v1/runs", s.registerRun)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.require(auth.PermRunWrite, "", s.cancelRun))

	// 住宿合作方容量报送。
	mux.HandleFunc("POST /v1/snapshots", s.recordSnapshot)

	// 文旅调度 / 文旅合作方。
	mux.HandleFunc("POST /v1/routes", s.registerRoute)
	mux.HandleFunc("POST /v1/routes/{id}/capacity", s.require(auth.PermRouteWrite, "", s.setRouteCapacity))

	// 协调查询（缺口/方案/改线记录）。
	mux.HandleFunc("GET /v1/sessions/{id}/gaps", s.require(auth.PermCoordRead, "", s.gaps))
	mux.HandleFunc("POST /v1/sessions/{id}/plan", s.require(auth.PermCoordRead, "", s.plan))
	mux.HandleFunc("GET /v1/sessions/{id}/proposal", s.require(auth.PermCoordRead, "", s.getProposal))
	mux.HandleFunc("GET /v1/sessions/{id}/reroutes", s.require(auth.PermCoordRead, "", s.sessionReroutes))
	mux.HandleFunc("GET /v1/reroutes/{id}", s.require(auth.PermCoordRead, "", s.getReroute))

	// 人工确认（仅赛事运营）。
	mux.HandleFunc("POST /v1/reroutes/{id}/decision", s.require(auth.PermRerouteDecide, "", s.decideReroute))

	// 个人行程查询：仅持 itinerary:read 的岗位（运营指挥）。
	mux.HandleFunc("GET /v1/visitors/{token}/intent", s.require(auth.PermItineraryRead, "", s.getIntent))

	// 事件流水审计：仅运营指挥。
	mux.HandleFunc("GET /v1/events", s.require(auth.PermAuditRead, "", s.listEvents))

	return logging(mux)
}

// require 是固定权限的中间件包装。
func (s *Server) require(p auth.Permission, resourcePartner string,
	h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "缺少或无效的岗位令牌")
			return
		}
		if !id.Can(p, resourcePartner) {
			writeError(w, http.StatusForbidden, "岗位无权执行该操作")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	}
}

// requireOwned 先解析请求体中的 partner_id 再做归属校验。
func (s *Server) requireOwned(p auth.Permission, h func(http.ResponseWriter, *http.Request, auth.Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.authenticate(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "缺少或无效的岗位令牌")
			return
		}
		var probe struct {
			PartnerID string `json:"partner_id"`
		}
		body, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "请求体无法读取")
			return
		}
		_ = json.Unmarshal(body, &probe)
		if !id.Can(p, probe.PartnerID) {
			writeError(w, http.StatusForbidden, "岗位无权为该合作方执行操作")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), identityKey, id))
		r = withBody(r, body)
		h(w, r, id)
	}
}

func (s *Server) authenticate(r *http.Request) (auth.Identity, bool) {
	if s.res == nil {
		return auth.Identity{}, false
	}
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || token == "" {
		return auth.Identity{}, false
	}
	return s.res.Resolve(strings.TrimSpace(token))
}

func identityFrom(r *http.Request) auth.Identity {
	if id, ok := r.Context().Value(identityKey).(auth.Identity); ok {
		return id
	}
	return auth.Identity{}
}

func actor(r *http.Request) string {
	id := identityFrom(r)
	if id.Name != "" {
		return string(id.Role) + ":" + id.Name
	}
	return "anonymous"
}

// ----------------------------------------------------------------------------
// 处理器：基础资源
// ----------------------------------------------------------------------------

func (s *Server) registerVenue(w http.ResponseWriter, r *http.Request) {
	var in domain.Venue
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	v, err := s.app.RegisterVenue(actor(r), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) scheduleSession(w http.ResponseWriter, r *http.Request) {
	var in service.ScheduleSessionInput
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	sess, err := s.app.ScheduleSession(actor(r), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) registerPartner(w http.ResponseWriter, r *http.Request) {
	var in domain.Partner
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	p, err := s.app.RegisterPartner(actor(r), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) setPartnerStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Status domain.PartnerStatus `json:"status"`
		Reason string               `json:"reason"`
	}
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	if err := s.app.SetPartnerStatus(actor(r), id, in.Status, in.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"partner_id": id, "status": string(in.Status)})
}

func (s *Server) moveSession(w http.ResponseWriter, r *http.Request) {
	var in service.MoveSessionInput
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	rr, err := s.app.MoveSession(actor(r), r.PathValue("id"), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reroute": rr})
}

func (s *Server) scheduleClosure(w http.ResponseWriter, r *http.Request) {
	var in service.ClosureInput
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	reroutes, err := s.app.ScheduleClosure(actor(r), in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"reroutes": reroutes})
}

func (s *Server) cancelClosure(w http.ResponseWriter, r *http.Request) {
	if err := s.app.CancelClosure(actor(r), r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------------------
// 处理器：交通 / 住宿 / 文旅（含合作方归属校验）
// ----------------------------------------------------------------------------

func (s *Server) registerRun(w http.ResponseWriter, r *http.Request) {
	// 手工鉴权：需读取请求体中的合作方归属。
	s.requireOwned(auth.PermRunWrite, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		var in domain.ParseRunInput
		if ok := decodeBody(w, r, &in); !ok {
			return
		}
		run, err := s.app.ScheduleRun(actor(r), in)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, run)
	})(w, r)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	if id.Role == auth.RolePartner && !s.app.RunBelongsTo(r.PathValue("id"), id.PartnerID) {
		writeError(w, http.StatusForbidden, "无权取消他方班次")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	if err := s.app.CancelRun(actor(r), r.PathValue("id"), in.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recordSnapshot(w http.ResponseWriter, r *http.Request) {
	s.requireOwned(auth.PermSnapshotWrite, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		var in domain.SnapshotInput
		if ok := decodeBody(w, r, &in); !ok {
			return
		}
		snap, err := s.app.RecordSnapshot(actor(r), in)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		code := http.StatusCreated
		if snap.Late {
			code = http.StatusAccepted // 迟报已受理并触发改线复核
		}
		writeJSON(w, code, snap)
	})(w, r)
}

func (s *Server) registerRoute(w http.ResponseWriter, r *http.Request) {
	s.requireOwned(auth.PermRouteWrite, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		var in domain.ThemeRoute
		if ok := decodeBody(w, r, &in); !ok {
			return
		}
		route, err := s.app.RegisterRoute(actor(r), in)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, route)
	})(w, r)
}

func (s *Server) setRouteCapacity(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	if id.Role == auth.RolePartner {
		// 合作方只能调整本合作方线路；校验线路归属。
		if !s.ownsRoute(id, r.PathValue("id")) {
			writeError(w, http.StatusForbidden, "无权调整他方线路")
			return
		}
	}
	var in struct {
		Date     string `json:"date"`
		Capacity int    `json:"capacity"`
	}
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	if err := s.app.SetRouteCapacity(actor(r), r.PathValue("id"), in.Date, in.Capacity); err != nil {
		writeDomainError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownsRoute 通过应用状态确认线路归属。
func (s *Server) ownsRoute(id auth.Identity, routeID string) bool {
	return s.app.RouteBelongsTo(routeID, id.PartnerID)
}

// ----------------------------------------------------------------------------
// 处理器：观众意向
// ----------------------------------------------------------------------------

func (s *Server) submitIntent(w http.ResponseWriter, r *http.Request) {
	var in domain.IntentInput
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	res, err := s.app.SubmitIntent(in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	code := http.StatusCreated
	if res.Deduped {
		code = http.StatusOK
	}
	writeJSON(w, code, res)
}

// ----------------------------------------------------------------------------
// 处理器：缺口 / 方案 / 改线
// ----------------------------------------------------------------------------

func (s *Server) gaps(w http.ResponseWriter, r *http.Request) {
	rep, err := s.app.GapReport(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	p, err := s.app.PlanSession(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getProposal(w http.ResponseWriter, r *http.Request) {
	p, err := s.app.Proposal(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) sessionReroutes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"reroutes": s.app.ReroutesOf(r.PathValue("id")),
	})
}

func (s *Server) getReroute(w http.ResponseWriter, r *http.Request) {
	rr, err := s.app.Reroute(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

func (s *Server) decideReroute(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if ok := decodeBody(w, r, &in); !ok {
		return
	}
	rr, err := s.app.DecideReroute(actor(r), r.PathValue("id"), in.Action, in.Note)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// ----------------------------------------------------------------------------
// 处理器：个人行程与审计
// ----------------------------------------------------------------------------

func (s *Server) getIntent(w http.ResponseWriter, r *http.Request) {
	in, err := s.app.Intent(r.PathValue("token"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_, _ = w.Write([]byte(`{"events":[`))
	first := true
	_ = s.app.ReplayEvents(func(e domain.Event) error {
		if !first {
			_, _ = w.Write([]byte(","))
		}
		first = false
		return enc.Encode(e)
	})
	_, _ = w.Write([]byte(`]}`))
}

// ----------------------------------------------------------------------------
// 工具
// ----------------------------------------------------------------------------

func writeDomainError(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, ve.Error())
		return
	}
	var nf *domain.NotFoundError
	if errors.As(err, &nf) {
		writeError(w, http.StatusNotFound, nf.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
