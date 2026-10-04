package campaign

import (
	"testing"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/config"
)

var t0 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func TestChargeBudgetCap(t *testing.T) {
	c := newCampaign(config.CampaignConfig{ID: "c", BudgetMicros: 100}, t0)
	if !c.Charge(60, t0) {
		t.Fatal("first charge should pass")
	}
	if c.Charge(50, t0) {
		t.Fatal("charge beyond budget must fail")
	}
	if c.SpentMicros() != 60 {
		t.Fatalf("failed charge must not change spent, got %d", c.SpentMicros())
	}
	if !c.Charge(40, t0) {
		t.Fatal("exact remaining should pass")
	}
	if c.RemainingMicros() != 0 {
		t.Fatal("remaining should be 0")
	}
	ok, reason := c.Eligible(t0)
	if ok || reason != ReasonNoBudget {
		t.Fatalf("exhausted campaign must be ineligible/no_budget, got %v %s", ok, reason)
	}
}

func TestPauseStopsEligibility(t *testing.T) {
	c := newCampaign(config.CampaignConfig{ID: "c", BudgetMicros: 1000}, t0)
	c.Pause()
	ok, reason := c.Eligible(t0)
	if ok || reason != ReasonPaused {
		t.Fatalf("paused must be ineligible, got %v %s", ok, reason)
	}
	if c.Charge(10, t0) {
		t.Fatal("paused campaign must not be charged")
	}
}

func TestPacingSmooth(t *testing.T) {
	// 10 个切片，预算 1000，每片目标 100，不允许突发。
	cfg := config.CampaignConfig{
		ID:           "c",
		BudgetMicros: 1000,
		Pacing: config.PacingConfig{
			Horizon:         10 * time.Second,
			Slice:           time.Second,
			BurstPermillion: 0,
		},
	}
	c := newCampaign(cfg, t0)
	// 第 0 片：最多花 100，瞬间花 300 应被拒绝（不能瞬间放完）。
	if c.Charge(300, t0) {
		t.Fatal("pacing must reject burst beyond first-slice cap")
	}
	if !c.Charge(100, t0) {
		t.Fatal("charge up to slice target should pass")
	}
	if c.Charge(1, t0) {
		t.Fatal("charge beyond slice cap must fail")
	}
	// 推进到第 1 片：累计上限 200。
	if !c.Charge(100, t0.Add(1*time.Second)) {
		t.Fatal("charge in next slice should pass")
	}
	// 到第 4 片（idx=4，served=5）：累计上限 500；再补 300。
	if !c.Charge(300, t0.Add(4*time.Second)) {
		t.Fatal("catching up to elapsed time should pass")
	}
	if c.SpentMicros() != 500 {
		t.Fatalf("want spent 500, got %d", c.SpentMicros())
	}
}

func TestPacingBurstAndFinalCap(t *testing.T) {
	cfg := config.CampaignConfig{
		ID:           "c",
		BudgetMicros: 1000,
		Pacing: config.PacingConfig{
			Horizon:         10 * time.Second,
			Slice:           time.Second,
			BurstPermillion: 500_000, // 允许 50% 突发 => 首片上限 150
		},
	}
	c := newCampaign(cfg, t0)
	if !c.Charge(150, t0) {
		t.Fatal("burst of 50% should be allowed")
	}
	if c.Charge(1, t0) {
		t.Fatal("burst beyond allowance must fail")
	}
	// Horizon 结束后上限恒为预算。
	if !c.Charge(850, t0.Add(30*time.Second)) {
		t.Fatal("after horizon full budget should be spendable")
	}
	if c.SpentMicros() != 1000 {
		t.Fatalf("want 1000, got %d", c.SpentMicros())
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry([]config.CampaignConfig{
		{ID: "a", BudgetMicros: 10},
		{ID: "b", BudgetMicros: 20},
	}, t0, nil)
	if reg.Get("a") == nil || reg.Get("zz") != nil {
		t.Fatal("registry lookup broken")
	}
	if ids := reg.IDs(); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("id order mismatch: %v", ids)
	}
}
