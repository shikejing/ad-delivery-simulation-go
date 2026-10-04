// Package events 实现曝光、点击、转化事件的幂等记账。
//
// 幂等语义：每条事件携带全局唯一 ID，账本按 ID 去重——重复投递
// 与迟到事件不会改变统计结果，统计永远等于"已收到的去重事件集合"
// 的函数，与到达顺序和到达次数无关。
package events

import (
	"sync"
	"time"
)

// Type 事件类型。
type Type string

const (
	Impression Type = "impression"
	Click      Type = "click"
	Conversion Type = "conversion"
)

// Event 是一条待记账事件。
type Event struct {
	ID         string // 全局唯一事件 ID，用于幂等去重
	Type       Type
	CampaignID string
	UserID     string
	PriceCents int64 // 仅曝光事件携带计费价
	Timestamp  time.Time
}

// Stats 是一个计划的累计统计。
type Stats struct {
	Impressions int
	Clicks      int
	Conversions int
	SpendCents  int64
}

// Ledger 是幂等的事件账本，并发安全。
type Ledger struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	stats map[string]*Stats
}

// NewLedger 创建空账本。
func NewLedger() *Ledger {
	return &Ledger{
		seen:  make(map[string]struct{}),
		stats: make(map[string]*Stats),
	}
}

// Record 记账一条事件。返回 true 表示事件首次被接受并计入统计；
// 返回 false 表示重复事件（含迟到重投），被忽略，不产生任何影响。
func (l *Ledger) Record(e Event) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.seen[e.ID]; dup {
		return false
	}
	l.seen[e.ID] = struct{}{}
	st := l.stats[e.CampaignID]
	if st == nil {
		st = &Stats{}
		l.stats[e.CampaignID] = st
	}
	switch e.Type {
	case Impression:
		st.Impressions++
		st.SpendCents += e.PriceCents
	case Click:
		st.Clicks++
	case Conversion:
		st.Conversions++
	}
	return true
}

// Stats 返回指定计划的统计快照。
func (l *Ledger) Stats(campaignID string) Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, ok := l.stats[campaignID]; ok {
		return *st
	}
	return Stats{}
}

// Snapshot 返回全部计划的统计快照。
func (l *Ledger) Snapshot() map[string]Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Stats, len(l.stats))
	for id, st := range l.stats {
		out[id] = *st
	}
	return out
}

// Recorded 报告某事件 ID 是否已被记账（主要用于测试）。
func (l *Ledger) Recorded(eventID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.seen[eventID]
	return ok
}
