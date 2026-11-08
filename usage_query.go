package main

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type usageRouterKey struct{ model, attribution string }
type usageGroupKey struct {
	value  string
	router usageRouterKey
}

type usagePageOptions struct {
	Sort, Order   string
	Offset, Limit int
}

type usageGroupOptions struct {
	Dimension string
	usagePageOptions
}

func (store *usageStore) Overview(filter usageFilter, granularity string) (usageOverview, error) {
	if granularity == "" {
		granularity = "hour"
	}
	result, err := store.queryUsage(filter, granularity, nil, nil)
	return result.Overview, err
}

func (store *usageStore) Groups(filter usageFilter, dimension, sortField, order string, offset, limit int) (usageGroupPage, error) {
	result, err := store.queryUsage(filter, "", &usageGroupOptions{dimension, usagePageOptions{sortField, order, offset, limit}}, nil)
	return result.Groups, err
}

func (store *usageStore) Requests(filter usageFilter, sortField, order string, offset, limit int) (usageRequestPage, error) {
	result, err := store.queryUsage(filter, "", nil, &usagePageOptions{sortField, order, offset, limit})
	return result.Requests, err
}

func (store *usageStore) Dashboard(filter usageFilter, granularity string, groups usageGroupOptions, requests usagePageOptions) (usageDashboard, error) {
	if granularity == "" {
		granularity = "hour"
	}
	return store.queryUsage(filter, granularity, &groups, &requests)
}

func (store *usageStore) queryUsage(filter usageFilter, granularity string, groups *usageGroupOptions, requests *usagePageOptions) (usageDashboard, error) {
	var result usageDashboard
	var diagnostics *usageAccountingDiagnostics
	if granularity != "" && nativeDiagnosticsAvailable() {
		diagnostics = &usageAccountingDiagnostics{operation: diagnosticOperationID(), started: time.Now(), filter: filter, groups: make(map[accountingDiagnosticKey]accountingDiagnosticTotals)}
	}
	err := store.readSnapshot(func(connection *sql.Conn, retentionDays int) error {
		book, err := priceBookFromSQLite(connection)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		result = usageDashboard{SchemaVersion: usageSchemaVersion, GeneratedAt: now, PriceBookRevision: book.Revision}
		resolver := newModelPriceResolver(book.Prices, book.SyncSettings)
		var overview *usageOverviewAccumulator
		if granularity != "" {
			overview = newUsageOverviewAccumulator(filter, granularity, now, retentionDays, store.LastError())
		}
		var grouped *usageGroupsAccumulator
		if groups != nil {
			grouped = &usageGroupsAccumulator{dimension: groups.Dimension, groups: make(map[usageGroupKey]*groupAccumulator)}
		}
		indexed := requests != nil && requests.Sort == "time" && !filter.hasDimensions()
		withCosts := overview != nil || grouped != nil || requests != nil && requests.Sort == "cost"
		var selection *usageRequestSelection
		if requests != nil && !indexed {
			selection = newUsageRequestSelection(*requests, withCosts)
			if requests.Offset > 0 {
				total, err := countUsageRecords(connection, filter)
				if err != nil {
					return err
				}
				if requests.Offset >= total {
					selection.capacity = 0
				}
			}
		}
		if overview != nil || grouped != nil || selection != nil {
			err := scanUsageRecords(connection, filter, func(record storedUsageRecord) {
				var cost estimatedCost
				if withCosts {
					cost = estimateUsageCost(record, resolver)
				}
				if diagnostics != nil {
					diagnostics.add(record, cost)
				}
				if overview != nil {
					overview.add(record, cost)
				}
				if grouped != nil {
					grouped.add(record, cost)
				}
				if selection != nil {
					selection.add(record, cost)
				}
			})
			if err != nil {
				return err
			}
		}
		if overview != nil {
			result.Overview = overview.finish()
		}
		if grouped != nil {
			result.Groups = grouped.finish(*groups, now)
		}
		if indexed {
			total, err := countUsageRecords(connection, filter)
			if err != nil {
				return err
			}
			items, err := indexedUsageRequestPage(connection, filter, *requests, resolver)
			if err != nil {
				return err
			}
			result.Requests = usageRequestPage{SchemaVersion: usageSchemaVersion, GeneratedAt: now, PriceBookRevision: book.Revision, Total: total, Offset: requests.Offset, Limit: requests.Limit, Items: items}
		} else if selection != nil {
			result.Requests = selection.finish(resolver, book.Revision, now)
		}
		return nil
	})
	if diagnostics != nil {
		diagnostics.finish(result.PriceBookRevision, err)
	}
	if err != nil {
		store.recordError(err)
		return usageDashboard{}, err
	}
	return result, nil
}

