package events

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	l := NewLedger()
	ev := Event{ID: "e1", Type: Impression, CampaignID: "a", PriceCents: 50}
	if !l.Record(ev) {
		t.Fatal("first delivery must be accepted")
	}
	for i := 0; i < 5; i++ {
		if l.Record(ev) {
			t.Fatal("duplicate delivery must be ignored")
		}
	}
	st := l.Stats("a")
	if st.Impressions != 1 || st.SpendCents != 50 {
		t.Fatalf("duplicate billing detected: %+v", st)
	}
}

func TestLateEventsDoNotSkewStats(t *testing.T) {
	l := NewLedger()
	base := time.Unix(1700000000, 0)
	// 迟到事件：时间戳远早于其他事件，乱序到达。
	evs := []Event{
		{ID: "c1", Type: Click, CampaignID: "a", Timestamp: base.Add(time.Minute)},
		{ID: "i1", Type: Impression, CampaignID: "a", PriceCents: 30, Timestamp: base},
		{ID: "v1", Type: Conversion, CampaignID: "a", Timestamp: base.Add(2 * time.Minute)},
		{ID: "i1", Type: Impression, CampaignID: "a", PriceCents: 30, Timestamp: base}, // 迟到重投
	}
	for _, e := range evs {
		l.Record(e)
	}
	st := l.Stats("a")
	want := Stats{Impressions: 1, Clicks: 1, Conversions: 1, SpendCents: 30}
	if st != want {
		t.Fatalf("stats=%+v, want %+v", st, want)
	}
}

func TestConcurrentRecording(t *testing.T) {
	l := NewLedger()
	const goroutines = 20
	const perG = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				id := fmt.Sprintf("g%d-i%d", g, i)
				// 同一事件并发重复投递两次，只应计一次。
				l.Record(Event{ID: id, Type: Impression, CampaignID: "a", PriceCents: 1})
				l.Record(Event{ID: id, Type: Impression, CampaignID: "a", PriceCents: 1})
			}
		}(g)
	}
	wg.Wait()
	st := l.Stats("a")
	if st.Impressions != goroutines*perG || st.SpendCents != goroutines*perG {
		t.Fatalf("concurrent accounting inconsistent: %+v", st)
	}
}
