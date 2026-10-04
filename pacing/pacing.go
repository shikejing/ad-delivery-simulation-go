// Package pacing 实现计划预算与投放节奏控制。
//
// 节奏语义：每个计划有 PacingWindow 长度的投放周期，预算在周期内按
// 时间线性释放——时刻 t 允许累计花费的上限为 budget * elapsed/window，
// 从而避免预算在周期开始时被瞬间打满。预算耗尽或计划被暂停后，
// CanBid 立即返回 false，计划停止参与竞价。
package pacing

import (
	"errors"
	"sync"
	"time"
)

// ErrBudgetExceeded 表示计费金额超出剩余预算。
var ErrBudgetExceeded = errors.New("pacing: charge exceeds remaining budget")

// Pacer 控制单个计划的预算消耗与节奏。零值不可用，请用 NewPacer。
type Pacer struct {
	mu     sync.Mutex
	budget int64
	spent  int64
	paused bool
	window time.Duration
	start  time.Time
	now    func() time.Time
}

// NewPacer 创建 Pacer。now 为时钟，便于测试注入确定性时间。
func NewPacer(budgetCents int64, window time.Duration, start time.Time, now func() time.Time) *Pacer {
	if now == nil {
		now = time.Now
	}
	return &Pacer{budget: budgetCents, window: window, start: start, now: now}
}

// allowedSpend 返回当前时刻按线性节奏允许累计花费的上限。
func (p *Pacer) allowedSpend() int64 {
	elapsed := p.now().Sub(p.start)
	if elapsed <= 0 {
		return 0
	}
	if elapsed >= p.window {
		return p.budget
	}
	// 用整数运算避免浮点误差：budget * elapsed / window
	return p.budget * int64(elapsed) / int64(p.window)
}

// CanBid 报告计划当前是否可参与竞价。
func (p *Pacer) CanBid() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.canBidLocked()
}

func (p *Pacer) canBidLocked() bool {
	if p.paused {
		return false
	}
	if p.spent >= p.budget {
		return false
	}
	return p.spent < p.allowedSpend()
}

// Charge 记账一笔花费。金额不得超过剩余预算，否则整体拒绝并返回
// ErrBudgetExceeded，保证花费永不超过预算上限。
func (p *Pacer) Charge(priceCents int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if priceCents < 0 {
		return ErrBudgetExceeded
	}
	if p.spent+priceCents > p.budget {
		return ErrBudgetExceeded
	}
	p.spent += priceCents
	return nil
}

// Pause 暂停计划，立即生效。
func (p *Pacer) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
}

// Resume 恢复计划（预算与节奏限制仍然生效）。
func (p *Pacer) Resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
}

// Spent 返回已花费金额。
func (p *Pacer) Spent() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spent
}

// Remaining 返回剩余预算。
func (p *Pacer) Remaining() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.budget - p.spent
}

// Paused 报告计划是否被暂停。
func (p *Pacer) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Ledger 管理所有计划的 Pacer，并发安全。
type Ledger struct {
	mu     sync.RWMutex
	pacers map[string]*Pacer
}

// NewLedger 创建空的预算账本。
func NewLedger() *Ledger {
	return &Ledger{pacers: make(map[string]*Pacer)}
}

// Add 注册一个计划。重复注册同一 ID 会覆盖旧 Pacer。
func (l *Ledger) Add(campaignID string, p *Pacer) {
	l.mu.Lock()
	l.pacers[campaignID] = p
	l.mu.Unlock()
}

// Get 返回计划的 Pacer，不存在时返回 nil。
func (l *Ledger) Get(campaignID string) *Pacer {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pacers[campaignID]
}

// CanBid 报告计划是否可参与竞价；未注册的计划视为不可参与。
func (l *Ledger) CanBid(campaignID string) bool {
	p := l.Get(campaignID)
	return p != nil && p.CanBid()
}

// Charge 对计划记账一笔花费；计划不存在时返回 ErrBudgetExceeded。
func (l *Ledger) Charge(campaignID string, priceCents int64) error {
	p := l.Get(campaignID)
	if p == nil {
		return ErrBudgetExceeded
	}
	return p.Charge(priceCents)
}
