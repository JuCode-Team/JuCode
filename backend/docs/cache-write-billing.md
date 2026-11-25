# 缓存写入计费与估算机制（移植规范）

本文档描述 LLM 网关的**缓存 token 计费**与**缓存写入估算**机制，覆盖所有技术细节，供其他语言/系统移植。参考实现为 Go，但下文以语言无关的伪代码 + 公式为主。

---

## 1. 核心概念：三个互斥的 token 集合

一次请求的输入侧 token 被划分为**三个不重叠的集合**，各有独立单价：

| 集合 | 含义 | 单价字段 | 典型倍率 |
|---|---|---|---|
| `input`（非缓存输入） | 本次未命中缓存、也非本次写入缓存的输入 token | `input_unit_price` | 1× |
| `cache_read`（缓存命中） | 命中已有缓存前缀的输入 token | `cache_read_input_price` | 0.1× |
| `cache_write`（缓存写入/创建） | 本次新写入缓存的输入 token | `cache_write_input_price`（5m）/ `cache_write_1h_input_price`（1h） | 5m ≈ 1.25×、1h ≈ 2×（Anthropic）；GPT 无 TTL 拆分，整段按 5m |

外加输出侧：

| 集合 | 含义 | 单价字段 |
|---|---|---|
| `output` | 输出 token | `output_unit_price` |

**关键不变量**：

```
输入侧总量 = input + cache_read + cache_write
```

三者互斥、加总等于完整 prompt 长度。**同一个 token 只属于一个集合，只按一个单价计一次费**。这是 Anthropic 与 OpenAI（GPT-5.6+）官方计费口径，也是整套机制的基石。

> ⚠️ 常见误区：认为"写入缓存的 token 既算 input 又算 write"（1× + 1.25× = 2.25×）。**错误**。官方口径是单次 1.25×——写入的 token 从 input 集合"改判"到 write 集合，不叠加。

---

## 2. 数据模型

### 2.1 每次请求解析出的 token 结构

```
TokenUsage {
    TokensIn              int  // 非缓存输入（= input 集合）
    TokensOut            int  // 输出
    CachedTokensIn        int  // 缓存命中（= cache_read 集合）
    CacheCreationTokensIn   int  // 缓存写入总量（= 5m + 1h，或上游只报了总量）
    CacheCreation5mTokensIn int  // Anthropic cache_creation.ephemeral_5m_input_tokens
    CacheCreation1hTokensIn int  // Anthropic cache_creation.ephemeral_1h_input_tokens
    OutputImages          int  // 输出图片数（可选，与本机制正交）
}
```

### 2.2 模型（Model）价格配置

```
Model {
    BillingMode          enum   // "per_token" | "per_request"
    PricePerRequest      decimal?  // per_request 模式
    InputUnitPrice       decimal?  // 每 1K token，下同
    OutputUnitPrice      decimal?
    CacheReadInputPrice  decimal?  // nil → 回退到 InputUnitPrice
    CacheWriteInputPrice   decimal?  // 5m 写入价；nil → 回退到 InputUnitPrice
    CacheWrite1hInputPrice decimal?  // 1h 写入价；nil → 回退到 CacheWriteInputPrice
    OutputImagePrice     decimal?  // 每张图，正交
    PriceTiers           []PriceTier?  // 按上下文长度阶梯计价，可选
    Currency             string    // "CNY" | "USD"，录入单位
}

PriceTier {
    UpToTokens           int64     // 上下文 token 上限（含）；0 = 无上限（兜底档）
    InputUnitPrice       decimal?  // 各价格 nil → 回退到模型基础价同名字段
    OutputUnitPrice      decimal?
    CacheReadInputPrice  decimal?
    CacheWriteInputPrice   decimal?
    CacheWrite1hInputPrice decimal?
}
```

### 2.3 渠道（Provider）配置

```
Provider {
    EstimateCacheWrite         bool  // 该上游不上报 cache_write 时，网关估算
    CacheWriteMinContextTokens int   // 估算门槛，默认 1024；0 = 不设门槛
    // ... 其他渠道字段
}
```

> **为什么开关在 Provider 而非 Model**：是否上报 `cache_write_tokens` 是**上游渠道的行为特征**，不是逻辑模型的属性。同一个模型经不同渠道路由，上报行为可能不同——所以按渠道配置。

