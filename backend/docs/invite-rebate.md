# 邀请返利（Invite / Referral Rebates）

邀请码拉新 + 两类奖励，全部发给**邀请人**，被邀请人没有注册礼金。
功能**默认关闭**，金额/比例默认 0：部署本身不改变任何行为，需管理员在
`/v1/admin/settings` 的 `invite_program` 中显式配置并启用。

邀请的结算表与现有代理/分销（agent）体系独立，但两种渠道的奖励互斥，见下节。

## 规则

### 与代理归属互斥

- 被邀请人已有有效代理归属时，不给邀请人发拉新奖励，也不为其后续用量创建
  消费返利；代理侧销售与分成仍按原逻辑入账。
- 代理归属不单独存列，以用户成功兑换过代理码的不可变记录
  （`redeem_records → redeem_codes.agent_id IS NOT NULL`）为准；代理码或代理
  资料之后的状态变化不抹除已发生的归因。
- 先有代理归属再填邀请码：保留邀请绑定作为审计记录，但不创建拉新奖励，
  也不创建消费返利。
- 先有邀请绑定、之后兑换代理码：从代理码兑换事务提交起，不再支付尚未发放的
  拉新奖励，不再创建或支付邀请消费返利。此前已经支付的邀请奖励/返利不追回。
- 自动发放与管理员手工 `pay` 都执行同一互斥守卫；与代理码兑换通过用户行锁
  串行化，事务提交顺序决定互斥生效边界。

### 拉新奖励（signup reward）

- 固定金额（`signup_reward_amount`），绑定成功后按达标条件发放。
- 达标条件（`signup_condition`，绑定时快照，后续改配置不影响已生成的奖励）：
  - `none`：绑定即达标
  - `email_verified`：被邀请人完成邮箱验证
  - `any_topup`：被邀请人个人 `topup` 类流水合计 > 0（站内订单、管理员充值、
    兑换码、套餐初始余额等，凡 kind=topup 均计入）
  - `min_topup`：上述合计 ≥ `signup_min_topup_amount`
- 程序未启用时绑定**不生成**奖励行；之后启用不追溯补发。

### 消费返利（consume rebate）

- 被邀请人后续**真实消费**的百分比（`consume_rebate_rate_pct`，下单时快照）。
- 仅限个人余额扣费（`billing_source=balance`）与按量套餐余额扣费
  （`plan_balance`）；**组织账本**（organization_balance）与**固定套餐额度**
  （plan 配额）消费不返利。
- 上限：`consume_rebate_cap_per_invitee`（单个被邀请人终身）、
  `consume_rebate_daily_cap_per_inviter`（邀请人每日），0 = 不限；触顶片段
  截断、超出跳过。`consume_rebate_window_days` 限定绑定后可返利的天数，0 = 不限。

### 状态机

- 奖励（InviteReward）：`pending → qualified → paid`；风控命中转 `held`；
  管理员可 `pay`（qualified/held/pending）、`reject`（未支付状态）、随绑定
  `void`。重复 pay / reject 幂等。
- 返利（InviteRebate）：无风控直接 `pending → paid`（自动）；风控命中建为
  `held` 等待管理员处理。

## 数据模型

- `users.invite_code`：8 位大写字母数字（排除 0/O/1/I），唯一，
  注册时/首次访问 `/v1/me/invite` 时懒生成，冲突自动重试。
- `users.invited_by_user_id`：绑定后回填（索引）。
- `invite_bindings`：invitee 唯一 → 不可重复绑定；记录注册 IP/UA 供风控。
- `invite_rewards`：`binding_id` 唯一 → 每绑定至多一条奖励。
- `invite_rebates`：`source_usage_log_id` 唯一 → **幂等锚点**，同一笔用量
  重复投递只产生一条返利。
- `balance_txes.kind` 新增 `invite_reward`、`invite_rebate`（ent 在 Postgres
  上以 varchar 存储 enum，加值为**增量变更**，无需重写历史数据；跑一次
  `jucode-migrate` 建新表/新列即可）。

## 配置（site_settings key `invite_program`，JSON）

```json
{
  "enabled": false,
  "signup_reward_enabled": false,
  "signup_reward_amount": "0",
  "signup_condition": "email_verified",
  "signup_min_topup_amount": "0",
  "consume_rebate_enabled": false,
  "consume_rebate_rate_pct": "0",
  "consume_rebate_window_days": 0,
  "consume_rebate_cap_per_invitee": "0",
  "consume_rebate_daily_cap_per_inviter": "0",
  "signup_daily_count_cap_per_inviter": 10,
  "signup_daily_amount_cap_per_inviter": "0",
  "inviter_min_age_hours": 24,
  "same_ip_daily_cap": 3,
  "auto_hold_on_risk": true,
  "require_inviter_email_verified": true,
  "review_amount_threshold": "0"
}
```

