# 飞书推送降噪：开发文档

状态：方案已与用户逐条确认（2026-10-14），待实现
分支：`feat/alert-noise`（基于 `origin/main` efa2cc9，worktree `E:\works\CPA-Manager-alert-noise`）
范围：只改 CPA-buddy 的告警分类、推送、汇总和状态持久化。不改采集、不改 `internal/history` 冻结字段、不碰 `internal/predict`。

## 1. 目标

即时推送只推**需要人动手的事**。其余事件不打扰，必要时出现在定时汇总的"待处理"里。

现状（origin/main 已核实）：

- 推送去向只有一个群（`FEISHU_CHAT_ID`），没有免打扰。
- 即时推送的事件类型：`quota_threshold`（90/95%）、`quota_exhausted`、`quota_reset`、`rate_limited`（账号级且超过 `COOLDOWN_ALERT_AFTER`=30m）、`rate_limit_cleared`、`credential_invalid`、`suspect_anomaly`、`stale_data`、`recovered`、`bootstrap_summary`。见 `internal/domain/types.go:491-506`。
- 同一轮的多条事件合并成一张卡片（`internal/app/app.go` `buildAlertMessage`，`internal/notify/render/alertnotice.go`）。
- 定时汇总：`SUMMARY_TIMES` 默认 `10:00,17:00`，每次都发。
- 主要噪音来源：刷新和恢复类"好消息"；网络抖动导致"异常→陈旧→已恢复"来回推；90%、95%、100% 连续推，叠加超过 90% 后改为 3 分钟一次的快速轮询。

## 2. 推送规则

### 2.1 即时推送（三类）

| 类型 | 触发条件 | 默认值 / 配置 |
|---|---|---|
| 凭据失效 `credential_invalid` | 沿用现有判定：401/403、token 刷新失败、Ollama cookie 失效 | 不变 |
| 渠道整体不可用 `channel_unavailable`（新） | 某个渠道的**已启用**账号全部不可用，并且最早恢复时间晚于当前时间加阈值 | `CHANNEL_UNAVAILABLE_AFTER=30m` |
| 监控停摆 `monitor_down`（新，替代即时的 `suspect_anomaly`/`stale_data`） | 某个渠道连续拿不到任何有效数据超过阈值；在一次故障期间只推一次 | `MONITOR_DOWN_AFTER=30m` |

"账号不可用"的判定：任一**账号级**窗口用尽（account scope）、处于账号级冷却，或者凭据失效，满足其一即算。只是模型级或分组级窗口用尽的账号仍算可用。

"最早恢复时间"取所有账号里最早的那个：用尽的账号取 `reset_at`，冷却的账号取预计恢复时间，失效的账号视为不会自动恢复。

已禁用的账号不计入渠道。如果某个渠道的账号全部被禁用，不推送（这是有意的人为操作）。

### 2.2 不再即时推送的事件

| 事件 | 新的去处 |
|---|---|
| 额度 90% / 95%、单个账号用尽 | 只更新状态，不推。会被"渠道整体不可用"和汇总里的"渠道紧张"覆盖 |
| 单个账号限流、冷却解除 | 不推 |
| 额度刷新、已恢复（`quota_reset`/`recovered`/`rate_limit_cleared`） | 不推，并且不写进汇总（汇总只看接下来半天） |
| 短暂的采集失败、数据陈旧 | 不推。如果持续超过 `MONITOR_DOWN_AFTER`，升级为 `monitor_down` |
| OMC 中断但已经改为直接查 CPA（依赖 `feat/omc-quota-source`） | 不算监控停摆。中断超过 `OMC_OUTAGE_PENDING_AFTER=2h` 时，作为待处理项写进汇总，不即时推 |

### 2.3 恢复与重复

- 问题解决后**不推**"已恢复"，对应的待处理项自动关闭。
- 问题一直没解决时**不重复推**。它会一直列在定时汇总的"待处理"里，直到解决。
- 同一个待处理项只在它第一次出现时即时推一次。重启不会导致重推，因为状态是持久化的。

### 2.4 免打扰

