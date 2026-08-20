package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	attributionRouted     = "routed"
	attributionDirect     = "direct"
	attributionUnresolved = "unattributed"

	attributionWindow    = 5 * time.Second
	attributionRetention = 24 * time.Hour
	fallbackRetention    = 30 * time.Second
	attributionPruneRate = time.Second
	maxAttributionMarks  = 50_000
)

type attributionResult struct {
	Kind        string
	RouterModel string
	Suppress    bool
}

type attributionMark struct {
	id uint64
}

type attributionMarker struct {
	id            uint64
	requestID     string
	direct        bool
	routerModel   string
	providerModel string
	startedAt     time.Time
	credential    [sha256.Size]byte
	hasCredential bool
	fallback      bool
	fallbackAt    time.Time
	capture       directUsageCapture
}

type attributionTracker struct {
	mu        sync.Mutex
	secret    [sha256.Size]byte
	markers   []attributionMarker
	nextID    uint64
	lastPrune time.Time
	now       func() time.Time
}

func newAttributionTracker(now func() time.Time) *attributionTracker {
	tracker := &attributionTracker{now: now}
	if tracker.now == nil {
		tracker.now = time.Now
	}
	if _, err := rand.Read(tracker.secret[:]); err != nil {
		stamp := tracker.now().UTC().AppendFormat(nil, time.RFC3339Nano)
		tracker.secret = sha256.Sum256(stamp)
	}
	return tracker
}

func (tracker *attributionTracker) MarkDirect(request pluginapi.ModelRouteRequest) attributionMark {
	if tracker == nil {
		return attributionMark{}
	}
	return tracker.add(true, "", request.RequestedModel, request.Headers, "", directUsageCapture{})
}

func (tracker *attributionTracker) MarkRouted(routerModel, providerModel string, headers http.Header, captures ...directUsageCapture) attributionMark {
	if tracker == nil {
		return attributionMark{}
	}
	var capture directUsageCapture
	if len(captures) > 0 {
		capture = captures[0]
	}
	return tracker.add(false, routerModel, providerModel, headers, "", capture)
}

func (tracker *attributionTracker) MarkDirectRequest(request pluginapi.RequestInterceptRequest, capture directUsageCapture) attributionMark {
	if tracker == nil {
		return attributionMark{}
	}
	return tracker.add(true, "", firstNonEmpty(request.Model, request.RequestedModel), request.Headers, request.RequestID, capture)
}

func (tracker *attributionTracker) add(direct bool, routerModel, providerModel string, headers http.Header, requestID string, capture directUsageCapture) attributionMark {
	credential := clientCredential(headers)
	fingerprint, hasCredential := tracker.fingerprint(credential)
	marker := attributionMarker{
		requestID:     strings.TrimSpace(requestID),
		direct:        direct,
		routerModel:   strings.TrimSpace(routerModel),
		providerModel: strings.TrimSpace(providerModel),
		startedAt:     tracker.now().UTC(),
		credential:    fingerprint,
		hasCredential: hasCredential,
		capture:       capture,
	}
	tracker.mu.Lock()
	tracker.pruneLocked(marker.startedAt)
	tracker.nextID++
	marker.id = tracker.nextID
	tracker.markers = append(tracker.markers, marker)
	if overflow := len(tracker.markers) - maxAttributionMarks; overflow > 0 {
		copy(tracker.markers, tracker.markers[overflow:])
		tracker.markers = tracker.markers[:maxAttributionMarks]
	}
	tracker.mu.Unlock()
	return attributionMark{id: marker.id}
}