更新校验（`NormalizeInviteProgram`）：比例 0–100、金额/上限 ≥ 0、条件枚举
合法，否则整体拒绝（金额配置错误就是真金白银，不做静默钳制）。

**公开子集**：`GET /v1/public/site-info` 仅暴露
`enabled / signup_reward_enabled / signup_reward_amount / signup_condition /
signup_min_topup_amount / consume_rebate_enabled / consume_rebate_rate_pct`，
风控阈值与上限字段一律不出（避免给刷单者提供说明书）。管理端
`GET/PUT /v1/admin/settings` 读写完整对象。

## HTTP API

### 用户侧

- `POST /v1/auth/register`：新增可选 `invite_code`。**校验在创建账号之前**：
  格式不合法返回 `400 invite_code_malformed`，查无此码返回
  `400 invite_code_unknown`，两种都不会建号。校验发生在邮箱验证码核销
  之前，避免「码用掉一半再被拒」逼用户重收验证码。空码不校验，照常注册。
  码属于被封禁账号仍绑定，风控在发放时冻结（保留证据链）。
  拒绝而不是静默忽略的原因：`invite_bindings.invitee_user_id` 唯一且只在
  注册时写入，被丢弃的邀请码事后**无法补绑**，静默失败会让邀请人永久失去
  归属（2026-09 客服案例）。
- `GET /v1/auth/invite-code/check?code=`：公开，注册页失焦时预检，恒返
  200，`{"valid":true}` 或 `{"valid":false,"reason":"malformed|unknown"}`。
  走的 `/v1/auth` 分组，已有 IP 限流（30 次/60 秒）。
- `GET /v1/me/invite`（登录）：`invite_code`、`invite_url`
  （`{site_url}/register?code=...`）、`program`（公开子集）、`stats`
  （invited/qualified/pending/held 计数、拉新与返利已发放合计）、
  `invitees` / `rewards` / `rebates` 列表（`limit`（默认 20）/`offset`
  分页，`total` 为受邀人数；邮箱一律脱敏 `a***@example.com`）。

### 管理侧（全部经过 AdminAudit 中间件自动落审计日志）

- `GET /v1/admin/invite/bindings?q=&status=&inviter_id=&invitee_id=&limit=&offset=`
- `GET /v1/admin/invite/rewards?status=&inviter_id=&invitee_id=&limit=&offset=`
- `GET /v1/admin/invite/rebates?status=&inviter_id=&invitee_id=&limit=&offset=`
- `POST /v1/admin/invite/rewards/:id/pay`（body 可选 `{"note":""}`）
- `POST /v1/admin/invite/rewards/:id/reject`
- `POST /v1/admin/invite/rebates/:id/pay`
- `POST /v1/admin/invite/rebates/:id/reject`
- `POST /v1/admin/invite/bindings/:id/void`：作废绑定并作废未支付的奖励/
  返利，**不追回**已支付部分；作废后不再产生新返利。

## 风控

`auto_hold_on_risk=true` 时命中任一项转 `held`（标签写入 `risk_flags`）：

| 标签 | 条件 |
| --- | --- |
| `inviter_not_active` | 邀请人状态非 active |
| `inviter_frozen` | 邀请人处于冻结期 |
| `inviter_account_too_new` | 邀请人账龄 < `inviter_min_age_hours` |
| `inviter_email_unverified` | `require_inviter_email_verified` 且邀请人邮箱未验证 |
| `invitee_ip_matches_inviter` | 被邀请人注册 IP 与邀请人自己被邀请时的注册 IP 相同（自邀模式） |
| `same_ip_daily_cap_exceeded` | 同邀请人+同注册 IP 当日绑定数超 `same_ip_daily_cap`（超出部分冻结） |
| `daily_signup_count_cap_exceeded` | 邀请人当日已支付/已达标拉新数超 `signup_daily_count_cap_per_inviter` |
| `daily_signup_amount_cap_exceeded` | 邀请人当日拉新金额超 `signup_daily_amount_cap_per_inviter` |

