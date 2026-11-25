# TOTP 二步验证

admin / cs / agent 账号的二步验证（RFC 6238，兼容 Google Authenticator / 1Password 等），普通用户也可以自愿绑定。本文也说明 `/internal/*` 管理端点的 token 鉴权。

## 行为总览

- **已绑定 TOTP 的账号**：登录变成两段式。密码正确后 `/v1/auth/login` 返回 `{"mfa_required": true, "mfa_token": "..."}`（不发 token、不回用户信息），前端再调 `/v1/auth/totp` 提交 6 位码或恢复码换取正式 token。
- **未绑定的 admin / cs / agent**：登录照常成功（宽限期，不锁账号），但响应和 `/v1/me` 带上 `totp_setup_required: true`，前端据此引导去设置页绑定。
- **普通用户**：不强制。schema 与代码路径对所有角色一致，强制范围由 `Account.TOTPRequiredForUser` 决定。

## 接口

### 登录

```
POST /v1/auth/login          # TOTP 账号返回 mfa_required + mfa_token
POST /v1/auth/totp           # { mfa_token, code } 或 { mfa_token, recovery_code }
```

- `mfa_token` 有效期 5 分钟、一次性；错 5 次码作废，需重新输密码。
- 错误码：`mfa_ticket_invalid`（票据过期/作废）、`totp_invalid_code`（码不对）。
- 每个 6 位码只能用一次（Redis 按 time-step 防重放）。

### 绑定 / 解绑（需登录）

```
GET  /v1/me/totp             # { enabled, confirmed_at, recovery_codes_left, setup_required }
POST /v1/me/totp/setup       # { secret, otpauth_url }，重复调用会轮换未确认的 secret
POST /v1/me/totp/confirm     # { code } → { recovery_codes: [10 个] }，明文只出现这一次
POST /v1/me/totp/disable     # { password, code }，code 可以是当前 6 位码或未用过的恢复码
```

恢复码 10 个、格式 `XXXXX-XXXXX`、一次性，库里只存 SHA-256。用户丢手机时用恢复码登录或解绑。

### 敏感操作 step-up

以下路由挂了 `RequireTOTPStepUp`：

- `POST /v1/admin/users/:id/topup`（改余额）
- `POST /v1/admin/finance/refunds/:id/approve|reject`（退款审批）
- `POST /v1/admin/redeem-codes`（发兑换码）
- `POST /v1/cs/refunds/:id/approve|reject`、`POST /v1/cs/users/:id/balance/adjust`

规则：绑定了 TOTP 的账号要在 `X-TOTP-Code` 头里带一个当前码，验一次开 10 分钟窗口；没绑定的账号直接放行（宽限期策略，靠登录引导补齐绑定）。缺码返回 401 + `totp_stepup_required`。

## /internal/* 鉴权

- `/internal/drain/*` 和 `/internal/metrics` 要求 `Authorization: Bearer $INTERNAL_API_TOKEN`。环境变量未设置时这些端点返回 503（fail closed）。
- `/internal/region/v1/*`（实验性功能，`region.enabled` 关闭时不挂载）用 `region.edges[].service_token` 鉴权，同样做常量时间比较，token 决定调用方的区域。

## 数据

`users` 表的 TOTP 列（均有默认值）：

| 列 | 说明 |
| --- | --- |
| `totp_enabled` | bool，默认 false |
| `totp_secret` | base32 secret；确认前是 pending 状态 |
| `totp_confirmed_at` | 确认时间 |
| `totp_recovery_codes` | 未使用恢复码的 SHA-256 列表 |

## 部署注意

1. 收紧宽限策略前，先让每位管理员完成绑定并保存恢复码。
2. 部署前设置 `INTERNAL_API_TOKEN`，否则 drain 和 metrics 接口不可用。
3. MFA 票据、防重放和 step-up 窗口都存在 Redis；Redis 不可用时两段式登录会失败，与 refresh token 的依赖相同。

## 测试

```
go test ./internal/pkg/totp/        # RFC 6238 官方向量 + 恢复码
JUCODE_TEST_PG_DSN=... go test ./internal/biz -run 'TOTP|RecoveryCode|StepUp'
```

覆盖：绑定/确认、错误码、防重放、恢复码一次性、票据一次性与尝试上限、未绑定 admin 的引导标记、解绑、step-up 窗口。
