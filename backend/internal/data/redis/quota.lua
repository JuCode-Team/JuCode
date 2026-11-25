-- Unconditional 3-window cost increment.
--
-- Overdraft is allowed by design: a single request can push `used` past
-- `quota`. BillingSvc.PreCheck (Go side) reads these counters and rejects
-- when used >= quota, so further requests are blocked. The old "refuse to
-- write when cost would exceed quota" behavior caused `used` to freeze and
-- PreCheck to keep letting requests through.
--
-- Cost is a fixed-point int64 scaled by 1e8 — the precision of every money
-- column in the schema (numeric(20,8)) — accumulated with INCRBY. It used to
-- be INCRBYFLOAT, which made these the one genuinely float-accumulated money
-- figure in the system, and they gate plan quota.
--
-- Keys written before that switch hold a decimal string, so each is rewritten
-- in place on first touch. Versioning the key namespace instead would zero
-- every live window and hand every user on a plan a fresh allowance.
--
-- Buckets number the windows since user_plans.start_at (biz.QuotaWindowsAt);
-- a restarted cycle counts from quota_anchor_at and its keys carry :a<unix ms>.
-- KEYS[1] = qta:<user_plan_id>:5h:<bucket>
-- KEYS[2] = qta:<user_plan_id>:weekly:<bucket>
-- KEYS[3] = qta:<user_plan_id>:monthly:<bucket>
-- ARGV[1] = cost (fixed-point integer string, e.g. "4050" for 0.0000405)
-- ARGV[2..4] = (unused; kept for call-site signature compatibility)
-- ARGV[5] = ttl_5h     (seconds)
-- ARGV[6] = ttl_weekly
-- ARGV[7] = ttl_monthly
--
-- Returns {used_5h, used_weekly, used_monthly} as fixed-point integers.

local function bump(key, cost, ttl)
  local cur = redis.call('GET', key)
  if cur and string.find(cur, '[.eE]') then
    local n = tonumber(cur) or 0
    local scaled = n * 1e8
    if scaled >= 0 then scaled = math.floor(scaled + 0.5) else scaled = -math.floor(-scaled + 0.5) end
    redis.call('SET', key, string.format('%d', scaled))
  end
  local v = redis.call('INCRBY', key, cost)
  redis.call('EXPIRE', key, ttl)
  return v
end

local cost = ARGV[1]
return {
  bump(KEYS[1], cost, tonumber(ARGV[5])),
  bump(KEYS[2], cost, tonumber(ARGV[6])),
  bump(KEYS[3], cost, tonumber(ARGV[7])),
}
