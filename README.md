# ad-delivery-simulation-go

一套完全在本地进程内运行的竞价广告投放模拟器，用于评估竞价决策与投放节奏的表现。不依赖任何外部服务，不使用真实投放数据；相同随机种子与相同输入下输出逐条一致、可复现。

## 构建与运行

```bash
go build ./...
go run ./cmd/adsim -seed 42 -requests 200
go test ./...          # 单元与端到端测试
go test -race ./...    # 竞态检测
```

## 模块划分

| 包 | 职责 |
|---|---|
| `config` | 配置结构与启动期校验 |
| `auction` | 竞价决策、胜出者选择、计费价计算 |
| `pacing` | 计划预算与平滑投放节奏 |
| `freqcap` | 滚动窗口频次控制（可持久化） |
| `events` | 曝光/点击/转化的幂等记账 |
| `sim` | 将以上组件组装为确定性模拟引擎 |
| `cmd/adsim` | 可复现的演示入口 |

## 竞价与计费规则

一次竞价请求（`auction.BidRequest`）可携带多个候选（`Candidate{CampaignID, BidCents, Quality}`）。每个候选依次经过滤：

1. **出价合法性**：出价 < 0 或质量分不在 (0,1] → `invalid_bid`
2. **底价**：出价低于底价（全局底价与计划底价取较大者）→ `below_floor`
3. **预算/节奏/暂停**：计划预算耗尽、超出节奏释放额度或被暂停 → `no_budget`
4. **频控**：该用户在该计划的滚动窗口内曝光已达上限 → `freq_capped`

合格候选按 `出价 × 质量分` 排序，最高分胜出；同分按 `CampaignID` 字典序，保证确定性。未胜出的合格候选标记为 `lost_auction`；无合格候选时结果为 `no_eligible`，具体原因见各候选的 `Decisions`。

**计费（广义二价）**：胜出者支付

```
price = max(底价, ⌊次高分 / 胜者质量分⌋ + 1)
```

再被钳制到 `min(price, 胜者出价, 全局价格上限)`。唯一合格候选直接付底价。

## 节奏语义（pacing）

每个计划有 `PacingWindow` 长度的投放周期，预算在周期内**按时间线性释放**：

```
时刻 t 允许累计花费上限 = Budget × elapsed / PacingWindow
```

- 花费达到当前释放额度后 `CanBid` 返回 false，避免预算在周期开始瞬间打满；
- 周期结束后释放全部预算；
- `Charge` 拒绝任何会使总花费超过预算上限的记账（整体拒绝，不留部分扣减）；
- 预算耗尽或 `Pause` 后计划**立即**停止参与竞价，`Resume` 可恢复（预算与节奏限制仍生效）。

## 频次控制

- 同一 `(campaign, user)` 在长度 `FreqWindow` 的**滚动窗口**内曝光不超过 `FreqCap` 次（0 表示不限）；
- 窗口随真实时间推进（非固定格子），过期曝光自动滑出；
- 配置 `Config.StatePath` 后状态每次变更落盘（临时文件 + rename），**进程重启后既有频控状态仍然生效**。

## 事件记账（幂等）

- 每条事件携带全局唯一 `ID`，账本按 ID 去重：**重复投递与迟到事件不改变统计结果**；
- 统计永远等于"已收到的去重事件集合"的函数，与到达顺序、到达次数无关；
- 曝光事件携带计费价，去重保证不会重复计费；
- 引擎内曝光事件 ID 由 `请求ID:计划ID` 确定性派生，天然幂等。

## 并发与可复现性

- 所有共享状态（预算、频控、账本）均有锁保护，`go test -race` 通过；
- 计费提交（频控计数 → 预算扣减 → 曝光记账）在引擎内串行化，任一时刻预算余额、频控计数与统计结果自洽；
- 引擎本身不含随机源；相同配置、时钟与输入序列产生逐条一致的结果（见 `TestDeterministicReplay`）。

## 配置项与启动校验

```go
type Config struct {
    Campaigns []CampaignConfig
    Floor     FloorConfig  // 全局底价区间 [MinCents, MaxCents]，MaxCents 兼作价格上限
    StatePath string       // 频控状态文件路径，空为仅内存
}

type CampaignConfig struct {
    ID           string
    BudgetCents  int64         // 预算上限（分），>= 0
    FloorCents   int64         // 计划底价（分），>= 0 且 <= 全局 MaxCents
    FreqCap      int           // 滚动窗口曝光上限，0 不限
    FreqWindow   time.Duration // FreqCap > 0 时必须 > 0
    PacingWindow time.Duration // 必须 > 0
}
```

`config.Validate` 在启动阶段执行，失败原因可用 `errors.Is` 区分：

| 错误 | 触发条件 |
|---|---|
| `ErrNegativeBudget` | 预算为负 |
| `ErrInvalidFreqCap` | 频率上限为负 |
| `ErrInvalidWindow` | 频控/节奏窗口非法 |
| `ErrFloorConflict` | 底价区间为负、Max < Min、计划底价为负或超过全局上限 |
| `ErrDuplicateID` / `ErrMissingID` | 计划 ID 重复或为空 |

校验是纯函数，失败时 `sim.New` 返回错误且不留下任何部分生效的配置或中间状态。
