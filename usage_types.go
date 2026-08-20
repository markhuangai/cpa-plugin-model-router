package main

import "time"

const usageSchemaVersion = 1

type usageCounters struct {
	Requests            uint64 `json:"requests"`
	FailedRequests      uint64 `json:"failed_requests"`
	InputTokens         uint64 `json:"input_tokens"`
	OutputTokens        uint64 `json:"output_tokens"`
	ReasoningTokens     uint64 `json:"reasoning_tokens"`
	CachedTokens        uint64 `json:"cached_tokens"`
	CacheReadTokens     uint64 `json:"cache_read_tokens"`
	CacheCreationTokens uint64 `json:"cache_creation_tokens"`
	TotalTokens         uint64 `json:"total_tokens"`
}

func (counters *usageCounters) add(record storedUsageRecord) {
	counters.Requests++
	if record.Failed {
		counters.FailedRequests++
	}
	counters.InputTokens += record.InputTokens
	counters.OutputTokens += record.OutputTokens
	counters.ReasoningTokens += record.ReasoningTokens
	counters.CachedTokens += record.CachedTokens
	counters.CacheReadTokens += record.CacheReadTokens
	counters.CacheCreationTokens += record.CacheCreationTokens
	counters.TotalTokens += record.TotalTokens
}

type storedUsageRecord struct {
	Sequence            uint64    `json:"sequence"`
	RequestedAt         time.Time `json:"requested_at"`
	Attribution         string    `json:"attribution"`
	RouterModel         string    `json:"router_model,omitempty"`
	Provider            string    `json:"provider,omitempty"`
	ExecutorType        string    `json:"executor_type,omitempty"`
	ProviderModel       string    `json:"provider_model"`
	ProviderAlias       string    `json:"provider_alias,omitempty"`
	Source              string    `json:"source,omitempty"`
	ReasoningEffort     string    `json:"reasoning_effort,omitempty"`
	ServiceTier         string    `json:"service_tier,omitempty"`
	MaskedAPIKey        string    `json:"masked_api_key,omitempty"`
	Generate            bool      `json:"generate"`
	Failed              bool      `json:"failed"`
	StatusCode          int       `json:"status_code,omitempty"`
	LatencyNS           uint64    `json:"latency_ns"`
	TTFTNS              uint64    `json:"ttft_ns"`
	InputTokens         uint64    `json:"input_tokens"`
	OutputTokens        uint64    `json:"output_tokens"`
	ReasoningTokens     uint64    `json:"reasoning_tokens"`
	CachedTokens        uint64    `json:"cached_tokens"`
	CacheReadTokens     uint64    `json:"cache_read_tokens"`
	CacheCreationTokens uint64    `json:"cache_creation_tokens"`
	TotalTokens         uint64    `json:"total_tokens"`
}

func (record storedUsageRecord) result() string {
	if !record.Failed {
		return "success"
	}
	if record.StatusCode > 0 {
		return "http_" + itoa(record.StatusCode)
	}
	return "failed"
}

type usageRequestDetail struct {
	storedUsageRecord
	Result        string         `json:"result"`
	GenerationNS  uint64         `json:"generation_ns"`
	TPS           float64        `json:"tps"`
	CacheHit      bool           `json:"cache_hit"`
	EstimatedCost *estimatedCost `json:"estimated_cost,omitempty"`
}

func requestDetail(record storedUsageRecord, resolver modelPriceResolver) usageRequestDetail {
	generation := record.LatencyNS
	if record.TTFTNS > 0 && record.LatencyNS >= record.TTFTNS {
		generation = record.LatencyNS - record.TTFTNS
	}
	tps := 0.0
	if generation > 0 {
		tps = float64(record.OutputTokens) / (float64(generation) / float64(time.Second))
	}
	cost := estimateUsageCost(record, resolver)
	return usageRequestDetail{
		storedUsageRecord: record,
		Result:            record.result(),
		GenerationNS:      generation,
		TPS:               tps,
		CacheHit:          record.CacheReadTokens > 0 || record.CachedTokens > 0,
		EstimatedCost:     &cost,
	}
}

type usageFilter struct {
	From          time.Time
	To            time.Time
	RouterModel   string
	ProviderModel string
	Source        string
	ServiceTier   string
	Result        string
}

