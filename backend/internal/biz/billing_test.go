package biz

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func tieredTestModel(tiers []entschema.PriceTier) *ent.Model {
	return &ent.Model{
		Name:                 "test-model",
		BillingMode:          entmodel.BillingModePerToken,
		InputUnitPrice:       dec("1"),   // per 1K
		OutputUnitPrice:      dec("10"),  // per 1K
		CacheReadInputPrice:  dec("0.1"), // per 1K
		CacheWriteInputPrice: dec("2"),   // per 1K
		Currency:             "CNY",
		PriceTiers:           tiers,
	}
}

func TestRegionalModelPriceAppliesGroupMultiplier(t *testing.T) {
	price := regionalModelPrice(tieredTestModel(nil), 0.2)
	if price.InputPer1K != "0.2" || price.OutputPer1K != "2" || price.CacheReadPer1K != "0.02" || price.CacheWritePer1K != "0.4" {
		t.Fatalf("regional price = %#v", price)
	}
}

func TestCalcCostFlatPricing(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 2}
	m := tieredTestModel(nil)

	// 1000 in ×1 + 1000 cached ×0.1 + 1000 write ×2 + 100 out ×10/1000
	base, final, err := svc.CalcCost(m, group, TokenUsage{
		TokensIn: 1000, CachedTokensIn: 1000, CacheCreationTokensIn: 1000, TokensOut: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("4.1"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
	if want := decimal.RequireFromString("8.2"); !final.Equal(want) {
		t.Fatalf("final = %s, want %s", final, want)
	}
}

func TestCalcCostImageTokenPricing(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)
	m.ImageInputUnitPrice = dec("5")
	m.ImageOutputUnitPrice = dec("20")

	base, _, err := svc.CalcCost(m, group, TokenUsage{
		TokensIn: 1000, TokensOut: 1000, ImageTokensIn: 2000, ImageTokensOut: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// text: 1 + 10, image: 2k*5/1k + 3k*20/1k
	if want := decimal.RequireFromString("81"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

func TestCalcCostImageTokenPricingFallsBackToTextPrices(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)

	base, _, err := svc.CalcCost(m, group, TokenUsage{ImageTokensIn: 2000, ImageTokensOut: 3000})
	if err != nil {
		t.Fatal(err)
	}
	// image prices unset: fall back to input=1/output=10.
	if want := decimal.RequireFromString("32"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

func TestCalcCostAudioTokenPricing(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)
	m.AudioInputUnitPrice = dec("8")
	m.AudioOutputUnitPrice = dec("80")

	base, _, err := svc.CalcCost(m, group, TokenUsage{
		TokensIn: 1000, TokensOut: 1000, AudioTokensIn: 2000, AudioTokensOut: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// text: 1 + 10, audio: 2k*8/1k + 3k*80/1k
	if want := decimal.RequireFromString("267"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

// An unpriced audio modality must bill at the text rate rather than free —
// every model row that predates these columns has them nil.
func TestCalcCostAudioTokenPricingFallsBackToTextPrices(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)

	base, _, err := svc.CalcCost(m, group, TokenUsage{AudioTokensIn: 2000, AudioTokensOut: 3000})
	if err != nil {
		t.Fatal(err)
	}
	// audio prices unset: fall back to input=1/output=10.
	if want := decimal.RequireFromString("32"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

// Audio input is prompt context, so it has to count toward tier selection —
// otherwise a long voice conversation stays on the cheapest band forever.
func TestAudioTokensCountTowardPriceTier(t *testing.T) {
	u := TokenUsage{TokensIn: 100, AudioTokensIn: 500}
	if got := u.InputContextTokens(); got != 600 {
		t.Fatalf("InputContextTokens = %d, want 600", got)
	}
}

func TestCalcCostTierSelection(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel([]entschema.PriceTier{
		{UpToTokens: 200_000, InputUnitPrice: dec("1"), OutputUnitPrice: dec("10")},
		{UpToTokens: 0, InputUnitPrice: dec("2"), OutputUnitPrice: dec("15")},
	})

	// Below the 200k bound: tier 1 prices.
	base, _, err := svc.CalcCost(m, group, TokenUsage{TokensIn: 100_000, TokensOut: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("110"); !base.Equal(want) { // 100k×1/1k + 1k×10/1k
		t.Fatalf("small-context base = %s, want %s", base, want)
	}

	// Above the bound: unbounded tier prices.
	base, _, err = svc.CalcCost(m, group, TokenUsage{TokensIn: 300_000, TokensOut: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("615"); !base.Equal(want) { // 300k×2/1k + 1k×15/1k
		t.Fatalf("large-context base = %s, want %s", base, want)
	}

	// Exactly at the bound (inclusive): still tier 1. Cache tokens count
	// toward the context size.
	base, _, err = svc.CalcCost(m, group, TokenUsage{TokensIn: 100_000, CachedTokensIn: 100_000, TokensOut: 0})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("110"); !base.Equal(want) { // 100k×1/1k + 100k×0.1/1k (cache read falls back to base column)
		t.Fatalf("boundary base = %s, want %s", base, want)
	}
}

func TestCalcCostTierFallbacks(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	// Tier overrides ONLY input price; output/cache fall back to base
	// columns; base cache columns unset → cache prices fall back to the
	// tier-effective input price.
	m := tieredTestModel([]entschema.PriceTier{
		{UpToTokens: 0, InputUnitPrice: dec("5")},
	})
	m.CacheReadInputPrice = nil
	m.CacheWriteInputPrice = nil

	base, _, err := svc.CalcCost(m, group, TokenUsage{
		TokensIn: 1000, CachedTokensIn: 1000, CacheCreationTokensIn: 1000, TokensOut: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 5 + 5 (cache read → tier input) + 5 (cache write → tier input) + 10 (base output)
	if want := decimal.RequireFromString("25"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}

	// Context beyond every bounded tier and no unbounded tier → flat base prices.
	m2 := tieredTestModel([]entschema.PriceTier{
		{UpToTokens: 1000, InputUnitPrice: dec("99")},
	})
	base, _, err = svc.CalcCost(m2, group, TokenUsage{TokensIn: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("2"); !base.Equal(want) { // 2000×1/1k base input
		t.Fatalf("no-tier-match base = %s, want %s", base, want)
	}
}

func TestApplyCacheWriteEstimate(t *testing.T) {
	// Min-context guard off (0) for the core reclassify cases.
	on := &ent.UpstreamAccount{EstimateCacheWrite: true}
	off := &ent.UpstreamAccount{EstimateCacheWrite: false}

	// Reclassifies the non-cached input remainder; read untouched;
	// input-side total (tier selection basis) preserved.
	u := TokenUsage{TokensIn: 5000, CachedTokensIn: 90000, TokensOut: 300}
	got, est := ApplyCacheWriteEstimate(on, u)
	if !est || got.TokensIn != 0 || got.CacheCreationTokensIn != 5000 ||
		got.CachedTokensIn != 90000 || got.TokensOut != 300 {
		t.Fatalf("reclassify failed: est=%v %+v", est, got)
	}
	if got.InputContextTokens() != u.InputContextTokens() {
		t.Fatalf("tier basis changed: %d != %d", got.InputContextTokens(), u.InputContextTokens())
	}

	// Upstream-reported write always wins — never overwritten.
	u = TokenUsage{TokensIn: 100, CachedTokensIn: 500, CacheCreationTokensIn: 200}
	if got, est := ApplyCacheWriteEstimate(on, u); est || got != u {
		t.Fatalf("real write must win: est=%v %+v", est, got)
	}

	// First turn / no cache read: full input reclassified (matches
	// Anthropic first-turn cache_creation billing).
	u = TokenUsage{TokensIn: 40000, TokensOut: 10}
	got, est = ApplyCacheWriteEstimate(on, u)
	if !est || got.TokensIn != 0 || got.CacheCreationTokensIn != 40000 {
		t.Fatalf("first-turn estimate failed: est=%v %+v", est, got)
	}

	// Nothing to reclassify.
	u = TokenUsage{TokensIn: 0, CachedTokensIn: 1000}
	if got, est := ApplyCacheWriteEstimate(on, u); est || got != u {
		t.Fatalf("zero input must not estimate: est=%v %+v", est, got)
	}

	// Flag off / nil provider: untouched.
	u = TokenUsage{TokensIn: 100}
	if got, est := ApplyCacheWriteEstimate(off, u); est || got != u {
		t.Fatalf("flag off must not estimate: est=%v %+v", est, got)
	}
	if got, est := ApplyCacheWriteEstimate(nil, u); est || got != u {
		t.Fatalf("nil provider must not estimate: est=%v %+v", est, got)
	}

	// Min-context guard: below the threshold, a short prompt writes
	// nothing to cache upstream, so leave it as plain input.
	guarded := &ent.UpstreamAccount{EstimateCacheWrite: true, CacheWriteMinContextTokens: 1024}
	if got, est := ApplyCacheWriteEstimate(guarded, TokenUsage{TokensIn: 500}); est || got.TokensIn != 500 || got.CacheCreationTokensIn != 0 {
		t.Fatalf("below-min must not estimate: est=%v %+v", est, got)
	}
	// At the threshold (inclusive-ish: total >= min) → estimates.
	if got, est := ApplyCacheWriteEstimate(guarded, TokenUsage{TokensIn: 1024}); !est || got.TokensIn != 0 || got.CacheCreationTokensIn != 1024 {
		t.Fatalf("at-min must estimate: est=%v %+v", est, got)
	}
	// Context total counts cache-read too: 200 input + 900 read = 1100 ≥ 1024.
	if got, est := ApplyCacheWriteEstimate(guarded, TokenUsage{TokensIn: 200, CachedTokensIn: 900}); !est || got.TokensIn != 0 || got.CacheCreationTokensIn != 200 {
		t.Fatalf("context-total must include cache read: est=%v %+v", est, got)
	}

	// After the audio split, TokensIn is the text remainder. A voice first
	// turn with almost no text used to reclassify the whole prompt; leaving
	// AudioTokensIn behind bills it at the audio rate instead of cache-write.
	voice := TokenUsage{TokensIn: 80, AudioTokensIn: 3920, TokensOut: 10}
	got, est = ApplyCacheWriteEstimate(on, voice)
	if !est || got.TokensIn != 0 || got.AudioTokensIn != 0 || got.CacheCreationTokensIn != 4000 {
		t.Fatalf("audio remainder must reclassify: est=%v %+v", est, got)
	}
	if got.InputContextTokens() != voice.InputContextTokens() {
		t.Fatalf("tier basis changed after audio estimate: %d != %d", got.InputContextTokens(), voice.InputContextTokens())
	}
	// Voice-only (no text remainder) still estimates — TokensIn==0 is not "nothing".
	got, est = ApplyCacheWriteEstimate(on, TokenUsage{AudioTokensIn: 2000})
	if !est || got.AudioTokensIn != 0 || got.CacheCreationTokensIn != 2000 {
		t.Fatalf("voice-only must estimate: est=%v %+v", est, got)
	}
}

func TestCalcCostIgnoresModelCurrency(t *testing.T) {
	// Regression guard: model prices are entered directly in points
	// (1 point = 1 CNY). A model whose legacy currency column says "USD"
	// must bill EXACTLY like a CNY model — the exchange rate must never
	// leak back into the user-facing billing path.
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 2}
	usage := TokenUsage{TokensIn: 1000, CachedTokensIn: 1000, CacheCreationTokensIn: 1000, TokensOut: 100}

	cnyModel := tieredTestModel(nil)
	usdModel := tieredTestModel(nil)
	usdModel.Currency = "USD"

	baseCNY, finalCNY, err := svc.CalcCost(cnyModel, group, usage)
	if err != nil {
		t.Fatal(err)
	}
	baseUSD, finalUSD, err := svc.CalcCost(usdModel, group, usage)
	if err != nil {
		t.Fatal(err)
	}
	if !baseUSD.Equal(baseCNY) || !finalUSD.Equal(finalCNY) {
		t.Fatalf("USD-tagged model billed differently: base %s vs %s, final %s vs %s",
			baseUSD, baseCNY, finalUSD, finalCNY)
	}
}

func TestCacheWriteSplit(t *testing.T) {
	fiveM, oneH := TokenUsage{CacheCreationTokensIn: 200}.CacheWriteSplit()
	if fiveM != 200 || oneH != 0 {
		t.Fatalf("aggregate-only split = %d/%d, want 200/0", fiveM, oneH)
	}

	fiveM, oneH = TokenUsage{
		CacheCreationTokensIn: 123, CacheCreation5mTokensIn: 100, CacheCreation1hTokensIn: 23,
	}.CacheWriteSplit()
	if fiveM != 100 || oneH != 23 {
		t.Fatalf("breakdown split = %d/%d, want 100/23", fiveM, oneH)
	}

	// Nested-only parse lifts the aggregate; split still follows the
	// breakdown even if someone left the total at 0.
	fiveM, oneH = TokenUsage{CacheCreation5mTokensIn: 5, CacheCreation1hTokensIn: 8}.CacheWriteSplit()
	if fiveM != 5 || oneH != 8 {
		t.Fatalf("nested-only split = %d/%d, want 5/8", fiveM, oneH)
	}
}

func TestCalcCostCacheWrite5mAnd1h(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)
	m.CacheWrite1hInputPrice = dec("4") // 5m write is 2/1K from the fixture

	base, _, err := svc.CalcCost(m, group, TokenUsage{
		CacheCreationTokensIn:   1000,
		CacheCreation5mTokensIn: 400,
		CacheCreation1hTokensIn: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 400×2/1K + 600×4/1K = 0.8 + 2.4 = 3.2 — not the old 1000×2/1K = 2
	if want := decimal.RequireFromString("3.2"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

func TestCalcCostCacheWrite1hFallsBackTo5mPrice(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil) // 1h price unset

	base, _, err := svc.CalcCost(m, group, TokenUsage{
		CacheCreationTokensIn:   1000,
		CacheCreation5mTokensIn: 400,
		CacheCreation1hTokensIn: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both buckets at the 5m write price (2/1K) = 2
	if want := decimal.RequireFromString("2"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

func TestCalcCostCacheWriteAggregateBillsAs5m(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)
	m.CacheWrite1hInputPrice = dec("4")

	base, _, err := svc.CalcCost(m, group, TokenUsage{CacheCreationTokensIn: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// No breakdown → whole aggregate at 5m (2/1K), not the 1h rate
	if want := decimal.RequireFromString("2"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s", base, want)
	}
}

func TestNormalizeUsageLogMaterializesCacheWrite5m(t *testing.T) {
	in := normalizeUsageLogInput(WriteUsageLogInput{CacheCreationTokensIn: 200})
	if in.CacheCreation5mTokensIn != 200 || in.CacheCreation1hTokensIn != 0 {
		t.Fatalf("aggregate-only log = 5m=%d 1h=%d, want 200/0", in.CacheCreation5mTokensIn, in.CacheCreation1hTokensIn)
	}

	in = normalizeUsageLogInput(WriteUsageLogInput{
		CacheCreationTokensIn: 0, CacheCreation5mTokensIn: 5, CacheCreation1hTokensIn: 8,
	})
	if in.CacheCreationTokensIn != 13 {
		t.Fatalf("nested-only log total = %d, want 13", in.CacheCreationTokensIn)
	}
}

func TestApplyCacheWriteEstimateClassifiesAs5m(t *testing.T) {
	on := &ent.UpstreamAccount{EstimateCacheWrite: true}
	got, est := ApplyCacheWriteEstimate(on, TokenUsage{TokensIn: 5000, CachedTokensIn: 90000})
	if !est || got.CacheCreationTokensIn != 5000 || got.CacheCreation5mTokensIn != 5000 || got.CacheCreation1hTokensIn != 0 {
		t.Fatalf("estimate must land in 5m: est=%v %+v", est, got)
	}
}

func TestCalcCostWithCacheWriteEstimate(t *testing.T) {
	// End-to-end: estimation then pricing. input 1/1K, write 2/1K.
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 1}
	m := tieredTestModel(nil)
	prov := &ent.UpstreamAccount{EstimateCacheWrite: true}

	u, est := ApplyCacheWriteEstimate(prov, TokenUsage{TokensIn: 10000, CachedTokensIn: 50000})
	if !est {
		t.Fatal("expected estimation to fire")
	}
	base, _, err := svc.CalcCost(m, group, u)
	if err != nil {
		t.Fatal(err)
	}
	// 10000 tokens billed ONCE at write price (2/1K) = 20, not
	// input(10) + write(20) = 30; plus 50000 cached ×0.1/1K = 5.
	if want := decimal.RequireFromString("25"); !base.Equal(want) {
		t.Fatalf("base = %s, want %s (single-charge at write rate)", base, want)
	}
}

func TestNormalizePriceTiers(t *testing.T) {
	// Sorts ascending, unbounded last.
	tiers, err := NormalizePriceTiers([]entschema.PriceTier{
		{UpToTokens: 0, InputUnitPrice: dec("3")},
		{UpToTokens: 200_000, InputUnitPrice: dec("2")},
		{UpToTokens: 32_000, InputUnitPrice: dec("1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tiers[0].UpToTokens != 32_000 || tiers[1].UpToTokens != 200_000 || tiers[2].UpToTokens != 0 {
		t.Fatalf("unexpected tier order: %+v", tiers)
	}

	if _, err := NormalizePriceTiers([]entschema.PriceTier{
		{UpToTokens: 0, InputUnitPrice: dec("1")},
		{UpToTokens: 0, InputUnitPrice: dec("2")},
	}); err == nil {
		t.Fatal("want error for two unbounded tiers")
	}
	if _, err := NormalizePriceTiers([]entschema.PriceTier{
		{UpToTokens: 100, InputUnitPrice: dec("1")},
		{UpToTokens: 100, InputUnitPrice: dec("2")},
	}); err == nil {
		t.Fatal("want error for duplicate bounds")
	}
	if _, err := NormalizePriceTiers([]entschema.PriceTier{
		{UpToTokens: 100, InputUnitPrice: dec("-1")},
	}); err == nil {
		t.Fatal("want error for negative price")
	}
	if _, err := NormalizePriceTiers([]entschema.PriceTier{
		{UpToTokens: 100},
	}); err == nil {
		t.Fatal("want error for tier with no prices")
	}
	if out, err := NormalizePriceTiers(nil); err != nil || out != nil {
		t.Fatalf("empty input should normalize to nil, got %v, %v", out, err)
	}
}

// TestCalcCostGenerationSurcharges checks the additive per-second and
// per-character surcharges used by the non-chat generation endpoints. A
// per_request base of 0 + a unit price yields pure per-unit billing.
func TestCalcCostGenerationSurcharges(t *testing.T) {
	svc := &BillingSvc{}
	group := &ent.ModelGroup{RateMultiplier: 2}

	// Pure per-second video: base 0 + 5s × 3 = 15, ×2 mult = 30.
	video := &ent.Model{
		Name:              "veo",
		BillingMode:       entmodel.BillingModePerRequest,
		PricePerRequest:   dec("0"),
		OutputSecondPrice: dec("3"),
		Currency:          "CNY",
	}
	base, final, err := svc.CalcCost(video, group, TokenUsage{OutputSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("15"); !base.Equal(want) {
		t.Fatalf("video base = %s, want %s", base, want)
	}
	if want := decimal.RequireFromString("30"); !final.Equal(want) {
		t.Fatalf("video final = %s, want %s", final, want)
	}

	// Pure per-character TTS: base 0 + 10 chars × 0.5 = 5, ×2 = 10.
	tts := &ent.Model{
		Name:                "cosyvoice",
		BillingMode:         entmodel.BillingModePerRequest,
		PricePerRequest:     dec("0"),
		InputCharacterPrice: dec("0.5"),
		Currency:            "CNY",
	}
	base, _, err = svc.CalcCost(tts, group, TokenUsage{InputCharacters: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := decimal.RequireFromString("5"); !base.Equal(want) {
		t.Fatalf("tts base = %s, want %s", base, want)
	}

	// Chat usage (no seconds/chars) is unaffected by the new fields.
	base, _, err = svc.CalcCost(video, group, TokenUsage{})
	if err != nil {
		t.Fatal(err)
	}
	if !base.Equal(decimal.Zero) {
		t.Fatalf("no-usage base = %s, want 0", base)
	}
}
