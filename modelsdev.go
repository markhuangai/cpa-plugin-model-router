package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	modelsDevCatalogURL      = "https://models.dev/api.json"
	modelsDevRequestTimeout  = 15 * time.Second
	modelsDevMaxResponseSize = 16 << 20
	modelsDevMaxModels       = 20_000
)

type modelsDevCatalogFetcher interface {
	fetch(context.Context) (map[string]modelsDevProvider, error)
}

type priceSyncState struct {
	mu sync.Mutex
}

type modelsDevFetcher struct {
	client *http.Client
	url    string
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Label  string                    `json:"label"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Cost         *modelsDevCost         `json:"cost"`
	Experimental *modelsDevExperimental `json:"experimental"`
}

type modelsDevExperimental struct {
	Modes map[string]modelsDevMode `json:"modes"`
}

type modelsDevMode struct {
	Cost     *modelsDevCost        `json:"cost"`
	Provider modelsDevModeProvider `json:"provider"`
}

type modelsDevModeProvider struct {
	Body modelsDevModeBody `json:"body"`
}

type modelsDevModeBody struct {
	ServiceTier string `json:"service_tier"`
}

type modelsDevCost struct {
	Input      float64             `json:"input"`
	Output     float64             `json:"output"`
	CacheRead  float64             `json:"cache_read"`
	CacheWrite float64             `json:"cache_write"`
	Tiers      []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input      float64           `json:"input"`
	Output     float64           `json:"output"`
	CacheRead  float64           `json:"cache_read"`
	CacheWrite float64           `json:"cache_write"`
	Tier       modelsDevTierKind `json:"tier"`
}

type modelsDevTierKind struct {
	Type string `json:"type"`
	Size uint64 `json:"size"`
}

type modelsDevCandidate struct {
	provider string
	model    string
	price    modelPrice
	rank     int
}

type modelsDevMatchResult struct {
	Prices    map[string]modelPrice
	Observed  int
	Matched   int
	Unmatched int
}

type syncModelPricesRequest struct {
	Source       string             `json:"source"`
	Revision     uint64             `json:"revision"`
	Models       []string           `json:"models"`
	SyncSettings *priceSyncSettings `json:"sync_settings,omitempty"`
}

func (plugin *modelRouterPlugin) syncModelsDev(request syncModelPricesRequest) (modelPriceBook, error) {
	if plugin.priceSync == nil {
		plugin.priceSync = &priceSyncState{}
	}
	if !plugin.priceSync.mu.TryLock() {
		return modelPriceBook{}, errors.New("model price synchronization is already running")
	}
	defer plugin.priceSync.mu.Unlock()
	if plugin.store == nil {
		return modelPriceBook{}, errors.New("usage storage is not initialized")
	}
	book, err := plugin.store.QueryPriceBook()
	if err != nil {
		return modelPriceBook{}, err
	}
	if book.Revision != request.Revision {
		return modelPriceBook{}, errPriceRevisionConflict
	}
	settings := book.SyncSettings
	if request.SyncSettings != nil {
		settings, err = normalizePriceSyncSettings(*request.SyncSettings)
		if err != nil {
			return modelPriceBook{}, err
		}
	}
	models, err := normalizeSyncModels(request.Models)
	if err != nil {
		return modelPriceBook{}, err
	}
	fetcher := plugin.modelsDevFetcher
	if fetcher == nil {
		fetcher = newModelsDevFetcher()
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevRequestTimeout)
	defer cancel()
	catalog, err := fetcher.fetch(ctx)
	if err != nil {
		return modelPriceBook{}, err
	}
	now := time.Now().UTC()
	matched, err := matchModelsDevPrices(catalog, models, settings, now)
	if err != nil {
		return modelPriceBook{}, err
	}
	metadata := priceSyncMetadata{Source: priceSourceModelsDev, CompletedAt: now, Observed: matched.Observed, Matched: matched.Matched, Unmatched: matched.Unmatched}
	return plugin.store.ApplyPriceSync(matched.Prices, settings, metadata, request.Revision)
}

func normalizeSyncModels(input []string) ([]string, error) {
	if len(input) > maxModelPriceEntries {
		return nil, fmt.Errorf("model synchronization must contain at most %d models", maxModelPriceEntries)
	}
	seen := make(map[string]struct{}, len(input))
	models := make([]string, 0, len(input))
	for _, raw := range input {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if !utf8.ValidString(model) || utf8.RuneCountInString(model) > 256 {
			return nil, fmt.Errorf("synchronized model name %q is invalid or too long", model)
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("model synchronization requires at least one CPA model")
	}
	sort.Strings(models)
	return models, nil
}

func newModelsDevFetcher() *modelsDevFetcher {
	expected, _ := url.Parse(modelsDevCatalogURL)
	client := &http.Client{
		Timeout: modelsDevRequestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("models.dev redirected too many times")
			}
			if request.URL.Scheme != "https" || !strings.EqualFold(request.URL.Host, expected.Host) {
				return errors.New("models.dev redirect must remain on the HTTPS models.dev host")
			}
			return nil
		},
	}
	return &modelsDevFetcher{client: client, url: modelsDevCatalogURL}
}

func (fetcher *modelsDevFetcher) fetch(ctx context.Context) (map[string]modelsDevProvider, error) {
	if fetcher == nil || fetcher.client == nil {
		return nil, errors.New("models.dev HTTP client is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fetcher.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create models.dev request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cpa-plugin-model-router/"+pluginVersion)
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models.dev catalog: HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("fetch models.dev catalog: expected application/json response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, modelsDevMaxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read models.dev catalog: %w", err)
	}
	if len(body) > modelsDevMaxResponseSize {
		return nil, fmt.Errorf("read models.dev catalog: response exceeds %d bytes", modelsDevMaxResponseSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var catalog map[string]modelsDevProvider
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("decode models.dev catalog: trailing JSON value")
	}
	if len(catalog) == 0 {
		return nil, errors.New("decode models.dev catalog: no providers found")
	}
	count := 0
	for _, provider := range catalog {
		count += len(provider.Models)
		if count > modelsDevMaxModels {
			return nil, fmt.Errorf("decode models.dev catalog: more than %d models", modelsDevMaxModels)
		}
	}
	return catalog, nil
}

func matchModelsDevPrices(catalog map[string]modelsDevProvider, observed []string, settings priceSyncSettings, now time.Time) (modelsDevMatchResult, error) {
	normalizedSettings, err := normalizePriceSyncSettings(settings)
	if err != nil {
		return modelsDevMatchResult{}, err
	}
	priority := make(map[string]int, len(normalizedSettings.ProviderPriority))
	for rank, provider := range normalizedSettings.ProviderPriority {
		priority[provider] = rank
	}
	candidates := make(map[string]modelsDevCandidate)
	for providerKey, provider := range catalog {
		providerName := firstNonEmpty(provider.ID, provider.Name, provider.Label, providerKey)
		normalizedProvider := normalizeCatalogName(providerName)
		rank, prioritized := priority[normalizedProvider]
		if !prioritized {
			rank = len(priority)
		}
		for modelKey, model := range provider.Models {
			if model.Cost == nil {
				continue
			}
			catalogModel := firstNonEmpty(model.ID, modelKey, model.Name)
			comparison := comparisonModelName(catalogModel, normalizedSettings)
			if comparison == "" {
				continue
			}
			candidate := modelsDevCandidate{provider: normalizedProvider, model: catalogModel, price: modelPriceFromModelsDev(model, normalizedProvider, catalogModel, now), rank: rank}
			current, exists := candidates[comparison]
			if !exists || candidateLess(candidate, current) {
				candidates[comparison] = candidate
			}
		}
	}
	result := modelsDevMatchResult{Prices: make(map[string]modelPrice), Observed: len(observed)}
	for _, model := range observed {
		candidate, ok := candidates[comparisonModelName(model, normalizedSettings)]
		if !ok {
			result.Unmatched++
			continue
		}
		result.Prices[model] = candidate.price
		result.Matched++
	}
	result.Prices, err = normalizeModelPrices(result.Prices, now)
	return result, err
}

func modelPriceFromModelsDev(catalog modelsDevModel, provider, model string, now time.Time) modelPrice {
	cost := *catalog.Cost
	price := modelPrice{
		tokenRates:      tokenRates{Input: cost.Input, Output: cost.Output, CacheRead: cost.CacheRead, CacheCreation: cost.CacheWrite},
		Source:          priceSourceModelsDev,
		CatalogProvider: provider,
		CatalogModel:    model,
		UpdatedAt:       now.UTC(),
	}
	price.ContextTiers = contextTiersFromModelsDev(cost)
	if catalog.Experimental != nil {
		names := make([]string, 0, len(catalog.Experimental.Modes))
		for name := range catalog.Experimental.Modes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			mode := catalog.Experimental.Modes[name]
			tier := strings.ToLower(strings.TrimSpace(mode.Provider.Body.ServiceTier))
			if mode.Cost == nil || tier == "" {
				continue
			}
			if price.ServiceTiers == nil {
				price.ServiceTiers = make(map[string]serviceTierPrice)
			}
			if _, exists := price.ServiceTiers[tier]; exists {
				continue
			}
			price.ServiceTiers[tier] = serviceTierPrice{
				tokenRates:   tokenRates{Input: mode.Cost.Input, Output: mode.Cost.Output, CacheRead: mode.Cost.CacheRead, CacheCreation: mode.Cost.CacheWrite},
				ContextTiers: contextTiersFromModelsDev(*mode.Cost),
			}
		}
	}
	return price
}

func contextTiersFromModelsDev(cost modelsDevCost) []contextPriceTier {
	tiers := make([]contextPriceTier, 0, len(cost.Tiers))
	for _, tier := range cost.Tiers {
		if tier.Tier.Type == "context" && tier.Tier.Size > 0 {
			tiers = append(tiers, contextPriceTier{Threshold: tier.Tier.Size, tokenRates: tokenRates{Input: tier.Input, Output: tier.Output, CacheRead: tier.CacheRead, CacheCreation: tier.CacheWrite}})
		}
	}
	return tiers
}

func candidateLess(left, right modelsDevCandidate) bool {
	if left.rank != right.rank {
		return left.rank < right.rank
	}
	if left.provider != right.provider {
		return left.provider < right.provider
	}
	return left.model < right.model
}