> **`CacheWriteMinContextTokens`（最小缓存上下文）**：上游只对达到最小长度的 prompt 做缓存——**OpenAI 为 1024 tokens**，Anthropic 按模型为 1024 / 2048 / 4096。低于此长度的 prompt **根本不会产生缓存写入**，若仍估算就会把这些短请求的 input 错误地按 1.25× 收费。因此估算前加一道门槛：输入侧上下文总量低于该值时不估算。做成可配是因为不同上游/模型的下限不同。默认 1024（OpenAI 值）；设为 0 关闭该守卫（总是估算）。

### 2.4 用量日志（写库字段）

```
UsageLog {
    TokensIn, TokensOut, CachedTokensIn, CacheCreationTokensIn  int
    CacheCreation5mTokensIn, CacheCreation1hTokensIn            int  // 无明细时写库把总量落到 5m
    CacheWriteEstimated  bool  // 该行的 cache_write 是网关估算而非上游上报
    CostBase, CostFinal  decimal
    // ...
}
```

---

## 3. 从上游 usage 解析 token（区分三种格式）

不同上游的 usage 结构不同，**核心差异在于 input 字段是"总量"还是"已扣除缓存的余量"**。

### 3.1 Anthropic Messages 格式

```jsonc
"usage": {
    "input_tokens": 10,                    // 已经是非缓存部分（余量语义）
    "output_tokens": 3,
    "cache_read_input_tokens": 500,        // 独立字段
    "cache_creation_input_tokens": 200,    // 独立字段（5m+1h 合计）
    "cache_creation": {                    // TTL 拆分；缺失则整段按 5m 计
        "ephemeral_5m_input_tokens": 177,
        "ephemeral_1h_input_tokens": 23
    }
}
```

直接映射，无需相减：

```
TokensIn                = input_tokens
CachedTokensIn          = cache_read_input_tokens
CacheCreationTokensIn   = max(cache_creation_input_tokens, 5m+1h)
CacheCreation5mTokensIn = cache_creation.ephemeral_5m_input_tokens
CacheCreation1hTokensIn = cache_creation.ephemeral_1h_input_tokens
TokensOut               = output_tokens
```

嵌套对象缺失时 5m/1h 保持 0；计费把总量整段落到 5m 价。`message_delta` 里的 0 不能覆盖 `message_start` 已报的明细（按字段取 max）。

### 3.2 OpenAI Chat Completions / Responses 格式

```jsonc
// Chat
"usage": {
    "prompt_tokens": 100,                  // 总量（包含缓存部分）
    "completion_tokens": 20,
    "prompt_tokens_details": {
        "cached_tokens": 30,               // 是 prompt_tokens 的子集
        "cache_write_tokens": 50           // GPT-5.6+，也是子集
    }
}

// Responses（字段名不同）
"usage": {
    "input_tokens": 100,                   // 总量
    "output_tokens": 20,
    "input_tokens_details": {
        "cached_tokens": 30,
        "cache_write_tokens": 50
    }
}
```

**input 是总量**，需相减得出非缓存余量：

```
total      = prompt_tokens (Chat) 或 input_tokens (Responses)
cached     = *_tokens_details.cached_tokens
cacheWrite = *_tokens_details.cache_write_tokens  // GPT-5.6+ 起，缺失则 0

TokensIn              = max(0, total - cached - cacheWrite)   // 钳制防负
CachedTokensIn        = cached
CacheCreationTokensIn = cacheWrite
TokensOut            = completion_tokens 或 output_tokens
```

> `cache_write_tokens` 在实际中可能出现在三个位置，解析时都要兼容：
> 1. `usage.prompt_tokens_details.cache_write_tokens`
> 2. `usage.input_tokens_details.cache_write_tokens`
> 3. `usage.cache_write_tokens`（部分中转放在 usage 顶层，作为 prompt_tokens 的兄弟字段）
>
> 优先级：details 内 > 顶层。取第一个非零值即可。

**官方口径确认（来自 OpenAI prompt caching 文档）**：

