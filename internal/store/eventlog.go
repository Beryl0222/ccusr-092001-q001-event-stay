// Package store 提供追加式事件日志：每行一个 JSON 事件，进程重启后完整重放。
//
// 写入采用“行缓冲落盘 + fsync + 序号单调”的方式；同一事件序列在任意机器上
// 重放都得到相同状态，因此服务重启结果一致。
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/beryl0222/event-stay-orchestrator/domain"
)

// EventLog 是基于 JSONL 文件的追加事件日志。
type EventLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
	seq  int64
}

// OpenEventLog 打开（必要时创建）事件日志，并扫描出当前最大序号。
func OpenEventLog(dir string) (*EventLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开事件日志失败: %w", err)
	}
	l := &EventLog{path: path, f: f, w: bufio.NewWriter(f)}
	if err := l.recoverSeq(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

// recoverSeq 扫描全文件确定最大序号，同时及早发现损坏行。
func (l *EventLog) recoverSeq() error {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(l.f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var maxSeq int64
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e domain.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("事件日志存在无法解析的记录: %w", err)
		}
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if _, err := l.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	l.seq = maxSeq
	return nil
}

// Append 原子追加一条事件并落盘。返回写入后的事件（含分配序号）。
func (l *EventLog) Append(e domain.Event) (domain.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	raw, err := json.Marshal(e)
	if err != nil {
		l.seq--
		return e, err
	}
	if _, err := l.w.Write(raw); err != nil {
		l.seq--
		return e, err
	}
	if err := l.w.WriteByte('\n'); err != nil {
		l.seq--
		return e, err
	}
	if err := l.w.Flush(); err != nil {
		l.seq--
		return e, err
	}
	if err := l.f.Sync(); err != nil { // 宕机不丢已确认事件
		l.seq--
		return e, err
	}
	return e, nil
}

// Seq 返回已持久化的最大序号。
func (l *EventLog) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Replay 从头读取全部事件，逐条交给回调。
func (l *EventLog) Replay(handle func(domain.Event) error) error {
	l.mu.Lock()
	path := l.path
	l.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e domain.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return err
		}
		if err := handle(e); err != nil {
			return fmt.Errorf("重放事件 seq=%d 失败: %w", e.Seq, err)
		}
	}
	return sc.Err()
}

// Close 关闭日志文件。
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
