// adsim 在本地运行一次确定性的竞价投放模拟。
// 相同随机种子与相同配置下，输出逐条一致、可复现。
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/auction"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/sim"
)

func main() {
	seed := flag.Int64("seed", 42, "随机种子")
	requests := flag.Int("requests", 200, "竞价请求数量")
	flag.Parse()

	cfg := &config.Config{
		Floor: config.FloorConfig{MinCents: 10, MaxCents: 500},
		Campaigns: []config.CampaignConfig{
			{ID: "camp-a", BudgetCents: 5000, FloorCents: 20, FreqCap: 3, FreqWindow: time.Minute, PacingWindow: 10 * time.Second},
			{ID: "camp-b", BudgetCents: 3000, FloorCents: 15, FreqCap: 5, FreqWindow: time.Minute, PacingWindow: 10 * time.Second},
			{ID: "camp-c", BudgetCents: 1000, FloorCents: 10, PacingWindow: 10 * time.Second},
		},
	}
	start := time.Unix(1700000000, 0)
	eng, err := sim.New(cfg, start, nil)
	if err != nil {
		fmt.Println("config rejected:", err)
		return
	}

	rng := rand.New(rand.NewSource(*seed))
	camps := []string{"camp-a", "camp-b", "camp-c"}
	for i := 0; i < *requests; i++ {
		req := auction.BidRequest{
			RequestID: fmt.Sprintf("req-%06d", i),
			UserID:    fmt.Sprintf("user-%d", rng.Intn(20)),
		}
		n := 1 + rng.Intn(len(camps))
		perm := rng.Perm(len(camps))
		for j := 0; j < n; j++ {
			req.Candidates = append(req.Candidates, auction.Candidate{
				CampaignID: camps[perm[j]],
				BidCents:   int64(5 + rng.Intn(200)),
				Quality:    0.3 + 0.7*rng.Float64(),
			})
		}
		res := eng.Bid(req)
		if res.Winner != nil {
			fmt.Printf("%s winner=%s price=%d\n", res.RequestID, res.Winner.CampaignID, res.PriceCents)
		} else {
			fmt.Printf("%s no-winner reason=%s\n", res.RequestID, res.Reason)
		}
	}

	snap := eng.Snapshot()
	ids := make([]string, 0, len(snap))
	for id := range snap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Println("--- final stats ---")
	for _, id := range ids {
		st := snap[id]
		fmt.Printf("%s impressions=%d clicks=%d conversions=%d spend=%d remaining=%d\n",
			id, st.Impressions, st.Clicks, st.Conversions, st.SpendCents, eng.Remaining(id))
	}
}
