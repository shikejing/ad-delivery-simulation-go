# 设计与语义说明

所有逻辑只在本地进程内运行：频控与事件数据写入本地 `StateDir`，
不访问任何外部服务。金额使用 `int64` 微元、质量使用百万分比整数运算。

## 1. 竞价与计费

### 排序
每个候选的有效价值：

```
effective = bid * qualityPermillion / 1_000_000
```

按 `effective` 降序；并列时按 `TieBreakKey`（由 `Seed + 请求ID + 候选位置 +
计划ID` 经 FNV-1a 派生）升序，再按请求内原始下标升序。**排序不含任何随机调用、
不依赖 goroutine 调度**，因此相同种子与相同输入集合下结果逐条一致。

### 成交价格（GSP 二价 + 底价）
胜出者按下一名的有效价值折算回胜出者质量，并与底价取大：

```
gsp    = ceil(runnerUpEffective * 1_000_000 / winnerQuality)
charge = max(floor, gsp)
```

- 单候选（或无有效第二名）时 `charge = floor`。
- 若 `charge > winner.bid`，视为该成交价**超出胜出者自身出价**，等同于底价不可达，
  本次请求无胜出者（`above_own_bid`），广告主永远不会为超过自己出价的价格付费。
- 计费在曝光时从对应计划预算中扣除，`spent + remaining = budget` 恒成立。

### 可区分淘汰原因（逐候选 + 无胜出者汇总）
候选级 `Rejected[].Reason`：

| 原因码 | 含义 |
|---|---|
 `below_min_bid` | 出价为负或低于 `floor.min_bid_micros`（无效出价，不进入排序） |
| `below_floor` | 参与了排序但未胜出 / 成交价达不到底价 |
| `campaign_paused` | 计划已暂停 |
| `campaign_no_budget` | 预算余额不足以承担成交费 |
| `campaign_paced` | 被平滑节奏限流 |
| `frequency_capped` | 用户当前窗口曝光数已达上限 |
| `unknown_campaign` | 候选引用了配置中不存在的计划 |

无胜出者汇总 `NoWinnerReason`：`no_candidates` / `all_invalid_bid` /
`all_below_floor` / `all_ineligible` / `above_own_bid`。

> 当排序第一的候选在**提交阶段**才因预算/节奏失败时，引擎会把它标记为
> `campaign_no_budget` / `campaign_paced` 后用剩余候选**重新竞价**，
> 因此次优候选有机会以正确的二价胜出，而不是直接流拍。

## 2. 预算与节奏

### 预算
- 任一计划累计花费 `spent <= budget`，扣费是"判定+写入"同一把锁内的原子操作。
- 余额不足时扣费失败且状态不变；引擎级再以单一互斥串行化所有请求，
  并发下也不会发生超支击穿（有并发测试 + `-race` 验证）。
- 预算花费随曝光写入事件流水，进程重启后回放恢复，已耗尽计划重启后仍不参与竞价。

### 节奏语义（线性平滑 + 突发额度）
以引擎启动时刻 `start` 为锚点，把 `pacing.horizon` 按 `pacing.slice` 等分为
`numSlices` 个切片。任意时刻 `t` 的**累计**花费上限：

```
idx    = (t - start) / slice               (0-based)
base   = budget * min(idx+1, numSlices) / numSlices
cap(t) = min(budget, base * (1 + burstPermillion/1_000_000))
```

- 上限随时间**线性增长**，投放初期不可能瞬间把预算放完；
- `burst_permillion` 允许在单切片目标之上高出一小段（如 `100_000` = +10%），
  但任何时刻都不允许超过 `budget`；
- horizon 结束后 `cap` 恒等于 `budget`（剩余预算可正常花完）；
- 判定只依赖 `(当前时间, 累计花费)`，无随机量，行为确定可复现；
- `horizon <= 0` 表示关闭平滑，仅受预算约束。

### 暂停/恢复
`PauseCampaign` 立即生效：`Eligible` 与 `Charge` 都拒绝暂停计划，
下一次竞价起该计划完全不参与。`ResumeCampaign` 恢复但不补充预算、不重置锚点。

