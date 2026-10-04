// Package ledger 对曝光、点击、转化事件进行幂等记账与统计。
//
// 幂等与迟到语义：
//   - 每个事件以 Event.ID 去重：重复投递的事件返回首次处理结论且绝不重复计费。
//   - 事件以 append-only 的 JSON Lines 流水落盘（每条 fsync），
//     进程重启时回放流水重建去重索引与统计，重复/迟到事件不造成重复记账。
//   - 漏斗顺序约束（同一 request+campaign+user 为一条链路）：
//     曝光 -> 点击 -> 转化。点击必须能找到对应曝光，转化必须能找到对应点击，
//     否则以 orphan_click / orphan_conversion 拒绝；事件时间早于上游事件
//     则以 out_of_order 拒绝。迟到但顺序合法的事件正常入账。
//   - 仅 Accepted 事件改变统计；被拒事件只记录其结论用于去重。
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventType 为事件类型。
type EventType string

const (
	EventImpression EventType = "impression"
	EventClick      EventType = "click"
	EventConversion EventType = "conversion"

	// 拒绝原因码。
	RejectUnknownType      = "unknown_type"
	RejectImpressionExists = "impression_exists"
	RejectOrphanClick      = "orphan_click"
	RejectOrphanConversion = "orphan_conversion"
	RejectOutOfOrder       = "out_of_order"
)

// Event 为一次投放事件。
type Event struct {
	ID          string    `json:"id"`
	Type        EventType `json:"type"`
	RequestID   string    `json:"request_id"`
	CampaignID  string    `json:"campaign_id"`
	User        string    `json:"user"`
	At          time.Time `json:"at"`
	ValueMicros int64     `json:"value_micros,omitempty"`
}

// RecordResult 为记账处理结论。
type RecordResult struct {
	Accepted bool
	// Duplicate 表示该事件 ID 此前已处理过（重复投递），本次被忽略。
	Duplicate bool
	// RejectReason 在既非幂等重复也不被接受时给出原因。
	RejectReason string
}

// CampaignStats 为单个计划的聚合统计。
type CampaignStats struct {
	Impressions int64 `json:"impressions"`
	Clicks      int64 `json:"clicks"`
	Conversions int64 `json:"conversions"`
	SpendMicros int64 `json:"spend_micros"`
	ValueMicros int64 `json:"value_micros"`
}

// Stats 为全量聚合统计。
type Stats struct {
	Campaigns map[string]CampaignStats `json:"campaigns"`
	Totals    CampaignStats            `json:"totals"`
}

type journalEntry struct {
	Event        Event  `json:"event"`
	Accepted     bool   `json:"accepted"`
	RejectReason string `json:"reject_reason,omitempty"`
}

// Ledger 为事件账本。
type Ledger struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	seen   map[string]journalEntry
	imps   map[string]time.Time
	clicks map[string]time.Time
	stats  map[string]*CampaignStats
	totals CampaignStats
}

// New 构建账本并从 path 回放既有流水（文件不存在视为空），随后以追加方式打开。
func New(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("ledger: create state dir: %w", err)
	}
	l := &Ledger{
		path:   path,
		seen:   make(map[string]journalEntry),
		imps:   make(map[string]time.Time),
		clicks: make(map[string]time.Time),
		stats:  make(map[string]*CampaignStats),
	}
	if err := l.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("ledger: open journal: %w", err)
	}
	l.f = f
	return l, nil
}

func (l *Ledger) replay() error {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ledger: open journal for replay: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("ledger: corrupt journal line: %w", err)
		}
		l.apply(e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ledger: replay scan: %w", err)
	}
	return nil
}

// apply 将一条流水结论作用于内存索引/统计。调用方需持有锁或处于构造期。
func (l *Ledger) apply(e journalEntry) {
	l.seen[e.Event.ID] = e
	if !e.Accepted {
		return
	}
	key := chainKey(e.Event)
	switch e.Event.Type {
	case EventImpression:
		l.imps[key] = e.Event.At
	case EventClick:
		l.clicks[key] = e.Event.At
	case EventConversion:
	}
	s := l.stats[e.Event.CampaignID]
	if s == nil {
		s = &CampaignStats{}
		l.stats[e.Event.CampaignID] = s
	}
	switch e.Event.Type {
	case EventImpression:
		s.Impressions++
		s.SpendMicros += e.Event.ValueMicros
		l.totals.Impressions++
		l.totals.SpendMicros += e.Event.ValueMicros
	case EventClick:
		s.Clicks++
		l.totals.Clicks++
	case EventConversion:
		s.Conversions++
		s.ValueMicros += e.Event.ValueMicros
		l.totals.Conversions++
		l.totals.ValueMicros += e.Event.ValueMicros
	}
}

func chainKey(e Event) string {
	return e.RequestID + "\x00" + e.CampaignID + "\x00" + e.User
}

// evaluateLocked 判定事件是否可接受。调用方需持有 l.mu。
func (l *Ledger) evaluateLocked(e Event) (bool, string) {
	key := chainKey(e)
	switch e.Type {
	case EventImpression:
		if _, exists := l.imps[key]; exists {
			return false, RejectImpressionExists
		}
		return true, ""
	case EventClick:
		impAt, ok := l.imps[key]
		if !ok {
			return false, RejectOrphanClick
		}
		if e.At.Before(impAt) {
			return false, RejectOutOfOrder
		}
		return true, ""
	case EventConversion:
		clickAt, ok := l.clicks[key]
		if !ok {
			return false, RejectOrphanConversion
		}
		if e.At.Before(clickAt) {
			return false, RejectOutOfOrder
		}
		return true, ""
	default:
		return false, RejectUnknownType
	}
}

// Record 幂等地处理一个事件，结论先落盘（fsync）再更新内存。
func (l *Ledger) Record(e Event) (RecordResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prior, ok := l.seen[e.ID]; ok {
		return RecordResult{
			Accepted:     prior.Accepted,
			Duplicate:    true,
			RejectReason: prior.RejectReason,
		}, nil
	}
	accepted, reason := l.evaluateLocked(e)
	entry := journalEntry{Event: e, Accepted: accepted, RejectReason: reason}
	data, err := json.Marshal(&entry)
	if err != nil {
		return RecordResult{}, fmt.Errorf("ledger: marshal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := l.f.Write(data); err != nil {
		return RecordResult{}, fmt.Errorf("ledger: append journal: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return RecordResult{}, fmt.Errorf("ledger: fsync journal: %w", err)
	}
	l.apply(entry)
	return RecordResult{Accepted: accepted, RejectReason: reason}, nil
}

// Stats 返回当前统计的深拷贝快照。
func (l *Ledger) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := Stats{
		Campaigns: make(map[string]CampaignStats, len(l.stats)),
		Totals:    l.totals,
	}
	for id, s := range l.stats {
		out.Campaigns[id] = *s
	}
	return out
}

// SpendByCampaign 返回各计划已接受曝光的累计花费，
// 由流水重放得到，供引擎在进程重启后恢复预算余额。
func (l *Ledger) SpendByCampaign() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.stats))
	for id, s := range l.stats {
		out[id] = s.SpendMicros
	}
	return out
}

// Close 关闭流水文件。
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