- `cached_tokens` 与 `cache_write_tokens` **都是 `prompt_tokens` 的子集**——所以上面的相减公式成立。写入的 token 不是在 `prompt_tokens` 之外叠加，而是其中一部分改按 1.25× 计（单次，非 2.25× 叠加）。
- 单次请求里 `cached_tokens` 与 `cache_write_tokens` **通常互斥**（一次请求要么读共享前缀、要么写它）；增长型多轮会话里可能都非零（读旧前缀 + 写本轮新增）。相减公式对两者同时非零也成立。
- **最小可缓存长度 1024 tokens**：低于此长度的 prompt 不会缓存、`cache_write_tokens` 恒为 0——这条对**估算**至关重要，见 §4.3 守卫 4。
- GPT-5.6+ 缓存 TTL 固定 `30m`；`prompt_cache_key` 影响路由命中率。这些只影响"上游是否发生写入/命中"，不改变我们对已上报值的解析与计费。

### 3.3 Responses 结束帧的嵌套

流式 Responses 的最终 usage 嵌在 `response.usage` 下（多一层），解析逻辑同 3.2，只是路径不同。

### 3.4 判别逻辑（合流）

同一个解析器要同时兼容两种口径。判别方法：**出现任一 Anthropic 专属字段（`cache_read_input_tokens` / `cache_creation_input_tokens`）→ 走余量语义；否则走 OpenAI 总量语义**。

---

## 4. 缓存写入估算（核心）

### 4.1 动机

部分上游（尤其一些 GPT 系中转）**根本不上报缓存写入量**——usage 里既没有 `cache_write_tokens` 也没有 `cache_creation_input_tokens`。对这类上游，本应按缓存写入价计费的 token 全被当成普通 input，导致**少收费且口径不统一**。

估算的做法：**把未命中缓存的 input 改判为缓存写入**。因为在 OpenAI 自动缓存 / Anthropic 显式缓存机制下，"本轮没命中缓存的前缀，基本就是本轮新写入缓存的部分"。

### 4.2 算法

```
function ApplyCacheWriteEstimate(provider, usage) -> (usage, estimated):
    // 守卫 1：开关关闭 / 无渠道 → 不动
    if provider == null or not provider.EstimateCacheWrite:
        return usage, false

    // 守卫 2：上游已上报真实写入量 → 真实数据永远优先，绝不覆盖
    if usage.CacheCreationTokensIn > 0:
        return usage, false

    // 守卫 3：没有可改判的 input → 不动
    if usage.TokensIn <= 0:
        return usage, false

    // 守卫 4：输入侧上下文低于缓存下限 → 上游不会写缓存，不估算
    // （否则短请求会被按 1.25× 多收）。门槛 <= 0 表示不设。
    min = provider.CacheWriteMinContextTokens
    if min > 0 and usage.InputContextTokens() < min:
        return usage, false

    // 改判：input 集合整体移入 cache_write 集合，按 5m 价计
    usage.CacheCreationTokensIn = remainder   // TokensIn + AudioTokensIn
    usage.CacheCreation5mTokensIn = remainder
    usage.CacheCreation1hTokensIn = 0
    usage.TokensIn = 0
    usage.AudioTokensIn = 0
    return usage, true
```

### 4.3 关键设计点（移植时务必保留）

1. **改判而非叠加**：`cache_write = TokensIn`，`TokensIn = 0`。这批 token 只按 cache_write 价计一次，不叠加 input 价。与官方口径一致。

2. **真实数据永远优先**：`CacheCreationTokensIn > 0`（上游上报了真实值）时，估算必须让位，绝不覆盖。估算只填补缺失。

3. **不以 `CachedTokensIn > 0` 为前置条件**：首轮请求（或永不上报 cache_read 的上游）会把**整个 input 改判为写入**——这恰好复刻 Anthropic 在缓存开启的首轮把整个 prompt 按 `cache_creation` 计费的行为。异常情况（缓存链路没生效）也没必要给用户减免这部分。

4. **保持输入侧总量不变**：改判前后 `input + cache_read + cache_write` 不变 → 阶梯计价的选档结果（见 §5.3）不受估算影响。

5. **最小上下文门槛（关键）**：上游有最小可缓存长度（OpenAI 1024 tokens；Anthropic 按模型 1024/2048/4096）。**低于此长度的 prompt 不产生任何缓存写入**——若仍估算就会把短请求的 input 按 1.25× 多收 0.25×。所以在改判前用 `InputContextTokens()`（输入侧总量）与 `CacheWriteMinContextTokens` 比较，低于门槛不估算，保留普通 input。门槛做成 Provider 可配（默认 1024，0 = 关闭守卫）。
   > 门槛用**总量**而非余量：一旦总 prompt ≥ 下限，缓存链路就生效，非缓存余量作为写入估算是合理的；总 prompt < 下限则整个请求都不缓存。

