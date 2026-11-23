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
	attributionPruneRate = time.Second
	maxAttributionMarks  = 50_000
)

type attributionResult struct {
	Kind         string
	RouterModel  string
	Target       string
	RouteAttempt int
	Operation    string
	Reason       string
}

type attributionMarker struct {
	id            uint64
	requestID     string
	direct        bool
	routerModel   string
	target        string
	providerModel string
	routeAttempt  int
	operation     string
	startedAt     time.Time
	finishedAt    time.Time
	credential    [sha256.Size]byte
	hasCredential bool
}

type attributionKey struct {
	model         string
	credential    [sha256.Size]byte
	hasCredential bool
}

type attributionTracker struct {
	mu         sync.Mutex
	secret     [sha256.Size]byte
	markers    map[uint64]*attributionMarker
	byModel    map[attributionKey]map[uint64]*attributionMarker
	directByID map[string]*attributionMarker
	firstID    uint64
	nextID     uint64
	lastPrune  time.Time
	now        func() time.Time
}

func newAttributionTracker(now func() time.Time) *attributionTracker {
	tracker := &attributionTracker{
		markers:    make(map[uint64]*attributionMarker),
		byModel:    make(map[attributionKey]map[uint64]*attributionMarker),
		directByID: make(map[string]*attributionMarker),
		firstID:    1,
		now:        now,
	}
	if tracker.now == nil {
		tracker.now = time.Now
	}
	if _, err := rand.Read(tracker.secret[:]); err != nil {
		stamp := tracker.now().UTC().AppendFormat(nil, time.RFC3339Nano)
		tracker.secret = sha256.Sum256(stamp)
	}
	return tracker
}

func (tracker *attributionTracker) MarkDirect(request pluginapi.ModelRouteRequest) {
	tracker.add(attributionMarker{direct: true, target: request.RequestedModel}, request.Headers)
}

func (tracker *attributionTracker) MarkRouted(routerModel, target string, headers http.Header, attempt int, operation string) uint64 {
	return tracker.add(attributionMarker{routerModel: routerModel, target: target, routeAttempt: attempt, operation: operation}, headers)
}

func (tracker *attributionTracker) MarkDirectRequest(request pluginapi.RequestInterceptRequest) {
	tracker.add(attributionMarker{
		direct:        true,
		requestID:     strings.TrimSpace(request.RequestID),
		target:        firstNonEmpty(strings.TrimSpace(request.RequestedModel), request.Model),
		providerModel: strings.TrimSpace(request.Model),
	}, request.Headers)
}

func (tracker *attributionTracker) FinishRouted(id uint64) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if marker := tracker.markers[id]; marker != nil {
		marker.finishedAt = tracker.now().UTC()
	}
}

func (tracker *attributionTracker) add(marker attributionMarker, headers http.Header) uint64 {
	if tracker == nil {
		return 0
	}
	marker.target = strings.TrimSpace(marker.target)
	marker.credential, marker.hasCredential = tracker.fingerprint(clientCredential(headers))
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	marker.startedAt = tracker.now().UTC()
	tracker.pruneLocked(marker.startedAt)
	tracker.nextID++
	marker.id = tracker.nextID
	tracker.markers[marker.id] = &marker
	tracker.indexMarker(&marker)
	if marker.requestID != "" {
		tracker.directByID[marker.requestID] = &marker
	}
	for len(tracker.markers) > maxAttributionMarks {
		tracker.removeMarker(tracker.markers[tracker.firstID])
		tracker.firstID++
	}
	return marker.id
}