- 配置 `QUIET_HOURS=23:00-08:00`，可以跨午夜，按 `TZ_NAME` 计算。设为空表示关闭免打扰。
- 免打扰期间**所有**即时推送都先攒着，不例外。
- 免打扰结束时，把**仍未解决**的攒下事件合成一条发出。如果攒下的事件都已经自行恢复，就不发。
- 攒下的队列需要持久化，重启后不丢。
- 免打扰期间到了 `SUMMARY_TIMES` 的时间点，也不发汇总。

### 2.5 预测耗尽提醒

本期不做。等 `internal/predict` 完成后单独接入，届时需要定好提前多久推、每个窗口最多推几次。

## 3. 定时汇总

### 3.1 什么时候发

到 `SUMMARY_TIMES` 时间点时，只有满足下列**任一**条件才发送，否则跳过：

1. 存在未解决的待处理项。
2. 某个渠道比较紧张：可用账号里剩余最多的那个，在任一账号级窗口的剩余比例低于 `SUMMARY_TIGHT_REMAINING=20`（百分比）。

### 3.2 内容（只看接下来半天）

```
[标题颜色 = 待处理项的最高级别；没有待处理项时显示"渠道紧张"的颜色]
结论：一句话，比如"Claude 紧张，有 1 项待处理"
待处理（N）：类型 · 账号 · 开始时间 · 建议动作
紧张渠道：渠道 · 最好的账号剩余 X% · 几点重置
▸ 各账号明细（折叠，沿用现有面板）
[查询按钮]
```

- 删掉"上次汇总以来"的计数，也删掉限流和 429 的统计块（`rateLimitBlock`）。需要回看过去时，通过查询或 agent（OMC MCP）获取。
- 预测功能完成后，在"紧张渠道"一块加上"可能不够用"和"建议用哪个账号"。
- 具体视觉交给 @designer，文案由 orchestrator 复核。

### 3.3 启动时的汇总

`bootstrap_summary` 保留不变，用来确认部署成功。

## 4. 待处理项（结构化记录）

为以后让 agent 接管做准备。本期只做存储和展示，不开放给 agent，也不加按钮。

```go
type PendingItem struct {
    ID         string    // 稳定 ID：hash(kind, credential key 或 provider, scope)
    Kind       string    // credential_invalid | channel_unavailable | monitor_down | omc_outage
    Provider   string
    CredKey    string    // 渠道级的项为空
    Alias      string
    Since      time.Time // 第一次出现的时间
    LastSeen   time.Time
    Action     string    // 建议动作，比如"重新登录后更新 cookie"
    Status     string    // open | resolved
    NotifiedAt time.Time // 即时推送时间；为空表示还没推（比如正处于免打扰）
    ResolvedAt time.Time
}
```

- 存放在现有的 state 文件里（`internal/state`），加载时兼容没有这个字段的旧文件。
- `resolved` 的项保留 7 天后清理。
- 以后 CPA-buddy 的 MCP（方案 c）可以把它直接作为 `pending_list` 工具开放出去。

## 5. 开关与配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ALERT_MODE` | `actionable` | 设为 `legacy` 时完全使用旧行为（包括旧的汇总），用于出问题时回退。稳定后删除 |
| `CHANNEL_UNAVAILABLE_AFTER` | `30m` | 最早恢复时间晚于这个时长才推 |
| `MONITOR_DOWN_AFTER` | `30m` | 持续拿不到数据超过这个时长才推 |
| `OMC_OUTAGE_PENDING_AFTER` | `2h` | OMC 中断超过这个时长才列入待处理 |
| `QUIET_HOURS` | `23:00-08:00` | 设为空表示关闭 |
| `SUMMARY_TIGHT_REMAINING` | `20` | 判定"渠道紧张"的剩余百分比 |

现有的 `QUOTA_THRESHOLD_*`、`ANOMALY_*`、`STALE_AFTER_FAILURES` 保留：它们仍然决定状态的判定，只是不再直接触发推送。每个配置都要补进 README、`.env.example`、`docker-compose.yml`。

## 6. 实现结构

在 `evaluate` 产生 `[]Alert` **之后**加一层推送策略，尽量不改 evaluate 内部的判定：

