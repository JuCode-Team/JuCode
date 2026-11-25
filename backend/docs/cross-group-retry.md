# 跨组重试（Cross-Group Retry，产品规则 D8=B）

## 行为

请求仍然**先选首选组**（既有路由逻辑不变：auto-route 按倍率升序选最便宜可用组；
「可用」的判据见 `biz.anyProviderSchedulable`——账号自身的健康状态与 Redis 运行时
冷却二者同时成立才算可用）。
只有当首选组的**所有渠道都失败**、且**尚未向客户端写出任何字节**时，网关才允许把
请求改投下一个候选组。

候选组必须同时满足：

1. **同一 `billing_source`**：plan_only 请求不会悄悄改扣余额，反之亦然。
2. **不超过该 API Key 的 `auto_route_max_rate`**（`api_keys.auto_route_max_rate`
   字段；`0` = 不设上限，与现有 `selectGroups` 首选路由语义一致）。
3. Key 已开启 `auto_route`。未开启 auto-route 的 Key 是"钉死单组"语义，且没有
   倍率上限字段可依据，保持今天的单组行为。
4. Key 的 `allowed_group_ids` 限制照常生效；`X-JuCode-Group` 显式钉组的请求
   永不跨组。

候选按倍率升序尝试（与首选路由同序）。

**尝试预算是「每组」的**：`llm.max_attempts` 是单个分组内的上游尝试次数，
换组时重置。若预算按请求计，分组渠道数只要不少于 `max_attempts`，跨组就从不
发生。
总量由 `maxGroupSwitches`（`llm/cross_group.go`，当前为 2）封顶，因此一个请求
最多落地 3 个分组、最多 `max_attempts × 3` 次上游尝试，流式请求另有总墙钟
超时兜底。

## 默认不跨组的流量

- **图像**（`/v1/images*`、chat 携带 `image_generation` 工具、`gpt-image-*` /
  `dall-e-*` 等模型名）
- **视频 / 异步 generation 任务**（`/v1/videos*`、generation task 轮询）

这些端点本身没有跨组路径；chat 上的图像类请求在网关里被显式排除
（`crossGroupRetryPermitted`）。

## 预留（hold）随换组重估

换组时用新组的倍率重新估价（`EstimateMaxCost`），然后：

| 新估价 vs 当前 hold | 动作 |
| --- | --- |
| 更便宜 / 相等 | **保持原 hold**。结算本来就按 `hold − 实际` 退差，多退即可。 |
| 更贵 | **先释放旧 hold，再按新估价重新预留**（新的服务端幂等键）。 |
| 补预留被拒（余额/限额不足） | **停止跨组**，请求以首选组的失败结束。 |

补预留走的就是普通 `Reserve`，因此低余额透支规则同样适用：新估价超过余额、
且账上没有别的在途 `llm:`/`ws:` hold 时，冻结全部余额当作"最后一笔"放行，
而不是拒绝——直连这个组本来也是这个待遇，跨组没有理由更严。余额已经归零、
另有在途请求（最后一笔不允许并行）、或组织/成员限额挡住实际冻结额时，补预留
才真的被拒；上面那行"余额/限额不足"就是这几种情况的统称。

补预留的 hold 用的仍是 `llm:` 前缀（`crossGroupHoldScope`，不是每个网关自选
的），否则最后一笔的串行化判据看不见它，第二个请求就能在旁边把同一份余额再
透支一遍。

**这里有个产品取舍要清楚**：候选组按倍率升序排，所以跨组落地的往往是比首选
组更贵的组——一个用户没选、直连路由也不会选的倍率。结算按实扣且不封顶，因此
一次跨组失败最多能把余额扣到「落地组倍率 / 首选组倍率」这个量级的负数。唯一
的暴露上限是 API Key 的 `auto_route_max_rate`，而它默认是 `0`（不限）。要收
紧就给开了 auto-route 的 Key 设一个非零默认倍率上限。

任意时刻最多只有**一个**在途 monetary hold，结算/释放永远只作用于它，因此
**不可能重复扣两份**。释放与重留之间进程崩溃只会提前退款，不会漏扣（未产出
任何输出，本来就不该计费）。plan 配额组的伪预留不含冻结金额，换组只是刷新
plan 状态。

换组还会重新执行组访问校验（`CheckAccess`）与计费预检
（`ResolvePlanState`）；并发额度与 RPM/TPM 票据按 `billing_source` 维度取值，
跨组不换 source，故沿用首选组取得的原额度/票据。

## 覆盖范围

- `/v1/chat/completions`、`/v1/responses`（含 compact）、`/anthropic/v1/messages`。
- WebSocket realtime、alpha search、图像、视频端点不参与跨组。

## 测试

`internal/biz/model_cross_group_test.go`（纯逻辑）与
`internal/llm/cross_group_test.go` + `internal/llm/cross_group_pg_test.go`
（Postgres + Redis 端到端，`JUCODE_TEST_PG_DSN` / `JUCODE_TEST_REDIS_ADDR`
选择性启用）覆盖：同 source 可切、不同 source 不可切、超倍率不可切、已写
下游字节后不可切、媒体不切，以及 hold 保持 / 补预留 / 超余额转最后一笔 /
并发在途时不够即停，且全程单 hold。
