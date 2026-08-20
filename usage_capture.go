package main

import (
	"bytes"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type directUsageCapture struct {
	requestedAt     time.Time
	completedAt     time.Time
	firstTokenAt    time.Time
	provider        string
	executorType    string
	providerModel   string
	source          string
	reasoningEffort string
	serviceTier     string
	accountingMode  string
	reasoningMode   string
	maskedAPIKey    string
	generate        bool
	failed          bool
	statusCode      int
	detail          pluginapi.UsageDetail
	streamPending   []byte
}

func newDirectUsageCapture(request pluginapi.RequestInterceptRequest, now time.Time) directUsageCapture {
	provider, model := splitProviderModel(firstNonEmpty(request.Model, request.RequestedModel))
	capture := directUsageCapture{
		requestedAt:     now.UTC(),
		provider:        provider,
		providerModel:   model,
		source:          provider,
		reasoningEffort: metadataString(request.Metadata, "reasoning_effort"),
		serviceTier:     metadataString(request.Metadata, "service_tier"),
		maskedAPIKey:    maskAPIKey(clientCredential(request.Headers)),
		generate:        metadataBoolDefault(request.Metadata, "generate", true),
	}
	capture.updateRequest(request)
	return capture
}

func newRoutedUsageCapture(request pluginapi.ExecutorRequest, target string, now time.Time) directUsageCapture {
	provider, model := splitProviderModel(target)
	return directUsageCapture{
		requestedAt:     now.UTC(),
		provider:        provider,
		providerModel:   model,
		source:          provider,
		executorType:    normalizeProtocol(firstNonEmpty(request.Format, request.SourceFormat)),
		reasoningEffort: metadataString(request.Metadata, "reasoning_effort"),
		serviceTier:     metadataString(request.Metadata, "service_tier"),
		maskedAPIKey:    maskAPIKey(clientCredential(request.Headers)),
		generate:        metadataBoolDefault(request.Metadata, "generate", true),
	}
}

func (capture *directUsageCapture) updateRequest(request pluginapi.RequestInterceptRequest) {
	if capture == nil {
		return
	}
	provider, model := splitProviderModel(firstNonEmpty(request.Model, capture.providerModel))
	if provider != "" {
		capture.provider = provider
		capture.source = provider
	}
	if model != "" {
		capture.providerModel = model
	}
	if format := normalizeProtocol(request.ToFormat); format != "" {
		capture.executorType = format
	}
	if value := metadataString(request.Metadata, "reasoning_effort"); value != "" {
		capture.reasoningEffort = value
	}
	if value := metadataString(request.Metadata, "service_tier"); value != "" {
		capture.serviceTier = value
	}
	capture.generate = metadataBoolDefault(request.Metadata, "generate", capture.generate)
}

func (capture *directUsageCapture) observeResponse(request pluginapi.ResponseInterceptRequest, now time.Time) {
	if capture == nil {
		return
	}
	capture.statusCode = request.StatusCode
	capture.completedAt = now.UTC()
	capture.observePayload(request.Body)
}

func (capture *directUsageCapture) observeStream(request pluginapi.StreamChunkInterceptRequest, now time.Time) {
	if capture == nil || request.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex || len(request.Body) == 0 {
		return
	}
	if capture.firstTokenAt.IsZero() {
		capture.firstTokenAt = now.UTC()
	}
	capture.observeStreamPayload(request.Body)
}

func (capture *directUsageCapture) complete(completion pluginapi.RequestCompletion) {
	if capture == nil {
		return
	}
	if !completion.StartedAt.IsZero() {
		capture.requestedAt = completion.StartedAt.UTC()
	}
	if completion.CompletedAt.IsZero() {
		capture.completedAt = time.Now().UTC()
	} else {
		capture.completedAt = completion.CompletedAt.UTC()
	}
	capture.statusCode = completion.StatusCode
	capture.failed = completion.Outcome != pluginapi.RequestCompletionSucceeded
	if !capture.failed && capture.statusCode == 0 {
		capture.statusCode = http.StatusOK
	}
	capture.flushStreamPending()
}

func (capture *directUsageCapture) finishAttempt(statusCode int, failed bool, completedAt time.Time) {
	if capture == nil {
		return
	}
	capture.statusCode = statusCode
	capture.failed = failed
	capture.completedAt = completedAt.UTC()
	if !failed && capture.statusCode == 0 {
		capture.statusCode = http.StatusOK
	}
	capture.flushStreamPending()
}

func (capture *directUsageCapture) observeStreamPayload(chunk []byte) {
	combined := make([]byte, 0, len(capture.streamPending)+len(chunk))
	combined = append(combined, capture.streamPending...)
	combined = append(combined, chunk...)
	capture.streamPending = nil
	combined = normalizeGluedSSEEvents(combined)
	trimmed := bytes.TrimSpace(combined)
	if gjson.ValidBytes(trimmed) {
		capture.observePayload(trimmed)
		return
	}
	complete, remainder := splitCompleteSSE(combined)
	if len(complete) == 0 {
		if len(combined) <= maxPendingStreamBytes {
			capture.streamPending = append([]byte(nil), combined...)
		}
		return
	}
	capture.observePayload(complete)
	if len(remainder) <= maxPendingStreamBytes {
		capture.streamPending = append([]byte(nil), remainder...)
	}
}

func (capture *directUsageCapture) flushStreamPending() {
	if capture == nil || len(capture.streamPending) == 0 {
		return
	}
	pending := capture.streamPending
	capture.streamPending = nil
	capture.observePayload(pending)
}

func (capture *directUsageCapture) observePayload(body []byte) {
	for _, payload := range usageJSONPayloads(body) {
		detail, tier, accounting, ok := parseUsagePayload(payload)
		if tier != "" {
			capture.serviceTier = tier
		}
		if accounting.AccountingMode != "" {
			capture.accountingMode = accounting.AccountingMode
		}
		if accounting.ReasoningMode != "" {
			capture.reasoningMode = accounting.ReasoningMode
		}
		if ok {
			mergeUsageDetail(&capture.detail, detail)
			synthesized := synthesizedUsageTotal(capture.detail, usagePayloadAccounting{AccountingMode: capture.accountingMode, ReasoningMode: capture.reasoningMode})
			capture.detail.TotalTokens = maxInt64(capture.detail.TotalTokens, synthesized)
		}
	}
}

func usageJSONPayloads(body []byte) [][]byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if gjson.ValidBytes(trimmed) {
		return [][]byte{append([]byte(nil), trimmed...)}
	}
	lines := bytes.Split(normalizeGluedSSEEvents(trimmed), []byte("\n"))
	result := make([][]byte, 0, len(lines))
	for _, line := range lines {
		_, payload, ok := sseDataLine(bytes.TrimSpace(line))
		if !ok {
			payload = bytes.TrimSpace(line)
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
			continue
		}
		result = append(result, append([]byte(nil), payload...))
	}
	return result
}

