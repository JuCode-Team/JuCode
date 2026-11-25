# 计费预留/结算（Reserve / Settle / Release）

本文档描述 `billing_holds` 预扣计费机制：状态机、产品规则、各热路径的接入方式，以及如何对账。

核心目标：新请求在余额 ≤ 0 时拒绝；余额低于 0.25 CNY 水线或不足以覆盖预估时走最后一笔（冻光 remaining，同一账户仅 1 个 `llm:`/`ws:` 在途）；已开工按实扣，余额可以变负。`CapAtHold`（取消时按可见输出重建）不因猜测用量制造欠费。任何可能产生费用的请求，在产生任何输出之前先 Reserve；结束后按实际用量结算（Settle）；未产生可计费输出则全额退回（Release）。

实现位置：

- 核心原语：`internal/biz/billing_reserve.go`
- 表结构：`internal/data/ent/schema/billing_hold.go`（表名 `billing_holds`）
- 接入点：`internal/llm/gateway.go`、`anthropic.go`、`billing_log.go`、`ws_session.go`、`video_handlers.go`
- 兜底清理（reaper）：`BillingSvc.StartHoldCleanup`，在 `cmd/jucode/main.go` 的后台 leader 中启动

## 状态机

一条 `billing_holds` 记录的生命周期：

```
                    Reserve
                       │
                       ▼
                 ┌──────────┐
                 │   held   │   资金已从 balance 冻结（扣减）
                 └────┬─────┘
        ┌─────────────┼──────────────────┐
        ▼             ▼                  ▼
  SettleReservation  ReleaseReservation  expires_at 到期
  （按实际结算）      （全额退回）         （reaper 全额退回）
        │             │                  │
        ▼             ▼                  ▼
  ┌──────────┐   ┌──────────┐      ┌──────────┐
  │ settled  │   │ released │      │ released │
  └──────────┘   └──────────┘      └──────────┘
```

- **held**：`Reserve` 在一个事务里 ① `INSERT billing_holds ... ON CONFLICT (request_key) DO NOTHING`；② 锁定账户行；③ `decideMonetaryReserve` 决定冻结额（普通路径为预估；最后一笔为全部 remaining）；④ 按该金额冻结。`available <= 0`，或最后一笔时已有其它未结算的 `llm:`/`ws:` hold，返回 `ErrInsufficientBalance`。视频 `video:` hold 不占最后一笔名额。
- **settled**：`SettleReservation(base, final, opts)` 在一个事务里锁定 hold 行与账户行，计算 `charged`：
  - `final ≤ amount`：退回差额，`charged = final`；
  - `final > amount` 且 `CapAtHold=false`：按 `final` 全额收取，差额从当前余额补收，**余额可以变负**；
  - `CapAtHold=true`（重建/估算的用量，见流取消）：`charged = min(final, amount)`，不因重建用量制造欠费。
  - `charged > 0` 时写入一条 `deduct` 台账（`balance_txes` / `organization_balance_txes`）。
  - `opts.TaskID` 非空时，同一事务内执行 `UPDATE generation_tasks SET billed=true ... WHERE id=$1 AND billed=false`；没抢到（`ErrTaskAlreadyBilled`）则**整个事务回滚**——视频"Charge 成功与 Billed=true 同一事务"。
- **released**：全额退回。对 `settled`/`released` 的重复 Settle/Release 是幂等的：重试 Settle 返回已记录的金额，不再动钱。
- **对已 released 的 hold 再 Settle**（reaper 与在途请求竞态）：走直接扣费兜底。精确用量按 `final` 全额扣（可负）；`CapAtHold` 最多扣原 hold 且钳制到当前余额 ≥ 0（重建用量不得制造欠费）。同一事务把 hold 转为 `settled`；重试只返回已记金额，不会重复扣款。

### 幂等键

`request_key` 唯一约束是请求级幂等键：

