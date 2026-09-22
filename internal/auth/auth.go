// Package auth 实现岗位级访问控制。令牌不内嵌角色语义，角色由服务端令牌表签发，
// 合作方令牌额外绑定 partner_id，用于归属校验（只能操作本合作方资源）。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
)

// Role 岗位标识。
type Role string

const (
	RoleOps       Role = "operations" // 赛事运营/运营指挥：全量查询与人工确认
	RoleTransport Role = "transport"  // 交通协调
	RoleVenue     Role = "venue"      // 场馆值守
	RoleCulture   Role = "culture"    // 文旅调度
	RolePartner   Role = "partner"    // 合作方（绑定具体合作方）
)

// Identity 是鉴权后的岗位身份。
type Identity struct {
	Token     string `json:"-"`
	Name      string `json:"name"`
	Role      Role   `json:"role"`
	PartnerID string `json:"partner_id,omitempty"` // 仅合作方
	Kind      string `json:"kind,omitempty"`       // 合作方类型 hotel/shuttle/attraction
}

// Permission 是细粒度操作口径。
type Permission string

const (
	PermScheduleWrite Permission = "schedule:write" // 场馆/赛程/合作方登记
	PermSessionMove   Permission = "session:move"   // 赛程改期
	PermClosureWrite  Permission = "closure:write"  // 临时关闭
	PermRunWrite      Permission = "run:write"      // 接驳班次编排
	PermSnapshotWrite Permission = "snapshot:write" // 酒店容量报送
	PermRouteWrite    Permission = "route:write"    // 主题线路编排
	PermCoordRead     Permission = "coord:read"     // 缺口/方案/改线查询
	PermRerouteDecide Permission = "reroute:decide" // 改线人工确认
	PermAuditRead     Permission = "audit:read"     // 事件流水查询
	PermItineraryRead Permission = "itinerary:read" // 个人行程查询（最小授权）
)

// rolePermissions 是岗位—权限矩阵。
var rolePermissions = map[Role][]Permission{
	RoleOps: {
		PermScheduleWrite, PermSessionMove, PermClosureWrite,
		PermRunWrite, PermSnapshotWrite, PermRouteWrite,
		PermCoordRead, PermRerouteDecide, PermAuditRead, PermItineraryRead,
	},
	RoleTransport: {PermRunWrite, PermCoordRead},
	RoleVenue:     {PermClosureWrite, PermCoordRead},
	RoleCulture:   {PermRouteWrite, PermCoordRead},
	// 合作方权限按类型在 Can() 中细化：仅能写本合作方资源。
	RolePartner: {PermCoordRead},
}

// tokenFile 令牌表文件结构。
type tokenFile struct {
	Tokens []tokenEntry `json:"tokens"`
}

type tokenEntry struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Role      Role   `json:"role"`
	PartnerID string `json:"partner_id"`
	Kind      string `json:"kind"` // hotel/shuttle/attraction，合作方类型
}

// Resolver 依据令牌解析岗位身份。
type Resolver struct {
	entries map[string]tokenEntry
}

// LoadTokens 从 JSON 文件加载令牌表。
func LoadTokens(path string) (*Resolver, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取令牌表失败: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("解析令牌表失败: %w", err)
	}
	r := &Resolver{entries: map[string]tokenEntry{}}
	for _, e := range tf.Tokens {
		if e.Token == "" {
			continue
		}
		if _, ok := r.entries[e.Token]; ok {
			return nil, fmt.Errorf("令牌表存在重复令牌: %s", e.Token)
		}
		r.entries[e.Token] = e
	}
	return r, nil
}

// Resolve 校验令牌并返回身份。
func (r *Resolver) Resolve(token string) (Identity, bool) {
	e, ok := r.entries[token]
	if !ok {
		return Identity{}, false
	}
	return Identity{Token: token, Name: e.Name, Role: e.Role, PartnerID: e.PartnerID, Kind: e.Kind}, true
}

// Can 判断身份是否持有权限；涉及合作方写入时校验资源归属与类型。
// resourcePartner 为资源归属的合作方 ID（非合作方资源传空串）。
func (id Identity) Can(p Permission, resourcePartner string) bool {
	if id.Role == RolePartner {
		// 合作方只能操作归属本合作方的资源。
		if resourcePartner != "" && resourcePartner != id.PartnerID {
			return false
		}
		switch p {
		case PermRunWrite:
			return id.Kind == "shuttle"
		case PermSnapshotWrite:
			return resourcePartner == id.PartnerID && id.Kind == "hotel"
		case PermRouteWrite:
			return id.Kind == "attraction"
		case PermCoordRead:
			return true
		default:
			return false
		}
	}
	for _, rp := range rolePermissions[id.Role] {
		if rp == p {
			return true
		}
	}
	return false
}