1. **`internal/notify/policy`（新）**：输入本轮的告警和报告，输出三类结果：即时推送、待处理项的新增或关闭、丢弃。`ALERT_MODE=legacy` 时原样透传。渠道整体不可用和监控停摆的判定也放在这里，基于报告里每个渠道的账号状态和最后一次成功采集的时间。
2. **`internal/state`**：增加待处理项和免打扰暂存队列，处理持久化与兼容。
3. **`internal/app`**：
   - 推送前判断是否处于免打扰时段。
   - 免打扰结束时把暂存的事件发出。
   - 汇总前判断是否需要发送。
   - 新模式下关闭"超过 90% 就进入 3 分钟快速轮询"。不推送了，加快轮询没有意义，还会增加上游查询。如果实现时发现快速轮询还有别的用途，先报告。
4. **`internal/notify/render`**：新的汇总卡片和新的即时卡片（凭据失效、渠道不可用、监控停摆、免打扰结束的合并消息）。视觉由 @designer 负责。
5. **配置和文档**：`internal/config` 和第 5 节列出的文件。

## 7. 测试要求

- policy 的表驱动测试：每种旧事件在新模式下的去处；legacy 模式原样透传。
- 渠道不可用：全部用尽且恢复晚于 30 分钟时推送；恢复早于 30 分钟时不推；只有模型级窗口用尽时不推；全部禁用时不推；同一次故障不重复推。
- 监控停摆：抖动不推；持续超过 30 分钟推一次；恢复后关闭；之后再次发生时再推。
- 免打扰：跨午夜判定；期间攒下；结束时只发仍未解决的；全部已恢复时不发；重启后暂存不丢。
- 汇总：没有待处理也不紧张时跳过；有待处理时发送；渠道紧张时发送；免打扰期间跳过。
- 待处理项：ID 稳定；重启不重复推；解决后关闭；7 天后清理；旧 state 文件能正常加载。
- 验证命令：`gofmt`、`go vet ./...`、`go test ./...`。

## 8. 依赖与顺序

- `feat/omc-quota-source` 已合入 `origin/main`，依赖已满足。`internal/collect` 的 `FallbackCollector` 现在直接实现 policy 的 `OutageSource`：连续失败即视为 OMC 中断，`QUOTA_SOURCE=omc` 时由 `cmd/cpa-manager/main.go` 注入。中断（连续失败或处于回退）持续超过 `OMC_OUTAGE_PENDING_AFTER` 打开 `omc_outage` 待处理项（不即时推，只进汇总），恢复后关闭；中断开始时间持久化到 state（`omc_outage_since`），跨重启不重置。回退期间数据仍在更新，不会触发 `monitor_down`。
- 监控停摆改成按渠道「最后一次拿到任何有效数据的时间」计时，计时点持久化在 state（`channel_tracks`），重启不重置。整个采集器失败（provider `Error`、零快照，例如 CPA 管理接口或 Ollama 整体不可达）以前不计时、不推送，现在计入；从未成功过的渠道从首次尝试时间开始计；渠道全部禁用或未配置不算。
- 和 `internal/predict` 没有代码依赖。

### 8.1 已接受的偏差

实现与本文档之间有三处已确认的偏差，均按现状保留：

1. `bootstrap_summary` 在免打扰期间照发。§2.4 说「所有即时推送都先攒着，不例外」，但启动确认是部署成功信号、一次性事件，实现中它与问题告警分开，免打扰也直接发出（见 `internal/app/app.go` 的 `splitBootstrap`）。
2. 汇总卡片没有独立的「查询」按钮，复用现有的 CPA 链接按钮。交互式查询留给 CPA 页面或 agent，本期不新增按钮。
3. 仅因凭据失效导致的渠道整体不可用不另推 `channel_unavailable`：这种情况已由每个账号的 `credential_invalid` 覆盖，`channel_unavailable` 只在存在非鉴权原因（账号级耗尽或冷却）时触发。

## 9. 不在本期范围

- 预测耗尽提醒；"可能不够用"和"建议用哪个账号"。
- agent 接管待处理项、卡片上的处理按钮、CPA-buddy MCP。
- 按级别或渠道分到不同的群。
- 周报。
