# 赛事停留链路协调（event-stay-orchestrator）

面向职业联赛与群众赛事的城市接待后端：把**赛事场次、观众行程意向、接驳班次、住宿容量、主题线路**
编排成可追踪的停留链路，回答各时段接待缺口，生成**不超载**的推荐方案，并在赛程改期、
场馆临时关闭、合作方迟报时自动改线、留痕并等待人工确认。

## 设计要点

- **事件溯源（Event Sourcing）**：一切状态变化都是追加事件（`data/events.jsonl`），
  内存状态仅由事件折演得到。相同事件序列在重放或进程重启后得到**完全一致**的结果；
  每次写入 fsync，重启不丢已确认事件。
- **确定性规划**：观众按访客令牌排序、资源平局按 ID 决断、时间统一 UTC+8，
  推荐方案带内容摘要 `digest`（不含生成时刻），相同输入必得相同摘要。
- **自动改线全程留痕**：每张改线单含触发事件序号、原因、影响范围（观众/酒店/班次/线路、未满足项）、
  目标窗口与人工确认记录（操作岗位、时间、备注）。改期/换馆/闭馆换馆**确认后才生效**。
- **实名但最小化**：不保存姓名、证件号；观众仅以一次性 `visitor_token` + 幂等键关联。
  个人行程查询仅持 `itinerary:read` 的岗位（运营指挥）可访问。
- **基础约定固化**：
  - 场馆承载等级 S/A/B = 18000/9000/4000 座，换馆不得低于原等级；
  - 交通时窗：首班 06:00、末班发车 23:30、跨日班次到达截止次日 01:30；
  - 住宿容量快照截止：入住日前一天 18:00，晚于记**迟报**并触发改线复核；同房夜以首次报送为准；
  - 合作方 `suspended` 后其班次/客房/线路不参与编排。

## 目录结构

```
domain/          领域模型、事件折演（State.Apply）、交通时窗、缺口报告、确定性不超载编排
internal/store/  追加式 JSONL 事件日志（fsync、重放）
internal/auth/   岗位令牌与权限矩阵（含合作方归属隔离）
internal/api/    HTTP 接口
service/         命令应用服务（改期/闭馆/迟报 → 自动改线 → 人工确认）
main.go          入口：-check / -addr / -data / -tokens
```

## 运行

```bash
go run . -check                                   # 核对 domain.json 基础约定
cp config/tokens.example.json config/tokens.json # 岗位令牌表（生产环境替换为强随机令牌）
go run . -addr :8000 -data ./data -tokens ./config/tokens.json
```

## 岗位权限矩阵

| 岗位（role） | 主要权限 |
|---|---|
| `operations` 运营指挥 | 全部登记、改期、闭馆、确认、缺口/方案、个人行程、事件审计 |
| `transport` 交通协调 | 班次编排、缺口/方案查询 |
| `venue` 场馆值守 | 临时关闭、缺口/方案查询 |
| `culture` 文旅调度 | 主题线路编排、缺口/方案查询 |
| `partner` 合作方 | 仅能读写**本合作方**资源（按 `kind` 限定班次/客房/线路） |

观众提交行程意向无需岗位令牌；其他接口均需 `Authorization: Bearer <token>`。

## 接口一览

| 方法与路径 | 说明 | 权限 |
|---|---|---|
| `POST /v1/venues` / `/sessions` / `/partners` | 基础资源登记 | operations |
| `POST /v1/partners/{id}/status` | 暂停/恢复合作方 | operations |
| `POST /v1/sessions/{id}/move` | 赛程改期/换馆（生成待确认改线单） | operations |
| `POST /v1/closures` `/v1/closures/{id}/cancel` | 场馆临时关闭/解除 | venue |
| `POST /v1/runs` `/v1/runs/{id}/cancel` | 接驳班次登记/取消 | transport / 本接驳方 |
| `POST /v1/snapshots` | 酒店按房夜报送容量（迟报 202 并复核） | 本酒店 |
| `POST /v1/routes` `/v1/routes/{id}/capacity` | 主题线路与按日槽位 | culture / 本文旅方 |
| `POST /v1/intents` | 观众行程意向（幂等键；重复提交取代旧版） | 公开 |
| `GET  /v1/sessions/{id}/gaps` | **各时段接待缺口**（场馆/接驳/酒店/线路，含 missing/late 数据状态） | 协调岗 |
| `POST /v1/sessions/{id}/plan` | **生成不超载推荐方案** | 协调岗 |
| `GET  /v1/sessions/{id}/proposal` `/reroutes` | 当前方案 / 改线记录 | 协调岗 |
| `POST /v1/reroutes/{id}/decision` | 人工确认 `confirmed` / 驳回 `rejected` | operations |
| `GET  /v1/visitors/{token}/intent` | 个人行程查询 | operations（itinerary:read） |
| `GET  /v1/events` | 事件流水审计 | operations（audit:read） |

### 典型流程

```bash
# 1) 观众提交跨日意向（提前到、延后走），重复提交同键幂等，新键取代旧版
curl -X POST localhost:8000/v1/intents -d '{
  "visitor_token":"alpha","idempotency_key":"k1","session_id":"s1","pax":2,
  "arrive_from":"2026-10-01T14:00:00+08:00","arrive_to":"2026-10-01T20:00:00+08:00",
  "depart_from":"2026-10-03T22:00:00+08:00","depart_to":"2026-10-04T01:00:00+08:00"}'

# 2) 查询各时段缺口（未报容量的房夜 data_status=missing，迟报=late）
curl -H "Authorization: Bearer ops-demo-token" \
  localhost:8000/v1/sessions/s1/gaps

# 3) 生成不超载推荐方案（digest 为内容摘要）
curl -X POST -H "Authorization: Bearer ops-demo-token" \
  localhost:8000/v1/sessions/s1/plan

# 4) 改期 → 得到待确认改线单（原因/影响范围/目标窗口）
curl -X POST -H "Authorization: Bearer ops-demo-token" \
  localhost:8000/v1/sessions/s1/move -d '{
  "venue_id":"v_s","start":"2026-10-04T19:30:00+08:00","end":"2026-10-04T22:00:00+08:00",
  "reason":"电视转播调整"}'

# 5) 运营指挥人工确认后赛程才生效
curl -X POST -H "Authorization: Bearer ops-demo-token" \
  localhost:8000/v1/reroutes/<rr_id>/decision -d '{"action":"confirmed","note":"按转播方案执行"}'
```

## 缺口与编排口径

- **场馆**：需求为意向总人数，供给为座席；闭馆窗口内供给记 0 并标注原因。
- **接驳**：按小时累计“届时必须到达/可离开”的人数与累计可用座位；
  方向总缺口取逐小时峰值，避免同一缺口跨小时重复计数。分配以整团为单位，
  任何班次占用不得超过其容量（不超载）。
- **住宿**：按房夜（本地时区切日，抵达夜住、离开日不住）汇总，每间房最多 2 人；
  同一观众跨夜优先在同一酒店续住。未报送房夜标 `missing`。
- **线路**：提前到达/延后返程形成的整日（不含抵达日、比赛日、离开日）各需 1 个出游槽位。

## 测试

```bash
go test -race ./...   # 领域规则、应用场景、权限矩阵、重放/重启一致性
```

覆盖场景：交通时窗校验、迟报判定、幂等与取代、跨日房夜/出游日、改期待确认、
闭馆选替代馆（等级约束）、驳回不生效、合作方越权隔离、重启后方案摘要一致。

`domain.json` 保存首批参与方、事件和数据边界约定，领域命名沿用其中称谓。
