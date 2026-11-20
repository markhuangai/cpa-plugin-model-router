package main

import (
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerModelNotAttempted = "not_attempted"
	providerModelUnknown      = "unknown"
)

func storedRecordFromUsage(record pluginapi.UsageRecord, attribution attributionResult) storedUsageRecord {
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	detail := record.Detail
	if detail.TotalTokens == 0 {
		detail.TotalTokens = synthesizedOfficialUsageTotal(record, detail)
	}
	accountingMode, reasoningMode := usageAccountingModes(record.Provider, record.ExecutorType)
	physicalModel := strings.TrimSpace(record.Model)
	providerModel, modelState := physicalModel, ""
	if isRouterOnlyRecord(record) {
		modelState = providerModelNotAttempted
		physicalModel, providerModel = "", ""
	} else if physicalModel == "" {
		modelState = providerModelUnknown
	} else {
		providerModel = qualifyProviderModel(physicalModel, firstNonEmpty(attribution.Target, record.Alias))
	}
	return storedUsageRecord{
		RequestedAt:         requestedAt,
		Attribution:         attribution.Kind,
		RouterModel:         attribution.RouterModel,
		RouteAttempt:        attribution.RouteAttempt,
		Provider:            strings.TrimSpace(record.Provider),
		ExecutorType:        strings.TrimSpace(record.ExecutorType),
		AccountingMode:      accountingMode,
		ReasoningMode:       reasoningMode,
		ProviderModel:       providerModel,
		PhysicalModel:       physicalModel,
		ProviderModelState:  modelState,
		ProviderAlias:       strings.TrimSpace(record.Alias),
		Source:              safeStoredUsageSource(record),
		ReasoningEffort:     strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:         strings.TrimSpace(record.ServiceTier),
		MaskedAPIKey:        maskAPIKey(record.APIKey),
		Generate:            record.Generate,
		Failed:              record.Failed,
		StatusCode:          record.Failure.StatusCode,
		LatencyNS:           durationUint64(record.Latency),
		TTFTNS:              durationUint64(record.TTFT),
		InputTokens:         nonnegativeUint64(detail.InputTokens),
		OutputTokens:        nonnegativeUint64(detail.OutputTokens),
		ReasoningTokens:     nonnegativeUint64(detail.ReasoningTokens),
		CachedTokens:        nonnegativeUint64(detail.CachedTokens),
		CacheReadTokens:     nonnegativeUint64(detail.CacheReadTokens),
		CacheCreationTokens: nonnegativeUint64(detail.CacheCreationTokens),
		TotalTokens:         nonnegativeUint64(detail.TotalTokens),
	}
}

func routerOnlyUsage(provider, executor string) bool {
	return equalFold(provider, pluginID) && (strings.TrimSpace(executor) == "" || equalFold(executor, "plugin:"+pluginID))
}

func isRouterOnlyRecord(record pluginapi.UsageRecord) bool {
	return routerOnlyUsage(record.Provider, record.ExecutorType) &&
		strings.TrimSpace(record.AuthID) == "" && strings.TrimSpace(record.AuthIndex) == ""
}

func routerAliasFromUsage(model, alias string) string {
	base, _, _ := splitThinkingSuffix(firstNonEmpty(strings.TrimSpace(model), strings.TrimSpace(alias)))
	return base
}

func qualifyProviderModel(physical, target string) string {
	target, _, _ = splitThinkingSuffix(target)
	physicalBase, _, _ := splitThinkingSuffix(physical)
	if equalFold(physicalBase, target) {
		return physical
	}
	prefix, rest, qualified := strings.Cut(target, "/")
	if !qualified || strings.TrimSpace(prefix) == "" || strings.TrimSpace(rest) == "" {
		return physical
	}
	return prefix + "/" + physical
}

func (record *storedUsageRecord) recoverProviderModel() {
	if record.PhysicalModel != "" || record.ProviderModelState != "" {
		return
	}
	if routerOnlyUsage(record.Provider, record.ExecutorType) {
		record.Attribution = attributionRouted
		record.RouterModel = routerAliasFromUsage(record.ProviderModel, record.ProviderAlias)
		record.ProviderModel = ""
		record.ProviderModelState = providerModelNotAttempted
		return
	}
	if record.ProviderModel == "" {
		record.ProviderModel = ""
		record.ProviderModelState = providerModelUnknown
		return
	}
	if record.ProviderAlias == "" {
		return
	}
	aliased, _, _ := splitThinkingSuffix(record.ProviderAlias)
	if equalFold(aliased, record.RouterModel) {
		return
	}
	_, remainder, qualified := strings.Cut(aliased, "/")
	if physical, _, _ := splitThinkingSuffix(record.ProviderModel); qualified && equalFold(remainder, physical) {
		record.PhysicalModel = record.ProviderModel
		record.ProviderModel = qualifyProviderModel(record.PhysicalModel, aliased)
	}
}

func (record storedUsageRecord) providerModelLabel() string {
	switch record.ProviderModelState {
	case providerModelNotAttempted:
		return "No provider attempted"
	case providerModelUnknown:
		return "Unknown provider model"
	default:
		return record.ProviderModel
	}
}

func synthesizedOfficialUsageTotal(record pluginapi.UsageRecord, detail pluginapi.UsageDetail) int64 {
	accountingMode, reasoningMode := usageAccountingModes(record.Provider, record.ExecutorType)
	values := []int64{detail.InputTokens, detail.OutputTokens}
	if accountingMode == accountingModeInputExcludesCache {
		values = append(values, detail.CacheReadTokens, detail.CacheCreationTokens)
	}
	if reasoningMode == reasoningModeSeparate {
		values = append(values, detail.ReasoningTokens)
	}
	return saturatingTokenSum(values...)
}

func saturatingTokenSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func safeStoredUsageSource(record pluginapi.UsageRecord) string {
	source := strings.TrimSpace(record.Source)
	fallback := firstNonEmpty(strings.TrimSpace(record.Provider), strings.TrimSpace(record.ExecutorType))
	if source == "" {
		return fallback
	}
	if isAPIKeyAuthType(record.AuthType) || sameNonemptyValue(source, record.APIKey) {
		return fallback
	}
	parsed, err := url.Parse(source)
	if err == nil && parsed.Hostname() != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return strings.TrimRight(parsed.String(), "/")
	}
	if looksLikeCredential(source) {
		return fallback
	}
	return source
}

func isAPIKeyAuthType(value string) bool {
	value = strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	return value == "apikey"
}

func sameNonemptyValue(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && left == right
}

func looksLikeCredential(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if hasCredentialPrefix(value) {
		return true
	}
	if len(value) < 8 || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	letters, digits := 0, 0
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z':
			letters++
		case character >= '0' && character <= '9':
			digits++
		}
	}
	return letters > 0 || digits > 0
}

func hasCredentialPrefix(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"bearer ", "basic ", "token ", "apikey ", "api-key ", "api_key ", "sk-", "sk_", "xai-", "gsk_", "aiza", "key-", "sess-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func durationUint64(value time.Duration) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func nonnegativeUint64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func sortedStrings(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
