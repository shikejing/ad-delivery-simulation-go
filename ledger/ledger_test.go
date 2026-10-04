package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func evt(id string, typ EventType, at time.Time, value int64) Event {
	return Event{ID: id, Type: typ, RequestID: "r1", CampaignID: "c1", User: "u1", At: at, ValueMicros: value}
}

func TestFunnelHappyPath(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if r, _ := l.Record(evt("e1", EventImpression, t0, 120)); !r.Accepted {
		t.Fatal("impression should be accepted")
	}
	if r, _ := l.Record(evt("e2", EventClick, t0.Add(time.Second), 0)); !r.Accepted {
		t.Fatal("click should be accepted")
	}
	if r, _ := l.Record(evt("e3", EventConversion, t0.Add(2*time.Second), 500)); !r.Accepted {
		t.Fatal("conversion should be accepted")
	}
	s := l.Stats()
	cs := s.Campaigns["c1"]
	if cs.Impressions != 1 || cs.Clicks != 1 || cs.Conversions != 1 || cs.SpendMicros != 120 || cs.ValueMicros != 500 {
		t.Fatalf("unexpected stats: %+v", cs)
	}
}

func TestDuplicateDeliveryIdempotent(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "events.log"))
	defer l.Close()
	r1, _ := l.Record(evt("e1", EventImpression, t0, 120))
	r2, _ := l.Record(evt("e1", EventImpression, t0, 999)) // 同 ID 重复投递且内容不同
	if !r1.Accepted || !r2.Duplicate || !r2.Accepted {
		t.Fatalf("second delivery must be duplicate of first verdict: %+v", r2)
	}
	// 重复点击同样幂等。
	l.Record(evt("c1", EventClick, t0, 0))
	r3, _ := l.Record(evt("c1", EventClick, t0, 0))
	if !r3.Duplicate {
		t.Fatal("duplicate click must be flagged")
	}
	s := l.Stats()
	if s.Campaigns["c1"].Impressions != 1 || s.Campaigns["c1"].Clicks != 1 || s.Campaigns["c1"].SpendMicros != 120 {
		t.Fatalf("stats must not deviate: %+v", s.Campaigns["c1"])
	}
}

func TestOrphanAndOutOfOrder(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "events.log"))
	defer l.Close()
	// 无曝光的点击。
	r, _ := l.Record(evt("x", EventClick, t0, 0))
	if r.Accepted || r.RejectReason != RejectOrphanClick {
		t.Fatalf("want orphan_click, got %+v", r)
	}
	l.Record(evt("i", EventImpression, t0, 10))
	// 早于曝光的点击 -> out_of_order。
	r, _ = l.Record(evt("early", EventClick, t0.Add(-time.Second), 0))
	if r.Accepted || r.RejectReason != RejectOutOfOrder {
		t.Fatalf("want out_of_order, got %+v", r)
	}
	// 无点击的转化 -> orphan_conversion。
	r, _ = l.Record(evt("v", EventConversion, t0, 1))
	if r.Accepted || r.RejectReason != RejectOrphanConversion {
		t.Fatalf("want orphan_conversion, got %+v", r)
	}
	l.Record(evt("c", EventClick, t0.Add(time.Second), 0))
	// 合法迟到转化（晚很久才回传）应该被接受。
	r, _ = l.Record(evt("v2", EventConversion, t0.Add(time.Hour), 7))
	if !r.Accepted {
		t.Fatalf("late-but-valid conversion should be accepted: %+v", r)
	}
	// 被拒事件自身也幂等：同 ID 重放返回 duplicate 且不改变统计。
	r, _ = l.Record(evt("x", EventClick, t0, 0))
	if !r.Duplicate || r.Accepted {
		t.Fatalf("rejected event replay must be duplicate: %+v", r)
	}
	s := l.Stats()
	if s.Campaigns["c1"].Clicks != 1 || s.Campaigns["c1"].Conversions != 1 {
		t.Fatalf("stats deviated: %+v", s.Campaigns["c1"])
	}
}

func TestReplayAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.log")
	l1, _ := New(path)
	l1.Record(evt("e1", EventImpression, t0, 120))
	l1.Record(evt("e2", EventClick, t0.Add(time.Second), 0))
	l1.Close()

	l2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	s := l2.Stats()
	if s.Campaigns["c1"].Impressions != 1 || s.Campaigns["c1"].Clicks != 1 || s.Campaigns["c1"].SpendMicros != 120 {
		t.Fatalf("replayed stats mismatch: %+v", s.Campaigns["c1"])
	}
	// 重启后重复事件仍被识别。
	if r, _ := l2.Record(evt("e1", EventImpression, t0, 120)); !r.Duplicate {
		t.Fatal("duplicate must be recognized after restart")
	}
}
