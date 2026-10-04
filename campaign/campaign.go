// Package campaign 管理投放计划的预算余额与平滑节奏控制。
//
// 节奏语义（线性平滑 + 突发额度）：
//   - 计划启用节奏后，以创建时刻 start 为锚点，按 Slice 将 Horizon 均分。
//   - 任意时刻 t 的累计花费上限为：
//     cap(t) = budget * ((t-start)/Slice + 1) / numSlices * (1 + burst)
//   - cap 随时间线性增长，预算不会在投放初期被瞬间放完；
//     最后一个切片之后 cap 恒等于 budget。
//   - 突发额度 BurstPermillion 允许在单一切片内高于目标一小段，
//     但永远不会超过预算上限。
//   - 该判断完全由 (时间, 累计花费) 决定，不含随机量，天然可复现。
package campaign

import (
	"sync"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/config"
)

// Status 表示计划状态。
type Status string

const (
	StatusActive Status = "active"
	StatusPaused Status = "paused"

	// Reason 常量给出资格失败的可区分原因。
	ReasonPaused   = "campaign_paused"
	ReasonNoBudget = "campaign_no_budget"
	ReasonPaced    = "campaign_paced"
)

// Campaign 是单个投放计划的运行时状态。
type Campaign struct {
	cfg       config.CampaignConfig
	pacing    config.PacingConfig
	start     time.Time
	numSlices int64

	mu          sync.Mutex
	status      Status
	spentMicros int64
}

// newCampaign 构造单个计划。start 为节奏锚点。
func newCampaign(cfg config.CampaignConfig, start time.Time) *Campaign {
	c := &Campaign{
		cfg:    cfg,
		pacing: cfg.Pacing,
		start:  start,
		status: StatusActive,
	}
	if cfg.Pacing.Horizon > 0 && cfg.Pacing.Slice > 0 {
		n := int64(cfg.Pacing.Horizon / cfg.Pacing.Slice)
		if cfg.Pacing.Horizon%cfg.Pacing.Slice != 0 {
			n++
		}
		c.numSlices = n
	}
	return c
}

// Registry 持有全部投放计划。
type Registry struct {
	mu        sync.RWMutex
	campaigns map[string]*Campaign
	order     []string
}

// NewRegistry 依据配置构建计划注册表，startAt 作为全部计划的节奏锚点。
// priorSpend 给出重启前各计划的累计花费（可为 nil），用于恢复预算余额。
func NewRegistry(cfgs []config.CampaignConfig, startAt time.Time, priorSpend map[string]int64) *Registry {
	r := &Registry{campaigns: make(map[string]*Campaign, len(cfgs))}
	for i := range cfgs {
		c := newCampaign(cfgs[i], startAt)
		if s, ok := priorSpend[c.cfg.ID]; ok && s > 0 {
			if s > c.cfg.BudgetMicros {
				s = c.cfg.BudgetMicros // 防御：流水不可能超预算，截断以保证不变量
			}
			c.spentMicros = s
		}
		r.campaigns[c.cfg.ID] = c
		r.order = append(r.order, c.cfg.ID)
	}
	return r
}

// Get 返回指定计划。
func (r *Registry) Get(id string) *Campaign {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.campaigns[id]
}

// IDs 返回全部计划 ID（顺序与配置一致）。
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// ID 返回计划 ID。
func (c *Campaign) ID() string { return c.cfg.ID }

// BudgetMicros 返回预算上限。
func (c *Campaign) BudgetMicros() int64 { return c.cfg.BudgetMicros }

// SpentMicros 返回累计花费。
func (c *Campaign) SpentMicros() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spentMicros
}

// RemainingMicros 返回预算余额。
func (c *Campaign) RemainingMicros() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.BudgetMicros - c.spentMicros
}

// Status 返回计划状态。
func (c *Campaign) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Pause 立即暂停计划；暂停后不再参与任何竞价。
func (c *Campaign) Pause() {
	c.mu.Lock()
	c.status = StatusPaused
	c.mu.Unlock()
}

// Resume 恢复计划（不会补充预算，也不会重置节奏锚点）。
func (c *Campaign) Resume() {
	c.mu.Lock()
	c.status = StatusActive
	c.mu.Unlock()
}

// cumulativeCap 返回 now 时刻允许的累计花费上限（微元）。
// 调用方需持有 c.mu。
func (c *Campaign) cumulativeCap(now time.Time) int64 {
	if c.numSlices <= 0 {
		return c.cfg.BudgetMicros // 未启用平滑：仅受预算约束
	}
	elapsed := now.Sub(c.start)
	if elapsed < 0 {
		elapsed = 0
	}
	idx := int64(elapsed / c.pacing.Slice) // 从 0 开始的当前切片序号
	if idx >= c.numSlices {
		return c.cfg.BudgetMicros
	}
	served := idx + 1
	// base = budget * served / numSlices
	base := c.cfg.BudgetMicros * served / c.numSlices
	// allowance = base * (1 + burst)，不超过 budget
	allow := base + base*c.pacing.BurstPermillion/1_000_000
	if allow > c.cfg.BudgetMicros {
		allow = c.cfg.BudgetMicros
	}
	return allow
}

// Eligible 判断计划在 now 时刻是否具备参与竞价的资格（仅判定，不扣费）。
// reason 在返回 false 时给出可区分原因。
func (c *Campaign) Eligible(now time.Time) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == StatusPaused {
		return false, ReasonPaused
	}
	if c.spentMicros >= c.cfg.BudgetMicros {
		return false, ReasonNoBudget
	}
	return true, ""
}

// CanAfford 判断当前预算余额是否足以承担 priceMicros。
func (c *Campaign) CanAfford(priceMicros int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return priceMicros <= c.cfg.BudgetMicros-c.spentMicros
}

// AllowPace 判断在 now 时刻再花费 priceMicros 是否符合平滑节奏。
func (c *Campaign) AllowPace(now time.Time, priceMicros int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.numSlices <= 0 {
		return true
	}
	return c.spentMicros+priceMicros <= c.cumulativeCap(now)
}

// Charge 原子扣费；预算不足或突破节奏上限时返回 false 且状态保持不变。
func (c *Campaign) Charge(priceMicros int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == StatusPaused {
		return false
	}
	if priceMicros < 0 {
		return false
	}
	if c.spentMicros+priceMicros > c.cfg.BudgetMicros {
		return false
	}
	if c.numSlices > 0 && c.spentMicros+priceMicros > c.cumulativeCap(now) {
		return false
	}
	c.spentMicros += priceMicros
	return true
}

// Refund 回退一笔此前成功的扣费，仅用于提交阶段的补偿。
// 调用方必须保证该笔扣费确实发生，且回退后 spent 不会为负。
func (c *Campaign) Refund(priceMicros int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spentMicros -= priceMicros
}