type usagePayloadAccounting struct {
	AccountingMode string
	ReasoningMode  string
	TotalExplicit  bool
}

func parseUsagePayload(payload []byte) (pluginapi.UsageDetail, string, usagePayloadAccounting, bool) {
	root := gjson.ParseBytes(payload)
	tier := firstGJSONText(root, "response.service_tier", "service_tier", "interaction.service_tier")
	node := firstGJSONNode(root,
		"response.usage",
		"usage",
		"total_usage",
		"metadata.total_usage",
		"metadata.usage",
		"interaction.usage",
		"interaction.total_usage",
		"interaction.metadata.total_usage",
		"message.usage",
	)
	if !node.Exists() {
		node = firstGJSONNode(root, "response.usageMetadata", "usageMetadata", "usage_metadata")
	}
	if !node.Exists() || !node.IsObject() {
		return pluginapi.UsageDetail{}, tier, usagePayloadAccounting{}, false
	}

	if node.Get("promptTokenCount").Exists() || node.Get("candidatesTokenCount").Exists() {
		detail := pluginapi.UsageDetail{
			InputTokens:     gjsonSum(node, "promptTokenCount", "toolUsePromptTokenCount", "tool_use_prompt_token_count"),
			OutputTokens:    firstGJSONInt(node, "candidatesTokenCount"),
			ReasoningTokens: firstGJSONInt(node, "thoughtsTokenCount"),
			CachedTokens:    firstGJSONInt(node, "cachedContentTokenCount"),
			TotalTokens:     firstGJSONInt(node, "totalTokenCount"),
		}
		detail.CacheReadTokens = detail.CachedTokens
		if detail.TotalTokens == 0 {
			detail.TotalTokens = saturatingTokenSum(detail.InputTokens, detail.OutputTokens, detail.ReasoningTokens)
		}
		return detail, tier, usagePayloadAccounting{AccountingMode: accountingModeInputIncludesCache, ReasoningMode: reasoningModeSeparate, TotalExplicit: node.Get("totalTokenCount").Exists()}, hasUsageTokens(detail)
	}

	cacheRead := firstGJSONInt(node, "cache_read_input_tokens", "cache_read_tokens", "cacheReadTokens")
	cacheCreation := firstGJSONInt(node, "cache_creation_input_tokens", "cache_creation_tokens", "cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens")
	input := gjsonSum(node, "input_tokens", "tool_use_tokens", "total_tool_use_tokens", "toolUseTokens", "totalToolUseTokens")
	if !node.Get("input_tokens").Exists() {
		input = firstGJSONInt(node, "prompt_tokens", "total_input_tokens")
	}
	output := firstGJSONInt(node, "output_tokens", "completion_tokens", "total_output_tokens")
	reasoning := firstGJSONInt(node,
		"output_tokens_details.reasoning_tokens",
		"output_tokens_details.thinking_tokens",
		"completion_tokens_details.reasoning_tokens",
		"reasoning_tokens",
		"thinking_tokens",
		"total_thought_tokens",
	)
	cached := firstGJSONInt(node,
		"input_tokens_details.cached_tokens",
		"prompt_tokens_details.cached_tokens",
		"cached_tokens",
		"total_cached_tokens",
	)
	if cacheRead == 0 {
		cacheRead = cached
	}
	if cached == 0 {
		cached = cacheRead
	}
	detail := pluginapi.UsageDetail{
		InputTokens:         input,
		OutputTokens:        output,
		ReasoningTokens:     reasoning,
		CachedTokens:        cached,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
		TotalTokens:         firstGJSONInt(node, "total_tokens", "totalTokenCount"),
	}
	if detail.TotalTokens == 0 {
		if node.Get("cache_read_input_tokens").Exists() || node.Get("cache_creation_input_tokens").Exists() {
			detail.TotalTokens = saturatingTokenSum(input, output, cacheRead, cacheCreation)
		} else if node.Get("reasoning_tokens").Exists() || node.Get("thoughtsTokenCount").Exists() || node.Get("total_thought_tokens").Exists() {
			detail.TotalTokens = saturatingTokenSum(input, output, reasoning)
		} else {
			detail.TotalTokens = saturatingTokenSum(input, output)
		}
	}
	accounting := usagePayloadAccounting{}
	if node.Get("cache_read_input_tokens").Exists() || node.Get("cache_creation_input_tokens").Exists() {
		accounting.AccountingMode = accountingModeInputExcludesCache
	}
	if node.Get("thinking_tokens").Exists() || node.Get("total_thought_tokens").Exists() {
		accounting.ReasoningMode = reasoningModeSeparate
	}
	accounting.TotalExplicit = node.Get("total_tokens").Exists() || node.Get("totalTokenCount").Exists()
	return detail, tier, accounting, hasUsageTokens(detail)
}

