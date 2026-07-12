# 账号处理逻辑审查（0.2.19）

## 两条入口

| 入口 | 触发 | 作用 |
|------|------|------|
| `HandleUsage` | CPA 真实请求 usage 事件 | 运行中额度/死号处置 |
| `PatrolSweep` | 主动/定时巡查探测 | 主动筛死号 + 402 恢复 + 429 冷却 |

两者共用 `match.go` 分类器，但处置分支不完全对称（0.2.9 已对齐 429）。

## 权威错误矩阵（实现核对）

| 信号 | HandleUsage | Patrol |
|------|-------------|--------|
| 429 free-usage（MatchShortWindowQuota） | plugin_auto 冷却 + Tick 恢复 | **0.2.9：plugin_auto 冷却**；spending 冷却号上的 429 → 恢复启用 |
| 429 无识别信号 | 跳过 + 日志 | error，不删不存活 |
| 402 spending-limit | plugin_auto 冷却（signal=spending_limit） | 同上；候选含 spending 冷却号 |
| 403 region | 不删，warn | error，不删 |
| 403 chat endpoint / 真 permission | DELETE | DELETE |
| 401 invalid/expired | DELETE | DELETE |
| 404/405/422/5xx 探测 | n/a | error |
| 网络超时/代理失败 | n/a | error |
| 200 | 记用量 | alive；spending 冷却则 reenable |

## 所有权与手动禁用

- `Owner=cpa_xai_quota_plugin` + `disable_source=plugin_auto` + `!PreDisabled` 才允许自动启用（Tick / 巡查 reenable）。
- 已禁用且无本插件所有权 → 标 `user_manual`，永不自动启用。
- `SetDisabled` 竞态 prev=true → user_manual。

## 巡查候选

- **all**：启用中的 xAI + `spending_limit` 冷却号（用于恢复探测）。
- **不含** free-usage 冷却号（靠 Tick 到期恢复，避免无意义探测）。
- **spending_only**：仅 spending 冷却复查。
- 已禁用的非 spending 号不巡查。

## 已确认的实现缺口 / 风险

### P0（0.2.9 已修）
- 巡查遇 429 曾只标 `alive`，**未冷却**启用账号（与 HandleUsage 不一致）。现已对齐。

### P1（仍在）
1. **探测依赖代理/出口**：`patrol_proxy_url` 坏或直连超时 → 全 error，易误判“全挂”。
2. **默认/错误模型** 可导致 region 或假 402（模型选择责任在用户；自动换模默认关）。
3. **region 历史删除日志** 仍显示旧误删，非新行为。
4. **probe 循环 region 早期 return** 与末尾 region 分支重复（无害，可读性差）。

### P2
1. free-usage 与 spending 都用 `StateAutoDisabled`，仅靠 `Signal` 区分；UI 需明确展示 signal。
2. spending 冷却墙钟 24h 与真实恢复可能不一致（设计为巡查提前恢复）。
3. `recordProbeResult` 默认分支（0.2.19）：未知 action **计 error**，不再计 alive。
4. 表单 dirty 与配置 Reconfigure 仍可能偶发覆盖（0.2.8 已缓解）。

## 不变量（测试覆盖）

- `TestGuardDisableAndRecoverPluginAuto`
- `TestGuardNeverRecoversUserManual`
- `TestGuardDeletesPermissionDenied` / `TestGuardDeletesInvalidCredentials401`
- `TestGuardCooldownsSpendingLimit402`
- `TestRegionPermissionDeniedNotDead`
- `TestMatchGrokFreeUsageExhausted24h`
- `TestMatchSpendingLimitQuotaDistinctFrom429`

缺：patrol 路径 429 冷却的集成单测（建议 0.2.10 补）。

## 结论

- **429 不删除**：HandleUsage 与 Patrol 均成立。
- **巡查 429 的正确动作是冷却（启用号）或恢复（spending 冷却号）**，不是删除，也不是简单 alive。
- 账号状态机整体设计正确；主要问题在巡查与 usage 路径不对称、探测网络环境、历史误删日志污染观感。

## 0.2.19 增补

- 网络：timeout/cancel/DNS/TLS/connect 分桶 + 同模型最多 3 次重试（取消不重试）。
- 426/region/5xx/422 有独立 action，均不删除。
- 巡查 UI 汇总与 by_http 同源；去掉第二套动作 chips。
