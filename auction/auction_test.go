package auction

import "testing"

func allEligible(n int) ([]bool, []RejectReason) {
	e := make([]bool, n)
	r := make([]RejectReason, n)
	for i := range e {
		e[i] = true
	}
	return e, r
}

func TestMultiCandidateGSP(t *testing.T) {
	r := NewRunner(50, 100)
	cands := []Candidate{
		{CampaignID: "a", BidMicros: 500, QualityPermillion: 1_000_000, TieBreakKey: 1},
		{CampaignID: "b", BidMicros: 300, QualityPermillion: 1_000_000, TieBreakKey: 2},
		{CampaignID: "c", BidMicros: 200, QualityPermillion: 1_000_000, TieBreakKey: 3},
	}
	elig, reasons := allEligible(3)
	d := r.Run("req1", cands, elig, reasons)
	if d.Winner == nil {
		t.Fatalf("want winner, got reason %s", d.NoWinnerReason)
	}
	if d.Winner.Candidate.CampaignID != "a" {
		t.Fatalf("want a, got %s", d.Winner.Candidate.CampaignID)
	}
	if d.Winner.ChargeMicros != 300 {
		t.Fatalf("GSP charge want 300, got %d", d.Winner.ChargeMicros)
	}
	if len(d.Rejected) != 2 {
		t.Fatalf("want 2 rejected, got %d", len(d.Rejected))
	}
}

func TestQualityCanBeatHigherBid(t *testing.T) {
	r := NewRunner(0, 0)
	cands := []Candidate{
		{CampaignID: "low", BidMicros: 100, QualityPermillion: 2_000_000},  // eff 200
		{CampaignID: "high", BidMicros: 150, QualityPermillion: 1_000_000}, // eff 150
	}
	elig, reasons := allEligible(2)
	d := r.Run("r", cands, elig, reasons)
	if d.Winner.Candidate.CampaignID != "low" {
		t.Fatalf("want low by quality, got %+v", d.Winner)
	}
	// 第二名 eff=150 折算回胜出者质量 2.0 => ceil(150e6/2e6)=75
	if d.Winner.ChargeMicros != 75 {
		t.Fatalf("charge want 75, got %d", d.Winner.ChargeMicros)
	}
}

func TestFloorAppliesWhenSingleBidder(t *testing.T) {
	r := NewRunner(0, 100)
	cands := []Candidate{{CampaignID: "a", BidMicros: 500, QualityPermillion: 1_000_000}}
	elig, reasons := allEligible(1)
	d := r.Run("r", cands, elig, reasons)
	if d.Winner == nil || d.Winner.ChargeMicros != 100 {
		t.Fatalf("want charge=floor 100, got %+v", d.Winner)
	}
}

func TestBelowMinBid(t *testing.T) {
	r := NewRunner(100, 100)
	cands := []Candidate{{CampaignID: "a", BidMicros: 50}}
	elig, reasons := allEligible(1)
	d := r.Run("r", cands, elig, reasons)
	if d.Winner != nil {
		t.Fatal("want no winner")
	}
	if d.NoWinnerReason != NoWinnerInvalidBid {
		t.Fatalf("want all_invalid_bid, got %s", d.NoWinnerReason)
	}
	if d.Rejected[0].Reason != RejectBelowMinBid {
		t.Fatalf("want below_min_bid reason, got %s", d.Rejected[0].Reason)
	}
}

func TestFloorUnreachable(t *testing.T) {
	// 二价折算价高于最高出价自身 -> 无人可达底价。
	r := NewRunner(0, 400)
	cands := []Candidate{
		{CampaignID: "a", BidMicros: 300, QualityPermillion: 1_000_000},
	}
	elig, reasons := allEligible(1)
	d := r.Run("r", cands, elig, reasons)
	if d.Winner != nil {
		t.Fatalf("want no winner, got %+v", d.Winner)
	}
	if d.NoWinnerReason != NoWinnerAboveOwnBid {
		t.Fatalf("want above_own_bid, got %s", d.NoWinnerReason)
	}
}

func TestNoCandidates(t *testing.T) {
	r := NewRunner(0, 0)
	d := r.Run("r", nil, nil, nil)
	if d.Winner != nil || d.NoWinnerReason != NoWinnerCandidates {
		t.Fatalf("want no_candidates, got %+v", d)
	}
}

func TestIneligibleReasonsPreserved(t *testing.T) {
	r := NewRunner(0, 0)
	cands := []Candidate{
		{CampaignID: "a", BidMicros: 300},
		{CampaignID: "b", BidMicros: 200},
	}
	elig := []bool{true, false}
	reasons := []RejectReason{RejectNone, RejectFreqCapped}
	d := r.Run("r", cands, elig, reasons)
	if d.Winner == nil || d.Winner.Candidate.CampaignID != "a" {
		t.Fatalf("want a winner, got %+v", d)
	}
	if d.Rejected[0].Reason != RejectFreqCapped {
		t.Fatalf("want freq reason preserved, got %s", d.Rejected[0].Reason)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	r := NewRunner(0, 0)
	mk := func(key uint64) []Candidate {
		return []Candidate{
			{CampaignID: "x", BidMicros: 100, TieBreakKey: key},
			{CampaignID: "y", BidMicros: 100, TieBreakKey: key},
		}
	}
	elig, reasons := allEligible(2)
	d1 := r.Run("r", mk(7), elig, reasons)
	// 同键回退到原始下标 -> x 胜出。
	if d1.Winner.Candidate.CampaignID != "x" {
		t.Fatalf("want x by index, got %s", d1.Winner.Candidate.CampaignID)
	}
	cands := []Candidate{
		{CampaignID: "x", BidMicros: 100, TieBreakKey: 9},
		{CampaignID: "y", BidMicros: 100, TieBreakKey: 1},
	}
	d2 := r.Run("r", cands, elig, reasons)
	if d2.Winner.Candidate.CampaignID != "y" {
		t.Fatalf("want y by smaller tiebreak, got %s", d2.Winner.Candidate.CampaignID)
	}
}
