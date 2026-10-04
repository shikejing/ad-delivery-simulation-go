package sim

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/auction"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/events"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testConfig() *config.Config {
	return &config.Config{
		Floor: config.FloorConfig{MinCents: 10, MaxCents: 1000},
		Campaigns: []config.CampaignConfig{
			{ID: "a", BudgetCents: 500, FloorCents: 10, PacingWindow: time.Hour},
			{ID: "b", BudgetCents: 500, FloorCents: 10, FreqCap: 2, FreqWindow: time.Minute, PacingWindow: time.Hour},
		},
	}
}

func newEngine(t *testing.T, cfg *config.Config, clk *fakeClock) *Engine {
	t.Helper()
	eng, err := New(cfg, clk.Now(), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestInvalidConfigRejected(t *testing.T) {
	cfg := testConfig()
	cfg.Campaigns[0].BudgetCents = -1
	eng, err := New(cfg, time.Now(), nil)
	if err == nil || !errors.Is(err, config.ErrNegativeBudget) {
		t.Fatalf("want ErrNegativeBudget, got %v", err)
	}
	if eng != nil {
		t.Fatal("failed validation must not leave a partially built engine")
	}
}

func TestBudgetExhaustionStopsBidding(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := testConfig()
	eng := newEngine(t, cfg, clk)
	clk.Advance(time.Hour) // 放开节奏

	wins := 0
	for i := 0; i < 100; i++ {
		res := eng.Bid(auction.BidRequest{
			RequestID:  fmt.Sprintf("r%d", i),
			UserID:     fmt.Sprintf("u%d", i), // 避开频控
			Candidates: []auction.Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
		})
		if res.Winner != nil {
			wins++
		}
	}
	st := eng.Stats("a")
	if st.SpendCents > 500 {
		t.Fatalf("overspend: %d > 500", st.SpendCents)
	}
	if st.SpendCents != 500 || st.Impressions != wins {
		t.Fatalf("spend=%d impressions=%d wins=%d, budget should be exactly consumed", st.SpendCents, st.Impressions, wins)
	}
	// 预算耗尽后继续竞价必须失败且原因可区分。
	res := eng.Bid(auction.BidRequest{
		RequestID:  "after",
		UserID:     "ux",
		Candidates: []auction.Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
	})
	if res.Winner != nil || res.Reason != auction.RejectNoEligible ||
		res.Decisions[0].Reason != auction.RejectNoBudget {
		t.Fatalf("want no_budget after exhaustion, got %+v", res)
	}
}

func TestConcurrentBidsNoOverspend(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := testConfig()
	eng := newEngine(t, cfg, clk)
	clk.Advance(time.Hour)

	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				eng.Bid(auction.BidRequest{
					RequestID:  fmt.Sprintf("g%d-r%d", g, i),
					UserID:     fmt.Sprintf("u-%d-%d", g, i),
					Candidates: []auction.Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
				})
			}
		}(g)
	}
	wg.Wait()
	st := eng.Stats("a")
	if st.SpendCents > 500 {
		t.Fatalf("concurrent overspend: %d", st.SpendCents)
	}
	if eng.Remaining("a") != 500-st.SpendCents {
		t.Fatal("budget balance inconsistent with stats")
	}
}

func TestFreqCapEnforced(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	cfg := testConfig()
	eng := newEngine(t, cfg, clk)
	clk.Advance(time.Hour)

	mk := func(id string) auction.Result {
		return eng.Bid(auction.BidRequest{
			RequestID:  id,
			UserID:     "u1",
			Candidates: []auction.Candidate{{CampaignID: "b", BidCents: 100, Quality: 1.0}},
		})
	}
	if mk("r1").Winner == nil || mk("r2").Winner == nil {
		t.Fatal("first two impressions should win")
	}
	res := mk("r3")
	if res.Winner != nil || res.Decisions[0].Reason != auction.RejectFreqCapped {
		t.Fatalf("third impression must be freq capped, got %+v", res)
	}
	// 窗口推进后恢复。
	clk.Advance(2 * time.Minute)
	if mk("r4").Winner == nil {
		t.Fatal("freq window rolled, should win again")
	}
}

func TestPauseStopsImmediately(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	eng := newEngine(t, testConfig(), clk)
	clk.Advance(time.Hour)
	eng.Pause("a")
	res := eng.Bid(auction.BidRequest{
		RequestID:  "r1",
		UserID:     "u1",
		Candidates: []auction.Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
	})
	if res.Winner != nil || res.Decisions[0].Reason != auction.RejectNoBudget {
		t.Fatalf("paused campaign must not win, got %+v", res)
	}
	eng.Resume("a")
	res = eng.Bid(auction.BidRequest{
		RequestID:  "r2",
		UserID:     "u1",
		Candidates: []auction.Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
	})
	if res.Winner == nil {
		t.Fatal("resumed campaign should win")
	}
}

func TestDuplicateAndLateEvents(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	eng := newEngine(t, testConfig(), clk)
	ev := events.Event{ID: "click-1", Type: events.Click, CampaignID: "a", Timestamp: clk.Now()}
	if !eng.RecordEvent(ev) {
		t.Fatal("first delivery accepted")
	}
	if eng.RecordEvent(ev) {
		t.Fatal("duplicate must be rejected")
	}
	// 迟到事件（时间戳在过去）首次到达仍应计入。
	late := events.Event{ID: "conv-1", Type: events.Conversion, CampaignID: "a", Timestamp: clk.Now().Add(-time.Hour)}
	if !eng.RecordEvent(late) {
		t.Fatal("late event must be counted once")
	}
	if eng.RecordEvent(late) {
		t.Fatal("late duplicate must be rejected")
	}
	st := eng.Stats("a")
	if st.Clicks != 1 || st.Conversions != 1 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestDeterministicReplay(t *testing.T) {
	run := func() []string {
		clk := &fakeClock{now: time.Unix(0, 0)}
		eng := newEngine(t, testConfig(), clk)
		var out []string
		for i := 0; i < 50; i++ {
			if i == 25 {
				clk.Advance(time.Hour)
			}
			res := eng.Bid(auction.BidRequest{
				RequestID: fmt.Sprintf("r%d", i),
				UserID:    fmt.Sprintf("u%d", i%5),
				Candidates: []auction.Candidate{
					{CampaignID: "a", BidCents: int64(50 + i), Quality: 0.9},
					{CampaignID: "b", BidCents: int64(40 + i), Quality: 1.0},
				},
			})
			if res.Winner != nil {
				out = append(out, fmt.Sprintf("%s:%s:%d", res.RequestID, res.Winner.CampaignID, res.PriceCents))
			} else {
				out = append(out, fmt.Sprintf("%s:none:%s", res.RequestID, res.Reason))
			}
		}
		st := eng.Stats("a")
		out = append(out, fmt.Sprintf("stats-a:%d:%d", st.Impressions, st.SpendCents))
		return out
	}
	first := run()
	second := run()
	if len(first) != len(second) {
		t.Fatal("replay length mismatch")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("replay diverged at %d: %q vs %q", i, first[i], second[i])
		}
	}
}
