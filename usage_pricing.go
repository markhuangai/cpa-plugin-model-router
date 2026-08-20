package main

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	accountingModeInputExcludesCache = "input_excludes_cache"
	accountingModeInputIncludesCache = "input_includes_cache"
	priceSourceManual                = "manual"
	priceSourceModelsDev             = "models.dev"
	maxModelPriceEntries             = 10_000
	maxContextPriceTiers             = 16
	maxServiceTierPrices             = 16
	maxTokenPricePerMillion          = 1_000_000.0
)

type tokenRates struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
}

type contextPriceTier struct {
	Threshold uint64 `json:"threshold"`
	tokenRates
}

type serviceTierPrice struct {
	tokenRates
	ContextTiers []contextPriceTier `json:"context_tiers,omitempty"`
}

type modelPrice struct {
	tokenRates
	ContextTiers    []contextPriceTier          `json:"context_tiers,omitempty"`
	ServiceTiers    map[string]serviceTierPrice `json:"service_tiers,omitempty"`
	AccountingMode  string                      `json:"accounting_mode,omitempty"`
	Source          string                      `json:"source,omitempty"`
	CatalogProvider string                      `json:"catalog_provider,omitempty"`
	CatalogModel    string                      `json:"catalog_model,omitempty"`
	UpdatedAt       time.Time                   `json:"updated_at,omitempty"`
}

type priceSyncMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type priceSyncSettings struct {
	ProviderPriority []string           `json:"provider_priority"`
	IgnoredSuffixes  []string           `json:"ignored_suffixes"`
	Mappings         []priceSyncMapping `json:"mappings"`
}

type priceSyncMetadata struct {
	Source        string    `json:"source"`
	CompletedAt   time.Time `json:"completed_at"`
	Observed      int       `json:"observed"`
	Matched       int       `json:"matched"`
	Created       int       `json:"created"`
	Updated       int       `json:"updated"`
	SkippedManual int       `json:"skipped_manual"`
	Unmatched     int       `json:"unmatched"`
}

type modelPriceBook struct {
	SchemaVersion uint32                `json:"schema_version"`
	Revision      uint64                `json:"revision"`
	Prices        map[string]modelPrice `json:"prices"`
	SyncSettings  priceSyncSettings     `json:"sync_settings"`
	LastSync      *priceSyncMetadata    `json:"last_sync,omitempty"`
}

type saveModelPricesRequest struct {
	Revision     uint64                `json:"revision"`
	Prices       map[string]modelPrice `json:"prices"`
	SyncSettings priceSyncSettings     `json:"sync_settings"`
}

type estimatedCost struct {
	Priced                bool    `json:"priced"`
	Source                string  `json:"source,omitempty"`
	AccountingMode        string  `json:"accounting_mode,omitempty"`
	PriceServiceTier      string  `json:"price_service_tier,omitempty"`
	TierThreshold         uint64  `json:"tier_threshold,omitempty"`
	ContextTokens         uint64  `json:"context_tokens,omitempty"`
	BillableInputTokens   uint64  `json:"billable_input_tokens,omitempty"`
	BilledCacheReadTokens uint64  `json:"billed_cache_read_tokens,omitempty"`
	InputUSD              float64 `json:"input_usd"`
	OutputUSD             float64 `json:"output_usd"`
	CacheReadUSD          float64 `json:"cache_read_usd"`
	CacheCreationUSD      float64 `json:"cache_creation_usd"`
	TotalUSD              float64 `json:"total_usd"`
}

func defaultPriceSyncSettings() priceSyncSettings {
	return priceSyncSettings{
		ProviderPriority: []string{"openai", "google", "anthropic"},
		IgnoredSuffixes: []string{
			"-thinking", "-preview", "-xhigh", "-high", "-low", "(thinking)", "(xhigh)", "(high)", "(low)",
		},
	}
}

