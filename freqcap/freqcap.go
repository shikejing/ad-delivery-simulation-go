// Package freqcap 实现滚动时间窗口的频次控制。
//
// 语义：同一 (campaign, user) 在长度为 window 的滚动窗口内的曝光次数
// 不得超过上限。窗口随真实时间推进（非固定格子）。当配置了状态文件
// 路径时，每次变更都会落盘，进程重启后既有频控状态仍然生效。
package freqcap

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Counter 记录同一用户在滚动窗口内的曝光次数，并发安全。
type Counter struct {
	mu     sync.Mutex
	path   string
	now    func() time.Time
	events map[string][]int64 // key -> 已排序的曝光时间戳（unix nano）
}

// New 创建 Counter；path 非空时从该文件恢复状态并在变更时落盘。
// now 为时钟，传 nil 使用真实时间，测试可注入虚拟时钟。
func New(path string, now func() time.Time) *Counter {
	if now == nil {
		now = time.Now
	}
	c := &Counter{path: path, now: now, events: make(map[string][]int64)}
	if path != "" {
		c.load()
	}
	return c
}

func key(campaignID, userID string) string { return campaignID + "\x00" + userID }

// prune 移除窗口外的过期时间戳，调用时必须持有锁。
func (c *Counter) prune(k string, window time.Duration) {
	cutoff := c.now().Add(-window).UnixNano()
	ts := c.events[k]
	i := sort.Search(len(ts), func(i int) bool { return ts[i] > cutoff })
	if i > 0 {
		c.events[k] = append([]int64(nil), ts[i:]...)
	}
}

// Allow 判断该用户在该计划下是否还能再曝光一次；允许时立即计入一次。
// cap <= 0 表示不限频。
func (c *Counter) Allow(campaignID, userID string, cap int, window time.Duration) bool {
	if cap <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key(campaignID, userID)
	c.prune(k, window)
	if len(c.events[k]) >= cap {
		return false
	}
	c.events[k] = append(c.events[k], c.now().UnixNano())
	c.saveLocked()
	return true
}

// Count 返回当前窗口内已计次数（不计入新事件）。
func (c *Counter) Count(campaignID, userID string, window time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key(campaignID, userID)
	c.prune(k, window)
	return len(c.events[k])
}

// Reset 清空全部状态（主要用于测试）。
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = make(map[string][]int64)
	c.saveLocked()
}

type diskState struct {
	Events map[string][]int64 `json:"events"`
}

func (c *Counter) load() {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return // 文件不存在或不可读时从空状态开始
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil {
		return // 状态文件损坏时从空状态开始，避免进程无法启动
	}
	if st.Events != nil {
		c.events = st.Events
	}
}

// saveLocked 将状态落盘，调用时必须持有锁。写临时文件后重命名，
// 避免崩溃留下半截文件。
func (c *Counter) saveLocked() {
	if c.path == "" {
		return
	}
	data, err := json.Marshal(diskState{Events: c.events})
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}
