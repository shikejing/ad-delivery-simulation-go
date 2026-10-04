// Package auction 实现纯函数式的竞价排序与计费（GSP 二价 + 底价）。
//
// 排序键：有效价值 effective = bid * quality（质量为百万分比，整数运算）；
// 有效价值相同时按 TieBreakKey 升序，再按原始下标升序，因此在相同种子与
// 相同输入下排序结果逐条一致、与并发调度无关。
//
// 计费（GSP）：胜出者按"第二名有效价值折算回胜出者质量的出价"成交，
// 再与底价取大：
//
//	charge = max(floor, ceil(runnerUpEffective * 1_000_000 / winnerQuality))
//
// 无第二名时仅以底价成交。成交价高于胜出者自身出价时，按自身出价封顶
// （不会让广告主为超出自己出价的价格付费；此时该候选等价于不满足底价）。
package auction

// RejectReason 是候选被淘汰的可区分原因码。
type RejectReason string

const (
	// RejectNone 占位。
	RejectNone RejectReason = ""
	// RejectBelowMinBid 出价低于允许的最小出价（无效出价）。
	RejectBelowMinBid RejectReason = "below_min_bid"
	// RejectBelowFloor 按二价/底价折算后成交不可达（成交价达不到底价）。
	RejectBelowFloor RejectReason = "below_floor"
	// RejectNoBudget 计划预算不足。
	RejectNoBudget RejectReason = "campaign_no_budget"
	// RejectPaced 计划被节奏控制限流。
	RejectPaced RejectReason = "campaign_paced"
	// RejectPaused 计划已暂停。
	RejectPaused RejectReason = "campaign_paused"
	// RejectFreqCapped 用户在当前窗口内曝光达到频控上限。
	RejectFreqCapped RejectReason = "frequency_capped"
	// RejectUnknownCampaign 候选引用了配置中不存在的计划。
	RejectUnknownCampaign RejectReason = "unknown_campaign"
)

// NoWinner* 为无胜出者时的可区分原因。
const (
	NoWinnerCandidates  = "no_candidates"   // 请求没有携带任何候选
	NoWinnerInvalidBid  = "all_invalid_bid" // 所有候选出价均非法
	NoWinnerBelowFloor  = "all_below_floor" // 候选均无法达到底价
	NoWinnerIneligible  = "all_ineligible"  // 候选均未通过投放资格
	NoWinnerAboveOwnBid = "above_own_bid"   // 二价高于最高出价，等价于无人达底价
)

// Candidate 为参与本次竞价的一个广告候选。
type Candidate struct {
	CampaignID        string
	BidMicros         int64
	QualityPermillion int64
	// TieBreakKey 为有效价值并列时的确定性打散键（由调用方基于种子生成）。
	TieBreakKey uint64
}

// RejectedCandidate 记录候选及其可区分淘汰原因。
type RejectedCandidate struct {
	Candidate     Candidate
	Reason        RejectReason
	EffectiveRank int // 在有效竞价序列中的位次（-1 表示未进入序列）
}

// Winner 为胜出者及其计费价。
type Winner struct {
	Candidate    Candidate
	ChargeMicros int64
}

// Decision 为一次竞价请求的决策结果。
type Decision struct {
	RequestID string
	Winner    *Winner
	Rejected  []RejectedCandidate
	// NoWinnerReason 仅在 Winner 为 nil 时有意义。
	NoWinnerReason string
}

// Runner 执行竞价排序与计费。本身无状态，可被多个请求并发使用。
type Runner struct {
	minBidMicros int64
	floorMicros  int64
}

// NewRunner 创建 Runner。
func NewRunner(minBidMicros, floorMicros int64) *Runner {
	return &Runner{minBidMicros: minBidMicros, floorMicros: floorMicros}
}

type ranked struct {
	idx       int
	candidate Candidate
	effective int64
}

