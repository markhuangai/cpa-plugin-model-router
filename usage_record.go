package main

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	return storedUsageRecord{
		RequestedAt:         requestedAt,
		Attribution:         attribution.Kind,
		RouterModel:         attribution.RouterModel,
		Provider:            strings.TrimSpace(record.Provider),
		ExecutorType:        strings.TrimSpace(record.ExecutorType),
		ProviderModel:       firstNonEmpty(strings.TrimSpace(record.Model), strings.TrimSpace(record.Alias), "unknown"),
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

func synthesizedOfficialUsageTotal(record pluginapi.UsageRecord, detail pluginapi.UsageDetail) int64 {
	accounting := usagePayloadAccounting{AccountingMode: defaultAccountingMode(record.Provider, record.ExecutorType)}
	if equalFold(record.Provider, "google") || equalFold(record.Provider, "gemini") || equalFold(record.ExecutorType, "gemini") {
		accounting.ReasoningMode = reasoningModeSeparate
	}
	return synthesizedUsageTotal(detail, accounting)
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
	lower := strings.ToLower(value)
	for _, prefix := range []string{"bearer ", "basic ", "token ", "apikey ", "api-key ", "api_key ", "sk-", "sk_", "xai-", "gsk_", "aiza", "key-", "sess-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
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