func emptyModelPriceBook() modelPriceBook {
	return modelPriceBook{SchemaVersion: usageSchemaVersion, Prices: map[string]modelPrice{}, SyncSettings: defaultPriceSyncSettings()}
}

func normalizePriceSyncSettings(input priceSyncSettings) (priceSyncSettings, error) {
	defaults := defaultPriceSyncSettings()
	result := priceSyncSettings{}
	result.ProviderPriority = normalizeUniqueStrings(input.ProviderPriority, defaults.ProviderPriority, normalizeCatalogName)
	result.IgnoredSuffixes = normalizeUniqueStrings(input.IgnoredSuffixes, defaults.IgnoredSuffixes, func(value string) string {
		return strings.ToLower(strings.TrimSpace(value))
	})
	if len(input.Mappings) > maxModelPriceEntries {
		return priceSyncSettings{}, fmt.Errorf("model price mappings must contain at most %d entries", maxModelPriceEntries)
	}
	seen := make(map[string]struct{}, len(input.Mappings))
	for _, mapping := range input.Mappings {
		source, target := normalizeCatalogName(mapping.Source), normalizeCatalogName(mapping.Target)
		if source == "" || target == "" {
			return priceSyncSettings{}, fmt.Errorf("model price mappings require non-empty source and target")
		}
		key := source + "\x00" + target
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result.Mappings = append(result.Mappings, priceSyncMapping{Source: source, Target: target})
	}
	return result, nil
}

func normalizeUniqueStrings(input, fallback []string, normalize func(string) string) []string {
	usedFallback := len(input) == 0
	if len(input) == 0 {
		input = fallback
	}
	result := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		value := normalize(raw)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 && len(fallback) > 0 && !usedFallback {
		return normalizeUniqueStrings(fallback, nil, normalize)
	}
	return result
}

func normalizeModelPrices(input map[string]modelPrice, now time.Time) (map[string]modelPrice, error) {
	if len(input) > maxModelPriceEntries {
		return nil, fmt.Errorf("model prices must contain at most %d entries", maxModelPriceEntries)
	}
	result := make(map[string]modelPrice, len(input))
	seen := make(map[string]string, len(input))
	for rawModel, rawPrice := range input {
		model := strings.TrimSpace(rawModel)
		if model == "" || !utf8.ValidString(model) || utf8.RuneCountInString(model) > 256 {
			return nil, fmt.Errorf("model price name %q is invalid or too long", rawModel)
		}
		key := routeKey(model)
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("model price names %q and %q are duplicates after normalization", previous, rawModel)
		}
		seen[key] = rawModel
		price, err := normalizeModelPrice(model, rawPrice, now)
		if err != nil {
			return nil, err
		}
		result[model] = price
	}
	return result, nil
}

