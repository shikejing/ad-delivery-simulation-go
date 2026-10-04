package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/clock"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/ledger"
)

var t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func testConfig(t *testing.T, budget int64) (config.Config, *clock.Fake) {
	t.Helper()
	cfg := config.Config{
		Seed:      42,
		StateDir:  t.TempDir(),
		Floor:     config.FloorConfig{FloorMicros: 100, MinBidMicros: 100},
		Frequency: config.FrequencyConfig{Window: time.Minute, MaxImpressions: 2},
		Campaigns: []config.CampaignConfig{
			{ID: "a", BudgetMicros: budget, Pacing: config.PacingConfig{}},
			{ID: "b", BudgetMicros: budget, Pacing: config.PacingConfig{}},
		},
	}
	return cfg, clock.NewFake(t0)
}

func TestEndToEndMultiCandidate(t *testing.T) {
	cfg, clk := testConfig(t, 10000)
	e, err := New(cfg, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	d, err := e.Decide(BidRequest{
		ID:   "r1",
		User: "u1",
		At:   t0,
		Candidates: []CandidateInput{
			{CampaignID: "a", BidMicros: 500, QualityPermillion: 1_000_000},
			{CampaignID: "b", BidMicros: 300, QualityPermillion: 1_000_000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Winner == nil || d.Winner.Candidate.CampaignID != "a" || d.Winner.ChargeMicros != 300 {
		t.Fatalf("unexpected decision: %+v", d)
	}
	snaps, stats := e.Snapshot()
	if snaps[0].SpentMicros != 300 {
		t.Fatalf("want a spent 300, got %d", snaps[0].SpentMicros)
	}
	if stats.Totals.SpendMicros != 300 || stats.Totals.Impressions != 1 {
		t.Fatalf("stats mismatch: %+v", stats.Totals)
	}

	// 点击与转化正常入账。
	res, err := e.PostEvent(ledger.Event{ID: "k1", Type: ledger.EventClick, RequestID: "r1", CampaignID: "a", User: "u1", At: t0.Add(time.Second)})
	if err != nil || !res.Accepted {
		t.Fatalf("click: %+v %v", res, err)
	}
	res, _ = e.PostEvent(ledger.Event{ID: "v1", Type: ledger.EventConversion, RequestID: "r1", CampaignID: "a", User: "u1", At: t0.Add(2 * time.Second), ValueMicros: 99})
	if !res.Accepted {
		t.Fatalf("conversion: %+v", res)
	}
}

func TestBudgetExhaustionAcrossRequests(t *testing.T) {
	// 预算仅 250，每次底价成交 100：两胜后耗尽，第三次该计划被 no_budget 刷下。
	cfg, clk := testConfig(t, 250)
	// 只保留单计划候选，使耗尽后请求变为"无合格候选"。
	cfg.Campaigns = cfg.Campaigns[:1]
	e, _ := New(cfg, clk)
	defer e.Close()

	for i := 0; i < 2; i++ {
		d, err := e.Decide(BidRequest{
			ID: fmt.Sprintf("r%d", i), User: fmt.Sprintf("u%d", i), At: t0.Add(time.Duration(i) * time.Second),
			Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 500}},
		})
		if err != nil || d.Winner == nil {
			t.Fatalf("win %d: %+v err=%v", i, d, err)
		}
	}
	snaps, _ := e.Snapshot()
	if snaps[0].SpentMicros != 200 || snaps[0].RemainingMicros != 50 {
		t.Fatalf("unexpected spent: %+v", snaps[0])
	}
	// 此时再来一次成交价 100 的请求：余额 50 不足，计划被刷下，无合格候选。
	d, _ := e.Decide(BidRequest{
		ID: "r2", User: "u-x", At: t0.Add(2 * time.Second),
		Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 500}},
	})
	if d.Winner != nil || d.NoWinnerReason != "all_ineligible" {
		t.Fatalf("budget exhausted -> no eligible candidate: %+v", d)
	}
	snaps, _ = e.Snapshot()
	if snaps[0].SpentMicros != 200 {
		t.Fatalf("failed auction must not spend: %d", snaps[0].SpentMicros)
	}
}

func TestBudgetCannotBePierced(t *testing.T) {
	cfg, clk := testConfig(t, 250)
	e, _ := New(cfg, clk)
	defer e.Close()

	d1, _ := e.Decide(BidRequest{ID: "r1", User: "u1", At: t0, Candidates: []CandidateInput{
		{CampaignID: "a", BidMicros: 500}, {CampaignID: "b", BidMicros: 300},
	}})
	// a 无法承担 300（预算 250）-> 被 no_budget 刷下；b 成交，二价无其他合格者 -> 底价 100。
	if d1.Winner == nil || d1.Winner.Candidate.CampaignID != "b" || d1.Winner.ChargeMicros != 100 {
		t.Fatalf("r1: %+v", d1)
	}
	snaps, _ := e.Snapshot()
	if snaps[0].SpentMicros != 0 || snaps[1].SpentMicros != 100 {
		t.Fatalf("spend mismatch: %+v", snaps)
	}
	// 刷下的 a 在淘汰列表中带可区分原因。
	found := false
	for _, rj := range d1.Rejected {
		if rj.Candidate.CampaignID == "a" && rj.Reason == "campaign_no_budget" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a rejected with no_budget: %+v", d1.Rejected)
	}
}

func TestPauseStopsImmediately(t *testing.T) {
	cfg, clk := testConfig(t, 10000)
	e, _ := New(cfg, clk)
	defer e.Close()
	if err := e.PauseCampaign("a"); err != nil {
		t.Fatal(err)
	}
	d, _ := e.Decide(BidRequest{ID: "r1", User: "u1", At: t0, Candidates: []CandidateInput{
		{CampaignID: "a", BidMicros: 999}, {CampaignID: "b", BidMicros: 300},
	}})
	if d.Winner == nil || d.Winner.Candidate.CampaignID != "b" {
		t.Fatalf("paused a must not win: %+v", d)
	}
}

func TestFrequencyCapAcrossRequests(t *testing.T) {
	cfg, clk := testConfig(t, 100000)
	e, _ := New(cfg, clk)
	defer e.Close()
	for i := 0; i < 2; i++ {
		d, err := e.Decide(BidRequest{ID: fmt.Sprintf("r%d", i), User: "u1", At: t0, Candidates: []CandidateInput{
			{CampaignID: "a", BidMicros: 500},
		}})
		if err != nil || d.Winner == nil {
			t.Fatalf("imp %d: %+v err=%v", i, d, err)
		}
	}
	// 第三次：频控上限 2，应无胜出者，原因 all_ineligible。
	d, _ := e.Decide(BidRequest{ID: "r2", User: "u1", At: t0.Add(30 * time.Second), Candidates: []CandidateInput{
		{CampaignID: "a", BidMicros: 500},
	}})
	if d.Winner != nil || d.NoWinnerReason != "all_ineligible" {
		t.Fatalf("want freq-capped no winner: %+v", d)
	}
	// 窗口滚动后恢复。
	clk.Set(t0.Add(61 * time.Second))
	d, _ = e.Decide(BidRequest{ID: "r3", User: "u1", At: t0.Add(61 * time.Second), Candidates: []CandidateInput{
		{CampaignID: "a", BidMicros: 500},
	}})
	if d.Winner == nil {
		t.Fatalf("should win after window rolls: %+v", d)
	}
}

func TestDuplicateRequestIdempotent(t *testing.T) {
	cfg, clk := testConfig(t, 10000)
	e, _ := New(cfg, clk)
	defer e.Close()
	req := BidRequest{ID: "r1", User: "u1", At: t0, Candidates: []CandidateInput{
		{CampaignID: "a", BidMicros: 500}, {CampaignID: "b", BidMicros: 300},
	}}
	d1, _ := e.Decide(req)
	d2, _ := e.Decide(req)
	if d2.Winner == nil || d2.Winner.Candidate.CampaignID != d1.Winner.Candidate.CampaignID ||
		d2.Winner.ChargeMicros != d1.Winner.ChargeMicros {
		t.Fatal("replayed decision mismatch")
	}
	_, stats := e.Snapshot()
	if stats.Totals.Impressions != 1 || stats.Totals.SpendMicros != d1.Winner.ChargeMicros {
		t.Fatalf("replay must not double-bill: %+v", stats.Totals)
	}
}

func TestInvalidConfigRejectedAtStartup(t *testing.T) {
	cfg, _ := testConfig(t, 100)
	cfg.Campaigns[0].BudgetMicros = -5
	if _, err := New(cfg, clock.NewFake(t0)); err == nil {
		t.Fatal("negative budget must be rejected")
	} else {
		if !errors.Is(err, err) { // sanity
			t.Fatal(err)
		}
	}
}

func TestDeterministicAcrossEngines(t *testing.T) {
	build := func(t *testing.T) *Engine {
		cfg := config.Config{
			Seed:      777,
			StateDir:  t.TempDir(),
			Floor:     config.FloorConfig{FloorMicros: 0, MinBidMicros: 0},
			Frequency: config.FrequencyConfig{Window: time.Hour, MaxImpressions: 100},
			Campaigns: []config.CampaignConfig{
				{ID: "a", BudgetMicros: 1_000_000},
				{ID: "b", BudgetMicros: 1_000_000},
				{ID: "c", BudgetMicros: 1_000_000},
			},
		}
		e, err := New(cfg, clock.NewFake(t0))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	run := func(e *Engine) []string {
		out := make([]string, 0, 30)
		for i := 0; i < 30; i++ {
			d, err := e.Decide(BidRequest{
				ID:   fmt.Sprintf("req-%02d", i),
				User: fmt.Sprintf("user-%d", i%5),
				At:   t0.Add(time.Duration(i) * time.Second),
				Candidates: []CandidateInput{
					{CampaignID: "a", BidMicros: int64(100 + i%7)},
					{CampaignID: "b", BidMicros: int64(100 + i%5), QualityPermillion: 1_200_000},
					{CampaignID: "c", BidMicros: int64(100 + i%3)},
				},
			})
			if err != nil {
				panic(err)
			}
			if d.Winner != nil {
				out = append(out, fmt.Sprintf("%s:%d", d.Winner.Candidate.CampaignID, d.Winner.ChargeMicros))
			} else {
				out = append(out, "none:"+d.NoWinnerReason)
			}
		}
		return out
	}
	e1 := build(t)
	e2 := build(t)
	defer e1.Close()
	defer e2.Close()
	r1, r2 := run(e1), run(e2)
	if len(r1) != len(r2) {
		t.Fatal("length mismatch")
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("non-deterministic at %d: %s vs %s", i, r1[i], r2[i])
		}
	}
}

// TestBudgetRecoveryAcrossRestart：进程重启后预算余额与频控状态仍然生效。
func TestBudgetRecoveryAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Seed:      1,
		StateDir:  dir,
		Floor:     config.FloorConfig{FloorMicros: 100, MinBidMicros: 100},
		Frequency: config.FrequencyConfig{Window: time.Hour, MaxImpressions: 100},
		Campaigns: []config.CampaignConfig{{ID: "a", BudgetMicros: 250}},
	}
	e1, err := New(cfg, clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	// 花掉 200（两次底价成交），用不同用户避免频控。
	for i := 0; i < 2; i++ {
		d, err := e1.Decide(BidRequest{
			ID: fmt.Sprintf("r%d", i), User: fmt.Sprintf("u%d", i), At: t0,
			Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 500}},
		})
		if err != nil || d.Winner == nil {
			t.Fatalf("win %d failed: %+v err=%v", i, d, err)
		}
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟进程重启：同目录新建引擎。
	e2, err := New(cfg, clock.NewFake(t0.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	snaps, _ := e2.Snapshot()
	if snaps[0].SpentMicros != 200 || snaps[0].RemainingMicros != 50 {
		t.Fatalf("budget not recovered after restart: %+v", snaps[0])
	}
	// 成交价 100 > 余额 50：计划应被刷下。
	d, err := e2.Decide(BidRequest{
		ID: "r2", User: "u9", At: t0.Add(time.Second),
		Candidates: []CandidateInput{{CampaignID: "a", BidMicros: 500}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Winner != nil {
		t.Fatalf("recovered exhausted-ish budget must block: %+v", d)
	}
}
