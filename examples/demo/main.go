// Command demo 在本地跑一段可复现的竞价投放模拟，打印每次决策与最终统计。
// 用法: go run ./examples/demo
package main

import (
	"fmt"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/clock"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/engine"
	"github.com/shikejing/ad-delivery-simulation-go/ledger"
)

func main() {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)

	// camp-a 关闭平滑（仅受预算约束），靠较高质量参与竞争；
	// camp-b 启用线性平滑，投放初期花费受单切片上限约束。
	cfg := config.Config{
		Seed:      42,
		StateDir:  ".state/demo",
		Floor:     config.FloorConfig{FloorMicros: 100, MinBidMicros: 100},
		Frequency: config.FrequencyConfig{Window: 2 * time.Minute, MaxImpressions: 3},
		Campaigns: []config.CampaignConfig{
			{ID: "camp-a", BudgetMicros: 2000, Pacing: config.PacingConfig{}},
			{ID: "camp-b", BudgetMicros: 2000, Pacing: config.PacingConfig{
				Horizon: 10 * time.Minute, Slice: time.Minute, BurstPermillion: 100_000,
			}},
		},
	}

	eng, err := engine.New(cfg, clk)
	must(err)
	defer eng.Close()

	for i := 0; i < 5; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		d, err := eng.Decide(engine.BidRequest{
			ID:   reqID,
			User: "user-1",
			At:   t0.Add(time.Duration(i) * 20 * time.Second),
			Candidates: []engine.CandidateInput{
				{CampaignID: "camp-a", BidMicros: 500, QualityPermillion: 1_200_000},
				{CampaignID: "camp-b", BidMicros: 400},
			},
		})
		must(err)
		if d.Winner != nil {
			fmt.Printf("%s: winner=%s charge=%d\n", reqID, d.Winner.Candidate.CampaignID, d.Winner.ChargeMicros)
			// 回传点击（第 0、2 次）。
			if i%2 == 0 {
				_, err := eng.PostEvent(ledger.Event{
					ID: "click-" + reqID, Type: ledger.EventClick,
					RequestID: reqID, CampaignID: d.Winner.Candidate.CampaignID,
					User: "user-1", At: t0.Add(time.Duration(i)*20*time.Second + time.Second),
				})
				must(err)
			}
		} else {
			fmt.Printf("%s: no winner (%s)\n", reqID, d.NoWinnerReason)
		}
	}

	snaps, stats := eng.Snapshot()
	fmt.Println("\n--- campaign snapshots ---")
	for _, s := range snaps {
		fmt.Printf("%s: status=%s budget=%d spent=%d remaining=%d\n",
			s.ID, s.Status, s.BudgetMicros, s.SpentMicros, s.RemainingMicros)
	}
	fmt.Println("--- totals ---")
	fmt.Printf("impressions=%d clicks=%d conversions=%d spend=%d\n",
		stats.Totals.Impressions, stats.Totals.Clicks, stats.Totals.Conversions, stats.Totals.SpendMicros)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