6. **副作用**：门槛以上、估算生效时 input 恒为 0（未命中缓存的部分全被改判）。这是估算精度的固有代价——真实上报下 input 是个位数的"尾巴"（超出缓存前缀边界、或 128-token 对齐边界之外的部分），但估算无法从 usage 得知尾巴多大。误差量级为几十~几百 token，对 10 万级上下文是噪声，且方向偏向平台（多收不少收）。

### 4.4 调用时机（关键）

```
// 在计价之前、且以「实际服务本次请求的渠道」为判断依据
usage = parseUpstreamUsage(response)
usage, estimated = ApplyCacheWriteEstimate(servingProvider, usage)  // ← 用实际渠道
(base, final) = CalcCost(model, group, usage)
writeUsageLog({ ...usage, CacheWriteEstimated: estimated, CostBase: base, CostFinal: final })
```

**实现要求**：
- 估算必须在 `CalcCost` **之前**，且**计费与日志写入使用同一份改判后的 usage**（否则会出现"扣的钱和日志对不上"的 bug）。
- 用**实际服务本次请求的渠道**判断（重试/故障转移后可能不是首选渠道）。
- 不要修改上游解析器的原始状态（如流式累加器），只在一份副本上改判。
- **仅在实际计费的行上估算**。计费的行有两类：① 成功路径（`chargeAndLog`）；② 客户端提前断开（499）**且上游已上报真实 usage** 的结算路径（`cancelBillableUsage`）——两者跑同一套改判，保证断开与完成同口径（否则挂断可以躲掉缓存写入价）。其余错误路径写的日志行不计费，保留上游原始 token 形态，不估算。
- **499 且上游未报 usage 的重建路径绝不估算**：输入侧没有观察值（`TokensIn=0`），改判无从谈起；猜测缓存比例去编造输入会让失败的请求也被扣款。该路径只按已转发可见输出（文本/tool-call 参数/图片）计费。

---

## 5. 成本计算

### 5.1 per_request 模式

```
base = model.PricePerRequest    // 必须非 nil，否则报错
```

### 5.2 per_token 模式（基础公式）

单价均为**每 1K token**，故除以 1000：

```
perK(price, n) = (price == null or n == 0) ? 0 : price * n / 1000

base = perK(inputPrice,      usage.TokensIn)
     + perK(cacheReadPrice,  usage.CachedTokensIn)
     + perK(cacheWrite5mPrice, fiveM)
     + perK(cacheWrite1hPrice, oneH)
     + perK(outputPrice,     usage.TokensOut)
     + (usage.OutputImages > 0 ? OutputImagePrice * OutputImages : 0)  // 图片正交叠加
```

**缓存价格回退**：

```
cacheReadPrice  = model.CacheReadInputPrice  ?? inputPrice   // nil 回退到 input 价
cacheWrite5mPrice = model.CacheWriteInputPrice ?? inputPrice
cacheWrite1hPrice = model.CacheWrite1hInputPrice ?? cacheWrite5mPrice
(fiveM, oneH) = (5m==0 && 1h==0) ? (CacheCreationTokensIn, 0) : (5m, 1h)
```

> 回退设计的附带好处：若误开启估算但未配 cache_write 价，估算变为**成本中性**（按 input 价计，等于没改判），不会意外多收。

### 5.3 阶梯计价（可选，按上下文长度）

选档依据是**输入侧总量**（与缓存状态无关）：

```
InputContextTokens = usage.TokensIn + usage.CachedTokensIn + usage.CacheCreationTokensIn

function pickTier(tiers, ctxTokens):
    // tiers 已按 UpToTokens 升序，无上限档（UpToTokens=0）排最后
    for tier in tiers:
        if tier.UpToTokens == 0 or ctxTokens <= tier.UpToTokens:
            return tier
    return null   // 无匹配档 → 用基础价（flat pricing）
```

命中档位后，**档内非 nil 的价格覆盖基础价**，nil 的回退到基础价同名字段：

