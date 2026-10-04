package config

import (
	"errors"
	"testing"
	"time"
)

func baseConfig() Config {
	return Config{
		StateDir:  "/tmp/ad-sim-test",
		Floor:     FloorConfig{FloorMicros: 100, MinBidMicros: 100},
		Frequency: FrequencyConfig{Window: time.Minute, MaxImpressions: 3},
		Campaigns: []CampaignConfig{
			{ID: "c1", BudgetMicros: 1000, Pacing: PacingConfig{}},
		},
	}
}

func TestValidateOK(t *testing.T) {
	c := baseConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateNegativeBudget(t *testing.T) {
	c := baseConfig()
	c.Campaigns[0].BudgetMicros = -1
	err := c.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != ErrNegativeBudget {
		t.Fatalf("want negative_budget, got %v", err)
	}
}

func TestValidateFrequency(t *testing.T) {
	c := baseConfig()
	c.Frequency.Window = 0
	var ve *ValidationError
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrInvalidFreqWindow {
		t.Fatalf("want invalid window, got %v", err)
	}

	c = baseConfig()
	c.Frequency.MaxImpressions = 0
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrInvalidFreqMax {
		t.Fatalf("want invalid max, got %v", err)
	}
}

func TestValidateFloorContradiction(t *testing.T) {
	c := baseConfig()
	c.Floor.FloorMicros = 200
	c.Floor.MinBidMicros = 100
	var ve *ValidationError
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrFloorContradiction {
		t.Fatalf("want contradiction, got %v", err)
	}
}

func TestValidateNegativeFloor(t *testing.T) {
	c := baseConfig()
	c.Floor.FloorMicros = -1
	var ve *ValidationError
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrNegativeFloor {
		t.Fatalf("want negative floor, got %v", err)
	}
}

func TestValidateDuplicateCampaign(t *testing.T) {
	c := baseConfig()
	c.Campaigns = append(c.Campaigns, c.Campaigns[0])
	var ve *ValidationError
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrDuplicateCampaignID {
		t.Fatalf("want duplicate, got %v", err)
	}
}

func TestValidatePacing(t *testing.T) {
	c := baseConfig()
	c.Campaigns[0].Pacing = PacingConfig{Horizon: time.Minute, Slice: 2 * time.Minute}
	var ve *ValidationError
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrInvalidPacingSlice {
		t.Fatalf("want slice > horizon, got %v", err)
	}

	c = baseConfig()
	c.Campaigns[0].Pacing = PacingConfig{Horizon: time.Minute, Slice: time.Second, BurstPermillion: 2_000_000}
	if err := c.Validate(); !errors.As(err, &ve) || ve.Code != ErrInvalidBurst {
		t.Fatalf("want invalid burst, got %v", err)
	}
}

func TestValidateNoSideEffects(t *testing.T) {
	c := baseConfig()
	c.StateDir = ""
	if err := c.Validate(); err == nil {
		t.Fatal("want error for empty state dir")
	}
}
