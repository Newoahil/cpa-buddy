# CPA Manager

源码仓库：[Newoahil/CPA-Manager](https://github.com/Newoahil/CPA-Manager)。

CPA 额度观察与通知 sidecar：状态页、Feishu 和可选 webhook，支持既有旧版
管理 API 与官方 v8，无需先升级 CPA。此处支持的是来源文档锚定的接口契约，
不是对所有 fork/构建或线上额度端点可用性的保证。**尚未生产联调。**

## 部署

使用 `docker-compose.yml`，通过部署环境注入 `CPA_MANAGEMENT_KEY`；不把密钥提交
到仓库。连接同一内部网络，核实服务名、端口和管理 API 是否启用。旧 fork 的
预期地址可为 `http://cli-proxy-api:8318`，必须按实际运行配置验证；示例默认
`http://cpa:8317` 并非自动发现结果。Compose 需要已存在的 `dokploy-network`。

保持单副本与 `/data` named volume：状态/outbox 单写，Feishu 长连接也是单实例。
通知渠道可选，webhook 与 Feishu 功能均保留。状态服务监听 `:8080`；通过部署平台
按需配置内部访问或路由。镜像使用 Go 1.24 构建和 distroless nonroot 运行。

| 配置 | 默认 | 可选值/意义 |
| --- | --- | --- |
| `CPA_API_VERSION` | `auto` | `auto`、`v0`、`v8` |
| `CPA_QUOTA_STRATEGY` | `auto` | `auto`、`normalized`、`proxy` |
| `ANTIGRAVITY_QUOTA_PROFILE` | `current` | `current`、`legacy`，独立于管理版本 |
| `CPA_TIMEOUT` | `20s` | 单个管理请求超时 |
| `CPA_CONTEXT_OVERRIDES_JSON` | 空 | 按 opaque Credential Key 绑定 `account_id` / `project_id`，仅 sidecar 内存 |
| `QUOTA_SOURCE` | `cpa` | `cpa` 直接查 CPA 管理 API（现状）；`omc` 改从 OMC 只读 MCP 读取已采集额度 |
| `OMC_MCP_URL` | 空 | `QUOTA_SOURCE=omc` 时必填；OMC 的 Streamable HTTP MCP 地址，如 `http://omc:8080/omc/api/mcp` |
| `OMC_MCP_KEY` | 空 | `QUOTA_SOURCE=omc` 时必填；OMC 只读 bearer key。属密钥：不记录、不渲染、不落盘，错误信息只报设置名 |
| `OMC_FALLBACK_AFTER` | `3` | OMC 连续失败达到该次数后，回退为直接查 CPA；每轮先试 OMC，恢复后自动切回 |
| `POLL_INTERVAL` | `15m` | 自动采集周期，至少一分钟 |
| `LOG_DIR` / `LOG_FILE_MAX_MB` / `LOG_FILE_KEEP` | `/data/logs` / `10` / `10` | 日志另存一份到持久卷并按大小轮转（容器日志只保留很短一段）；`LOG_DIR=off` 关闭。查看：在容器终端执行 `tail -n 200 /data/logs/cpa-manager.log` |
| `FAST_POLL_INTERVAL` | `3m` | 有账号用量超过提醒线时改用的采集周期；`0` 关闭，否则至少 `1m` |
| `QUOTA_THRESHOLD_NOTICE` / `_WARN` / `_URGENT` | `80` / `90` / `100` | 已用百分比阈值：提醒 / 告警 / 用满 |
| `COOLDOWN_POLL_INTERVAL` | `60s` | 限流冷却巡检周期，仅读取凭证列表（不请求额度/上游）；`0` 关闭，否则至少 `15s` |
| `COOLDOWN_ALERT_AFTER` | `30m` | 只有 CPA 给出的恢复时间在该时长之后才推送（超出 CPA 自身最长 30 分钟退避，基本是额度用满）；CPA 日常的短冷却一律不推送，只计入日报 |
| `ALERT_MODE` | `actionable` | `actionable` 只即时推送需要人动手的事（凭据失效、渠道整体不可用、监控停摆）；`legacy` 完整还原旧行为，仅作回退开关 |
| `CHANNEL_UNAVAILABLE_AFTER` | `30m` | 渠道（全部已启用账号不可用）最早恢复时间晚于该时长才推；仅模型/分组级耗尽、全部禁用不算 |
| `MONITOR_DOWN_AFTER` | `30m` | 按渠道「最后一次拿到任何有效数据的时间」计时（持久化，重启不重置）；整个采集器失败也计入，超过该时长推一次（替代即时的 `suspect`/`stale`）；抖动不推 |
| `OMC_OUTAGE_PENDING_AFTER` | `2h` | OMC 中断（连续失败或处于回退）超过该时长才列入「待处理」，不即时推；`QUOTA_SOURCE=omc` 时由 `FallbackCollector` 提供中断信号 |
| `QUIET_HOURS` | `23:00-08:00` | 免打扰时段（可跨午夜，按 `TZ_NAME`）；期间即时推送先攒着，结束或恢复后合并发出；设空关闭 |
| `SUMMARY_TIGHT_REMAINING` | `20` | 渠道紧张判定的剩余百分比，是定时汇总发送的条件之一 |
| `QUOTA_HISTORY_DIR` | `<STATE_PATH 所在目录>/history` | 额度历史（append-only JSONL，按 `TZ_NAME` 的日界分文件），默认 `/data/history` |
| `QUOTA_HISTORY_RETENTION` | `2160h` | 额度历史保留时长；启动与跨天时删除过期的整天文件（只按文件名日期，不动其它文件） |

限流冷却巡检读取 CPA 凭证列表里的 `cooldowns` / `next_retry_after`，每个「凭证 + 范围 + 模型」
为一个 episode。CPA 收到上游 429/5xx 后会自行短暂冷却并自动恢复，触发很频繁，**不推送**；
只有恢复时间晚于 `COOLDOWN_ALERT_AFTER` 的 episode 才发一次「额度用满」（`rate_limited`），
结束后发一次「已恢复」（`rate_limit_cleared`）。未推送的 episode 按凭证累计次数与最长时长，随下一次日报发出后清零。
episode 计时只保存在内存，重启会重新计时（重启期间已结束的告警不会补发 cleared）；被禁用或已删除
的凭证不参与。CPA 的 `status_message` 可能含上游原文，**不会**被读取、存储、记录或渲染。额度接口返回
429 仍不是结论（不判耗尽/失效），仅在快照上保留 `Code: "429"`。

默认 `ALERT_MODE=actionable` 下即时推送只保留三类：凭据失效、渠道整体不可用
（已启用账号全部不可用且最早恢复晚于 `CHANNEL_UNAVAILABLE_AFTER`）、监控停摆
（渠道连续无有效数据超过 `MONITOR_DOWN_AFTER`，一次故障只推一次）。90%/95%
阈值、单个账号用尽、刷新与恢复、短暂陈旧都不再即时推送：未解决的问题作为结构化
「待处理项」存进状态文件，随定时汇总展示，解决后自动关闭。定时汇总仅在存在待处理项
或渠道紧张（`SUMMARY_TIGHT_REMAINING`）时发送，免打扰期间跳过；`bootstrap_summary`
不变。待处理项保留 7 天（已解决），旧状态文件可直接加载。

完整配置见 `.env.example`。`QUOTA_THRESHOLDS_JSON` 可按 `codex`、`claude`、
`gemini-cli`、`antigravity`、`ollama` 覆盖阈值。

## 版本与额度策略

- `auto` 先请求 v8 credentials（`?page=1&page_size=100`，探测响应复用为第一页），必须得到有效 `files` 数组。仅 v8 **404** 才尝试
  v0 auth-files；401/403、超时、429、5xx、HTML、缺失/null files 均不会降级。
  两边 404 表示管理不可用/版本未知，也可能是路由或管理开关问题。
- 显式 v0/v8 不探测另一版本。watcher 和 collector 共用并发安全 Client。
  v8 列表最多 1000 页、每页最多 100 条，检查重复页/handle 和分页一致性；v0
  单次读取。空 files 合法，`files:[{}]` 或错误身份字段类型不锁定版本。
  失败不会发布半份凭证上下文。
- 所有管理 HTTP 请求共用串行 gate；**仅管理鉴权 401/403 使用全局负缓存**，
  至少退避 5 分钟（包含 quota/proxy 外层鉴权失败）。list 与 capability 的
  连接错误、429、5xx 分别按各自控制面端点退避，从 30 秒指数增长至最多
  15 分钟，不短路 quota/proxy。单个凭证的 quota/proxy 其他错误只影响本次
  请求，不阻断其他凭证或 Ping；调用者 context 取消/截止不写入负缓存。
- `CPA_TIMEOUT` 仅为每次 HTTP 请求的预算，每页重新计时，不包含 gate 排队。
  gate 每次等待最多 30 秒；list/capability 共享 flight 各有固定 **5 分钟整体
  上限**（含排队、版本协商和全部分页），watcher 使用同一整体上限。单次 HTTP
  预算仍受剩余整体预算约束；不新增配置。waiter 可独立提前取消，短 leader
  不会取消其他仍在等待的调用者，flight 不会无限存活。
- capability 成功和失败（包括 404、错误 schema）均按本次凭证列表 generation
  缓存，同轮凭证不会反复查询。下一次成功 List 使结果失效；失败 List 不失效。
  新 generation 的请求仍遵守尚未到期的鉴权/控制面 HTTP 退避窗口。
  旧 generation 的在途 capability flight 完成后才允许新 flight，避免堆积后台任务。
- v0 的 `auto` 走管理代理。v8 的 `auto` 严格读取 quota/providers，结合凭证
  的 quota 能力元数据/既有 probe 选择 normalized；确认无实现时才预先选择
  provider proxy。能力查询不可用或结构错误会显式报告，用户可选 `proxy` 排障。
- v8 官方统一额度接口依赖插件 QuotaProvider 或已经配置的 quota_probe，并不
  保证内置这四类适配。`normalized` 只适用于 v8；v0 返回 unsupported。
  一次 normalized 501/502 不会触发代理重试或切换到 v0。
- **normalized scope**：核实的 v8.0.3 `QuotaGroup/QuotaBucket` 只有
  `displayName`、`window`、fraction/reset/description，没有明确 account/model
  scope。合法数值作为成功 snapshot 保留，每个窗口明确标记 `scope=unknown`、
  空 `scope_id`；可显示窗口用量（包括 100%）并产生范围未知的窗口告警，但不能
  据此判定全账号耗尽或建议停用整个凭证。不从组名或窗口名推断 account/model。
  通用失败同样不臆测额度拒绝的适用范围；缺失或非法数字仍为 parse 失败。
- `proxy` 通过当前版本管理 API 转发固定 provider 请求，外层管理鉴权与上游
  `Bearer $TOKEN$` header 分离。外层 200 不代表上游成功。

代理具备 Codex、Claude、Gemini CLI、Antigravity current/legacy 的源码契约适配，
**不表示默认四家都可用**。Codex 需要列表
中明确的 ChatGPT account ID；Gemini CLI 需要 project_id 或已验证的 account
末尾 `email (project)` 格式；Antigravity 使用明确 project 元数据或以下显式覆盖。字段冲突、
缺失或不安全时不会猜测 ID、下载凭证或使用默认 project。上下文仅存于 Client
内部，不写入公开 Credential、状态文件或日志。

### 旧版 Google OAuth 必需上下文与绑定

固定旧 fork 的 Google 列表可能是 `provider="gemini"`、`account_type="oauth"`，
只有邮箱，没有 project；Antigravity 同样可能缺 project。**此时必须为每个
目标账号配置已核实的 `project_id`，否则 proxy 返回未知/解析失败并且不请求上游。**
已核实来源是旧 fork `buildAuthFileEntry` 与 `Auth.AccountInfo`（源码锚点见
`docs/compatibility-sources.md`）；只在 v0 列表且 `account_type` 明确为 `oauth`
时将 `gemini` 归一为 `gemini-cli`。`api_key` 或缺少 OAuth 标记不改分类。

1. 由账号管理员从该账号既有 OAuth onboarding/项目绑定记录核实项目；Codex
   account ID 可从管理列表的 sanitized claims 得到。不要把随便一个 Cloud
   project、邮箱、文件名、auth_index 当成额度上下文。sidecar 不下载原始凭证、
   不请求项目发现接口、不猜默认项目、不写 CPA。
2. 从 sidecar 已有 JSON 状态/状态记录的 `credential.key` 取 **opaque Key**。
   如果当前页面没有显示 Key，可以在管理员本机离线计算：
   `canonicalProvider + "/" + hex(SHA256(UTF8(canonicalProvider + ":" + trim(auth_index))))[:16]`。
   canonical provider 使用小写规范值；`anthropic` → `claude`，上面已确认的旧版
   Google OAuth → `gemini-cli`。仅在可信本地工具中从现有管理列表取 handle 输入；
   不打印、不复制到工单/日志/配置，不下载凭证。对外只传计算出的 Key。
3. 仅注入 sidecar 环境，例如下列 Key 是占位示例，必须替换为实际 opaque Key：

   ```json
   {"gemini-cli/0123456789abcdef":{"project_id":"verified-project"},"antigravity/fedcba9876543210":{"project_id":"another-verified-project"}}
   ```

   配置名是 `CPA_CONTEXT_OVERRIDES_JSON`。值只允许非空字符串 `account_id` /
   `project_id`；未知字段、重复 JSON key、控制字符和 `$TOKEN$` 拒绝。覆盖只能
   补齐或确认现有值，不能掩盖冲突或错误类型。Key 必须在本次完整列表中唯一
   对应已存在、可寻址账号；不存在或有歧义时整次列表失败，不发布部分上下文。

持久化 Key 总是按规范 provider 计算。为了迁移旧身份，额外接受从当前列表
原始 provider 大小写/旧名称计算的历史 **opaque Key**（包括已确认旧 OAuth 的
`gemini/<hash>`）；不匹配 alias 显示名、邮箱、短 ID 或文件名。多个 Key 若指向
同一账号但给出矛盾值则拒绝；任何一 Key 多目标也拒绝。provider 归一化可能使
旧状态记录产生一次身份迁移，需要主代理协调状态衔接。

配置和合成测试不代表线上验证。旧 Google OAuth 的项目绑定、权限、端点接受
及 CPA 自动刷新行为均**尚未实测**。

只有严格识别的数字窗口才表示成功；零是有效测量，缺失/null 不是零，越界
fraction 不截断为耗尽。生产路径不使用启发式百分比猜测。上游 401 表示凭证
拒绝；403 权限原因未知、429 限流及 5xx 都不会被武断认定额度耗尽或凭证过期。
v0/v8 来源分别记录为 `cpa-v0`/`cpa-v8`。

## OMC 额度来源（`QUOTA_SOURCE=omc`）

架构上 CPA 是唯一数据源，OMC 是唯一主动查询上游额度的一方，CPA-buddy 负责处理、
预警、预测与播报。设 `QUOTA_SOURCE=omc` 后，CPA-buddy 不再向 CPA 查 CPA 渠道额度，
改从 OMC 的只读 MCP 端点读取 OMC 已采集的结果；Ollama 仍由 CPA-buddy 自行采集，不变。

- 最小 MCP 客户端：JSON-RPC over HTTP POST，握手 `initialize` → `notifications/initialized`
  → `tools/call`，`Accept: application/json, text/event-stream`，Bearer 只读 key。
  优先解析 `result.structuredContent`，缺失时回退 `content[0].text` 的 JSON；
  处理 JSON-RPC error 与 `isError`；协议版本 `2025-03-26`（OMC 支持）。OMC 端为无状态
  JSON 响应，客户端缓存协商出的协议版本，不新增第三方依赖。
- 使用工具 `quota_list`：每个 item 含 `auth_index`、`provider`、`status`、`plan`、
  `windows[]`（`id`/`label`/`kind`/`scope`/`model`/`used_percent`/`reset_at_ms`/
  `period_hours`/`reset_accuracy`…）与 `observed_at_ms`。
- **身份关联**：继续用 CPA 管理接口 `ListCredentials`（非上游额度查询，冷却巡检每 60s
  已在调用）提供 `Credential.Key`/`Alias` 等身份与别名，再按 `auth_index` 关联 OMC
  的额度。CPA-buddy 的 `Credential.Key` 是 `provider + "/" + hex(SHA256(provider + ":" +
  auth_index))[:16]` 的 opaque 值，只由 provider 与 auth_index 决定，与 OMC 的
  `auth_index`（同样取自 CPA 凭证列表的 handle）一一对应；因此切换数据源不会重置状态、
  也不会让别名退化成掩码邮箱。仅处理 CPA 渠道（`codex`/`claude`/`antigravity`/
  `gemini-cli`）。
- 字段语义与现有 `CPACollector` 保持一致：`Windows` 的 `Name`/`Label`/`Scope`/`ScopeID`/
  `UsedPercent`/`ResetAt`/`WindowSeconds`、`Plan`；`FetchedAt`/`LastSuccessAt` 用 OMC 的
  `observed_at_ms`（观测时间）而非采集时刻，`Stale`/`OK`/`Failure` 由 OMC `status` 映射。
  OMC `scope` 的 `standard`→账号、`model`→模型、`group`→分组（`code_review` 记为分组）。
- OMC 中不存在的凭据，或 OMC 不支持刷新的 provider（如 Gemini CLI），按「未上报/不可用」
  处理（`unsupported`，不臆造数字），由既有引擎按缺测保留旧证据推进过期判定。
- **回退**：`QUOTA_SOURCE=omc` 时每轮先试 OMC；连续失败达 `OMC_FALLBACK_AFTER` 后，
  该轮改为直接查 CPA 并记一条明确日志/状态；OMC 恢复后自动切回。回退期间抑制全局 3 分钟
  快速轮询，改用常规 `POLL_INTERVAL`，避免直接查上游变密。回退仍复用现有 `CPACollector`，
  因此预警/通知规则、日报、history JSONL 格式都不变。连续失败（或处于回退）持续超过
  `OMC_OUTAGE_PENDING_AFTER` 时打开 `omc_outage` 待处理项（只进汇总，不即时推），恢复后关闭；
  中断开始时间持久化，跨重启不重置。

OMC 接口属于另一条线的能力（`quota_list` 及其只读 key）；实际内网地址需按部署环境提供，
本仓库不做发现。

## 行为边界与验证

sidecar 不主动改变凭证启停/策略，不安装插件、写 probe、下载原始凭证，
不读取会消费记录的 usage queue，不调用 refresh/reset/disable API。
**额度调用可能触发 CPA 内部自动 OAuth 刷新**，插件也可能有自身行为，因此不
承诺请求零副作用。旧 fork 与 v8 对各 provider 的刷新能力并不相同。
`Disabled=true` 的凭证在 collector 跳过，既不 quota/proxy 请求也不触发刷新；
`Unavailable=true` 是 CPA 的运行状态标记，仍保留探测，snapshot 的
`credential.unavailable` 明确保留该状态，不能解释为 disabled。

来源与冻结 parser 契约见 `docs/compatibility-sources.md`。测试均为合成契约
fixture/httptest，不使用现网账户或真实返回。回归入口：

```sh
go vet ./...
go test -race ./...
```

仍需部署环境确认路由、OAuth scopes、账户/项目绑定、token 刷新能力、插件
capability/schema、代理策略和上游端点/UA 接受情况。额度端点属于私有/非官方
上游协议，源码支持不等于在线服务保证。

## 许可证、来源与修改重建

本项目采用 **GNU Lesser General Public License v3.0 only**
（SPDX：`LGPL-3.0-only`）。见 [LICENSE](LICENSE)、GNU 官方完整文本
[COPYING.LESSER](COPYING.LESSER) 及其并入的 [COPYING](COPYING)。项目不提供担保。
第三方材料保留各自版权与许可，详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)
及 [`licenses/third-party/`](licenses/third-party/)。

