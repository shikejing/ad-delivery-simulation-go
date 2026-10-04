// Package config 定义模拟器的配置结构与启动期校验。
package config

import (
	"errors"
	"fmt"
	"time"
)

// 可区分的校验失败原因，调用方可用 errors.Is 判定。
var (
	ErrNegativeBudget = errors.New("config: negative budget")
	ErrInvalidFreqCap = errors.New("config: invalid frequency cap")
	ErrFloorConflict  = errors.New("config: conflicting floor prices")
	ErrDuplicateID    = errors.New("config: duplicate campaign id")
	ErrMissingID      = errors.New("config: missing campaign id")
	ErrInvalidWindow  = errors.New("config: invalid pacing/freq window")
)

// CampaignConfig 描述单个投放计划。
type CampaignConfig struct {
	ID           string
	BudgetCents  int64         // 计划总预算（分），必须 >= 0
	FloorCents   int64         // 该计划可接受的最低计费价（分），必须 >= 0
	FreqCap      int           // 同一用户在滚动窗口内的最大曝光次数，0 表示不限
	FreqWindow   time.Duration // 频控滚动窗口长度；FreqCap>0 时必须 > 0
	PacingWindow time.Duration // 预算平滑投放的时间窗口，必须 > 0
}

// FloorConfig 全局底价配置。
type FloorConfig struct {
	MinCents int64 // 全局最低底价，必须 >= 0
	MaxCents int64 // 全局最高底价，必须 >= MinCents
}

// Config 是模拟器的完整配置。
type Config struct {
	Campaigns []CampaignConfig
	Floor     FloorConfig
	// StatePath 为频控等持久化状态文件路径，空字符串表示仅内存。
	StatePath string
}

// Validate 在启动阶段校验配置，失败时返回可区分原因的错误。
// 该校验为纯函数，不修改任何状态，因此失败不会留下部分生效的配置。
func Validate(c *Config) error {
	if c == nil {
		return fmt.Errorf("%w: nil config", ErrMissingID)
	}
	if c.Floor.MinCents < 0 || c.Floor.MaxCents < 0 || c.Floor.MaxCents < c.Floor.MinCents {
		return fmt.Errorf("%w: global floor [%d,%d] invalid",
			ErrFloorConflict, c.Floor.MinCents, c.Floor.MaxCents)
	}
	seen := make(map[string]struct{}, len(c.Campaigns))
	for _, camp := range c.Campaigns {
		if camp.ID == "" {
			return fmt.Errorf("%w: empty campaign id", ErrMissingID)
		}
		if _, dup := seen[camp.ID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateID, camp.ID)
		}
		seen[camp.ID] = struct{}{}
		if camp.BudgetCents < 0 {
			return fmt.Errorf("%w: campaign %q budget=%d", ErrNegativeBudget, camp.ID, camp.BudgetCents)
		}
		if camp.FreqCap < 0 {
			return fmt.Errorf("%w: campaign %q freq cap=%d", ErrInvalidFreqCap, camp.ID, camp.FreqCap)
		}
		if camp.FreqCap > 0 && camp.FreqWindow <= 0 {
			return fmt.Errorf("%w: campaign %q window=%s", ErrInvalidWindow, camp.ID, camp.FreqWindow)
		}
		if camp.PacingWindow <= 0 {
			return fmt.Errorf("%w: campaign %q pacing window=%s", ErrInvalidWindow, camp.ID, camp.PacingWindow)
		}
		// 计划底价与全局底价区间互相矛盾：计划底价高于全局最高底价，
		// 意味着该计划任何计费价都会被全局上限拒绝。
		if camp.FloorCents < 0 {
			return fmt.Errorf("%w: campaign %q floor=%d", ErrFloorConflict, camp.ID, camp.FloorCents)
		}
		if c.Floor.MaxCents > 0 && camp.FloorCents > c.Floor.MaxCents {
			return fmt.Errorf("%w: campaign %q floor %d exceeds global max %d",
				ErrFloorConflict, camp.ID, camp.FloorCents, c.Floor.MaxCents)
		}
	}
	return nil
}