func synthesizedUsageTotal(detail pluginapi.UsageDetail, accounting usagePayloadAccounting) int64 {
	values := []int64{detail.InputTokens, detail.OutputTokens}
	if accounting.AccountingMode == accountingModeInputExcludesCache {
		values = append(values, detail.CacheReadTokens, detail.CacheCreationTokens)
	}
	if accounting.ReasoningMode == reasoningModeSeparate {
		values = append(values, detail.ReasoningTokens)
	}
	return saturatingTokenSum(values...)
}

func mergeUsageDetail(current *pluginapi.UsageDetail, next pluginapi.UsageDetail) {
	if current == nil {
		return
	}
	current.InputTokens = maxInt64(current.InputTokens, next.InputTokens)
	current.OutputTokens = maxInt64(current.OutputTokens, next.OutputTokens)
	current.ReasoningTokens = maxInt64(current.ReasoningTokens, next.ReasoningTokens)
	current.CachedTokens = maxInt64(current.CachedTokens, next.CachedTokens)
	current.CacheReadTokens = maxInt64(current.CacheReadTokens, next.CacheReadTokens)
	current.CacheCreationTokens = maxInt64(current.CacheCreationTokens, next.CacheCreationTokens)
	current.TotalTokens = maxInt64(current.TotalTokens, next.TotalTokens)
}

