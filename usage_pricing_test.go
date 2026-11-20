package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestEstimateUsageCostAccountingTiersAndServiceTier(t *testing.T) {
	price := modelPrice{
		tokenRates:     tokenRates{Input: 1, Output: 2, CacheRead: .5, CacheCreation: .75},
		AccountingMode: accountingModeInputIncludesCache,
		ContextTiers:   []contextPriceTier{{Threshold: 100, tokenRates: tokenRates{Input: 10, Output: 20, CacheRead: 5, CacheCreation: 7.5}}},
		ServiceTiers: map[string]serviceTierPrice{
			"priority": {tokenRates: tokenRates{Input: 3, Output: 4, CacheRead: 1, CacheCreation: 2}},
		},
		Source: priceSourceManual,
	}
	resolver := newModelPriceResolver(map[string]modelPrice{"gpt-5.4": price}, defaultPriceSyncSettings())
	record := storedUsageRecord{ProviderModel: "gpt-5.4(high)", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 20, CacheCreationTokens: 5}
	cost := estimateUsageCost(record, resolver)
	if !cost.Priced || cost.BillableInputTokens != 75 || cost.ContextTokens != 100 || cost.TierThreshold != 0 || !near(cost.TotalUSD, .00012875) {
		t.Fatalf("base cost = %#v", cost)
	}
	record.InputTokens = 130
	cost = estimateUsageCost(record, resolver)
	if cost.TierThreshold != 100 || cost.BillableInputTokens != 105 || !near(cost.InputUSD, .00105) {
		t.Fatalf("context-tier cost = %#v", cost)
	}
	record.ServiceTier = "priority"
	cost = estimateUsageCost(record, resolver)
	if cost.PriceServiceTier != "priority" || cost.TierThreshold != 0 || !near(cost.InputUSD, .000315) {
		t.Fatalf("service-tier cost = %#v", cost)
	}
	geminiResolver := newModelPriceResolver(map[string]modelPrice{
		"gemini-model": {tokenRates: tokenRates{Output: 2}, Source: priceSourceManual},
	}, defaultPriceSyncSettings())
	geminiCost := estimateUsageCost(storedUsageRecord{ProviderModel: "gemini-model", Provider: "google", OutputTokens: 4, ReasoningTokens: 3}, geminiResolver)
	if !near(geminiCost.OutputUSD, .000014) {
		t.Fatalf("separately reported reasoning output cost = %#v", geminiCost)
	}
	creationOnly := estimateUsageCost(storedUsageRecord{ProviderModel: "gpt-5.4", InputTokens: 100, CachedTokens: 5, CacheCreationTokens: 5}, resolver)
	if creationOnly.BilledCacheReadTokens != 0 || creationOnly.CacheReadUSD != 0 || !near(creationOnly.CacheCreationUSD, .00000375) {
		t.Fatalf("creation-only cache cost = %#v", creationOnly)
	}
	legacyRead := estimateUsageCost(storedUsageRecord{ProviderModel: "gpt-5.4", InputTokens: 100, CachedTokens: 5}, resolver)
	if legacyRead.BilledCacheReadTokens != 5 || legacyRead.BillableInputTokens != 95 || !near(legacyRead.CacheReadUSD, .0000025) {
		t.Fatalf("legacy cached-only read cost = %#v", legacyRead)
	}
}

func TestUsageAccountingModesMatchCPAFamilies(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		executor   string
		accounting string
		reasoning  string
	}{
		{name: "openai", provider: "openai", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeIncluded},
		{name: "anthropic", provider: "anthropic", accounting: accountingModeInputExcludesCache, reasoning: reasoningModeIncluded},
		{name: "anthropic variant", provider: "custom-anthropic", accounting: accountingModeInputExcludesCache, reasoning: reasoningModeIncluded},
		{name: "openai compatibility precedence", provider: "anthropic", executor: "OpenAICompatExecutor", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeIncluded},
		{name: "gemini", provider: "google", executor: "GeminiExecutor", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeSeparate},
		{name: "vertex", provider: "vertex", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeSeparate},
		{name: "vertex claude", provider: "vertex", executor: "ClaudeExecutor", accounting: accountingModeInputExcludesCache, reasoning: reasoningModeIncluded},
		{name: "aistudio", provider: "aistudio", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeSeparate},
		{name: "antigravity", provider: "antigravity", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeSeparate},
		{name: "interactions", provider: "interactions", accounting: accountingModeInputIncludesCache, reasoning: reasoningModeSeparate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounting, reasoning := usageAccountingModes(test.provider, test.executor)
			if accounting != test.accounting || reasoning != test.reasoning {
				t.Fatalf("usageAccountingModes(%q, %q) = %q, %q; want %q, %q", test.provider, test.executor, accounting, reasoning, test.accounting, test.reasoning)
			}
		})
	}
}