| 路径 | key |
|---|---|
| OpenAI 网关 / Anthropic | `llm:<server_uuid>` |
| Realtime WS 每轮 | `ws:<server_uuid>:turn<N>` |
| 视频提交 | `video:<generation_task_uuid>` |
| 视频结算兜底（hold 已消失时） | `video:<generation_task_uuid>:settle` |

所有 key 均由服务端生成，不能使用客户端可控的 `X-Request-Id`。重复 Reserve 相同 key：用户、组织、套餐、来源一致，hold 仍是 `held`，且 `existing.amount <= requested.amount`（最后一笔会缩小 hold）时返回同一 hold（不重复冻结）；身份不一致或已有 hold 更大返回 `ErrHoldIdentityConflict`，已结束返回 `ErrHoldFinished`。

### TTL 与崩溃安全

每条 hold 带 `expires_at`（请求 2h / WS 每轮 1h / 视频 24h）。进程崩溃后遗留的 hold 由后台 leader 的 reaper（每 5 分钟）退回。reaper 只会"提前退钱"，不会丢账：在途请求结算时发现 hold 已被回收，走已 released 的直接扣费兜底。

## 费用估算（EstimateMaxCost）

宁多预留不可少预留，结算时退差。低额度时的最后一笔规则不改变本公式。

- 输入 tokens：`请求体字节数 / 2`（对英文约 2 倍超估，覆盖 CJK），下限 64、上限 262144（防 base64 图片撑爆估算）。
- 输出 tokens：客户端 `max_tokens` / `max_completion_tokens` / `max_output_tokens`；未指定按 32768。
- 输入单价取"可能适用的最高价"（含 cache-write 价、price tiers）。
- 视频按 `seconds`（未指定按 15s）× `output_second_price`。
- 组倍率（rate_multiplier）与池限流倍率（pool throttle）均计入。
- **模型价格缺失/配置错误时估算为 0**：预留 0（不冻结资金也不阻断流量），结算阶段的 CalcCost 错误照旧上报——估算失败不惩罚用户，收费失败照常记录。

## 产品规则与实现对照

| 规则 | 实现 |
|---|---|
| 预检/新请求在余额≤0 拒绝；已开工按实扣可负；<0.25 或预估不足时最后一笔且 LLM/WS 1 在途 | 预检 `Sign() <= 0` 即拒。Reserve 锁账户后 `decideMonetaryReserve`：普通路径冻预估；最后一笔冻 remaining 且其它 `llm:`/`ws:` hold 必须为 0。`SettleReservation` 按 `final` 全额扣（可负）；`CapAtHold` 为 `min(final, hold)`。直接扣费兜底与主路径同口径 |
| 套餐配额"最后一笔可越线，之后拒绝" | plan_only（非 pay_as_you_go）返回**伪预留**（不冻结资金），Settle 走原有 Redis `ConsumeQuota`，行为不变；`ResolvePlanState` 的 PeekQuota 预检不变 |
| 预扣最大费用、多退少补（D2=C） | Reserve(估算) → Settle(实际)，见状态机 |
| 组织月/成员限额在出结果之前占用 | `reserveOrganization` 在组织行锁内按**实际冻结额**（最后一笔是 remaining，不是原预估）计算 `已结算支出 + 未结算 holds + 本次冻结 > 限额` 即拒绝；结算阶段无条件记账（超限也记，只是上报错误），堵死"输出已给、记账被拒、下次白嫖" |
| WS 每轮重新预检/预留 | 桥接模式：每个 `response.create` 重入 HTTP handler（天然带 Reserve）；直连代理模式：`realtimeMeter.gateTurn` 在帧转发**之前**Reserve，失败写错误帧、丢帧并**关闭会话**；服务端 VAD 自主开启的轮次结算后重跑预检，余额耗尽即关闭会话 |
| 流式中途断开 / 最后一跳失败按已观察 usage 结算 | `logFinal` 只在 failover 无法继续时调用。任意终态失败（499 挂断、`stream_incomplete`、idle/total timeout、`response.failed`、502 耗尽）若已观察用量：上游已报 usage 则按精确三分结算（input / cache_read / cache_write 各按各价，且对 `estimate_cache_write` 渠道跑与成功路径相同的 `ApplyCacheWriteEstimate` 改判，见 `cancelBillableUsage` / `failureShouldSettle`）；否则只按已转发可见输出重建（文本 + 已转发 tool-call 参数 ≈ 3 字节/token + 图片，`CapAtHold=true`），**不用预留阶段的输入高估当账单**，也不猜测缓存比例编造输入。没有任何可观察用量则 Release。中间 failover hop 不调用 `logFinal`，不单独结算。usage_log 落的是实际计费的 token 分解（含 cache 列与 `cache_write_estimated` 标记） |
| 视频 Charge 与 Billed 原子、可重试 | 余额扣款通过 `SettleReservation(TaskID)` 单 PG 事务；套餐扣量通过 Redis `ConsumeQuotaOnce` 幂等标记保证崩溃重试不重复扣量，扣量成功后才标 billed；结算失败任务保持非终态由 reconciler 重试；任务失败/过期 Release |
| 缺时长不得按 0 收 | `settleVideoCompletion`：按秒模型且 `seconds<=0` → 拒绝结算、`slog.Warn` 告警、任务保持 pending（billed=false、非终态），后续带时长的 poll 可正常结算；2h 后 reconciler 过期失败并退回 hold |
| 金额一律 `decimal.Decimal` | hold 金额、结算、台账均为 `numeric(20,8)`；无 float 金额 |

