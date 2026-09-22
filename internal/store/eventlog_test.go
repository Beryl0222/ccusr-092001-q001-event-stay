package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/beryl0222/event-stay-orchestrator/domain"
)

// 追加事件后重开日志：序号连续、事件可完整重放。
func TestAppendAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	log, err := OpenEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(domain.VenueRegistered{Venue: domain.Venue{ID: "v1", Tier: domain.TierA, Seats: 9000}})
	for i := 0; i < 3; i++ {
		if _, err := log.Append(domain.Event{Type: domain.EvVenueRegistered, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if log.Seq() != 3 {
		t.Fatalf("序号期望 3，实际 %d", log.Seq())
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	log2, err := OpenEventLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log2.Close()
	if log2.Seq() != 3 {
		t.Fatalf("重开后序号期望 3，实际 %d", log2.Seq())
	}
	count := 0
	if err := log2.Replay(func(domain.Event) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("重放事件数期望 3，实际 %d", count)
	}
	// 重开后继续追加，序号不重复。
	e, err := log2.Append(domain.Event{Type: domain.EvVenueRegistered, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 4 {
		t.Fatalf("新事件序号期望 4，实际 %d", e.Seq)
	}
}
