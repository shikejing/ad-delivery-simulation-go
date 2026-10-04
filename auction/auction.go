// Package auction 实现竞价请求的接收、胜出者选择与计费价计算。
//
// 决策规则：
//   - 候选依次经过：出价合法性 -> 底价 -> 预算/暂停 -> 频控 的过滤，
//     每个被淘汰的候选都带有可区分的淘汰原因。
//   - 合格候选按 出价*质量分 的分数排序，最高分胜出（同分按 CampaignID
//     字典序，保证确定性）。
//   - 计费为广义二价：胜出者支付 max(底价, ceil(次高分/胜者质量分))，
//     且不超过自身出价与全局价格上限。
package auction

import (
	"sort"

	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/freqcap"
	"github.com/shikejing/ad-delivery-simulation-go/pacing"
)

// RejectReason 区分候选被淘汰的原因。
type RejectReason string

const (
	RejectInvalidBid  RejectReason = "invalid_bid"  // 出价或质量分非法
	RejectBelowFloor  RejectReason = "below_floor"  // 出价低于底价
	RejectNoBudget    RejectReason = "no_budget"    // 计划预算不足、节奏受限或被暂停
	RejectFreqCapped  RejectReason = "freq_capped"  // 触发频次控制
	RejectLostAuction RejectReason = "lost_auction" // 有效但未胜出
	RejectNoEligible  RejectReason = "no_eligible"  // 本次请求无合格候选
)

// Candidate 是一次竞价请求携带的单个广告候选。
type Candidate struct {
	CampaignID string
	BidCents   int64   // 出价（分）
	Quality    float64 // 质量分，(0,1]
}

// BidRequest 是一次竞价请求。
type BidRequest struct {
	RequestID  string
	UserID     string
	Candidates []Candidate
}

// Decision 是单个候选的决策结果。
type Decision struct {
	CampaignID string
	Won        bool
	Reason     RejectReason // 未胜出时的淘汰原因
}

// Result 是一次竞价的完整结果。
type Result struct {
	RequestID  string
	Winner     *Decision    // 无合格候选时为 nil
	Reason     RejectReason // 无胜出者时为 RejectNoEligible
	PriceCents int64        // 胜出者的计费价（二价计费）
	Decisions  []Decision   // 与请求中候选一一对应，顺序一致
}

// Auctioneer 执行竞价决策。依赖的预算账本与频控计数器均为并发安全。
type Auctioneer struct {
	floor  config.FloorConfig
	camps  map[string]config.CampaignConfig
	pacing *pacing.Ledger
	freq   *freqcap.Counter
}

// New 创建 Auctioneer。
func New(floor config.FloorConfig, camps []config.CampaignConfig, pl *pacing.Ledger, fc *freqcap.Counter) *Auctioneer {
	m := make(map[string]config.CampaignConfig, len(camps))
	for _, c := range camps {
		m[c.ID] = c
	}
	return &Auctioneer{floor: floor, camps: m, pacing: pl, freq: fc}
}

// floorFor 返回候选适用的底价：全局底价与计划底价取较大者。
func (a *Auctioneer) floorFor(campaignID string) int64 {
	f := a.floor.MinCents
	if c, ok := a.camps[campaignID]; ok && c.FloorCents > f {
		f = c.FloorCents
	}
	return f
}

// Run 执行一次竞价。并发调用安全。
func (a *Auctioneer) Run(req BidRequest) Result {
	res := Result{RequestID: req.RequestID, Reason: RejectNoEligible}
	type scored struct {
		idx   int
		cand  Candidate
		score float64
	}
	var elig []scored
	for i, c := range req.Candidates {
		ok, reason := a.eligibleFor(req.UserID, c)
		if !ok {
			res.Decisions = append(res.Decisions, Decision{CampaignID: c.CampaignID, Reason: reason})
			continue
		}
		elig = append(elig, scored{idx: i, cand: c, score: float64(c.BidCents) * c.Quality})
		res.Decisions = append(res.Decisions, Decision{CampaignID: c.CampaignID})
	}
	if len(elig) == 0 {
		return res
	}
	// 确定性排序：分数降序，同分按 CampaignID 升序。
	sort.Slice(elig, func(i, j int) bool {
		if elig[i].score != elig[j].score {
			return elig[i].score > elig[j].score
		}
		return elig[i].cand.CampaignID < elig[j].cand.CampaignID
	})
	win := elig[0]
	floor := a.floorFor(win.cand.CampaignID)
	price := floor
	if len(elig) > 1 {
		// 二价：使胜者分数恰好打平次高分的最低价格。
		second := elig[1].score
		p := int64(second/win.cand.Quality) + 1
		if p > price {
			price = p
		}
	}
	if price > win.cand.BidCents {
		price = win.cand.BidCents
	}
	if a.floor.MaxCents > 0 && price > a.floor.MaxCents {
		price = a.floor.MaxCents
	}
	res.Winner = &Decision{CampaignID: win.cand.CampaignID, Won: true}
	res.PriceCents = price
	res.Reason = ""
	// 标记其余合格候选为竞价失败。
	for _, e := range elig[1:] {
		res.Decisions[e.idx].Reason = RejectLostAuction
	}
	res.Decisions[win.idx] = *res.Winner
	return res
}

// eligibleFor 带用户上下文的候选过滤。
func (a *Auctioneer) eligibleFor(userID string, c Candidate) (bool, RejectReason) {
	if c.BidCents < 0 || c.Quality <= 0 || c.Quality > 1 {
		return false, RejectInvalidBid
	}
	if c.BidCents < a.floorFor(c.CampaignID) {
		return false, RejectBelowFloor
	}
	if a.pacing != nil && !a.pacing.CanBid(c.CampaignID) {
		return false, RejectNoBudget
	}
	if a.freq != nil {
		if camp, ok := a.camps[c.CampaignID]; ok && camp.FreqCap > 0 {
			if a.freq.Count(c.CampaignID, userID, camp.FreqWindow) >= camp.FreqCap {
				return false, RejectFreqCapped
			}
		}
	}
	return true, ""
}
