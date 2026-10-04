// Package sim 把竞价、预算节奏、频控与事件记账组装为可复现的本地模拟引擎。
//
// 可复现性：引擎本身不含随机源；相同的配置、时钟与输入请求序列必然
// 产生逐条一致的结果。计费提交（频控计数 -> 预算扣减 -> 曝光记账）
// 在引擎内串行化，保证并发下预算余额、频控计数与统计结果自洽。
package sim

import (
	"sync"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/auction"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/events"
	"github.com/shikejing/ad-delivery-simulation-go/freqcap"
	"github.com/shikejing/ad-delivery-simulation-go/pacing"
)

// Engine 是模拟引擎入口，并发安全。
type Engine struct {
	auc    *auction.Auctioneer
	pacing *pacing.Ledger
	freq   *freqcap.Counter
	ledger *events.Ledger
	camps  map[string]config.CampaignConfig
	commit sync.Mutex // 串行化计费提交，保证预算/频控/统计一致
}

// New 创建引擎。配置先经 config.Validate 校验，失败时返回错误且
// 不留下任何部分生效的状态。now 为时钟，传 nil 使用真实时间。
func New(cfg *config.Config, start time.Time, now func() time.Time) (*Engine, error) {
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	pl := pacing.NewLedger()
	camps := make(map[string]config.CampaignConfig, len(cfg.Campaigns))
	for _, c := range cfg.Campaigns {
		pl.Add(c.ID, pacing.NewPacer(c.BudgetCents, c.PacingWindow, start, now))
		camps[c.ID] = c
	}
	fc := freqcap.New(cfg.StatePath, now)
	e := &Engine{
		pacing: pl,
		freq:   fc,
		ledger: events.NewLedger(),
		camps:  camps,
	}
	e.auc = auction.New(cfg.Floor, cfg.Campaigns, pl, fc)
	return e, nil
}

// Bid 处理一次竞价请求。若有胜出者，引擎原子地完成频控计数、
// 预算扣减与曝光事件记账；任一步骤失败则本次无胜出且无副作用。
func (e *Engine) Bid(req auction.BidRequest) auction.Result {
	res := e.auc.Run(req)
	if res.Winner == nil {
		return res
	}
	e.commit.Lock()
	defer e.commit.Unlock()
	camp := e.camps[res.Winner.CampaignID]
	// 频控计数（竞态下可能在 Run 之后触顶）。
	if !e.freq.Allow(camp.ID, req.UserID, camp.FreqCap, camp.FreqWindow) {
		res.Winner = nil
		res.Reason = auction.RejectFreqCapped
		res.PriceCents = 0
		return res
	}
	// 预算扣减（保证总花费永不超过预算上限）。
	if err := e.pacing.Charge(camp.ID, res.PriceCents); err != nil {
		res.Winner = nil
		res.Reason = auction.RejectNoBudget
		res.PriceCents = 0
		return res
	}
	// 曝光记账：事件 ID 由请求与计划确定性派生，天然幂等。
	e.ledger.Record(events.Event{
		ID:         req.RequestID + ":" + camp.ID,
		Type:       events.Impression,
		CampaignID: camp.ID,
		UserID:     req.UserID,
		PriceCents: res.PriceCents,
	})
	return res
}

// RecordEvent 记账一条点击/转化（或外部曝光）事件，幂等。
func (e *Engine) RecordEvent(ev events.Event) bool {
	return e.ledger.Record(ev)
}

// Pause 暂停计划，立即生效。
func (e *Engine) Pause(campaignID string) {
	if p := e.pacing.Get(campaignID); p != nil {
		p.Pause()
	}
}

// Resume 恢复计划。
func (e *Engine) Resume(campaignID string) {
	if p := e.pacing.Get(campaignID); p != nil {
		p.Resume()
	}
}

// Stats 返回计划的统计快照。
func (e *Engine) Stats(campaignID string) events.Stats {
	return e.ledger.Stats(campaignID)
}

// Snapshot 返回全部计划的统计快照。
func (e *Engine) Snapshot() map[string]events.Stats {
	return e.ledger.Snapshot()
}

// Remaining 返回计划剩余预算；计划不存在时返回 0。
func (e *Engine) Remaining(campaignID string) int64 {
	if p := e.pacing.Get(campaignID); p != nil {
		return p.Remaining()
	}
	return 0
}
