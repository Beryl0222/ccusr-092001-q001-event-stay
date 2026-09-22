package service

import (
	"sort"
	"time"

	"github.com/beryl0222/event-stay-orchestrator/domain"
)

// parseWindow 解析本地时间窗口并校验先后顺序。
func parseWindow(startStr, endStr string) (time.Time, time.Time, error) {
	if startStr == "" || endStr == "" {
		return time.Time{}, time.Time{}, &domain.ValidationError{Msg: "开始与结束时间必填"}
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return time.Time{}, time.Time{}, &domain.ValidationError{Msg: "开始时间格式无效（RFC3339）"}
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return time.Time{}, time.Time{}, &domain.ValidationError{Msg: "结束时间格式无效（RFC3339）"}
	}
	start, end = start.In(domain.Local), end.In(domain.Local)
	if !end.After(start) {
		return time.Time{}, time.Time{}, &domain.ValidationError{Msg: "结束时间须晚于开始时间"}
	}
	return start, end, nil
}

// windowsOverlap 判断两个半开区间 [s1,e1) 与 [s2,e2) 是否相交。
func windowsOverlap(s1, e1, s2, e2 time.Time) bool {
	return s1.Before(e2) && s2.Before(e1)
}

// sortedSessionIDs 按场次 ID 排序，保证批量复核顺序确定。
func (a *App) sortedSessionIDs() []string {
	ids := make([]string, 0, len(a.state.Sessions))
	for id := range a.state.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sortVenues 按替代选择规则排序：等级高者优先、座席接近需求者优先、ID 最小兜底。
// 候选已过滤为等级不低于原馆且座席足够。
func sortVenues(candidates []*domain.Venue, cur *domain.Venue) {
	sort.Slice(candidates, func(i, j int) bool {
		ti, tj := tierRank(candidates[i].Tier), tierRank(candidates[j].Tier)
		if ti != tj {
			return ti > tj // 优先更高等级，尽量减少降级观感
		}
		if candidates[i].Seats != candidates[j].Seats {
			return candidates[i].Seats < candidates[j].Seats // 座席适度（避免过度占用大馆）
		}
		return candidates[i].ID < candidates[j].ID
	})
}
