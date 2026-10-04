package freqcap

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

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

func TestRollingWindow(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	c := New("", clk.Now)
	const cap, win = 2, time.Minute
	if !c.Allow("camp", "u1", cap, win) {
		t.Fatal("first impression should be allowed")
	}
	clk.Advance(30 * time.Second) // 两次曝光错开 30 秒
	if !c.Allow("camp", "u1", cap, win) {
		t.Fatal("second impression should be allowed")
	}
	if c.Allow("camp", "u1", cap, win) {
		t.Fatal("third impression within window must be capped")
	}
	// 不同用户互不影响。
	if !c.Allow("camp", "u2", cap, win) {
		t.Fatal("other user should not be capped")
	}
	// 滚动窗口：再推进 31 秒，最早的一次（t+0）滑出窗口，
	// 但第二次（t+30s）仍在窗口内。
	clk.Advance(31 * time.Second)
	if !c.Allow("camp", "u1", cap, win) {
		t.Fatal("window rolled, should allow again")
	}
	if got := c.Count("camp", "u1", win); got != 2 {
		t.Fatalf("count=%d, want 2", got)
	}
}

func TestNoCapWhenZero(t *testing.T) {
	c := New("", nil)
	for i := 0; i < 100; i++ {
		if !c.Allow("camp", "u", 0, time.Minute) {
			t.Fatal("cap=0 means unlimited")
		}
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "freq.json")
	clk := &fakeClock{now: time.Unix(1000, 0)}
	c1 := New(path, clk.Now)
	const cap, win = 2, time.Hour
	c1.Allow("camp", "u1", cap, win)
	c1.Allow("camp", "u1", cap, win)

	// 模拟进程重启：新实例从同一状态文件恢复。
	c2 := New(path, clk.Now)
	if c2.Allow("camp", "u1", cap, win) {
		t.Fatal("state must survive restart: user should still be capped")
	}
	if got := c2.Count("camp", "u1", win); got != 2 {
		t.Fatalf("count=%d, want 2", got)
	}
	// 窗口随真实时间推进，重启后过期事件依然失效。
	clk.Advance(2 * time.Hour)
	c3 := New(path, clk.Now)
	if !c3.Allow("camp", "u1", cap, win) {
		t.Fatal("expired window should allow again after restart")
	}
}

func TestConcurrentAllow(t *testing.T) {
	c := New("", nil)
	const cap, win = 50, time.Minute
	var wg sync.WaitGroup
	allowed := make(chan bool, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed <- c.Allow("camp", "u", cap, win)
		}()
	}
	wg.Wait()
	close(allowed)
	n := 0
	for ok := range allowed {
		if ok {
			n++
		}
	}
	if n != cap {
		t.Fatalf("allowed=%d, want exactly %d under concurrency", n, cap)
	}
}