## 各热路径的接线

**OpenAI 网关（`gateway.go`，非流/流式）**：路由 → 访问检查 → `ResolvePlanState` 预检 → 并发闸 → 限速 peek → **Reserve** → provider 循环。终态出口：

- 成功：`chargeAndLog` → `SettleReservation`（无预留的路径回落到直接 Charge）；
- 客户端取消（499）以及其它最后一跳失败（`stream_incomplete`、timeout、`response.failed`、502 耗尽）：`logFinal` 按**已观察** usage 结算（上游已报则精确三分 + 与成功路径一致的缓存写入改判，否则仅已转发可见输出——文本、tool-call 参数、图片，`CapAtHold`）；无可观察用量则 Release。预留输入高估不进入账单。中间 failover hop 不结算；
- 未产生输出的终态拒绝（4xx 直写 usage log）：显式 `releaseReservation`。

Reserve 失败即拒绝请求（402 余额不足 / 429 限额 / 503 内部错误——**fail closed**，拿不到 hold 就不放行），并写 usage log。

**Anthropic（`anthropic.go`）**：同上，位置在请求翻译之后、provider 循环之前；`max_tokens` 直接来自请求体。

**Realtime WS 直连代理（`ws_session.go`）**：`pumpWebSockets` 的 client→upstream 方向挂 `gateTurn`。注意直连模式断开在轮次中间时无任何 usage 帧可依据，该轮 hold 由 `wait()` 全额退回（与 HTTP 流不同，HTTP 流有已转发文本可重建）。若收到终态帧却没有 usage，立即关闭会话并退回当前 hold，禁止下一轮沿用旧 hold。

**视频（`video_handlers.go`）**：提交时 Reserve（24h TTL）；提交失败/无 adaptor/任务落库失败即 Release；成功完成时 `settleVideoCompletion` 先结算再置终态（结算失败保持 running 由 reconciler 重试）；任务失败或 2h 过期时 Release。

## 如何对账

**不变量**（对每个账户）：

```
账面 balance = Σ(台账 amount) − Σ(status='held' 的 hold amount)
```

即：hold 处于 `held` 期间，资金已离开 `balance` 但尚未写台账；`settled` 时写入一条 `deduct` 台账并退回差额；`released` 时全额回到 balance。对账 SQL：

