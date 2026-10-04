// Package engine 把竞价、预算节奏、频控与幂等记账装配为一条本地模拟链路。
//
// 并发与可复现：引擎以单一互斥串行化"一次竞价决策的全部判定与提交"
// （资格过滤 -> 排序计费 -> 预算扣费 -> 频控落盘 -> 曝光记账），
// 因此任一时刻预算余额、频控计数、统计结果三者自洽，不存在击穿与数据竞争；
// 排序并列时用"种子 + 请求 ID + 候选位置"派生确定性打散键，
// 相同随机种子与相同输入集合下输出逐条一致，与 goroutine 调度无关。
package engine

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shikejing/ad-delivery-simulation-go/auction"
	"github.com/shikejing/ad-delivery-simulation-go/campaign"
	"github.com/shikejing/ad-delivery-simulation-go/clock"
	"github.com/shikejing/ad-delivery-simulation-go/config"
	"github.com/shikejing/ad-delivery-simulation-go/frequency"
	"github.com/shikejing/ad-delivery-simulation-go/ledger"
)

// CandidateInput 为一次竞价请求中的候选输入。
type CandidateInput struct {
	CampaignID        string
	BidMicros         int64
	QualityPermillion int64
}

// BidRequest 为一次竞价请求，可携带多个候选。
type BidRequest struct {
	ID         string
	User       string
	At         time.Time // 零值表示使用引擎时钟的当前时间
	Candidates []CandidateInput
}

// 运行期错误原因码。
const (
	ErrEmptyRequestID  = "empty_request_id"
	ErrEmptyUser       = "empty_user"
	ErrUnknownCampaign = "unknown_campaign"
)

// Engine 为模拟链路门面。所有方法可被并发调用，内部保证自洽。
type Engine struct {
	cfg    config.Config
	clk    clock.Clock
	runner *auction.Runner
	reg    *campaign.Registry
	fcap   *frequency.Cap
	led    *ledger.Ledger

	mu      sync.Mutex
	decided map[string]auction.Decision
	closed  bool
}

// New 在启动阶段校验配置并装配引擎；校验失败时返回 *config.ValidationError。
// 任何失败都不会留下部分生效的配置或中间状态文件。
func New(cfg config.Config, clk clock.Clock) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if clk == nil {
		clk = clock.System{}
	}
	// 先建好目录，再顺序初始化各组件；任一组件失败则回滚已创建的内容。
	createdDir := false
	if _, err := os.Stat(cfg.StateDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
			return nil, fmt.Errorf("engine: create state dir: %w", err)
		}
		createdDir = true
	}
	freqPath := filepath.Join(cfg.StateDir, "frequency.json")
	journalPath := filepath.Join(cfg.StateDir, "events.log")

	fcap, err := frequency.NewCap(cfg.Frequency.Window, cfg.Frequency.MaxImpressions, freqPath)
	if err != nil {
		if createdDir {
			_ = os.RemoveAll(cfg.StateDir)
		}
		return nil, err
	}
	led, err := ledger.New(journalPath)
	if err != nil {
		if createdDir {
			_ = os.RemoveAll(cfg.StateDir)
		}
		return nil, err
	}
	// 重启后从曝光流水恢复各计划已花费预算，保证预算耗尽状态跨进程生效。
	reg := campaign.NewRegistry(cfg.Campaigns, clk.Now(), led.SpendByCampaign())
	return &Engine{
		cfg:     cfg,
		clk:     clk,
		runner:  auction.NewRunner(cfg.Floor.MinBidMicros, cfg.Floor.FloorMicros),
		reg:     reg,
		fcap:    fcap,
		led:     led,
		decided: make(map[string]auction.Decision),
	}, nil
}

func runtimeError(code, format string, args ...any) error {
	return &RuntimeError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// RuntimeError 为非校验类的可区分运行期错误。
type RuntimeError struct {
	Code string
	Msg  string
}

func (e *RuntimeError) Error() string { return fmt.Sprintf("%s (code=%s)", e.Msg, e.Code) }

// tieBreakKey 基于种子与请求内容派生确定性打散键。
func (e *Engine) tieBreakKey(reqID string, pos int, campaignID string) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	seed := e.cfg.Seed
	for i := 0; i < 8; i++ {
		buf[i] = byte(seed >> (uint(i) * 8))
	}
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(reqID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(campaignID))
	_, _ = h.Write([]byte{0})
	for i := 0; i < 8; i++ {
		buf[i] = byte(pos >> (uint(i) * 8))
	}
	_, _ = h.Write(buf[:])
	return h.Sum64()
}

