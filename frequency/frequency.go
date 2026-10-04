// Package frequency 实现基于滚动时间窗口的用户曝光频次控制。
//
// 语义：
//   - 每个用户维护其最近曝光时间戳序列；判定时只保留 (now-window, now] 内的记录，
//     这是随真实时间不断向前滚动的滑动窗口。
//   - 窗口内曝光数达到上限后，新曝光一律拒绝，直到窗口内最早记录滑出窗口。
//   - 每次接受曝光都以"写临时文件 + fsync + 原子 rename + 目录 fsync"落盘，
//     因此进程重启后既有频控状态继续生效，且不会读到半截文件。
package frequency

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cap 是频控器。
type Cap struct {
	mu             sync.Mutex
	window         time.Duration
	maxImpressions int
	statePath      string
	events         map[string][]time.Time
}

type persistedState struct {
	Version int                 `json:"version"`
	Window  string              `json:"window"`
	Events  map[string][]string `json:"events"`
}

// NewCap 构建频控器并从 statePath 载入既有状态；文件不存在时视为空状态。
func NewCap(window time.Duration, maxImpressions int, statePath string) (*Cap, error) {
	if window <= 0 {
		return nil, fmt.Errorf("frequency: window must be > 0, got %s", window)
	}
	if maxImpressions <= 0 {
		return nil, fmt.Errorf("frequency: max impressions must be > 0, got %d", maxImpressions)
	}
	c := &Cap{
		window:         window,
		maxImpressions: maxImpressions,
		statePath:      statePath,
		events:         make(map[string][]time.Time),
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, fmt.Errorf("frequency: create state dir: %w", err)
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cap) load() error {
	data, err := os.ReadFile(c.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("frequency: read state: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return fmt.Errorf("frequency: corrupt state file: %w", err)
	}
	for u, ts := range ps.Events {
		times := make([]time.Time, 0, len(ts))
		for _, s := range ts {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return fmt.Errorf("frequency: corrupt timestamp %q: %w", s, err)
			}
			times = append(times, t)
		}
		c.events[u] = times
	}
	return nil
}

// persistLocked 将当前状态原子写盘。调用方需持有 c.mu。
func (c *Cap) persistLocked() error {
	ps := persistedState{Version: 1, Window: c.window.String(), Events: map[string][]string{}}
	for u, ts := range c.events {
		if len(ts) == 0 {
			continue
		}
		strs := make([]string, len(ts))
		for i, t := range ts {
			strs[i] = t.Format(time.RFC3339Nano)
		}
		ps.Events[u] = strs
	}
	data, err := json.MarshalIndent(&ps, "", "  ")
	if err != nil {
		return fmt.Errorf("frequency: marshal state: %w", err)
	}
	dir := filepath.Dir(c.statePath)
	tmp, err := os.CreateTemp(dir, ".freq-*.tmp")
	if err != nil {
		return fmt.Errorf("frequency: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("frequency: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("frequency: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("frequency: close temp: %w", err)
	}
	if err := os.Rename(tmpName, c.statePath); err != nil {
		return fmt.Errorf("frequency: rename: %w", err)
	}
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// pruneLocked 删除 user 在 now 时刻已滑出窗口的记录。调用方需持有 c.mu。
func (c *Cap) pruneLocked(user string, now time.Time) {
	old := c.events[user]
	cutoff := now.Add(-c.window)
	kept := make([]time.Time, 0, len(old))
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(c.events, user)
	} else {
		c.events[user] = kept
	}
}

// Allowed 判断 user 在 now 时刻是否还允许一次新曝光（不改变计数、不落盘）。
func (c *Cap) Allowed(user string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(user, now)
	return len(c.events[user]) < c.maxImpressions
}

// Record 记录 user 在 now 时刻的一次曝光并原子落盘；
// 若记录后将超出上限则拒绝，状态与文件均保持不变。
func (c *Cap) Record(user string, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(user, now)
	if len(c.events[user]) >= c.maxImpressions {
		return false, nil
	}
	c.events[user] = append(c.events[user], now)
	if err := c.persistLocked(); err != nil {
		// 落盘失败：回滚内存中的追加，保证内存与磁盘一致。
		evs := c.events[user]
		c.events[user] = evs[:len(evs)-1]
		if len(c.events[user]) == 0 {
			delete(c.events, user)
		}
		return false, err
	}
	return true, nil
}

// Count 返回 user 在 now 时刻窗口内的有效曝光数。
func (c *Cap) Count(user string, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(user, now)
	return len(c.events[user])
}

// Release 撤销 user 最近一次成功 Record 的曝光并原子落盘，
// 仅用于提交阶段补偿；无记录可撤销时返回 false。
func (c *Cap) Release(user string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := c.events[user]
	if len(evs) == 0 {
		return false, nil
	}
	evs = evs[:len(evs)-1]
	if len(evs) == 0 {
		delete(c.events, user)
	} else {
		c.events[user] = evs
	}
	if err := c.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}
