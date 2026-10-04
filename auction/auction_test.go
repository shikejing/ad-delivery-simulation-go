package auction

import (
	"testing"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/freqcap"
	"github.com/shikejing/ad-delivery-simulation-go/pacing"
)

func newAuctioneer(t *testing.T, camps []config.CampaignConfig, floor config.FloorConfig) *Auctioneer {
	t.Helper()
	pl := pacing.NewLedger()
	start := time.Unix(0, 0)
	now := func() time.Time { return start.Add(time.Hour) } // 放开节奏
	for _, c := range camps {
		pl.Add(c.ID, pacing.NewPacer(c.BudgetCents, c.PacingWindow, start, now))
	}
	return New(floor, camps, pl, freqcap.New("", nil))
}

func TestMultiCandidateWinnerAndSecondPrice(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "a", BudgetCents: 10000, FloorCents: 10, PacingWindow: time.Hour},
		{ID: "b", BudgetCents: 10000, FloorCents: 10, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{MinCents: 10, MaxCents: 1000})
	res := auc.Run(BidRequest{
		RequestID: "r1",
		UserID:    "u1",
		Candidates: []Candidate{
			{CampaignID: "a", BidCents: 100, Quality: 1.0}, // score 100
			{CampaignID: "b", BidCents: 80, Quality: 1.0},  // score 80
		},
	})
	if res.Winner == nil || res.Winner.CampaignID != "a" {
		t.Fatalf("winner=%+v, want a", res.Winner)
	}
	// 二价：80/1.0 + 1 = 81
	if res.PriceCents != 81 {
		t.Fatalf("price=%d, want 81", res.PriceCents)
	}
	if res.Decisions[1].Reason != RejectLostAuction {
		t.Fatalf("loser reason=%q, want lost_auction", res.Decisions[1].Reason)
	}
}

func TestQualityBreaksTie(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "a", BudgetCents: 10000, FloorCents: 0, PacingWindow: time.Hour},
		{ID: "b", BudgetCents: 10000, FloorCents: 0, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{})
	res := auc.Run(BidRequest{
		RequestID: "r1",
		UserID:    "u1",
		Candidates: []Candidate{
			{CampaignID: "a", BidCents: 100, Quality: 0.5}, // score 50
			{CampaignID: "b", BidCents: 60, Quality: 1.0},  // score 60
		},
	})
	if res.Winner == nil || res.Winner.CampaignID != "b" {
		t.Fatalf("winner=%+v, want b (higher score via quality)", res.Winner)
	}
	// 二价：50/1.0 + 1 = 51
	if res.PriceCents != 51 {
		t.Fatalf("price=%d, want 51", res.PriceCents)
	}
}

func TestSingleEligiblePaysFloor(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "a", BudgetCents: 10000, FloorCents: 25, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{MinCents: 10})
	res := auc.Run(BidRequest{
		RequestID:  "r1",
		UserID:     "u1",
		Candidates: []Candidate{{CampaignID: "a", BidCents: 100, Quality: 1.0}},
	})
	if res.Winner == nil || res.PriceCents != 25 {
		t.Fatalf("single eligible should pay floor 25, got %+v price=%d", res.Winner, res.PriceCents)
	}
}

func TestDistinguishableRejectReasons(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "low", BudgetCents: 10000, FloorCents: 50, PacingWindow: time.Hour},
		{ID: "bad", BudgetCents: 10000, PacingWindow: time.Hour},
		{ID: "ok", BudgetCents: 10000, FloorCents: 10, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{MinCents: 10})
	res := auc.Run(BidRequest{
		RequestID: "r1",
		UserID:    "u1",
		Candidates: []Candidate{
			{CampaignID: "low", BidCents: 30, Quality: 1.0}, // 低于计划底价 50
			{CampaignID: "bad", BidCents: -5, Quality: 1.0}, // 非法出价
			{CampaignID: "ok", BidCents: 60, Quality: 1.0},  // 合格
		},
	})
	if res.Decisions[0].Reason != RejectBelowFloor {
		t.Fatalf("reason=%q, want below_floor", res.Decisions[0].Reason)
	}
	if res.Decisions[1].Reason != RejectInvalidBid {
		t.Fatalf("reason=%q, want invalid_bid", res.Decisions[1].Reason)
	}
	if res.Winner == nil || res.Winner.CampaignID != "ok" {
		t.Fatalf("winner=%+v, want ok", res.Winner)
	}
}

func TestNoEligibleCandidates(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "a", BudgetCents: 10000, FloorCents: 100, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{MinCents: 100})
	res := auc.Run(BidRequest{
		RequestID:  "r1",
		UserID:     "u1",
		Candidates: []Candidate{{CampaignID: "a", BidCents: 10, Quality: 1.0}},
	})
	if res.Winner != nil || res.Reason != RejectNoEligible {
		t.Fatalf("want no winner with no_eligible, got %+v reason=%q", res.Winner, res.Reason)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	camps := []config.CampaignConfig{
		{ID: "a", BudgetCents: 10000, PacingWindow: time.Hour},
		{ID: "b", BudgetCents: 10000, PacingWindow: time.Hour},
	}
	auc := newAuctioneer(t, camps, config.FloorConfig{})
	mk := func() Result {
		return auc.Run(BidRequest{
			RequestID: "r1",
			UserID:    "u1",
			Candidates: []Candidate{
				{CampaignID: "b", BidCents: 100, Quality: 1.0},
				{CampaignID: "a", BidCents: 100, Quality: 1.0},
			},
		})
	}
	for i := 0; i < 10; i++ {
		if res := mk(); res.Winner.CampaignID != "a" {
			t.Fatalf("tie must break deterministically by campaign id, got %s", res.Winner.CampaignID)
		}
	}
}