func TestModelPriceResolverMappingsAndAmbiguity(t *testing.T) {
	settings := defaultPriceSyncSettings()
	settings.Mappings = []priceSyncMapping{{Source: "gpt-5.4-latest", Target: "gpt-5.4"}}
	resolver := newModelPriceResolver(map[string]modelPrice{"gpt-5.4": {tokenRates: tokenRates{Input: 1}}}, settings)
	record := storedUsageRecord{ProviderModel: "openai/gpt-5.4-latest(high)", PhysicalModel: "gpt-5.4-latest(high)"}
	if cost := estimateUsageCost(record, resolver); cost.priceKey != "gpt-5.4" || cost.priceDecision != "generic_price" {
		t.Fatalf("mapped price = %#v", cost)
	}
	resolver = newModelPriceResolver(map[string]modelPrice{
		"openai/gpt-5.4": {tokenRates: tokenRates{Input: 1}},
		"other/gpt-5.4":  {tokenRates: tokenRates{Input: 2}},
	}, settings)
	if cost := estimateUsageCost(storedUsageRecord{ProviderModel: "gpt-5.4"}, resolver); cost.Priced {
		t.Fatal("bare models must not borrow a scoped rate")
	}
}

func TestUsagePriceScopesPreservePhysicalPath(t *testing.T) {
	settings := defaultPriceSyncSettings()
	settings.Mappings = []priceSyncMapping{{Source: "gpt-latest", Target: "gpt"}}
	rates := func(input float64) modelPrice { return modelPrice{tokenRates: tokenRates{Input: input}} }
	resolver := newModelPriceResolver(map[string]modelPrice{
		"east/vendor/gpt": rates(2), "west/vendor/gpt": rates(5), "vendor/gpt": rates(3),
		"other/gpt-2": rates(7), "gpt": rates(19),
	}, settings)
	for _, test := range []struct {
		key, physical, priceKey, state string
		total                          float64
	}{
		{"east/vendor/gpt-latest(high)", "vendor/gpt-latest(high)", "east/vendor/gpt", "", 2},
		{"west/vendor/gpt", "vendor/gpt", "west/vendor/gpt", "", 5},
		{"north/vendor/gpt", "vendor/gpt", "vendor/gpt", "", 3},
		{"vendor/gpt", "vendor/gpt", "vendor/gpt", "", 3},
		{"north/other-vendor/gpt", "other-vendor/gpt", "", "", 0},
		{"east/gpt-2", "gpt-2", "", "", 0},
		{"gpt", "gpt", "", providerModelUnknown, 0},
	} {
		cost := estimateUsageCost(storedUsageRecord{ProviderModel: test.key, PhysicalModel: test.physical, ProviderModelState: test.state, InputTokens: 1_000_000}, resolver)
		if cost.priceKey != test.priceKey || cost.TotalUSD != test.total || cost.Priced != (test.priceKey != "") {
			t.Errorf("cost for %q = %#v, want key %q and total %v", test.key, cost, test.priceKey, test.total)
		}
	}
}