Ollama 部分保守按 LGPL 改编处理，保留 `jacklee-code/ollama-cloud-quota-monitor`
及其上游 `Wei-Shaw/sub2api` 的来源。第三方说明中的固定提交是发布时对照锚点，
不是对实际开发时参考版本的追溯证明；本项目不暗示上游作者或服务商背书。

仓库保留完整 Go 源码、测试、固定依赖清单及 Docker/Compose 构建部署材料，允许
按上述许可修改并重新构建，包括修改 `internal/ollama` 后重建静态程序。
在所需发布提交的干净 checkout 中，使用 Go 1.24：

```sh
go mod download
go vet -mod=readonly ./...
go test -race -mod=readonly ./...
CGO_ENABLED=0 go build -mod=readonly -trimpath -o ./bin/cpa-manager ./cmd/cpa-manager
docker build --build-arg REVISION="$(git rev-parse HEAD)" -t cpa-manager:local .
```

镜像将许可和声明放在 `/usr/share/licenses/cpa-manager/`，并保留基础镜像本身的
声明。它是静态链接构建，不是可替换共享库机制。分发二进制或镜像时，应在下载处
同时提供对应提交的完整源码／构建材料及依赖源码的等效获取方式，保留许可文件，
并记录镜像 digest、Go 版本和源码 revision；不要把 `unknown` revision 的本机构建
当作已绑定源码的正式制品。`REVISION` label 和仓库链接不替代对应源码的实际提供。
基础镜像标签会变化，其实际 digest 对应的组件许可也须随制品保留和核对。