```sql
-- 个人账户偏差（应为 0 行；充值等非 deduct 台账同样计入 Σ台账）
SELECT u.id, u.balance,
       COALESCE(tx.total, 0)  AS ledger_sum,
       COALESCE(h.held, 0)    AS held_sum
FROM users u
LEFT JOIN (SELECT user_id, SUM(amount) AS total FROM balance_txes GROUP BY user_id) tx ON tx.user_id = u.id
LEFT JOIN (SELECT user_id, SUM(amount) AS held FROM billing_holds
           WHERE scope='user' AND status='held' GROUP BY user_id) h ON h.user_id = u.id
WHERE u.balance <> COALESCE(tx.total, 0) - COALESCE(h.held, 0);
```

组织账户同理（`organizations` / `organization_balance_txes` / `scope='organization'`）。

> 注意：上面的等式假设台账从账户创建起完整。历史数据若存在旧的直接改余额操作，应以「变更前的差值」为基线做增量对账。

**日常巡检**：

```sql
-- 1. 负余额来自已开工按实扣（允许）；新请求在 <=0 时会被拒绝。
--    大量并发欠费或持续膨胀说明最后一笔串行失效。
SELECT id, balance FROM users WHERE balance < 0;
SELECT id, balance FROM organizations WHERE balance < 0;

-- 2. 过期未回收的 hold（reaper 每 5 分钟跑；持续存在说明 reaper 挂了）
SELECT count(*) FROM billing_holds WHERE status='held' AND expires_at < NOW() - INTERVAL '15 minutes';

-- 3. settled 金额与台账互查：每条 settled hold 应有对应 deduct 台账
--    （note 含 request id，可按时间窗抽样核对）

-- 4. 视频 pending 告警：succeeded 前置但未计费的任务（缺时长）
SELECT id, request_id, model FROM generation_tasks
WHERE status='running' AND billed=false AND created_at < NOW() - INTERVAL '30 minutes';
```

**usage_logs 口径**：`cost_final` 记录**实际收到的钱**。精确结算写完整 `CostFinal`（余额可负，不再有钳制缺口）。`CapAtHold` 仍可能少于 ask（重建用量少收，不报欠费）。

## 已知取舍 / 残留风险

- **组织限额引入 holds 聚合**：`reserveOrganization` 在组织行锁内多一次 `SUM(billing_holds)`，高并发组织下锁竞争会加剧（与原 charge 路径同级别，均为组织行锁串行）。
- **估算过保守不再挡住小额余额用户**：未带 `max_tokens` 的请求仍按 32768 输出 tokens 估算，但低于 0.25 或预估不足时走最后一笔冻 remaining，而不是直接 402。恶意欠费由 LLM/WS 1 在途限制。
- **HTTP 流最后一跳不收未观察输入**：499 / `stream_incomplete` / timeout / `response.failed` 且上游未报 usage 时只按已转发可见输出重建（文本 + tool-call 参数 + 图片；thinking 增量不落累加器，天然不计），不再用 `body/2` 预留高估当输入账单，也不猜测 cache_read/cache_write 比例去编输入。完整回复已经下流、usage 帧尚未到达就中断时，会少收输入侧——宁可少收，也不再把失败请求显示成「0 token 仍扣款」。上游已报 usage 时与成功路径完全同口径（精确三分 + `ApplyCacheWriteEstimate`），不存在把 cache hit 按 input 全价收的路径。中间 failover hop 不结算。
- **WS 直连轮次中断不结算**（无可观察 usage），hold 全额退回——少收不超收。
- **plan-quota 视频**跨 Redis/PG 无法使用物理单事务；Redis 扣量与幂等标记在一个 Lua 中完成，PG billed 仅在扣量成功后写入。若进程在两者之间崩溃，任务保持非终态，重试命中幂等标记后只补 billed，不会重复扣量或漏扣。
- **对账基线**：库里已有的 `balance ≠ Σ台账` 偏差不会被纠正；对账前先对当时的差额做一次快照，作为基线。
