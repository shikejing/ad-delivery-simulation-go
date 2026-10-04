package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/clock"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/ledger"
)

// TestConcurrentBudgetNoPierce：大量并发请求竞争一个小预算计划，
// 花费绝不可能超过预算，且与流水统计自洽。
func TestConcurrentBudgetNoPierce(t *testing.T) {
	const budget = 1000
	const goroutines = 64
	cfg := config.Config{
		Seed:      1,
		StateDir:  t.TempDir(),
		Floor:     config.FloorConfig{FloorMicros: 100, MinBidMicros: 100},
		Frequency: config.FrequencyConfig{Window: time.Hour, MaxImpressions: 1_000_000},
		Campaigns: []config.CampaignConfig{
			{ID: "a", BudgetMicros: budget, Pacing: config.PacingConfig{}},
		},
	}
	e, err := New(cfg, clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var wg sync.WaitGroup
	var wins int64
	var mu sync.Mutex
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			d, derr := e.Decide(BidRequest{
				ID:         fmt.Sprintf("req-%d", i),
				User:       fmt.Sprintf("user-%d", i%8),
				At:         t0.Add(time.Duration(i) * time.Millisecond),
				Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 500}},
			})
			if derr != nil {
				t.Errorf("decide: %v", derr)
				return
			}
			if d.Winner != nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	snaps, stats := e.Snapshot()
	spent := snaps[0].SpentMicros
	if spent < 0 || spent > budget {
		t.Fatalf("budget pierced: spent=%d budget=%d", spent, budget)
	}
	// 每次成交恰好底价 100。
	if wins*100 != spent {
		t.Fatalf("wins(%d)*100 != spent(%d)", wins, spent)
	}
	if stats.Totals.SpendMicros != spent || stats.Totals.Impressions != wins {
		t.Fatalf("ledger not self-consistent with budget: totals=%+v spent=%d wins=%d",
			stats.Totals, spent, wins)
	}
	if stats.Totals.SpendMicros+snaps[0].RemainingMicros != budget {
		t.Fatal("spent + remaining != budget")
	}
}

// TestConcurrentPostEvents：并发投递事件（含重复 ID），统计必须准确。
func TestConcurrentPostEvents(t *testing.T) {
	cfg := config.Config{
		Seed:      1,
		StateDir:  t.TempDir(),
		Floor:     config.FloorConfig{FloorMicros: 0, MinBidMicros: 0},
		Frequency: config.FrequencyConfig{Window: time.Hour, MaxImpressions: 1_000_000},
		Campaigns: []config.CampaignConfig{{ID: "a", BudgetMicros: 1_000_000}},
	}
	e, _ := New(cfg, clock.NewFake(t0))
	defer e.Close()

	// 先建立 100 条曝光作为漏斗锚点。
	for i := 0; i < 100; i++ {
		_, err := e.Decide(BidRequest{
			ID: fmt.Sprintf("r%d", i), User: "u", At: t0,
			Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 10}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// 200 个 goroutine 各投一个点击，但只有 100 个唯一 ID，其余为重复投递。
	var wg sync.WaitGroup
	wg.Add(200)
	for i := 0; i < 200; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := i % 100
			ev := ledger.Event{
				ID:         fmt.Sprintf("click-%d", id),
				Type:       ledger.EventClick,
				RequestID:  fmt.Sprintf("r%d", id),
				CampaignID: "a",
				User:       "u",
				At:         t0.Add(time.Second),
			}
			if _, err := e.PostEvent(ev); err != nil {
				t.Errorf("post: %v", err)
			}
		}()
	}
	wg.Wait()
	_, stats := e.Snapshot()
	if stats.Campaigns["a"].Clicks != 100 {
		t.Fatalf("want exactly 100 unique clicks, got %d", stats.Campaigns["a"].Clicks)
	}
	if stats.Campaigns["a"].Impressions != 100 {
		t.Fatalf("want 100 impressions, got %d", stats.Campaigns["a"].Impressions)
	}
}

// TestConcurrentMixedReadWrite：并发竞价与快照读取，校验余额恒自洽。
func TestConcurrentMixedReadWrite(t *testing.T) {
	cfg := config.Config{
		Seed:      9,
		StateDir:  t.TempDir(),
		Floor:     config.FloorConfig{FloorMicros: 10, MinBidMicros: 10},
		Frequency: config.FrequencyConfig{Window: time.Hour, MaxImpressions: 1_000_000},
		Campaigns: []config.CampaignConfig{
			{ID: "a", BudgetMicros: 500},
			{ID: "b", BudgetMicros: 500},
		},
	}
	e, _ := New(cfg, clock.NewFake(t0))
	defer e.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snaps, stats := e.Snapshot()
				for _, s := range snaps {
					if s.SpentMicros+s.RemainingMicros != s.BudgetMicros {
						t.Errorf("inconsistent snapshot: %+v", s)
					}
				}
				var spend int64
				for _, s := range snaps {
					spend += s.SpentMicros
				}
				if stats.Totals.SpendMicros != spend {
					t.Errorf("ledger spend %d != campaign spend %d", stats.Totals.SpendMicros, spend)
				}
			}
		}
	}()

	for i := 0; i < 200; i++ {
		_, _ = e.Decide(BidRequest{
			ID: fmt.Sprintf("req-%d", i), User: fmt.Sprintf("u%d", i%20), At: t0,
			Candidates: []CandidateInput{
				{CampaignID: "a", BidMicros: int64(20 + i%50)},
				{CampaignID: "b", BidMicros: int64(20 + i%40)},
			},
		})
	}
	close(stop)
	wg.Wait()

	snaps, _ := e.Snapshot()
	for _, s := range snaps {
		if s.SpentMicros > s.BudgetMicros {
			t.Fatalf("final budget overrun: %+v", s)
		}
	}
}
