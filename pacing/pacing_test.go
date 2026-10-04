package pacing

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock 可手动推进的时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestBudgetNeverExceeded(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	p := NewPacer(100, time.Hour, clk.Now(), clk.Now)
	clk.Advance(time.Hour) // 放开节奏限制
	if err := p.Charge(60); err != nil {
		t.Fatal(err)
	}
	if err := p.Charge(50); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
	if p.Spent() != 60 {
		t.Fatalf("rejected charge must not apply, spent=%d", p.Spent())
	}
	if err := p.Charge(40); err != nil {
		t.Fatal(err)
	}
	if p.Remaining() != 0 || p.CanBid() {
		t.Fatal("budget exhausted: CanBid must be false")
	}
}

func TestPacingSmoothsSpend(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	p := NewPacer(1000, 10*time.Second, clk.Now(), clk.Now)
	if p.CanBid() {
		t.Fatal("no budget should be released at t=0")
	}
	clk.Advance(5 * time.Second) // 释放 500
	if !p.CanBid() {
		t.Fatal("half window elapsed, should be able to bid")
	}
	if err := p.Charge(500); err != nil {
		t.Fatal(err)
	}
	if p.CanBid() {
		t.Fatal("spent up to paced allowance, must stop")
	}
	clk.Advance(5 * time.Second) // 窗口结束，释放全部
	if !p.CanBid() {
		t.Fatal("window over, remaining budget should be biddable")
	}
}

func TestPauseStopsImmediately(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	p := NewPacer(100, time.Second, clk.Now(), clk.Now)
	clk.Advance(time.Second)
	p.Pause()
	if p.CanBid() {
		t.Fatal("paused campaign must not bid")
	}
	p.Resume()
	if !p.CanBid() {
		t.Fatal("resumed campaign should bid again")
	}
}

func TestConcurrentChargeNoOverspend(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	const budget = 1000
	p := NewPacer(budget, time.Second, clk.Now(), clk.Now)
	clk.Advance(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Charge(10) // 100 * 10 = 1000，恰好等于预算
		}()
	}
	wg.Wait()
	if p.Spent() > budget {
		t.Fatalf("overspend: spent=%d budget=%d", p.Spent(), budget)
	}
	if p.Spent() != budget {
		t.Fatalf("expected exact budget consumed, spent=%d", p.Spent())
	}
}

func TestLedgerMissingCampaign(t *testing.T) {
	l := NewLedger()
	if l.CanBid("nope") {
		t.Fatal("unknown campaign must not bid")
	}
	if err := l.Charge("nope", 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("want ErrBudgetExceeded, got %v", err)
	}
}
