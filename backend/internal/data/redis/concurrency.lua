-- Per-user in-flight slot counters.
--
-- Duplicate of concurrencyAcquireLuaSrc / concurrencyReleaseLuaSrc in
-- internal/biz/concurrency.go. Kept here for ops review / external tooling
-- and MUST stay in sync (same convention as quota.lua).
--
-- Why Lua instead of plain INCR/EXPIRE/DECR:
--
--   1. INCR and EXPIRE were separate commands. An INCR that landed while the
--      EXPIRE failed left a counter with no TTL at all.
--
--   2. release() used to DECR unconditionally. The counter TTL was a flat 5
--      minutes, shorter than stream_total_timeout_seconds (default 600s), so
--      a long stream could outlive its own counter. The DECR then ran against
--      a missing key, Redis created it at -1, and that user permanently held
--      one extra slot. The deficit accumulated across occurrences.
--
-- Key: concur:u:<user_id>:ovr
--      concur:u:<user_id>:bal
--      concur:u:<user_id>:plan:<plan_id>


-- ============================ acquire ============================
--
-- KEYS[1] = counter key
-- ARGV[1] = limit (max concurrent slots)
-- ARGV[2] = ttl (seconds; derived from stream_total_timeout_seconds + grace)
--
-- Returns: 0 when rejected (already at cap, counter left untouched),
--          otherwise the caller's slot number (1..limit).

local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])

local n = redis.call('INCR', KEYS[1])
if n < 1 then
  -- Counter was negative: legacy damage from the release-on-expired-key bug.
  -- Self-heal to a correct single holder rather than carrying the deficit.
  redis.call('SET', KEYS[1], 1)
  n = 1
end
redis.call('EXPIRE', KEYS[1], ttl)

if n > limit then
  redis.call('DECR', KEYS[1])
  return 0
end
return n


-- ============================ release ============================
--
-- KEYS[1] = counter key
--
-- Returns: remaining holders after this release (0 when the key was already
--          gone, which is the case the old unconditional DECR got wrong).

local v = redis.call('GET', KEYS[1])
if not v then
  -- Key already expired (request outlived the TTL). Nothing to give back;
  -- decrementing here is exactly what used to mint the -1 counters.
  return 0
end
local n = tonumber(v)
if n == nil or n <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
n = redis.call('DECR', KEYS[1])
if n <= 0 then
  -- Last holder left: drop the key instead of parking a 0 with a long TTL.
  redis.call('DEL', KEYS[1])
end
return n
