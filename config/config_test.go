package config

import (
	"errors"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Floor: FloorConfig{MinCents: 10, MaxCents: 100},
		Campaigns: []CampaignConfig{
			{ID: "a", BudgetCents: 1000, FloorCents: 20, FreqCap: 3, FreqWindow: time.Minute, PacingWindow: time.Hour},
		},
	}
}

func TestValidateOK(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestValidateNegativeBudget(t *testing.T) {
	c := validConfig()
	c.Campaigns[0].BudgetCents = -1
	if err := Validate(c); !errors.Is(err, ErrNegativeBudget) {
		t.Fatalf("want ErrNegativeBudget, got %v", err)
	}
}

func TestValidateInvalidFreqCap(t *testing.T) {
	c := validConfig()
	c.Campaigns[0].FreqCap = -2
	if err := Validate(c); !errors.Is(err, ErrInvalidFreqCap) {
		t.Fatalf("want ErrInvalidFreqCap, got %v", err)
	}
	c = validConfig()
	c.Campaigns[0].FreqWindow = 0 // 有上限但窗口非法
	if err := Validate(c); !errors.Is(err, ErrInvalidWindow) {
		t.Fatalf("want ErrInvalidWindow, got %v", err)
	}
}

func TestValidateFloorConflict(t *testing.T) {
	c := validConfig()
	c.Floor.MaxCents = 5 // 低于 MinCents
	if err := Validate(c); !errors.Is(err, ErrFloorConflict) {
		t.Fatalf("want ErrFloorConflict, got %v", err)
	}
	c = validConfig()
	c.Campaigns[0].FloorCents = 200 // 高于全局上限
	if err := Validate(c); !errors.Is(err, ErrFloorConflict) {
		t.Fatalf("want ErrFloorConflict, got %v", err)
	}
}

func TestValidateDuplicateAndMissingID(t *testing.T) {
	c := validConfig()
	c.Campaigns = append(c.Campaigns, c.Campaigns[0])
	if err := Validate(c); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("want ErrDuplicateID, got %v", err)
	}
	c = validConfig()
	c.Campaigns[0].ID = ""
	if err := Validate(c); !errors.Is(err, ErrMissingID) {
		t.Fatalf("want ErrMissingID, got %v", err)
	}
}

func TestValidateIsPureAndDistinguishable(t *testing.T) {
	c := validConfig()
	c.Campaigns[0].BudgetCents = -1
	err1 := Validate(c)
	err2 := Validate(c)
	if c.Campaigns[0].BudgetCents != -1 || len(c.Campaigns) != 1 {
		t.Fatal("Validate mutated config")
	}
	if !errors.Is(err1, ErrNegativeBudget) || !errors.Is(err2, ErrNegativeBudget) {
		t.Fatal("repeated validation should return same distinguishable reason")
	}
	if errors.Is(err1, ErrFloorConflict) {
		t.Fatal("reasons must be distinguishable")
	}
}
