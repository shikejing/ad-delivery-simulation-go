// Package config 定义竞价模拟链路的配置项及其启动期校验。
package config

import (
	"fmt"
	"time"
)

// 金额一律使用 int64 微元（Micros）表示，避免浮点运算带来的不确定性。
// 质量因子使用百万分比（Permillion）表示：1_000_000 表示 1.0。

// FloorConfig 配置底价相关参数。
type FloorConfig struct {
	// FloorMicros 为默认底价，即最低可接受成交价格（>=0）。
	FloorMicros int64
	// MinBidMicros 为允许参与竞价的最小出价，必须 >= FloorMicros，
	// 否则视为底价配置自相矛盾。
	MinBidMicros int64
}

// FrequencyConfig 配置频次控制。
type FrequencyConfig struct {
	// Window 为滚动时间窗口长度，必须 > 0。
	Window time.Duration
	// MaxImpressions 为单个用户在一个窗口内允许的最大曝光数，必须 > 0。
	MaxImpressions int
}

// PacingConfig 配置单个投放计划的平滑节奏。
type PacingConfig struct {
	// Horizon 为计划的预计投放时长；<=0 表示不做平滑（仅受预算约束）。
	Horizon time.Duration
	// Slice 为平滑切片长度，用于计算每段时间的目标花费与突发额度，
	// 启用平滑时必须 > 0 且 <= Horizon。
	Slice time.Duration
	// BurstPermillion 为相对单切片目标额度允许的突发比例（百万分比），
	// 例如 100_000 表示允许超出目标 10%。取值范围 [0, 1_000_000]。
	BurstPermillion int64
}

// CampaignConfig 为单个投放计划的配置。
type CampaignConfig struct {
	ID           string
	BudgetMicros int64
	Pacing       PacingConfig
}

// Config 为整条模拟链路的配置。
type Config struct {
	// Seed 为确定性随机种子，用于排序并列时的可复现打散。
	Seed int64
	// StateDir 为频控状态与记账流水的持久化目录（进程内本地文件）。
	StateDir string
	// Floor 为底价配置。
	Floor FloorConfig
	// Frequency 为频控配置。
	Frequency FrequencyConfig
	// Campaigns 为投放计划列表。
	Campaigns []CampaignConfig
}

// 校验失败原因码。
const (
	ErrNegativeBudget       = "negative_budget"
	ErrDuplicateCampaignID  = "duplicate_campaign_id"
	ErrEmptyCampaignID      = "empty_campaign_id"
	ErrNegativeFloor        = "negative_floor"
	ErrNegativeMinBid       = "negative_min_bid"
	ErrFloorContradiction   = "floor_contradiction"
	ErrInvalidFreqWindow    = "invalid_frequency_window"
	ErrInvalidFreqMax       = "invalid_frequency_max"
	ErrInvalidPacingHorizon = "invalid_pacing_horizon"
	ErrInvalidPacingSlice   = "invalid_pacing_slice"
	ErrInvalidBurst         = "invalid_burst_permillion"
	ErrEmptyStateDir        = "empty_state_dir"
)

// ValidationError 表示可区分的启动期配置校验失败。
type ValidationError struct {
	// Field 为出错的配置字段路径。
	Field string
	// Code 为机器可判别的失败原因码。
	Code string
	Msg  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config validation failed at %s: %s (code=%s)", e.Field, e.Msg, e.Code)
}

func ve(field, code, format string, args ...any) error {
	return &ValidationError{Field: field, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Validate 在启动阶段校验整份配置；首个非法项会立即返回可区分的失败原因，
// 不产生任何副作用（不创建目录、不写文件）。
func (c *Config) Validate() error {
	if c.StateDir == "" {
		return ve("state_dir", ErrEmptyStateDir, "state_dir must not be empty")
	}
	if c.Floor.FloorMicros < 0 {
		return ve("floor.floor_micros", ErrNegativeFloor,
			"floor must be >= 0, got %d", c.Floor.FloorMicros)
	}
	if c.Floor.MinBidMicros < 0 {
		return ve("floor.min_bid_micros", ErrNegativeMinBid,
			"min bid must be >= 0, got %d", c.Floor.MinBidMicros)
	}
	if c.Floor.MinBidMicros < c.Floor.FloorMicros {
		return ve("floor.min_bid_micros", ErrFloorContradiction,
			"min bid %d must be >= floor %d", c.Floor.MinBidMicros, c.Floor.FloorMicros)
	}
	if c.Frequency.Window <= 0 {
		return ve("frequency.window", ErrInvalidFreqWindow,
			"frequency window must be > 0, got %s", c.Frequency.Window)
	}
	if c.Frequency.MaxImpressions <= 0 {
		return ve("frequency.max_impressions", ErrInvalidFreqMax,
			"frequency max impressions must be > 0, got %d", c.Frequency.MaxImpressions)
	}
	seen := make(map[string]struct{}, len(c.Campaigns))
	for i := range c.Campaigns {
		camp := &c.Campaigns[i]
		field := fmt.Sprintf("campaigns[%d]", i)
		if camp.ID == "" {
			return ve(field+".id", ErrEmptyCampaignID, "campaign id must not be empty")
		}
		if _, dup := seen[camp.ID]; dup {
			return ve(field+".id", ErrDuplicateCampaignID, "duplicate campaign id %q", camp.ID)
		}
		seen[camp.ID] = struct{}{}
		if camp.BudgetMicros < 0 {
			return ve(field+".budget_micros", ErrNegativeBudget,
				"budget must be >= 0, got %d", camp.BudgetMicros)
		}
		p := &camp.Pacing
		pfield := field + ".pacing"
		if p.Horizon < 0 {
			return ve(pfield+".horizon", ErrInvalidPacingHorizon,
				"pacing horizon must be >= 0 (0 disables pacing), got %s", p.Horizon)
		}
		if p.Horizon > 0 {
			if p.Slice <= 0 {
				return ve(pfield+".slice", ErrInvalidPacingSlice,
					"pacing slice must be > 0 when pacing is enabled, got %s", p.Slice)
			}
			if p.Slice > p.Horizon {
				return ve(pfield+".slice", ErrInvalidPacingSlice,
					"pacing slice %s must be <= horizon %s", p.Slice, p.Horizon)
			}
		}
		if p.BurstPermillion < 0 || p.BurstPermillion > 1_000_000 {
			return ve(pfield+".burst_permillion", ErrInvalidBurst,
				"burst permillion must be in [0, 1000000], got %d", p.BurstPermillion)
		}
	}
	return nil
}
