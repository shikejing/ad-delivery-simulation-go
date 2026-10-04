// Package clock 提供可注入、可推进的时钟抽象。
//
// 模拟链路使用该抽象获取"当前时间"，从而：
//   - 在测试中以确定性方式推进时间（验证滚动窗口、迟到事件等语义）；
//   - 在生产形态下使用系统真实时间（进程重启后频控窗口随真实时间滚动）。
package clock

import "time"

// Clock 表示可获取当前时刻的时钟。
type Clock interface {
	// Now 返回当前时间。
	Now() time.Time
}

// System 使用操作系统真实时间。
type System struct{}

// Now 实现 Clock。
func (System) Now() time.Time { return time.Now() }

// Fake 是可手动设定/推进的确定性时钟。
type Fake struct {
	now time.Time
}

// NewFake 以 t 为初始时刻构造假时钟。
func NewFake(t time.Time) *Fake {
	return &Fake{now: t}
}

// Now 返回假时钟当前时刻。
func (c *Fake) Now() time.Time { return c.now }

// Set 直接设置当前时刻。
func (c *Fake) Set(t time.Time) { c.now = t }

// Advance 将当前时刻向前推进 d。
func (c *Fake) Advance(d time.Duration) { c.now = c.now.Add(d) }