// Decide 处理一次竞价请求。
func (e *Engine) Decide(req BidRequest) (auction.Decision, error) {
	if req.ID == "" {
		return auction.Decision{}, runtimeError(ErrEmptyRequestID, "bid request id must not be empty")
	}
	if req.User == "" {
		return auction.Decision{}, runtimeError(ErrEmptyUser, "bid request user must not be empty")
	}
	now := req.At
	if now.IsZero() {
		now = e.clk.Now()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return auction.Decision{}, errors.New("engine: closed")
	}
	if d, ok := e.decided[req.ID]; ok {
		// 同一请求 ID 重放：返回首次决策，绝不二次扣费/记账。
		return d, nil
	}

	cands := make([]auction.Candidate, len(req.Candidates))
	for i, in := range req.Candidates {
		cands[i] = auction.Candidate{
			CampaignID:        in.CampaignID,
			BidMicros:         in.BidMicros,
			QualityPermillion: in.QualityPermillion,
			TieBreakKey:       e.tieBreakKey(req.ID, i, in.CampaignID),
		}
	}

	// blocked 记录在提交循环中被预算/节奏/频控刷下的候选。
	blocked := make(map[int]auction.RejectReason)
	var final auction.Decision
	for {
		eligible := make([]bool, len(cands))
		reasons := make([]auction.RejectReason, len(cands))
		for i, cand := range cands {
			if reason, ok := blocked[i]; ok {
				eligible[i] = false
				reasons[i] = reason
				continue
			}
			reason := e.checkEligibility(cand.CampaignID, req.User, now)
			eligible[i] = reason == auction.RejectNone
			reasons[i] = reason
		}

		d := e.runner.Run(req.ID, cands, eligible, reasons)
		if d.Winner == nil {
			final = d
			break
		}
		win := d.Winner
		camp := e.reg.Get(win.Candidate.CampaignID)
		// 原子提交（逐步补偿）：
		//  1) 占用频控名额（资格判定已通过，本锁内必成功）
		//  2) 预算+节奏扣费；失败则释放频控名额并把候选标记后重排
		//  3) 曝光幂等记账；落盘错误则回滚扣费与频控并向上返回
		freqOK, err := e.fcap.Record(req.User, now)
		if err != nil {
			return auction.Decision{}, err
		}
		if !freqOK {
			for i := range cands {
				blocked[i] = auction.RejectFreqCapped
			}
			continue
		}
		if !camp.Charge(win.ChargeMicros, now) {
			if _, rerr := e.fcap.Release(req.User); rerr != nil {
				return auction.Decision{}, rerr
			}
			if !camp.CanAfford(win.ChargeMicros) {
				blocked[indexOf(cands, win.Candidate.CampaignID)] = auction.RejectNoBudget
			} else {
				blocked[indexOf(cands, win.Candidate.CampaignID)] = auction.RejectPaced
			}
			continue
		}
		imp := ledger.Event{
			ID:          "imp:" + req.ID,
			Type:        ledger.EventImpression,
			RequestID:   req.ID,
			CampaignID:  win.Candidate.CampaignID,
			User:        req.User,
			At:          now,
			ValueMicros: win.ChargeMicros,
		}
		if _, lerr := e.led.Record(imp); lerr != nil {
			camp.Refund(win.ChargeMicros)
			_, _ = e.fcap.Release(req.User)
			return auction.Decision{}, lerr
		}
		final = d
		break
	}

	e.decided[req.ID] = final
	return final, nil
}

// checkEligibility 在调用方持有 e.mu 的前提下判定候选投放资格。
func (e *Engine) checkEligibility(campaignID, user string, now time.Time) auction.RejectReason {
	c := e.reg.Get(campaignID)
	if c == nil {
		return auction.RejectUnknownCampaign
	}
	if ok, reason := c.Eligible(now); !ok {
		switch reason {
		case campaign.ReasonPaused:
			return auction.RejectPaused
		case campaign.ReasonNoBudget:
			return auction.RejectNoBudget
		case campaign.ReasonPaced:
			return auction.RejectPaced
		default:
			return auction.RejectPaused
		}
	}
	if !e.fcap.Allowed(user, now) {
		return auction.RejectFreqCapped
	}
	return auction.RejectNone
}

func indexOf(cands []auction.Candidate, campaignID string) int {
	for i := range cands {
		if cands[i].CampaignID == campaignID {
			return i
		}
	}
	return -1
}

// PostEvent 处理外部回传的点击/转化等事件（幂等）。
func (e *Engine) PostEvent(ev ledger.Event) (ledger.RecordResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ledger.RecordResult{}, errors.New("engine: closed")
	}
	if e.reg.Get(ev.CampaignID) == nil {
		return ledger.RecordResult{}, runtimeError(ErrUnknownCampaign, "unknown campaign %q", ev.CampaignID)
	}
	return e.led.Record(ev)
}

// PauseCampaign 立即暂停计划；暂停后的下一次竞价起该计划不再参与。
func (e *Engine) PauseCampaign(id string) error {
	c := e.reg.Get(id)
	if c == nil {
		return runtimeError(ErrUnknownCampaign, "unknown campaign %q", id)
	}
	c.Pause()
	return nil
}

// ResumeCampaign 恢复计划。
func (e *Engine) ResumeCampaign(id string) error {
	c := e.reg.Get(id)
	if c == nil {
		return runtimeError(ErrUnknownCampaign, "unknown campaign %q", id)
	}
	c.Resume()
	return nil
}

// CampaignSnapshot 为计划观测快照。
type CampaignSnapshot struct {
	ID              string
	Status          campaign.Status
	BudgetMicros    int64
	SpentMicros     int64
	RemainingMicros int64
}

// Snapshot 返回全部计划余额/状态与统计的一致性快照。
func (e *Engine) Snapshot() ([]CampaignSnapshot, ledger.Stats) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := e.reg.IDs()
	snaps := make([]CampaignSnapshot, 0, len(ids))
	for _, id := range ids {
		c := e.reg.Get(id)
		snaps = append(snaps, CampaignSnapshot{
			ID:              c.ID(),
			Status:          c.Status(),
			BudgetMicros:    c.BudgetMicros(),
			SpentMicros:     c.SpentMicros(),
			RemainingMicros: c.RemainingMicros(),
		})
	}
	return snaps, e.led.Stats()
}

// Close 刷盘并释放资源。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return e.led.Close()
}