func normalizeModelPrice(model string, price modelPrice, now time.Time) (modelPrice, error) {
	if err := validateRates(model, price.tokenRates); err != nil {
		return modelPrice{}, err
	}
	mode := strings.TrimSpace(price.AccountingMode)
	if mode != "" && mode != accountingModeInputExcludesCache && mode != accountingModeInputIncludesCache {
		return modelPrice{}, fmt.Errorf("model price %q has unsupported accounting_mode", model)
	}
	contextTiers, err := normalizeContextTiers(model, price.ContextTiers)
	if err != nil {
		return modelPrice{}, err
	}
	if len(price.ServiceTiers) > maxServiceTierPrices {
		return modelPrice{}, fmt.Errorf("model price %q has more than %d service tiers", model, maxServiceTierPrices)
	}
	serviceTiers := make(map[string]serviceTierPrice, len(price.ServiceTiers))
	serviceTierNames := make(map[string]string, len(price.ServiceTiers))
	for rawTier, schedule := range price.ServiceTiers {
		tier := strings.ToLower(strings.TrimSpace(rawTier))
		if tier == "" || utf8.RuneCountInString(tier) > 128 {
			return modelPrice{}, fmt.Errorf("model price %q has an invalid service tier", model)
		}
		if previous, exists := serviceTierNames[tier]; exists {
			return modelPrice{}, fmt.Errorf("model price %q service tiers %q and %q are duplicates after normalization", model, previous, rawTier)
		}
		serviceTierNames[tier] = rawTier
		if err := validateRates(model+" service tier "+tier, schedule.tokenRates); err != nil {
			return modelPrice{}, err
		}
		tiers, err := normalizeContextTiers(model+" service tier "+tier, schedule.ContextTiers)
		if err != nil {
			return modelPrice{}, err
		}
		schedule.ContextTiers = tiers
		serviceTiers[tier] = schedule
	}
	source := strings.TrimSpace(price.Source)
	if source == "" {
		source = priceSourceManual
	}
	if source != priceSourceManual && source != priceSourceModelsDev {
		return modelPrice{}, fmt.Errorf("model price %q has unsupported source", model)
	}
	price.ContextTiers = contextTiers
	price.ServiceTiers = serviceTiers
	price.AccountingMode = mode
	price.Source = source
	price.CatalogProvider = strings.TrimSpace(price.CatalogProvider)
	price.CatalogModel = strings.TrimSpace(price.CatalogModel)
	if price.UpdatedAt.IsZero() {
		price.UpdatedAt = now.UTC()
	} else {
		price.UpdatedAt = price.UpdatedAt.UTC()
	}
	return price, nil
}

func normalizeContextTiers(model string, tiers []contextPriceTier) ([]contextPriceTier, error) {
	if len(tiers) > maxContextPriceTiers {
		return nil, fmt.Errorf("model price %q has more than %d context tiers", model, maxContextPriceTiers)
	}
	result := append([]contextPriceTier(nil), tiers...)
	sort.Slice(result, func(left, right int) bool { return result[left].Threshold < result[right].Threshold })
	for index, tier := range result {
		if tier.Threshold == 0 || (index > 0 && tier.Threshold == result[index-1].Threshold) {
			return nil, fmt.Errorf("model price %q has an invalid context threshold", model)
		}
		if err := validateRates(model+" context tier", tier.tokenRates); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func validateRates(model string, rates tokenRates) error {
	for name, value := range map[string]float64{"input": rates.Input, "output": rates.Output, "cache_read": rates.CacheRead, "cache_creation": rates.CacheCreation} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxTokenPricePerMillion {
			return fmt.Errorf("model price %q %s must be between 0 and %.0f", model, name, maxTokenPricePerMillion)
		}
	}
	return nil
}

type resolvedModelPrice struct {
	price     modelPrice
	ambiguous bool
}

type modelPriceResolver struct {
	exact      map[string]modelPrice
	settings   priceSyncSettings
	normalized map[string]resolvedModelPrice
}

func newModelPriceResolver(prices map[string]modelPrice, settings priceSyncSettings) modelPriceResolver {
	resolver := modelPriceResolver{exact: prices}
	normalizedSettings, err := normalizePriceSyncSettings(settings)
	if err != nil {
		return resolver
	}
	resolver.settings = normalizedSettings
	resolver.normalized = make(map[string]resolvedModelPrice, len(prices))
	keys := make([]string, 0, len(prices))
	for key := range prices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		comparison := comparisonModelName(key, normalizedSettings)
		current, exists := resolver.normalized[comparison]
		if !exists {
			resolver.normalized[comparison] = resolvedModelPrice{price: prices[key]}
		} else if !sameBillablePrice(current.price, prices[key]) {
			current.ambiguous = true
			resolver.normalized[comparison] = current
		}
	}
	return resolver
}

func (resolver modelPriceResolver) resolve(model string) (modelPrice, bool) {
	if price, ok := resolver.exact[model]; ok {
		return price, true
	}
	for key, price := range resolver.exact {
		if strings.EqualFold(key, model) {
			return price, true
		}
	}
	resolved, ok := resolver.normalized[comparisonModelName(model, resolver.settings)]
	return resolved.price, ok && !resolved.ambiguous
}