func (filter usageFilter) matches(record storedUsageRecord) bool {
	if !filter.From.IsZero() && record.RequestedAt.Before(filter.From) {
		return false
	}
	if !filter.To.IsZero() && !record.RequestedAt.Before(filter.To) {
		return false
	}
	if filter.RouterModel != "" {
		value := record.RouterModel
		if filter.RouterModel == attributionDirect || filter.RouterModel == attributionUnresolved {
			value = record.Attribution
		}
		if !equalFold(value, filter.RouterModel) {
			return false
		}
	}
	return (filter.ProviderModel == "" || equalFold(record.ProviderModel, filter.ProviderModel)) &&
		(filter.Source == "" || equalFold(record.Source, filter.Source)) &&
		(filter.ServiceTier == "" || equalFold(record.ServiceTier, filter.ServiceTier)) &&
		(filter.Result == "" || equalFold(record.result(), filter.Result))
}

type usageSeriesPoint struct {
	Time string `json:"time"`
	usageCounters
	AverageLatencyNS uint64  `json:"average_latency_ns"`
	AverageTTFTNS    uint64  `json:"average_ttft_ns"`
	AverageTPS       float64 `json:"average_tps"`
	CostUSD          float64 `json:"cost_usd"`

	latencyTotal   uint64
	ttftTotal      uint64
	tpsTotal       float64
	timingRequests uint64
}

type usageModelStats struct {
	Model       string `json:"model"`
	Attribution string `json:"attribution,omitempty"`
	usageCounters
	CostUSD float64 `json:"cost_usd"`
}

type usageCostSummary struct {
	Requests         uint64  `json:"requests"`
	PricedRequests   uint64  `json:"priced_requests"`
	UnpricedRequests uint64  `json:"unpriced_requests"`
	InputUSD         float64 `json:"input_usd"`
	OutputUSD        float64 `json:"output_usd"`
	CacheReadUSD     float64 `json:"cache_read_usd"`
	CacheCreationUSD float64 `json:"cache_creation_usd"`
	TotalUSD         float64 `json:"total_usd"`
}

func (summary *usageCostSummary) add(cost estimatedCost) {
	summary.Requests++
	if cost.Priced {
		summary.PricedRequests++
	} else {
		summary.UnpricedRequests++
	}
	summary.InputUSD += cost.InputUSD
	summary.OutputUSD += cost.OutputUSD
	summary.CacheReadUSD += cost.CacheReadUSD
	summary.CacheCreationUSD += cost.CacheCreationUSD
	summary.TotalUSD += cost.TotalUSD
}

type usageOverview struct {
	SchemaVersion  uint32             `json:"schema_version"`
	GeneratedAt    time.Time          `json:"generated_at"`
	From           time.Time          `json:"from"`
	To             time.Time          `json:"to"`
	RetainedSince  time.Time          `json:"retained_since"`
	Summary        usageCounters      `json:"summary"`
	Costs          usageCostSummary   `json:"costs"`
	Series         []usageSeriesPoint `json:"series"`
	RouterModels   []usageModelStats  `json:"router_models"`
	ProviderModels []usageModelStats  `json:"provider_models"`
	Sources        []string           `json:"sources"`
	ServiceTiers   []string           `json:"service_tiers"`
	StorageError   string             `json:"storage_error,omitempty"`
}

type usageGroup struct {
	Key           string `json:"key"`
	RouterModel   string `json:"router_model,omitempty"`
	Attribution   string `json:"attribution,omitempty"`
	ProviderModel string `json:"provider_model,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Source        string `json:"source,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
	Result        string `json:"result,omitempty"`
	usageCounters
	AverageLatencyNS uint64  `json:"average_latency_ns"`
	AverageTTFTNS    uint64  `json:"average_ttft_ns"`
	AverageTPS       float64 `json:"average_tps"`
	CostUSD          float64 `json:"cost_usd"`
	PricedRequests   uint64  `json:"priced_requests"`
}

type usageGroupPage struct {
	SchemaVersion uint32       `json:"schema_version"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Dimension     string       `json:"dimension"`
	Total         int          `json:"total"`
	Offset        int          `json:"offset"`
	Limit         int          `json:"limit"`
	Items         []usageGroup `json:"items"`
}

type usageRequestPage struct {
	SchemaVersion     uint32               `json:"schema_version"`
	GeneratedAt       time.Time            `json:"generated_at"`
	PriceBookRevision uint64               `json:"price_book_revision"`
	Total             int                  `json:"total"`
	Offset            int                  `json:"offset"`
	Limit             int                  `json:"limit"`
	Items             []usageRequestDetail `json:"items"`
}

func equalFold(left, right string) bool {
	return routeKey(left) == routeKey(right)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		buffer[position] = '-'
	}
	return string(buffer[position:])
}