func (tracker *attributionTracker) Match(record pluginapi.UsageRecord) attributionResult {
	if tracker == nil {
		return attributionResult{Kind: attributionUnresolved}
	}
	hasRequestedAt := !record.RequestedAt.IsZero()
	requestedAt := record.RequestedAt.UTC()
	if !hasRequestedAt {
		requestedAt = tracker.now().UTC()
	}
	fingerprint, hasCredential := tracker.fingerprint(strings.TrimSpace(record.APIKey))
	models := compactModels(record.Model, record.Alias)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.pruneLocked(tracker.now().UTC())
	indexes := tracker.matchingIndexesLocked(requestedAt, fingerprint, hasCredential, models, false, hasRequestedAt)
	if len(indexes) == 0 {
		indexes = tracker.matchingIndexesLocked(requestedAt, fingerprint, hasCredential, models, true, hasRequestedAt)
	}
	if len(indexes) == 0 {
		return attributionResult{Kind: attributionUnresolved}
	}
	if !hasRequestedAt {
		// A timestamp-less record cannot distinguish an active marker from a fallback tombstone.
		fallbackIndexes := make([]int, 0, len(indexes))
		for _, candidate := range indexes {
			if tracker.markers[candidate].fallback {
				fallbackIndexes = append(fallbackIndexes, candidate)
			}
		}
		if len(fallbackIndexes) > 0 {
			index := tracker.closestIndexLocked(fallbackIndexes, requestedAt)
			closest := tracker.equallyCloseIndexesLocked(fallbackIndexes, requestedAt, index)
			if tracker.conflictingIndexesLocked(closest) {
				tracker.removeIndexesLocked(closest)
				return attributionResult{Kind: attributionUnresolved}
			}
			tracker.removeIndexesLocked([]int{index})
			return attributionResult{Suppress: true}
		}
		activeIndexes := make([]int, 0, len(indexes))
		for _, candidate := range indexes {
			if !tracker.markers[candidate].fallback {
				activeIndexes = append(activeIndexes, candidate)
			}
		}
		if len(activeIndexes) > 1 {
			return attributionResult{Kind: attributionUnresolved}
		}
	}
	index := tracker.closestIndexLocked(indexes, requestedAt)
	closest := tracker.equallyCloseIndexesLocked(indexes, requestedAt, index)
	if tracker.conflictingIndexesLocked(closest) {
		tracker.removeIndexesLocked(closest)
		return attributionResult{Kind: attributionUnresolved}
	}
	marker := tracker.markers[index]
	tracker.removeIndexesLocked([]int{index})
	if marker.fallback {
		return attributionResult{Suppress: true}
	}
	if marker.direct {
		return attributionResult{Kind: attributionDirect}
	}
	return attributionResult{Kind: attributionRouted, RouterModel: marker.routerModel}
}

func (tracker *attributionTracker) equallyCloseIndexesLocked(indexes []int, requestedAt time.Time, closest int) []int {
	delta := absoluteDuration(tracker.markers[closest].startedAt.Sub(requestedAt))
	result := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if absoluteDuration(tracker.markers[index].startedAt.Sub(requestedAt)) == delta {
			result = append(result, index)
		}
	}
	return result
}

func (tracker *attributionTracker) conflictingIndexesLocked(indexes []int) bool {
	if len(indexes) < 2 {
		return false
	}
	first := tracker.markers[indexes[0]]
	for _, index := range indexes[1:] {
		marker := tracker.markers[index]
		if marker.direct != first.direct || !equalFold(marker.routerModel, first.routerModel) {
			return true
		}
	}
	return false
}

func (tracker *attributionTracker) removeIndexesLocked(indexes []int) {
	remove := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		remove[index] = struct{}{}
	}
	kept := tracker.markers[:0]
	for index, marker := range tracker.markers {
		if _, found := remove[index]; !found {
			kept = append(kept, marker)
		}
	}
	for index := len(kept); index < len(tracker.markers); index++ {
		tracker.markers[index] = attributionMarker{}
	}
	tracker.markers = kept
}

func (tracker *attributionTracker) closestIndexLocked(indexes []int, requestedAt time.Time) int {
	closest := indexes[0]
	closestDelta := absoluteDuration(tracker.markers[closest].startedAt.Sub(requestedAt))
	for _, index := range indexes[1:] {
		delta := absoluteDuration(tracker.markers[index].startedAt.Sub(requestedAt))
		if delta < closestDelta || (delta == closestDelta && tracker.markers[index].id < tracker.markers[closest].id) {
			closest = index
			closestDelta = delta
		}
	}
	return closest
}

func absoluteDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func (tracker *attributionTracker) updateDirectRequest(request pluginapi.RequestInterceptRequest) {
	if tracker == nil || strings.TrimSpace(request.RequestID) == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for index := len(tracker.markers) - 1; index >= 0; index-- {
		marker := &tracker.markers[index]
		if marker.direct && !marker.fallback && marker.requestID == request.RequestID {
			marker.providerModel = firstNonEmpty(strings.TrimSpace(request.Model), marker.providerModel)
			marker.capture.updateRequest(request)
			return
		}
	}
}

func (tracker *attributionTracker) observeDirectResponse(request pluginapi.ResponseInterceptRequest) {
	if tracker == nil || strings.TrimSpace(request.RequestID) == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if marker := tracker.directMarkerLocked(request.RequestID); marker != nil && !marker.fallback {
		marker.capture.observeResponse(request, tracker.now().UTC())
	}
}