func (tracker *attributionTracker) Match(record pluginapi.UsageRecord) attributionResult {
	if tracker == nil {
		return attributionResult{Kind: attributionUnresolved, Reason: "tracker_unavailable"}
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
	var marker *attributionMarker
	var attempt int
	var operation string
	reason := "route_marker"
	for _, model := range models {
		key := attributionKey{model: model, credential: fingerprint, hasCredential: hasCredential}
		for _, candidate := range tracker.byModel[key] {
			if candidate == marker || (hasRequestedAt && !candidate.includes(requestedAt)) {
				continue
			}
			if marker == nil {
				marker, attempt, operation = candidate, candidate.routeAttempt, candidate.operation
				continue
			}
			if !hasRequestedAt || !marker.sameRoute(candidate) {
				return attributionResult{Kind: attributionUnresolved, Reason: "conflicting_markers"}
			}
			reason = "route_consensus"
			if attempt != candidate.routeAttempt {
				attempt = 0
			}
			if operation != candidate.operation {
				operation = ""
			}
			if candidate.startedAt.After(marker.startedAt) ||
				(candidate.startedAt.Equal(marker.startedAt) && candidate.id < marker.id) {
				marker = candidate
			}
		}
	}
	if marker == nil {
		return attributionResult{Kind: attributionUnresolved, Reason: "no_matching_marker"}
	}
	if marker.direct {
		tracker.removeMarker(marker)
		return attributionResult{Kind: attributionDirect, Target: marker.target, Reason: "direct_marker"}
	}
	// One host dispatch can yield multiple official records when CPA retries internally.
	if !hasRequestedAt {
		attempt = 0
	}
	return attributionResult{
		Kind: attributionRouted, RouterModel: marker.routerModel, Target: marker.target,
		RouteAttempt: attempt, Operation: operation, Reason: reason,
	}
}

func (marker *attributionMarker) sameRoute(other *attributionMarker) bool {
	return marker.direct == other.direct && equalFold(marker.routerModel, other.routerModel) && equalFold(marker.target, other.target)
}

func (tracker *attributionTracker) indexMarker(marker *attributionMarker) {
	for _, model := range compactModels(marker.target, marker.providerModel) {
		key := attributionKey{model: model, credential: marker.credential, hasCredential: marker.hasCredential}
		if tracker.byModel[key] == nil {
			tracker.byModel[key] = make(map[uint64]*attributionMarker)
		}
		tracker.byModel[key][marker.id] = marker
	}
}

func (tracker *attributionTracker) unindexMarker(marker *attributionMarker) {
	for _, model := range compactModels(marker.target, marker.providerModel) {
		key := attributionKey{model: model, credential: marker.credential, hasCredential: marker.hasCredential}
		bucket := tracker.byModel[key]
		delete(bucket, marker.id)
		if len(bucket) == 0 {
			delete(tracker.byModel, key)
		}
	}
}

func (tracker *attributionTracker) removeMarker(marker *attributionMarker) {
	if marker == nil {
		return
	}
	delete(tracker.markers, marker.id)
	tracker.unindexMarker(marker)
	if tracker.directByID[marker.requestID] == marker {
		delete(tracker.directByID, marker.requestID)
	}
}

func (tracker *attributionTracker) updateDirectRequest(request pluginapi.RequestInterceptRequest) {
	requestID := strings.TrimSpace(request.RequestID)
	if tracker == nil || requestID == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if marker := tracker.directByID[requestID]; marker != nil {
		tracker.unindexMarker(marker)
		marker.providerModel = firstNonEmpty(strings.TrimSpace(request.Model), marker.providerModel)
		tracker.indexMarker(marker)
	}
}

func (marker attributionMarker) includes(requestedAt time.Time) bool {
	if requestedAt.Before(marker.startedAt) {
		return false
	}
	if marker.direct {
		return requestedAt.Sub(marker.startedAt) <= attributionWindow
	}
	return marker.finishedAt.IsZero() || !requestedAt.After(marker.finishedAt)
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
	for tracker.firstID <= tracker.nextID {
		marker := tracker.markers[tracker.firstID]
		if marker != nil && !marker.startedAt.Before(activeCutoff) {
			break
		}
		tracker.removeMarker(marker)
		tracker.firstID++
	}
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
		value = normalizedAttributionModel(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		models = append(models, value)
	}
	return models
}

func normalizedAttributionModel(value string) string {
	value, _, _ = splitThinkingSuffix(value)
	return strings.ToLower(value)
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