func (filter usageFilter) hasDimensions() bool {
	return filter.Attribution != "" || filter.RouterModel != "" || filter.ProviderModel != "" || filter.Source != "" || filter.ServiceTier != "" || filter.Result != ""
}

func countUsageRecords(connection *sql.Conn, filter usageFilter) (int, error) {
	where, args := usageTimeWhere(filter)
	var count int
	err := connection.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM requests`+where, args...).Scan(&count)
	return count, err
}

func indexedUsageRequestPage(connection *sql.Conn, filter usageFilter, options usagePageOptions, resolver modelPriceResolver) ([]usageRequestDetail, error) {
	where, args := usageTimeWhere(filter)
	order := " DESC"
	if options.Order == "asc" {
		order = " ASC"
	}
	rows, err := connection.QueryContext(context.Background(), `SELECT payload FROM requests`+where+` ORDER BY requested_at_ns`+order+`, sequence`+order+` LIMIT ? OFFSET ?`, append(args, options.Limit, options.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]usageRequestDetail, 0)
	var value sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		var record storedUsageRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		items = append(items, requestDetail(record, resolver))
	}
	return items, rows.Err()
}

type usageOverviewAccumulator struct {
	overview                       usageOverview
	granularity                    string
	series                         map[string]*usageSeriesPoint
	routerModels                   map[usageRouterKey]*usageModelStats
	providerModels                 map[string]*usageModelStats
	sources, serviceTiers, results map[string]struct{}
}

func newUsageOverviewAccumulator(filter usageFilter, granularity string, now time.Time, retentionDays int, storageError string) *usageOverviewAccumulator {
	return &usageOverviewAccumulator{
		overview:    usageOverview{SchemaVersion: usageSchemaVersion, GeneratedAt: now, From: filter.From, To: filter.To, RetainedSince: now.AddDate(0, 0, -retentionDays), StorageError: storageError},
		granularity: granularity, series: make(map[string]*usageSeriesPoint), routerModels: make(map[usageRouterKey]*usageModelStats), providerModels: make(map[string]*usageModelStats),
		sources: make(map[string]struct{}), serviceTiers: make(map[string]struct{}), results: make(map[string]struct{}),
	}
}

func (acc *usageOverviewAccumulator) add(record storedUsageRecord, cost estimatedCost) {
	overview, granularity, series := &acc.overview, acc.granularity, acc.series
	routerModels, providerModels := acc.routerModels, acc.providerModels
	sources, serviceTiers, results := acc.sources, acc.serviceTiers, acc.results

	overview.Summary.add(record)
	overview.Costs.add(cost)
	bucket := usageBucket(record.RequestedAt, granularity)
	key := bucket.Format(time.RFC3339)
	point := series[key]
	if point == nil {
		point = &usageSeriesPoint{Time: key}
		series[key] = point
	}
	point.usageCounters.add(record)
	mode := effectiveAccountingMode(record, cost.AccountingMode)
	if mode == accountingModeInputIncludesCache {
		point.CacheReadIncludedTokens += effectiveCacheReadTokens(record)
	}
	if reasoningIncludedInOutput(record) {
		point.ReasoningIncludedTokens += record.ReasoningTokens
	}
	point.usageTimingTotals.add(record)
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

func (acc *usageOverviewAccumulator) finish() usageOverview {
	overview, series := &acc.overview, acc.series
	routerModels, providerModels := acc.routerModels, acc.providerModels
	sources, serviceTiers, results := acc.sources, acc.serviceTiers, acc.results
	for _, point := range series {
		point.AverageLatencyNS, point.AverageTTFTNS, point.AverageTPS = point.usageTimingTotals.averages(point.Requests)
		overview.Series = append(overview.Series, *point)
	}
	sort.Slice(overview.Series, func(left, right int) bool { return overview.Series[left].Time < overview.Series[right].Time })
	overview.RouterModels = modelStatsValues(routerModels)
	overview.ProviderModels = modelStatsValues(providerModels)
	overview.Sources = sortedStrings(sources)
	overview.ServiceTiers = sortedStrings(serviceTiers)
	overview.Results = sortedStrings(results)
	return *overview
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
	group usageGroup
	usageTimingTotals
}

type usageGroupsAccumulator struct {
	dimension string
	groups    map[usageGroupKey]*groupAccumulator
}

func (acc *usageGroupsAccumulator) add(record storedUsageRecord, cost estimatedCost) {
	dimension, groups := acc.dimension, acc.groups

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
	accumulator.usageTimingTotals.add(record)
	accumulator.group.CostUSD += cost.TotalUSD
	if cost.Priced {
		accumulator.group.PricedRequests++
	}
}

func (acc *usageGroupsAccumulator) finish(options usageGroupOptions, now time.Time) usageGroupPage {
	groups, dimension, sortField, order, offset, limit := acc.groups, options.Dimension, options.Sort, options.Order, options.Offset, options.Limit
	items := make([]usageGroup, 0, len(groups))
	for _, accumulator := range groups {
		accumulator.group.AverageLatencyNS, accumulator.group.AverageTTFTNS, accumulator.group.AverageTPS = accumulator.usageTimingTotals.averages(accumulator.group.Requests)
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
	return usageGroupPage{SchemaVersion: usageSchemaVersion, GeneratedAt: now, Dimension: dimension, Total: total, Offset: offset, Limit: limit, Items: items}
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
	end := offset + min(limit, len(items)-offset)
	return items[offset:end]
}

type usageRequestCandidate struct {
	storedUsageRecord
	cost      estimatedCost
	sortValue float64
}

type usageRequestSelection struct {
	options         usagePageOptions
	items           []usageRequestCandidate
	total, capacity int
	withCosts       bool
}

func newUsageRequestSelection(options usagePageOptions, withCosts bool) *usageRequestSelection {
	capacity := int(^uint(0) >> 1)
	if options.Offset <= capacity-options.Limit {
		capacity = options.Offset + options.Limit
	}
	return &usageRequestSelection{options: options, capacity: capacity, withCosts: withCosts}
}

func (selection *usageRequestSelection) better(left, right usageRequestCandidate) bool {
	comparison := compareRequests(left, right, selection.options.Sort)
	if comparison == 0 {
		comparison = compareUint64(left.Sequence, right.Sequence)
	}
	if selection.options.Order == "asc" {
		return comparison < 0
	}
	return comparison > 0
}

func (selection *usageRequestSelection) Len() int { return len(selection.items) }
func (selection *usageRequestSelection) Less(i, j int) bool {
	return selection.better(selection.items[j], selection.items[i])
}
func (selection *usageRequestSelection) Swap(i, j int) {
	selection.items[i], selection.items[j] = selection.items[j], selection.items[i]
}
func (selection *usageRequestSelection) Push(value any) {
	selection.items = append(selection.items, value.(usageRequestCandidate))
}
func (selection *usageRequestSelection) Pop() any {
	last := len(selection.items) - 1
	value := selection.items[last]
	selection.items[last] = usageRequestCandidate{}
	selection.items = selection.items[:last]
	return value
}

func (selection *usageRequestSelection) add(record storedUsageRecord, cost estimatedCost) {
	selection.total++
	if selection.capacity == 0 {
		return
	}
	candidate := usageRequestCandidate{storedUsageRecord: record, cost: cost}
	if selection.options.Sort == "cost" {
		candidate.sortValue = cost.TotalUSD
	}
	if selection.options.Sort == "tps" {
		_, candidate.sortValue = recordTiming(record)
	}
	if len(selection.items) < selection.capacity {
		selection.items = append(selection.items, candidate)
		if len(selection.items) == selection.capacity {
			heap.Init(selection)
		}
	} else if selection.better(candidate, selection.items[0]) {
		selection.items[0] = candidate
		heap.Fix(selection, 0)
	}
}

func (selection *usageRequestSelection) finish(resolver modelPriceResolver, revision uint64, now time.Time) usageRequestPage {
	options := selection.options
	items := make([]usageRequestDetail, 0)
	if options.Offset < len(selection.items) {
		sort.Slice(selection.items, func(i, j int) bool { return selection.better(selection.items[i], selection.items[j]) })
		end := options.Offset + min(options.Limit, len(selection.items)-options.Offset)
		for _, candidate := range selection.items[options.Offset:end] {
			if selection.withCosts {
				items = append(items, requestDetailWithCost(candidate.storedUsageRecord, candidate.cost))
			} else {
				items = append(items, requestDetail(candidate.storedUsageRecord, resolver))
			}
		}
	}
	return usageRequestPage{SchemaVersion: usageSchemaVersion, GeneratedAt: now, PriceBookRevision: revision, Total: selection.total, Offset: options.Offset, Limit: options.Limit, Items: items}
}

func compareRequests(left, right usageRequestCandidate, field string) int {
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
		return strings.Compare(left.result(), right.result())
	case "latency":
		return compareUint64(left.LatencyNS, right.LatencyNS)
	case "ttft":
		return compareUint64(left.TTFTNS, right.TTFTNS)
	case "tps":
		return compareFloat(left.sortValue, right.sortValue)
	case "total_tokens":
		return compareUint64(left.TotalTokens, right.TotalTokens)
	case "cost":
		return compareFloat(left.sortValue, right.sortValue)
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

type accountingDiagnosticKey struct {
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Executor  string `json:"executor"`
	Override  string `json:"price_override"`
	Stored    string `json:"stored_mode"`
	Default   string `json:"default_mode"`
	Effective string `json:"effective_mode"`
	Priced    bool   `json:"priced"`
}

type accountingDiagnosticTotals struct {
	Requests      uint64 `json:"requests"`
	Input         uint64 `json:"input_tokens"`
	CacheRead     uint64 `json:"cache_read_tokens"`
	CacheCreation uint64 `json:"cache_creation_tokens"`
	Included      uint64 `json:"cache_read_included_tokens"`
}

type usageAccountingDiagnostics struct {
	operation string
	started   time.Time
	filter    usageFilter
	groups    map[accountingDiagnosticKey]accountingDiagnosticTotals
}

func (diagnostics *usageAccountingDiagnostics) add(record storedUsageRecord, cost estimatedCost) {
	key := accountingDiagnosticKey{Model: record.ProviderModel, Provider: record.Provider, Executor: record.ExecutorType, Override: cost.priceAccountingMode, Stored: record.AccountingMode, Default: defaultAccountingMode(record.Provider, record.ExecutorType), Effective: effectiveAccountingMode(record, cost.AccountingMode), Priced: cost.Priced}
	totals := diagnostics.groups[key]
	totals.Requests++
	totals.Input += record.InputTokens
	totals.CacheRead += effectiveCacheReadTokens(record)
	totals.CacheCreation += record.CacheCreationTokens
	if key.Effective == accountingModeInputIncludesCache {
		totals.Included += effectiveCacheReadTokens(record)
	}
	diagnostics.groups[key] = totals
}

func (diagnostics *usageAccountingDiagnostics) finish(revision uint64, err error) {
	type decision struct {
		accountingDiagnosticKey
		accountingDiagnosticTotals
	}
	groups := make([]decision, 0, len(diagnostics.groups))
	for key, totals := range diagnostics.groups {
		groups = append(groups, decision{key, totals})
	}
	sort.Slice(groups, func(i, j int) bool {
		left, _ := json.Marshal(groups[i])
		right, _ := json.Marshal(groups[j])
		return string(left) < string(right)
	})
	filter := diagnostics.filter
	source := filter.Source
	if looksLikeCredential(source) {
		source = "[omitted: credential-shaped source filter]"
	}
	fields := map[string]any{"operation_id": diagnostics.operation, "price_book_revision": revision, "from": filter.From, "to": filter.To, "attribution": diagnosticFilterValue(filter.Attribution), "router_model": diagnosticFilterValue(filter.RouterModel), "provider_model": diagnosticFilterValue(filter.ProviderModel), "source": diagnosticFilterValue(source), "service_tier": diagnosticFilterValue(filter.ServiceTier), "result": diagnosticFilterValue(filter.Result), "groups": groups}
	logModelRouterJSON(diagnostics.operation, "model_router.accounting.decisions", fields)
	outcome := diagnosticErrorFields(err)
	outcome["operation_id"] = diagnostics.operation
	outcome["price_book_revision"] = revision
	outcome["elapsed_ms"] = time.Since(diagnostics.started).Milliseconds()
	outcome["decision_groups"] = len(groups)
	if err != nil {
		outcome["outcome"] = "failed"
	} else {
		outcome["outcome"] = "complete"
	}
	logModelRouterEvent("", "debug", "model_router.query.complete", outcome)
}
