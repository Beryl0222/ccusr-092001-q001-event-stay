// 事件溯源内核：所有领域变更都以追加事件的形式落盘，状态只能由事件重放得到。
// 事件采用当地民用时间（不带时区的墙钟时间），并以哈希链串联，保证重放可校验、结果确定。
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event 是事件日志中的最小信封。Type 使用 domain.json 约定的中文事件名。
type Event struct {
	ID      string          `json:"id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Time    string          `json:"time"` // 当地民用时间 2006-01-02T15:04:05
	Actor   string          `json:"actor"`
	Idem    string          `json:"idem,omitempty"` // 幂等键，命令首次写入时记录
	Payload json.RawMessage `json:"payload"`
	Hash    string          `json:"hash"` // sha256(prevHash || seq || type || 规范化载荷)
}

// 事件类型，命名沿用 domain.json 的“事件”约定。
const (
	evVenueRegistered  = "场馆登记"
	evPartnerStatus    = "合作方状态"
	evMatchScheduled   = "赛程发布"
	evMatchRescheduled = "场次改期"
	evShuttlePlanned   = "接驳班次计划"
	evShuttleAdjusted  = "接驳班次调整"
	evWindowSet        = "交通时窗设定"
	evCapacitySnapshot = "容量快照"
	evRoutePlanned     = "主题线路编排"
	evClosureStarted   = "临时关闭"
	evClosureEnded     = "关闭解除"
	evIntentReceived   = "行程意向提交"
	evRerouteProposed  = "改线建议"
	evPlanConfirmed    = "方案确认"
	evAccessAudited    = "查询审计"
)

// ErrConflict 表示命令与当前状态冲突（如旧修订号重复提交、确认时状态已变化）。
var ErrConflict = errors.New("状态冲突")

// ErrNotFound 资源不存在。
var ErrNotFound = errors.New("资源不存在")

// Clock 返回当前当地民用时间；测试可替换。
type Clock func() string

func defaultClock() string { return civilNow(time.Now()) }

// civilNow 输出当地墙钟时间，跨日判定一律以此格式的日期段为准。
func civilNow(t time.Time) string {
	return t.Format("2006-01-02T15:04:05")
}

const (
	civilLen = 19 // 2006-01-02T15:04:05
	hourLen  = 13 // 2006-01-02T15
	dayLen   = 10 // 2006-01-02
)

// parseCivil 解析当地民用时间。
func parseCivil(s string) (time.Time, error) {
	if len(s) == hourLen {
		s += ":00:00"
	} else if len(s) == dayLen {
		s += "T00:00:00"
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间格式应为 2006-01-02T15:04:05：%q：%w", s, err)
	}
	return t, nil
}

func hourBucket(s string) string {
	if len(s) < hourLen {
		return s
	}
	return s[:hourLen]
}

func dayBucket(s string) string {
	if len(s) < dayLen {
		return s
	}
	return s[:dayLen]
}

// canonicalPayload 返回载荷的稳定字节形式：解码为通用结构后按键排序重新编码，
// 使同一逻辑载荷无论原始字段顺序如何，哈希都一致。
func canonicalPayload(raw json.RawMessage) []byte {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return append([]byte(nil), raw...)
	}
	var sb strings.Builder
	encodeCanonical(&sb, v)
	return []byte(sb.String())
}

func encodeCanonical(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case json.Number:
		sb.WriteString(t.String())
	case string:
		b, _ := json.Marshal(t)
		sb.Write(b)
	case []any:
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			encodeCanonical(sb, e)
		}
		sb.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			sb.Write(kb)
			sb.WriteByte(':')
			encodeCanonical(sb, t[k])
		}
		sb.WriteByte('}')
	default:
		b, _ := json.Marshal(v)
		sb.Write(b)
	}
}

// computeHash 形成哈希链：prevHash 与本条事件的序号、类型、规范化载荷共同入链。
func computeHash(prevHash string, seq int64, typ string, payload json.RawMessage) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|", prevHash, seq, typ)
	h.Write(canonicalPayload(payload))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Store 是事件存储的最小接口：只能追加与顺序读取。
type Store interface {
	Append(typ, actor, idem string, payload any, clock Clock) (Event, error)
	Events() []Event
	Path() string // 内存存储返回 ":memory:"
}

type memStore struct {
	mu     sync.Mutex
	events []Event
}

func newMemStore() *memStore { return &memStore{} }

func (s *memStore) Path() string { return ":memory:" }

// appendRaw 由内部（文件存储重放）使用，直接落一条已编好号的事件。
func (s *memStore) appendLocked(e Event) {
	s.events = append(s.events, e)
}

func (s *memStore) Append(typ, actor, idem string, payload any, clock Clock) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := int64(len(s.events)) + 1
	prev := ""
	if len(s.events) > 0 {
		prev = s.events[len(s.events)-1].Hash
	}
	e := Event{
		ID:      fmt.Sprintf("evt-%06d", seq),
		Seq:     seq,
		Type:    typ,
		Time:    clock(),
		Actor:   actor,
		Idem:    idem,
		Payload: raw,
	}
	e.Hash = computeHash(prev, seq, typ, raw)
	s.events = append(s.events, e)
	return e, nil
}

func (s *memStore) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// fileStore 在内存存储之外把事件逐行追加到 JSONL 文件；重启时重放恢复。
type fileStore struct {
	*memStore
	path string
	f    *os.File
	w    *bufio.Writer
}

// openFileStore 打开（或创建）JSONL 事件日志并重放已有内容，校验哈希链。
func openFileStore(path string) (*fileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	fs := &fileStore{memStore: newMemStore(), path: path, f: f}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	prev := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("事件日志损坏（%s）：%w", path, err)
		}
		if e.Seq != int64(len(fs.events)+1) || computeHash(prev, e.Seq, e.Type, e.Payload) != e.Hash {
			return nil, fmt.Errorf("事件哈希链校验失败，位置 seq=%d", e.Seq)
		}
		fs.events = append(fs.events, e)
		prev = e.Hash
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	fs.w = bufio.NewWriter(f)
	return fs, nil
}

func (s *fileStore) Append(typ, actor, idem string, payload any, clock Clock) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	seq := int64(len(s.events)) + 1
	prev := ""
	if len(s.events) > 0 {
		prev = s.events[len(s.events)-1].Hash
	}
	e := Event{
		ID:      fmt.Sprintf("evt-%06d", seq),
		Seq:     seq,
		Type:    typ,
		Time:    clock(),
		Actor:   actor,
		Idem:    idem,
		Payload: raw,
		Hash:    computeHash(prev, seq, typ, raw),
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	if err := s.w.Flush(); err != nil {
		return Event{}, err
	}
	if err := s.f.Sync(); err != nil {
		return Event{}, err
	}
	s.events = append(s.events, e)
	return e, nil
}

func (s *fileStore) Path() string { return s.path }
