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
	if !cost.Priced || cost.BillableInputTokens != 75 || cost.ContextTokens != 100 || !near(cost.TotalUSD, .00012875) {
		t.Fatalf("base cost = %#v", cost)
	}
	record.InputTokens = 130
	cost = estimateUsageCost(record, resolver)
	if cost.TierThreshold != 100 || !near(cost.InputUSD, .00105) {
		t.Fatalf("context-tier cost = %#v", cost)
	}
	record.ServiceTier = "priority"
	cost = estimateUsageCost(record, resolver)
	if cost.PriceServiceTier != "priority" || cost.TierThreshold != 0 || !near(cost.InputUSD, .000315) {
		t.Fatalf("service-tier cost = %#v", cost)
	}
}

func TestModelPriceResolverMappingsAndAmbiguity(t *testing.T) {
	settings := defaultPriceSyncSettings()
	settings.Mappings = []priceSyncMapping{{Source: "gpt-5.4-latest", Target: "gpt-5.4"}}
	resolver := newModelPriceResolver(map[string]modelPrice{"gpt-5.4": {tokenRates: tokenRates{Input: 1}}}, settings)
	if price, ok := resolver.resolve("openai/gpt-5.4-latest(high)"); !ok || price.Input != 1 {
		t.Fatalf("mapped price = %#v, %v", price, ok)
	}
	resolver = newModelPriceResolver(map[string]modelPrice{
		"openai/gpt-5.4": {tokenRates: tokenRates{Input: 1}},
		"other/gpt-5.4":  {tokenRates: tokenRates{Input: 2}},
	}, settings)
	if _, ok := resolver.resolve("gpt-5.4"); ok {
		t.Fatal("ambiguous normalized prices must not resolve")
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

func near(left, right float64) bool {
	return math.Abs(left-right) < 1e-12
}
