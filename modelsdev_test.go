package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type staticModelsDevFetcher struct {
	catalog map[string]modelsDevProvider
	err     error
}

func (fetcher staticModelsDevFetcher) fetch(context.Context) (map[string]modelsDevProvider, error) {
	return fetcher.catalog, fetcher.err
}

func TestModelsDevFetcherDecodesTLSCatalog(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Accept") != "application/json" || request.Header.Get("User-Agent") == "" {
			t.Fatalf("request headers = %#v", request.Header)
		}
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"openai": map[string]any{
				"id": "openai",
				"models": map[string]any{
					"gpt-5.4": map[string]any{"id": "gpt-5.4", "cost": map[string]float64{"input": 1, "output": 2}},
				},
			},
		})
	}))
	defer server.Close()

	fetcher := &modelsDevFetcher{client: server.Client(), url: server.URL}
	catalog, err := fetcher.fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if catalog["openai"].Models["gpt-5.4"].Cost.Input != 1 || catalog["openai"].Models["gpt-5.4"].Cost.Output != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
}

func TestModelsDevSyncPersistsTiersAndPreservesManualPrices(t *testing.T) {
	store, err := openUsageStore(filepath.Join(t.TempDir(), "usage.db"), 365)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	book, err := store.SavePriceBook(saveModelPricesRequest{Prices: map[string]modelPrice{
		"manual-model": {tokenRates: tokenRates{Input: 9, Output: 10}},
	}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	catalog := map[string]modelsDevProvider{
		"other": {
			ID: "other",
			Models: map[string]modelsDevModel{
				"gpt-5.4": {ID: "gpt-5.4", Cost: &modelsDevCost{Input: 99, Output: 99}},
			},
		},
		"openai": {
			ID: "openai",
			Models: map[string]modelsDevModel{
				"manual-model": {ID: "manual-model", Cost: &modelsDevCost{Input: 1, Output: 2}},
				"gpt-5.4": {
					ID: "gpt-5.4",
					Cost: &modelsDevCost{
						Input: 1, Output: 2, CacheRead: .25, CacheWrite: .5,
						Tiers: []modelsDevCostTier{{Input: 3, Output: 4, Tier: modelsDevTierKind{Type: "context", Size: 128_000}}},
					},
					Experimental: &modelsDevExperimental{Modes: map[string]modelsDevMode{
						"priority": {
							Cost:     &modelsDevCost{Input: 5, Output: 6},
							Provider: modelsDevModeProvider{Body: modelsDevModeBody{ServiceTier: "priority"}},
						},
					}},
				},
			},
		},
	}
	plugin := &modelRouterPlugin{store: store, modelsDevFetcher: staticModelsDevFetcher{catalog: catalog}, priceSync: &priceSyncState{}}
	synced, err := plugin.syncModelsDev(syncModelPricesRequest{
		Source:   priceSourceModelsDev,
		Revision: book.Revision,
		Models:   []string{"manual-model", "gpt-5.4(high)", "missing-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if synced.Prices["manual-model"].Input != 9 || synced.Prices["manual-model"].Source != priceSourceManual {
		t.Fatalf("manual price was overwritten: %#v", synced.Prices["manual-model"])
	}
	price, ok := synced.Prices["gpt-5.4(high)"]
	if !ok || price.Input != 1 || price.CatalogProvider != "openai" || price.Source != priceSourceModelsDev {
		t.Fatalf("synchronized price = %#v", price)
	}
	if len(price.ContextTiers) != 1 || price.ContextTiers[0].Threshold != 128_000 || price.ServiceTiers["priority"].Input != 5 {
		t.Fatalf("synchronized tiers = %#v", price)
	}
	if synced.LastSync == nil || synced.LastSync.Observed != 3 || synced.LastSync.Matched != 2 || synced.LastSync.Unmatched != 1 || synced.LastSync.SkippedManual != 1 || synced.LastSync.Created != 1 {
		t.Fatalf("sync metadata = %#v", synced.LastSync)
	}

	reopened, err := store.QueryPriceBook()
	if err != nil || reopened.Revision != synced.Revision || reopened.Prices["gpt-5.4(high)"].Input != 1 {
		t.Fatalf("persisted synchronized book = %#v, %v", reopened, err)
	}
}

func TestNormalizeSyncModelsDeduplicatesNormalizedNames(t *testing.T) {
	models, err := normalizeSyncModels([]string{"GPT-5", " gpt-5 ", "claude-3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "GPT-5" || models[1] != "claude-3" {
		t.Fatalf("normalized sync models = %#v", models)
	}
}