## 3. 频次控制

- 为每个用户保留曝光时间戳序列，判定/记录时只保留 `(now-window, now]` 的记录
  （滑动窗口，边界 `now-window` 时刻的记录已滑出）。
- 窗口内计数达到 `frequency.max_impressions` 后新曝光被拒绝，
  直到最早的记录随时间滑出窗口。
- 每次接受曝光都以「临时文件 + fsync + 原子 rename + 目录 fsync」落盘；
  拒绝不写盘，写盘失败回滚内存，保证内存与磁盘一致。
- 进程重启从 `StateDir/frequency.json` 恢复，窗口随真实时间继续滚动。

## 4. 幂等记账与事件语义

事件以 append-only JSON Lines（`StateDir/events.log`）保存，每条 fsync，
重启时整段回放重建索引与统计。

- **去重键为 `Event.ID`**：重复投递返回首次处理结论（`Duplicate=true`），
  绝不重复计费；即使重复投递携带的金额不同，也完全忽略，统计不偏离。
- 漏斗链路键为 `request_id + campaign_id + user`，顺序约束：
  - 点击必须存在对应曝光，否则 `orphan_click`；
  - 转化必须存在对应点击，否则 `orphan_conversion`；
  - 事件时间早于上游事件（点击早于曝光、转化早于点击）为 `out_of_order`；
  - **迟到但顺序合法**的事件（如数小时后才回传的转化）正常入账。
- 被拒绝的事件也会记录其结论用于去重，重放被拒事件返回 `Duplicate`。
- 曝光事件由引擎在竞价胜出时自动写入，ID 固定为 `"imp:"+request_id`；
  同一竞价请求 ID 重放返回首次决策，绝不二次扣费或记账。

## 5. 并发一致性

引擎以单一互斥串行化「一次决策」的完整过程：

```
资格过滤(预算/状态/频控) → 排序计费 → 占频控名额 → 扣费 → 曝光记账
```

任何一步失败都补偿此前已成功的步骤（释放频控名额 / 回退扣费）。
因此任意时刻观测到的预算余额、频控计数、账本统计三者互相自洽，
`sum(spent) == ledger.total_spend`，无数据竞争（`go test -race` 通过）。

> 这里有意选择粗粒度串行：决策提交包含多次磁盘 fsync，串行实现保证了
> 「余额/频控/统计」的强一致与跨并发调度的逐请求可复现，这是本模拟器的首要目标。

## 6. 配置项与启动校验

| 字段 | 约束 | 失败码 |
|---|---|---|
| `state_dir` | 非空 | `empty_state_dir` |
| `floor.floor_micros` | `>= 0` | `negative_floor` |
| `floor.min_bid_micros` | `>= 0` 且 `>= floor_micros` | `negative_min_bid` / `floor_contradiction` |
| `frequency.window` | `> 0` | `invalid_frequency_window` |
| `frequency.max_impressions` | `> 0` | `invalid_frequency_max` |
| `campaigns[i].id` | 非空且不重复 | `empty_campaign_id` / `duplicate_campaign_id` |
| `campaigns[i].budget_micros` | `>= 0` | `negative_budget` |
| `campaigns[i].pacing.horizon` | `>= 0`（0 关闭平滑） | `invalid_pacing_horizon` |
| `campaigns[i].pacing.slice` | 启用平滑时 `> 0` 且 `<= horizon` | `invalid_pacing_slice` |
| `campaigns[i].pacing.burst_permillion` | `[0, 1_000_000]` | `invalid_burst_permillion` |

`engine.New` 先调用纯函数式的 `Validate`（不产生任何文件副作用），
再顺序初始化持久化组件；任一组件初始化失败都会回滚本次新建的状态目录，
不会留下部分生效的配置或中间状态。错误统一为 `*config.ValidationError`，
携带 `Field` / `Code` / `Msg`，可按 `Code` 机器判别。
