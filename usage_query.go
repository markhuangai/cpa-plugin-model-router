package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type usageRouterKey struct {
	model       string
	attribution string
}

type usageGroupKey struct {
	value  string
	router usageRouterKey
}

func (store *usageStore) Overview(filter usageFilter, granularity string) (usageOverview, error) {
	records, err := store.records(filter)
	if err != nil {
		return usageOverview{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageOverview{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	now := time.Now().UTC()
	overview := usageOverview{
		SchemaVersion: usageSchemaVersion,
		GeneratedAt:   now,
		From:          filter.From,
		To:            filter.To,
		RetainedSince: store.RetainedSince(now),
		StorageError:  store.LastError(),
	}
	series := make(map[string]*usageSeriesPoint)
	routerModels := make(map[usageRouterKey]*usageModelStats)
	providerModels := make(map[string]*usageModelStats)
	sources := make(map[string]struct{})
	serviceTiers := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, record := range records {
		overview.Summary.add(record)
		cost := estimateUsageCost(record, resolver)
		overview.Costs.add(cost)
		bucket := usageBucket(record.RequestedAt, granularity)
		key := bucket.Format(time.RFC3339)
		point := series[key]
		if point == nil {
			point = &usageSeriesPoint{Time: key}
			series[key] = point
		}
		point.usageCounters.add(record)
		mode := cost.AccountingMode
		if mode == "" {
			mode = defaultAccountingMode(record.Provider, record.ExecutorType)
		}
		if mode == accountingModeInputIncludesCache {
			cacheRead := record.CacheReadTokens
			if cacheRead == 0 {
				cacheRead = record.CachedTokens
			}
			point.CacheReadIncludedTokens += cacheRead
		}
		point.latencyTotal += record.LatencyNS
		point.ttftTotal += record.TTFTNS
		if record.TTFTNS > 0 {
			point.ttftRequests++
		}
		generation := record.LatencyNS
		if record.TTFTNS > 0 && record.LatencyNS >= record.TTFTNS {
			generation -= record.TTFTNS
		}
		if generation > 0 {
			point.tpsTotal += float64(record.OutputTokens) / (float64(generation) / float64(time.Second))
			point.timingRequests++
		}
		point.CostUSD += cost.TotalUSD

		routerKey := usageRouterKey{model: record.RouterModel, attribution: record.Attribution}
		routerLabel, routerAttribution := record.RouterModel, record.Attribution
		if record.Attribution == attributionDirect {
			routerKey.model, routerLabel = "", ""
		} else if record.Attribution == attributionUnresolved {
			routerKey.model, routerLabel = "", ""
		}
		router := routerModels[routerKey]
		if router == nil {
			router = &usageModelStats{Model: routerLabel, Attribution: routerAttribution}
			routerModels[routerKey] = router
		}
		router.usageCounters.add(record)
		router.CostUSD += cost.TotalUSD

		provider := providerModels[record.ProviderModel]
		if provider == nil {
			provider = &usageModelStats{Model: record.ProviderModel}
			providerModels[record.ProviderModel] = provider
		}
		provider.usageCounters.add(record)
		provider.CostUSD += cost.TotalUSD
		if record.Source != "" {
			sources[record.Source] = struct{}{}
		}
		if record.ServiceTier != "" {
			serviceTiers[record.ServiceTier] = struct{}{}
		}
		results[record.result()] = struct{}{}
	}
	for _, point := range series {
		if point.Requests > 0 {
			point.AverageLatencyNS = point.latencyTotal / point.Requests
		}
		if point.ttftRequests > 0 {
			point.AverageTTFTNS = point.ttftTotal / point.ttftRequests
		}
		if point.timingRequests > 0 {
			point.AverageTPS = point.tpsTotal / float64(point.timingRequests)
		}
		overview.Series = append(overview.Series, *point)
	}
	sort.Slice(overview.Series, func(left, right int) bool { return overview.Series[left].Time < overview.Series[right].Time })
	overview.RouterModels = modelStatsValues(routerModels)
	overview.ProviderModels = modelStatsValues(providerModels)
	overview.Sources = sortedStrings(sources)
	overview.ServiceTiers = sortedStrings(serviceTiers)
	overview.Results = sortedStrings(results)
	return overview, nil
}

func usageBucket(value time.Time, granularity string) time.Time {
	value = value.UTC()
	switch granularity {
	case "minute":
		return value.Truncate(time.Minute)
	case "day":
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return value.Truncate(time.Hour)
	}
}

func modelStatsValues[K comparable](values map[K]*usageModelStats) []usageModelStats {
	result := make([]usageModelStats, 0, len(values))
	for _, value := range values {
		result = append(result, *value)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].TotalTokens != result[right].TotalTokens {
			return result[left].TotalTokens > result[right].TotalTokens
		}
		return strings.ToLower(result[left].Model) < strings.ToLower(result[right].Model)
	})
	return result
}

type groupAccumulator struct {
	group          usageGroup
	latencyTotal   uint64
	ttftTotal      uint64
	ttftRequests   uint64
	tpsTotal       float64
	timingRequests uint64
}

func (store *usageStore) Groups(filter usageFilter, dimension, sortField, order string, offset, limit int) (usageGroupPage, error) {
	records, err := store.records(filter)
	if err != nil {
		return usageGroupPage{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageGroupPage{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	groups := make(map[usageGroupKey]*groupAccumulator)
	for _, record := range records {
		value := usageDimensionValue(record, dimension)
		key := usageGroupKey{value: value}
		if dimension == "router_model" {
			key = usageGroupKey{router: usageRouterKey{model: record.RouterModel, attribution: record.Attribution}}
		}
		accumulator := groups[key]
		if accumulator == nil {
			accumulator = &groupAccumulator{group: usageGroup{Key: value}}
			assignGroupDimension(&accumulator.group, record, dimension)
			groups[key] = accumulator
		}
		accumulator.group.usageCounters.add(record)
		accumulator.latencyTotal += record.LatencyNS
		accumulator.ttftTotal += record.TTFTNS
		if record.TTFTNS > 0 {
			accumulator.ttftRequests++
		}
		generation := record.LatencyNS
		if record.TTFTNS > 0 && record.LatencyNS >= record.TTFTNS {
			generation -= record.TTFTNS
		}
		if generation > 0 {
			accumulator.tpsTotal += float64(record.OutputTokens) / (float64(generation) / float64(time.Second))
			accumulator.timingRequests++
		}
		cost := estimateUsageCost(record, resolver)
		accumulator.group.CostUSD += cost.TotalUSD
		if cost.Priced {
			accumulator.group.PricedRequests++
		}
	}
	items := make([]usageGroup, 0, len(groups))
	for _, accumulator := range groups {
		requests := accumulator.group.Requests
		if requests > 0 {
			accumulator.group.AverageLatencyNS = accumulator.latencyTotal / requests
		}
		if accumulator.ttftRequests > 0 {
			accumulator.group.AverageTTFTNS = accumulator.ttftTotal / accumulator.ttftRequests
		}
		if accumulator.timingRequests > 0 {
			accumulator.group.AverageTPS = accumulator.tpsTotal / float64(accumulator.timingRequests)
		}
		items = append(items, accumulator.group)
	}
	sort.SliceStable(items, func(left, right int) bool {
		comparison := compareGroups(items[left], items[right], sortField)
		if comparison == 0 {
			comparison = strings.Compare(strings.ToLower(items[left].Key), strings.ToLower(items[right].Key))
		}
		if order == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})
	total := len(items)
	items = paginateGroups(items, offset, limit)
	return usageGroupPage{SchemaVersion: usageSchemaVersion, GeneratedAt: time.Now().UTC(), Dimension: dimension, Total: total, Offset: offset, Limit: limit, Items: items}, nil
}

func usageDimensionValue(record storedUsageRecord, dimension string) string {
	switch dimension {
	case "router_model":
		if record.Attribution == attributionDirect || record.Attribution == attributionUnresolved {
			return record.Attribution
		}
		return record.RouterModel
	case "provider":
		return firstNonEmpty(record.Provider, "unknown")
	case "source":
		return firstNonEmpty(record.Source, "unknown")
	case "service_tier":
		return firstNonEmpty(record.ServiceTier, "default")
	case "result":
		return record.result()
	default:
		return record.ProviderModel
	}
}

func assignGroupDimension(group *usageGroup, record storedUsageRecord, dimension string) {
	switch dimension {
	case "router_model":
		group.RouterModel, group.Attribution = record.RouterModel, record.Attribution
	case "provider":
		group.Provider = record.Provider
	case "source":
		group.Source = record.Source
	case "service_tier":
		group.ServiceTier = record.ServiceTier
	case "result":
		group.Result = record.result()
	default:
		group.ProviderModel = record.ProviderModel
	}
}

func compareGroups(left, right usageGroup, field string) int {
	switch field {
	case "key":
		return strings.Compare(strings.ToLower(left.Key), strings.ToLower(right.Key))
	case "requests":
		return compareUint64(left.Requests, right.Requests)
	case "failed_requests":
		return compareUint64(left.FailedRequests, right.FailedRequests)
	case "input_tokens":
		return compareUint64(left.InputTokens, right.InputTokens)
	case "output_tokens":
		return compareUint64(left.OutputTokens, right.OutputTokens)
	case "latency":
		return compareUint64(left.AverageLatencyNS, right.AverageLatencyNS)
	case "ttft":
		return compareUint64(left.AverageTTFTNS, right.AverageTTFTNS)
	case "tps":
		return compareFloat(left.AverageTPS, right.AverageTPS)
	case "cost":
		return compareFloat(left.CostUSD, right.CostUSD)
	default:
		return compareUint64(left.TotalTokens, right.TotalTokens)
	}
}

func paginateGroups(items []usageGroup, offset, limit int) []usageGroup {
	if offset >= len(items) {
		return []usageGroup{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

func (store *usageStore) Requests(filter usageFilter, sortField, order string, offset, limit int) (usageRequestPage, error) {
	records, err := store.records(filter)
	if err != nil {
		return usageRequestPage{}, err
	}
	book, err := store.QueryPriceBook()
	if err != nil {
		return usageRequestPage{}, err
	}
	resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
	items := make([]usageRequestDetail, 0, len(records))
	for _, record := range records {
		items = append(items, requestDetail(record, resolver))
	}
	sort.SliceStable(items, func(left, right int) bool {
		comparison := compareRequests(items[left], items[right], sortField)
		if comparison == 0 {
			comparison = compareUint64(items[left].Sequence, items[right].Sequence)
		}
		if order == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})
	total := len(items)
	if offset >= len(items) {
		items = []usageRequestDetail{}
	} else {
		end := offset + limit
		if end > len(items) {
			end = len(items)
		}
		items = items[offset:end]
	}
	return usageRequestPage{SchemaVersion: usageSchemaVersion, GeneratedAt: time.Now().UTC(), PriceBookRevision: book.Revision, Total: total, Offset: offset, Limit: limit, Items: items}, nil
}

func compareRequests(left, right usageRequestDetail, field string) int {
	switch field {
	case "router_model":
		return strings.Compare(strings.ToLower(usageDimensionValue(left.storedUsageRecord, "router_model")), strings.ToLower(usageDimensionValue(right.storedUsageRecord, "router_model")))
	case "provider_model":
		return strings.Compare(strings.ToLower(left.ProviderModel), strings.ToLower(right.ProviderModel))
	case "source":
		return strings.Compare(strings.ToLower(left.Source), strings.ToLower(right.Source))
	case "service_tier":
		return strings.Compare(strings.ToLower(left.ServiceTier), strings.ToLower(right.ServiceTier))
	case "result":
		return strings.Compare(left.Result, right.Result)
	case "latency":
		return compareUint64(left.LatencyNS, right.LatencyNS)
	case "ttft":
		return compareUint64(left.TTFTNS, right.TTFTNS)
	case "tps":
		return compareFloat(left.TPS, right.TPS)
	case "total_tokens":
		return compareUint64(left.TotalTokens, right.TotalTokens)
	case "cost":
		return compareFloat(left.EstimatedCost.TotalUSD, right.EstimatedCost.TotalUSD)
	default:
		if left.RequestedAt.Before(right.RequestedAt) {
			return -1
		}
		if left.RequestedAt.After(right.RequestedAt) {
			return 1
		}
		return 0
	}
}

func compareUint64(left, right uint64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareFloat(left, right float64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func validateQueryChoice(value, fallback string, allowed []string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback, nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value, nil
		}
	}
	return "", fmt.Errorf("unsupported value %q", value)
}