func TestNormalizePriceSyncSettingsRejectsConflictingMappingTargets(t *testing.T) {
	settings := defaultPriceSyncSettings()
	settings.Mappings = []priceSyncMapping{
		{Source: "Foo", Target: "bar"},
		{Source: "provider/foo", Target: "baz"},
	}
	if _, err := normalizePriceSyncSettings(settings); err == nil || !strings.Contains(err.Error(), "targets both") {
		t.Fatalf("normalizePriceSyncSettings() error = %v", err)
	}

	settings.Mappings[1].Target = "provider/bar"
	normalized, err := normalizePriceSyncSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Mappings) != 1 || normalized.Mappings[0] != (priceSyncMapping{Source: "foo", Target: "bar"}) {
		t.Fatalf("normalized mappings = %#v", normalized.Mappings)
	}
}

func TestNormalizeModelPricesRejectsNormalizedDuplicateNames(t *testing.T) {
	for _, prices := range []map[string]modelPrice{
		{"gpt-5": {}, " gpt-5 ": {}},
		{"GPT-5": {}, "gpt-5": {}},
	} {
		if _, err := normalizeModelPrices(prices, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("normalizeModelPrices(%#v) error = %v", prices, err)
		}
	}
}

func TestNormalizeModelPriceRejectsNormalizedDuplicateServiceTiers(t *testing.T) {
	price := modelPrice{ServiceTiers: map[string]serviceTierPrice{
		"Priority": {},
		"priority": {},
	}}
	if _, err := normalizeModelPrice("gpt-5", price, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("normalizeModelPrice() error = %v", err)
	}
}

func TestPriceBookRevisionAndManualSyncPrecedence(t *testing.T) {
	store, err := openUsageStore(t.TempDir()+"/usage.db", 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	book, err := store.SavePriceBook(saveModelPricesRequest{Prices: map[string]modelPrice{"manual": {tokenRates: tokenRates{Input: 1}}}}, now)
	if err != nil || book.Revision != 1 {
		t.Fatalf("saved book = %#v, %v", book, err)
	}
	if _, err := store.SavePriceBook(saveModelPricesRequest{Revision: 0, Prices: map[string]modelPrice{}}, now); err != errPriceRevisionConflict {
		t.Fatalf("stale revision error = %v", err)
	}
	synced, err := store.ApplyPriceSync(map[string]modelPrice{
		"manual": {tokenRates: tokenRates{Input: 9}, Source: priceSourceModelsDev},
		"synced": {tokenRates: tokenRates{Input: 2}, Source: priceSourceModelsDev},
	}, defaultPriceSyncSettings(), priceSyncMetadata{Source: priceSourceModelsDev}, book.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if synced.Prices["manual"].Input != 1 || synced.Prices["synced"].Input != 2 || synced.LastSync.SkippedManual != 1 {
		t.Fatalf("synced book = %#v", synced)
	}
}

func TestApplyPriceSyncPreservesCaseInsensitiveManualPrice(t *testing.T) {
	store, err := openUsageStore(t.TempDir()+"/usage.db", 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	book, err := store.SavePriceBook(saveModelPricesRequest{Prices: map[string]modelPrice{
		"GPT-5": {tokenRates: tokenRates{Input: 1}},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	synced, err := store.ApplyPriceSync(map[string]modelPrice{
		"gpt-5": {tokenRates: tokenRates{Input: 9}, Source: priceSourceModelsDev},
	}, defaultPriceSyncSettings(), priceSyncMetadata{Source: priceSourceModelsDev}, book.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced.Prices) != 1 || synced.Prices["GPT-5"].Input != 1 || synced.LastSync.SkippedManual != 1 {
		t.Fatalf("case-insensitive manual precedence = %#v", synced)
	}
}

func TestApplyPriceSyncRejectsNormalizedIncomingDuplicates(t *testing.T) {
	store, err := openUsageStore(t.TempDir()+"/usage.db", 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	book, err := store.QueryPriceBook()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyPriceSync(map[string]modelPrice{
		"GPT-5": {tokenRates: tokenRates{Input: 1}, Source: priceSourceModelsDev},
		"gpt-5": {tokenRates: tokenRates{Input: 2}, Source: priceSourceModelsDev},
	}, defaultPriceSyncSettings(), priceSyncMetadata{Source: priceSourceModelsDev}, book.Revision)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("normalized incoming duplicate error = %v", err)
	}
}

func near(left, right float64) bool {
	return math.Abs(left-right) < 1e-12
}
