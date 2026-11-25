-- Per-user RPM / TPM counters for the generation endpoints.
--
-- Duplicate of rateLimitCommitLuaSrc in internal/biz/rate_limit.go. Kept here
-- for ops review / external tooling and MUST stay in sync (same convention as
-- quota.lua and concurrency.lua).
--
-- Protocol (see RateLimitGate for the full rationale):
--
--   peek    read-only, before dispatch. Plain MGET on the Go side, no script:
--           nothing is mutated so there is nothing to make atomic.
--   commit  +1 request, AFTER an upstream accepted the request. Upstream 5xx
--           and connection failures therefore cost the caller nothing.
--   settle  += output tokens, after the response completed.
--
-- commit and settle are the same script with different KEYS/ARGV; there is no
-- pre-deduction of tokens, so no rollback path exists to get wrong.
--
-- Why Lua at all: INCRBY and EXPIRE as two separate commands can leave a
-- counter with no TTL when the second one is lost, which turns a 60s window
-- into a permanent one.
--
-- Keys (window = unix_seconds / 60):
--   rl:rpm:u:<user_id>:<tier>:<window>
--   rl:tpm:u:<user_id>:<tier>:<window>
-- where <tier> is one of  ovr | bal | plan:<plan_id>  so that moving a user
-- between tiers does not inherit the previous tier's counters.
--
-- KEYS[1] = counter key
-- ARGV[1] = increment (1 for commit, billed token count for settle)
-- ARGV[2] = ttl seconds (2 windows, so a settle landing just past the
--           boundary still finds the key it is topping up)
--
-- Returns: the counter value after the increment.

local n = redis.call('INCRBY', KEYS[1], tonumber(ARGV[1]))
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return n