func sameBillablePrice(left, right modelPrice) bool {
	return reflect.DeepEqual(left.tokenRates, right.tokenRates) &&
		reflect.DeepEqual(left.ContextTiers, right.ContextTiers) &&
		reflect.DeepEqual(left.ServiceTiers, right.ServiceTiers) &&
		left.AccountingMode == right.AccountingMode
}

func comparisonModelName(value string, settings priceSyncSettings) string {
	value = normalizeCatalogName(value)
	value = stripIgnoredModelSuffixes(value, settings.IgnoredSuffixes)
	for _, mapping := range settings.Mappings {
		if value == mapping.Source {
			value = mapping.Target
			break
		}
	}
	return stripIgnoredModelSuffixes(value, settings.IgnoredSuffixes)
}

func stripIgnoredModelSuffixes(value string, suffixes []string) string {
	for {
		previous := value
		for _, suffix := range suffixes {
			if strings.HasSuffix(value, suffix) {
				value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
				break
			}
		}
		if value == previous {
			return value
		}
	}
}

func normalizeCatalogName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(value, "/")
	return strings.TrimSpace(parts[len(parts)-1])
}

func estimateUsageCost(record storedUsageRecord, resolver modelPriceResolver) estimatedCost {
	price, ok := resolver.resolve(record.ProviderModel)
	if !ok {
		return estimatedCost{}
	}
	cacheRead := record.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = record.CachedTokens
	}
	cacheCreation := record.CacheCreationTokens
	mode := price.AccountingMode
	if mode == "" {
		mode = defaultAccountingMode(record.Provider, record.ExecutorType)
	}
	billableInput, contextTokens := record.InputTokens, record.InputTokens
	if mode == accountingModeInputIncludesCache {
		cacheTokens := saturatingAdd(cacheRead, cacheCreation)
		if billableInput > cacheTokens {
			billableInput -= cacheTokens
		} else {
			billableInput = 0
		}
		contextTokens = saturatingAdd(billableInput, cacheTokens)
	} else {
		contextTokens = saturatingAdd(record.InputTokens, saturatingAdd(cacheRead, cacheCreation))
	}
	rates, tiers := price.tokenRates, price.ContextTiers
	priceServiceTier := ""
	serviceTier := strings.ToLower(strings.TrimSpace(record.ServiceTier))
	if schedule, exists := price.ServiceTiers[serviceTier]; exists {
		rates, tiers, priceServiceTier = schedule.tokenRates, schedule.ContextTiers, serviceTier
	}
	selectedThreshold := uint64(0)
	for _, tier := range tiers {
		if contextTokens > tier.Threshold && tier.Threshold >= selectedThreshold {
			rates, selectedThreshold = tier.tokenRates, tier.Threshold
		}
	}
	result := estimatedCost{
		Priced:                true,
		Source:                price.Source,
		AccountingMode:        mode,
		PriceServiceTier:      priceServiceTier,
		TierThreshold:         selectedThreshold,
		ContextTokens:         contextTokens,
		BillableInputTokens:   billableInput,
		BilledCacheReadTokens: cacheRead,
		InputUSD:              tokenCostUSD(billableInput, rates.Input),
		OutputUSD:             tokenCostUSD(record.OutputTokens, rates.Output),
		CacheReadUSD:          tokenCostUSD(cacheRead, rates.CacheRead),
		CacheCreationUSD:      tokenCostUSD(cacheCreation, rates.CacheCreation),
	}
	result.TotalUSD = result.InputUSD + result.OutputUSD + result.CacheReadUSD + result.CacheCreationUSD
	return result
}

func defaultAccountingMode(provider, executor string) string {
	if equalFold(provider, "anthropic") || equalFold(executor, "claude") {
		return accountingModeInputExcludesCache
	}
	return accountingModeInputIncludesCache
}

func tokenCostUSD(tokens uint64, perMillion float64) float64 {
	return float64(tokens) * perMillion / 1_000_000
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}