```
tier = pickTier(model.PriceTiers, InputContextTokens)
if tier != null:
    inputPrice      = tier.InputUnitPrice      ?? model.InputUnitPrice
    outputPrice     = tier.OutputUnitPrice     ?? model.OutputUnitPrice
    cacheReadPrice  = tier.CacheReadInputPrice  ?? (base cache read logic)
    cacheWritePrice   = tier.CacheWriteInputPrice ?? (base 5m cache write logic)
    cacheWrite1hPrice = tier.CacheWrite1hInputPrice ?? (base 1h cache write logic)
// 然后 cacheRead/cacheWrite 若仍为 nil，再回退到 tier-effective inputPrice
```

> 因为 §4.3 保证估算不改变 `InputContextTokens`，估算与阶梯选档**互不干扰**。

### 5.4 积分计价 + 分组倍率

平台记账单位为**积分**：1 积分 = 1 元人民币（充值 1:1），存储数值沿用历史 CNY 数值。
模型价格直接按积分录入，计费链路**不做任何币种换算**（`models.currency` 为历史遗留列，计费不读取；
汇率仅保留在渠道结算链路，`ToCNY/FromCNY` 仅供结算使用）。

```
base  = Σ(tokens × 对应单价 / 1000) (+ 出图单价 × 图片数)   // 价格即积分单价
final = base * group.RateMultiplier                         // 分组费率倍率
// （可选）再叠加动态号池限速倍率等
```

---

## 6. 估算标记与前端展示

- 写库时把 `estimated` 存入 `usage_log.cache_write_estimated`，使估算行**永久可识别**，避免账单纠纷时说不清。
- 前端展示：估算出的缓存写入值带 `~` 前缀（如 `~50K`），真实上报的不带；详情面板标注"（估算）"。原则是**估算值不冒充真实数据**。

---

## 7. 边界情况与测试用例

| 场景 | 输入 usage | 期望结果 |
|---|---|---|
| 估算改判 | `input=5000, read=90000`，开关开 | `input=0, write=5000, read=90000`，estimated=true |
| 真实值优先 | `input=100, read=500, write=200`，开关开 | 原样不变，estimated=false |
| 首轮/无 read | `input=40000`，开关开 | `input=0, write=40000`，estimated=true（复刻 Anthropic 首轮） |
| 无可改判 | `input=0, read=1000`，开关开 | 原样不变，estimated=false |
| 低于门槛 | `input=500`，门槛=1024 | 原样不变，estimated=false（短请求不缓存） |
| 达到门槛 | `input=1024`，门槛=1024 | `input=0, write=1024`，estimated=true |
| 门槛计总量 | `input=200, read=900`（合 1100≥1024），门槛=1024 | `input=0, write=200`，estimated=true |
| 开关关闭 | `input=100` | 原样不变，estimated=false |
| 计费单次性 | 改判后 `write=10000`，input价1/1K、write价2/1K | write 计 20（非 input 10+write 20=30） |
| 阶梯不受影响 | 改判前后 | `InputContextTokens` 不变 → 选档一致 |
| OpenAI 相减钳制 | `total=40, cache_write=70`（脏数据） | `input=max(0, 40-0-70)=0` |

---

## 8. 移植检查清单

- [ ] 三集合互斥语义：`input + cache_read + cache_write = 输入侧总量`
- [ ] 解析器兼容 Anthropic（余量语义）与 OpenAI（总量相减）两种口径
- [ ] `cache_write_tokens` 三个可能位置都解析，details 内优先
- [ ] 相减后 `max(0, ...)` 钳制防负（脏数据保护）
- [ ] 估算四道守卫顺序不能变：开关 → 真实值优先 → 无可改判 → 最小上下文门槛
- [ ] 最小上下文门槛用**输入侧总量**（含 cache_read）比较，非仅 input；默认 1024，可配，0=关闭
- [ ] 估算改判不叠加（`input=0`），保持 `InputContextTokens` 不变
- [ ] 估算键控在**渠道**、用**实际服务渠道**、在**计价之前**、计价与日志用**同一份 usage**
- [ ] 仅计费行估算（成功路径 + 499 带真实 usage 的结算路径），其余错误路径保留原始形态；499 无 usage 的重建路径绝不估算
- [ ] 缓存价格 nil 回退到 input 价（成本中性保护）
- [ ] 估算行落 `cache_write_estimated` 标记 + 前端 `~` 展示
- [ ] 币种归一在分组倍率之前
```