**单笔金额人工复核**（`review_amount_threshold`，标签
`amount_over_threshold`）：单笔拉新奖励或单笔消费返利金额 ≥ 阈值时一律转
`held`，等待管理员 pay/reject；0 = 关闭。这是资金控制而非风控启发式——
**不受 `auto_hold_on_risk` 开关影响**，且**不截断金额**（不是上限，只强制
人工放行）。

硬性支付守卫（无论风控开关）：绑定已作废、被邀请人 banned/rejected、
邀请人非 active、或被邀请人已有代理归属时一律拒付（保持 held/原状态供人工复核）。
风控解除后重新评估（如邮箱补验证）可自动转付。
"当日"按 Asia/Shanghai 财务日历日计。

## 挂钩点（hooks）

所有异步入口（`EvaluateSignupAsync` / `ProcessRebateAsync`）nil-safe 且
fire-and-forget：邀请侧任何失败**不会**失败注册、充值或扣费主流程。
唯一例外是注册时对 `invite_code` 的前置校验（`ResolveInviter`）——它是
同步且会拒绝注册的，理由见上文「用户侧」。

| 事件 | 挂钩位置 | 动作 |
| --- | --- | --- |
| 注册 | `Account.Register` | `ResolveInviter` 先校验 `invite_code`（无效即拒绝）；懒生成邀请码；`BindOnRegister`（绑定 + 建奖励 + 首次评估） |
| 邮箱验证 | `Account.ConsumeRegisterVerification` / `ConsumeEmailChange` | 异步重评估 |
| 订单支付 | `FinanceSvc.MarkOrderPaid`（支付回调与管理员标记同走此函数）提交后 | 异步重评估 |
| 管理员充值 | admin `POST /users/:id/topup` 提交后 | 异步重评估 |
| 兑换码 | `RedeemSvc.Redeem` 提交后 | 异步重评估 |
| 管理员审批 | admin `POST /users/:id/approve` | 异步重评估 |
| 网关扣费 | `BillingSvc.WriteUsageLog`（同步与批量写共用入口；`RebateEligibleUsage` 先廉价过滤） | 异步返利 |

返利幂等实现：usage_log id 在 `normalizeUsageLogInput` 中**预分配**
（`WriteUsageLogInput.LogID`），返利行以其为唯一键，批量写迟延落库不影响
幂等性。返利上限计算与行创建通过邀请人行锁串行化；奖励状态变更也先锁定
奖励行，避免并发评估/支付/驳回覆盖终态或突破上限。热路径成本 =
一次函数调用 + 命中时一次 `go` 派发。

## v1 已知限制

- **退款不追回**：已支付的拉新奖励与消费返利在被邀请人退款后不 clawback。
- 被邀请人无注册礼金。
- 批量写模式下返利行可能先于其引用的 usage_log 落库（约一个 flush 间隔，
  `source_usage_log_id` 是普通唯一列而非外键，语义不受影响）；极端情况下
  批量缓冲丢弃（`ErrUsageLogDropped` 已另有告警）会导致该笔返利同样缺失，
  与用量日志的丢失语义一致。
- 邀请人风控依赖绑定记录中的注册 IP；没有独立的登录 IP 画像。
- 客服/管理员手工调账（`cs adjustBalance`）不允许直接使用
  `invite_reward`/`invite_rebate` kind——人工发放必须走邀请管理端点，
  保证台账可对账。
- 程序开关不追溯：关闭期间的绑定永远不补奖励；关闭后已有 pending 奖励
  冻结在原状态（不发放、不作废）。

## 测试

`internal/biz/invite_test.go`，与 usage-writer 测试同一 opt-in 模式：

```bash
docker run -d --rm -e POSTGRES_PASSWORD=test -e POSTGRES_DB=jucode_test \
  -p 5499:5432 --name jucode-pg-test postgres:16-alpine
JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
  go test ./internal/biz -run 'Invite|RebateEligible'
```

覆盖：注册绑码、无效码拒绝注册（格式错/查无此码，且不建号、不核销验证码）、
`ResolveInviter` 解析、重复绑定、三类达标条件、同 IP 与每日数量上限冻结、
邀请人风控冻结、单笔金额复核阈值（0 不冻结、达标冻结且不截断、低于阈值
沿用既有风控、人工放行全额入账，奖励与返利两侧）、停用不发放/不追溯、
返利幂等、返利上限截断、org/套餐配额不返利（`RebateEligibleUsage`
纯函数表驱动）、邀请与代理归属互斥（先代理/先邀请/无代理/不追回已发）、
管理员 pay/reject/void、配置校验与公开字段脱敏。