func (capture directUsageCapture) storedRecord(marker attributionMarker) storedUsageRecord {
	providerModel := firstNonEmpty(capture.providerModel, marker.providerModel, "unknown")
	provider, providerModel := splitProviderModel(providerModel)
	if capture.provider == "" {
		capture.provider = provider
	}
	if providerModel != "" {
		providerModel = strings.TrimSpace(providerModel)
	} else {
		providerModel = strings.TrimSpace(capture.providerModel)
	}
	requestedAt := capture.requestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = marker.startedAt.UTC()
	}
	completedAt := capture.completedAt.UTC()
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	latency := durationBetween(requestedAt, completedAt)
	ttft := durationBetween(requestedAt, capture.firstTokenAt.UTC())
	attribution := attributionRouted
	if marker.direct {
		attribution = attributionDirect
	}
	return storedUsageRecord{
		RequestedAt:         requestedAt,
		Attribution:         attribution,
		RouterModel:         marker.routerModel,
		Provider:            capture.provider,
		ExecutorType:        capture.executorType,
		AccountingMode:      capture.accountingMode,
		ReasoningMode:       capture.reasoningMode,
		ProviderModel:       firstNonEmpty(providerModel, "unknown"),
		Source:              capture.source,
		ReasoningEffort:     capture.reasoningEffort,
		ServiceTier:         capture.serviceTier,
		MaskedAPIKey:        capture.maskedAPIKey,
		Generate:            capture.generate,
		Failed:              capture.failed,
		StatusCode:          capture.statusCode,
		LatencyNS:           durationUint64(latency),
		TTFTNS:              durationUint64(ttft),
		InputTokens:         nonnegativeUint64(capture.detail.InputTokens),
		OutputTokens:        nonnegativeUint64(capture.detail.OutputTokens),
		ReasoningTokens:     nonnegativeUint64(capture.detail.ReasoningTokens),
		CachedTokens:        nonnegativeUint64(capture.detail.CachedTokens),
		CacheReadTokens:     nonnegativeUint64(capture.detail.CacheReadTokens),
		CacheCreationTokens: nonnegativeUint64(capture.detail.CacheCreationTokens),
		TotalTokens:         nonnegativeUint64(capture.detail.TotalTokens),
	}
}

func splitProviderModel(value string) (string, string) {
	value = strings.TrimSpace(value)
	provider, model, ok := strings.Cut(value, "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return "", value
	}
	return strings.TrimSpace(provider), strings.TrimSpace(model)
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func metadataBoolDefault(metadata map[string]any, key string, fallback bool) bool {
	if metadata == nil {
		return fallback
	}
	value, ok := metadata[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func firstGJSONNode(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if node := root.Get(path); node.Exists() {
			return node
		}
	}
	return gjson.Result{}
}

func firstGJSONText(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstGJSONInt(root gjson.Result, paths ...string) int64 {
	for _, path := range paths {
		if node := root.Get(path); node.Exists() {
			value := node.Int()
			if value > 0 {
				return value
			}
		}
	}
	return 0
}

func gjsonSum(root gjson.Result, paths ...string) int64 {
	values := make([]int64, 0, len(paths))
	for _, path := range paths {
		values = append(values, firstGJSONInt(root, path))
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

func hasUsageTokens(detail pluginapi.UsageDetail) bool {
	return detail.InputTokens > 0 || detail.OutputTokens > 0 || detail.ReasoningTokens > 0 || detail.CachedTokens > 0 ||
		detail.CacheReadTokens > 0 || detail.CacheCreationTokens > 0 || detail.TotalTokens > 0
}

func maxInt64(left, right int64) int64 {
	if right > left {
		return right
	}
	return left
}

func durationBetween(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func (p *modelRouterPlugin) recordUsageFallback(mark attributionMark, capture directUsageCapture) {
	if p == nil || p.store == nil || p.attribution == nil {
		return
	}
	marker, claimed := p.attribution.claim(mark)
	if claimed {
		_ = p.store.Record(capture.storedRecord(marker))
	}
}
