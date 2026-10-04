# 竞价广告投放模拟（本地、可复现）

纯本地进程内运行的竞价广告投放模拟器，不依赖任何外部服务，也不使用真实投放数据。
用于在本地评估**竞价决策**与**投放节奏**的表现。

- 金额单位：`int64` 微元（Micros），全程整数运算，避免浮点不确定性。
- 质量因子：百万分比（Permillion），`1_000_000` 表示质量系数 1.0。
- 时间：通过可注入的 `clock.Clock` 获取；生产用真实时钟，测试用可推进假时钟。

## 快速开始

```go
cfg := config.Config{
    Seed:     42,
    StateDir: "./.state", // 频控与事件流水持久化目录
    Floor:    config.FloorConfig{FloorMicros: 100, MinBidMicros: 100},
    Frequency: config.FrequencyConfig{Window: time.Minute, MaxImpressions: 3},
    Campaigns: []config.CampaignConfig{
        {ID: "a", BudgetMicros: 10000, Pacing: config.PacingConfig{
            Horizon: time.Hour, Slice: time.Minute, BurstPermillion: 100_000,
        }},
    },
}
eng, err := engine.New(cfg, clock.System{}) // 启动期校验，失败返回 *config.ValidationError
if err != nil { log.Fatal(err) }
defer eng.Close()

d, err := eng.Decide(engine.BidRequest{
    ID: "req-1", User: "user-1",
    Candidates: []engine.CandidateInput{
        {CampaignID: "a", BidMicros: 500, QualityPermillion: 1_200_000},
        {CampaignID: "b", BidMicros: 300},
    },
})
// d.Winner / d.Winner.ChargeMicros / d.Rejected(原因) / d.NoWinnerReason

// 点击/转化回传（幂等）
eng.PostEvent(ledger.Event{ID: "clk-1", Type: ledger.EventClick,
    RequestID: "req-1", CampaignID: d.Winner.Candidate.CampaignID, User: "user-1", At: time.Now()})
```

运行演示：`go run ./examples/demo`

## 包结构

| 包 | 职责 |
|---|---|
| `clock` | 真实/可推进时钟抽象 |
| `config` | 配置项与启动期校验（可区分失败原因码） |
| `auction` | 纯函数竞价排序与 GSP 计费，无状态 |
| `campaign` | 预算余额、暂停、线性平滑节奏 |
| `frequency` | 滚动窗口频控，原子落盘，重启生效 |
| `ledger` | 曝光/点击/转化幂等记账，append-only 流水回放 |
| `engine` | 装配整条链路，串行化决策，保证自洽与可复现 |

详见 [docs/design.md](docs/design.md)。

## 测试

```bash
go test ./...            # 全量测试
go test -race ./...      # 竞态检测
```

覆盖：多候选竞价与质量加权、GSP 计费与底价、预算耗尽与并发击穿、
暂停即时生效、频控滚动窗口与重启恢复、重复/迟到/乱序事件、
并发记账、相同种子下的逐条可复现。