func (tracker *attributionTracker) observeDirectStream(request pluginapi.StreamChunkInterceptRequest) {
	if tracker == nil || strings.TrimSpace(request.RequestID) == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if marker := tracker.directMarkerLocked(request.RequestID); marker != nil && !marker.fallback {
		marker.capture.observeStream(request, tracker.now().UTC())
	}
}

func (tracker *attributionTracker) claim(mark attributionMark) (attributionMarker, bool) {
	if tracker == nil || mark.id == 0 {
		return attributionMarker{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for index := range tracker.markers {
		marker := &tracker.markers[index]
		if marker.id == mark.id && !marker.fallback {
			marker.fallback = true
			marker.fallbackAt = tracker.now().UTC()
			return *marker, true
		}
	}
	return attributionMarker{}, false
}

func (tracker *attributionTracker) completeDirect(completion pluginapi.RequestCompletion) (attributionMarker, bool) {
	if tracker == nil || strings.TrimSpace(completion.RequestID) == "" {
		return attributionMarker{}, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	marker := tracker.directMarkerLocked(completion.RequestID)
	if marker == nil || marker.fallback {
		return attributionMarker{}, false
	}
	marker.capture.complete(completion)
	marker.fallback = true
	marker.fallbackAt = tracker.now().UTC()
	return *marker, true
}

func (tracker *attributionTracker) directMarkerLocked(requestID string) *attributionMarker {
	for index := len(tracker.markers) - 1; index >= 0; index-- {
		marker := &tracker.markers[index]
		if marker.direct && marker.requestID == requestID {
			return marker
		}
	}
	return nil
}

func (tracker *attributionTracker) matchingIndexesLocked(requestedAt time.Time, fingerprint [sha256.Size]byte, hasCredential bool, models []string, stripSuffix, enforceWindow bool) []int {
	indexes := make([]int, 0, 2)
	for index, marker := range tracker.markers {
		if marker.hasCredential != hasCredential || (hasCredential && !hmac.Equal(marker.credential[:], fingerprint[:])) {
			continue
		}
		if enforceWindow {
			delta := marker.startedAt.Sub(requestedAt)
			if delta < -attributionWindow || delta > attributionWindow {
				continue
			}
		}
		markerModel := normalizedAttributionModel(marker.providerModel, stripSuffix)
		for _, model := range models {
			if strings.EqualFold(markerModel, normalizedAttributionModel(model, stripSuffix)) {
				indexes = append(indexes, index)
				break
			}
		}
	}
	return indexes
}

func (tracker *attributionTracker) fingerprint(value string) ([sha256.Size]byte, bool) {
	var result [sha256.Size]byte
	value = strings.TrimSpace(value)
	if value == "" {
		return result, false
	}
	digest := hmac.New(sha256.New, tracker.secret[:])
	_, _ = digest.Write([]byte(value))
	copy(result[:], digest.Sum(nil))
	return result, true
}

func (tracker *attributionTracker) pruneLocked(now time.Time) {
	if !tracker.lastPrune.IsZero() && now.Sub(tracker.lastPrune) < attributionPruneRate {
		return
	}
	tracker.lastPrune = now
	activeCutoff := now.Add(-attributionRetention)
	fallbackCutoff := now.Add(-fallbackRetention)
	kept := tracker.markers[:0]
	for _, marker := range tracker.markers {
		expired := marker.startedAt.Before(activeCutoff)
		if marker.fallback {
			expired = marker.fallbackAt.IsZero() || marker.fallbackAt.Before(fallbackCutoff)
		}
		if !expired {
			kept = append(kept, marker)
		}
	}
	for index := len(kept); index < len(tracker.markers); index++ {
		tracker.markers[index] = attributionMarker{}
	}
	tracker.markers = kept
}

func clientCredential(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if authorization := strings.TrimSpace(headers.Get("Authorization")); authorization != "" {
		if scheme, value, ok := strings.Cut(authorization, " "); ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
			return strings.TrimSpace(value)
		}
		return authorization
	}
	for _, name := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func compactModels(values ...string) []string {
	models := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, value)
	}
	return models
}

func normalizedAttributionModel(value string, stripSuffix bool) string {
	value = strings.TrimSpace(value)
	if stripSuffix {
		value, _, _ = splitThinkingSuffix(value)
	}
	return value
}

func maskAPIKey(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) < 8 {
		return "******"
	}
	return string(runes[:2]) + "******" + string(runes[len(runes)-2:])
}