// Run 对候选执行竞价。
//
// 输入的 eligible / eligibilityReason 与 candidates 一一对应：
// eligible=false 表示该候选因预算/节奏/暂停/频控不具备投放资格，
// 原因记录在 eligibilityReason 中，这类候选不进入排序序列。
func (r *Runner) Run(requestID string, candidates []Candidate, eligible []bool, eligibilityReason []RejectReason) Decision {
	d := Decision{RequestID: requestID}
	if len(candidates) == 0 {
		d.NoWinnerReason = NoWinnerCandidates
		return d
	}

	valid := make([]ranked, 0, len(candidates))
	invalidBid := 0
	ineligible := 0
	for i, cand := range candidates {
		if !eligible[i] {
			ineligible++
			reason := eligibilityReason[i]
			if reason == RejectNone {
				reason = RejectPaused
			}
			d.Rejected = append(d.Rejected, RejectedCandidate{
				Candidate: cand, Reason: reason, EffectiveRank: -1,
			})
			continue
		}
		if cand.BidMicros < 0 || cand.BidMicros < r.minBidMicros || cand.QualityPermillion < 0 {
			invalidBid++
			d.Rejected = append(d.Rejected, RejectedCandidate{
				Candidate: cand, Reason: RejectBelowMinBid, EffectiveRank: -1,
			})
			continue
		}
		quality := cand.QualityPermillion
		if quality == 0 {
			quality = 1_000_000 // 未提供质量因子时视为 1.0
		}
		eff := cand.BidMicros * quality / 1_000_000
		valid = append(valid, ranked{idx: i, candidate: cand, effective: eff})
	}

	if len(valid) == 0 {
		switch {
		case invalidBid > 0:
			d.NoWinnerReason = NoWinnerInvalidBid
		case ineligible > 0:
			d.NoWinnerReason = NoWinnerIneligible
		default:
			d.NoWinnerReason = NoWinnerIneligible
		}
		return d
	}

	// 确定性降序排序：有效价值降序 -> TieBreakKey 升序 -> 原始下标升序。
	sortRanked(valid)

	winnerRank := valid[0]
	// GSP：用第二名有效价值折算胜出者质量对应的出价。
	runnerUp := r.floorMicros
	if len(valid) >= 2 {
		winnerQuality := winnerRank.candidate.QualityPermillion
		if winnerQuality == 0 {
			winnerQuality = 1_000_000
		}
		gsp := (valid[1].effective*1_000_000 + winnerQuality - 1) / winnerQuality
		if gsp > runnerUp {
			runnerUp = gsp
		}
	}
	if runnerUp > winnerRank.candidate.BidMicros {
		// 折算成交价超过胜出者自身出价：无人满足底价。
		for _, rk := range valid {
			reason := RejectBelowFloor
			if rk.idx == winnerRank.idx {
				d.Rejected = append(d.Rejected, RejectedCandidate{
					Candidate: rk.candidate, Reason: reason, EffectiveRank: 0,
				})
				continue
			}
			d.Rejected = append(d.Rejected, RejectedCandidate{
				Candidate: rk.candidate, Reason: reason, EffectiveRank: -1,
			})
		}
		d.NoWinnerReason = NoWinnerAboveOwnBid
		return d
	}

	for i := 1; i < len(valid); i++ {
		rk := valid[i]
		d.Rejected = append(d.Rejected, RejectedCandidate{
			Candidate: rk.candidate, Reason: RejectBelowFloor, EffectiveRank: i,
		})
	}

	d.Winner = &Winner{Candidate: winnerRank.candidate, ChargeMicros: runnerUp}
	return d
}

func sortRanked(v []ranked) {
	// 简单插入排序：候选规模小且实现零分配、行为确定。
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && lessRanked(v[j], v[j-1]); j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// lessRanked 返回 a 是否应排在 b 前面。
func lessRanked(a, b ranked) bool {
	if a.effective != b.effective {
		return a.effective > b.effective
	}
	if a.candidate.TieBreakKey != b.candidate.TieBreakKey {
		return a.candidate.TieBreakKey < b.candidate.TieBreakKey
	}
	return a.idx < b.idx
}
