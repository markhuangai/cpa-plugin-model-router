package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

var (
	usageMetaBucket     = []byte("meta")
	usageRequestsBucket = []byte("requests")
	usageMinutesBucket  = []byte("minutes")

	usageSchemaKey      = []byte("schema_version")
	usageSequenceKey    = []byte("next_sequence")
	usageLastPruneKey   = []byte("last_prune_day")
	usagePricesKey      = []byte("prices")
	usagePreferencesKey = []byte("preferences")

	errPriceRevisionConflict = errors.New("model price revision conflict")
)

type usageStore struct {
	mu            sync.RWMutex
	db            *bolt.DB
	path          string
	retentionDays int
	errorMu       sync.RWMutex
	lastError     string
}

type minuteAggregate struct {
	Minute        time.Time `json:"minute"`
	Attribution   string    `json:"attribution"`
	RouterModel   string    `json:"router_model,omitempty"`
	ProviderModel string    `json:"provider_model"`
	Provider      string    `json:"provider,omitempty"`
	Source        string    `json:"source,omitempty"`
	ServiceTier   string    `json:"service_tier,omitempty"`
	Result        string    `json:"result"`
	usageCounters
	LatencyTotal uint64 `json:"latency_total_ns"`
	TTFTTotal    uint64 `json:"ttft_total_ns"`
}

func openUsageStore(path string, retentionDays int) (*usageStore, error) {
	store := &usageStore{}
	if err := store.Reconfigure(path, retentionDays); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *usageStore) Reconfigure(path string, retentionDays int) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" {
		return errors.New("data_path must not be empty")
	}
	if retentionDays < 1 || retentionDays > maxRetentionDays {
		return fmt.Errorf("retention_days must be between 1 and %d", maxRetentionDays)
	}
	store.mu.RLock()
	samePath := store.db != nil && store.path == path
	store.mu.RUnlock()
	if samePath {
		store.mu.Lock()
		store.retentionDays = retentionDays
		err := pruneUsageDatabase(store.db, retentionDays, time.Now().UTC(), true)
		store.mu.Unlock()
		if err != nil {
			store.recordError(err)
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create usage data directory: %w", err)
	}
	database, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return fmt.Errorf("open usage database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return fmt.Errorf("restrict usage database permissions: %w", err)
	}
	if err := initializeUsageDatabase(database); err != nil {
		_ = database.Close()
		return err
	}
	if err := pruneUsageDatabase(database, retentionDays, time.Now().UTC(), true); err != nil {
		_ = database.Close()
		return err
	}
	store.mu.Lock()
	previous := store.db
	store.db = database
	store.path = path
	store.retentionDays = retentionDays
	store.mu.Unlock()
	if previous != nil {
		if err := previous.Close(); err != nil {
			store.recordError(fmt.Errorf("close previous usage database: %w", err))
		}
	}
	store.clearError()
	return nil
}

func initializeUsageDatabase(database *bolt.DB) error {
	return database.Update(func(transaction *bolt.Tx) error {
		meta, err := transaction.CreateBucketIfNotExists(usageMetaBucket)
		if err != nil {
			return err
		}
		for _, name := range [][]byte{usageRequestsBucket, usageMinutesBucket} {
			if _, err := transaction.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if raw := meta.Get(usageSchemaKey); len(raw) > 0 {
			version := binary.BigEndian.Uint32(paddedUint32(raw))
			if version > usageSchemaVersion {
				return fmt.Errorf("usage database schema %d is newer than supported schema %d", version, usageSchemaVersion)
			}
			if version < usageSchemaVersion {
				return fmt.Errorf("usage database schema %d requires an unsupported migration", version)
			}
			return nil
		}
		var version [4]byte
		binary.BigEndian.PutUint32(version[:], usageSchemaVersion)
		return meta.Put(usageSchemaKey, version[:])
	})
}

func paddedUint32(raw []byte) []byte {
	if len(raw) >= 4 {
		return raw[len(raw)-4:]
	}
	result := make([]byte, 4)
	copy(result[4-len(raw):], raw)
	return result
}

func (store *usageStore) Record(record storedUsageRecord) error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return errors.New("usage database is closed")
	}
	err := store.db.Update(func(transaction *bolt.Tx) error {
		meta := transaction.Bucket(usageMetaBucket)
		requests := transaction.Bucket(usageRequestsBucket)
		minutes := transaction.Bucket(usageMinutesBucket)
		sequence := decodeUint64(meta.Get(usageSequenceKey)) + 1
		record.Sequence = sequence
		var encodedSequence [8]byte
		binary.BigEndian.PutUint64(encodedSequence[:], sequence)
		if err := meta.Put(usageSequenceKey, encodedSequence[:]); err != nil {
			return err
		}
		key := usageRequestKey(record.RequestedAt, sequence)
		value, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := requests.Put(key, value); err != nil {
			return err
		}
		if err := updateMinuteAggregate(minutes, record); err != nil {
			return err
		}
		return pruneUsageTransaction(transaction, store.retentionDays, time.Now().UTC(), false)
	})
	if err != nil {
		store.recordError(err)
		log.Printf("model-router: persist usage: %v", err)
	} else {
		store.clearError()
	}
	return err
}

func updateMinuteAggregate(bucket *bolt.Bucket, record storedUsageRecord) error {
	minute := record.RequestedAt.UTC().Truncate(time.Minute)
	dimensions, err := json.Marshal([]string{record.Attribution, record.RouterModel, record.ProviderModel, record.Provider, record.Source, record.ServiceTier, record.result()})
	if err != nil {
		return err
	}
	key := append(timestampPrefix(minute), dimensions...)
	aggregate := minuteAggregate{
		Minute: minute, Attribution: record.Attribution, RouterModel: record.RouterModel, ProviderModel: record.ProviderModel,
		Provider: record.Provider, Source: record.Source, ServiceTier: record.ServiceTier, Result: record.result(),
	}
	if raw := bucket.Get(key); len(raw) > 0 {
		if err := json.Unmarshal(raw, &aggregate); err != nil {
			return fmt.Errorf("decode minute aggregate: %w", err)
		}
	}
	aggregate.usageCounters.add(record)
	aggregate.LatencyTotal += record.LatencyNS
	aggregate.TTFTTotal += record.TTFTNS
	value, err := json.Marshal(aggregate)
	if err != nil {
		return err
	}
	return bucket.Put(key, value)
}

func usageRequestKey(requestedAt time.Time, sequence uint64) []byte {
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(requestedAt.UTC().UnixNano()))
	binary.BigEndian.PutUint64(key[8:], sequence)
	return key
}

func timestampPrefix(value time.Time) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(value.UTC().UnixNano()))
	return key
}

func decodeUint64(raw []byte) uint64 {
	if len(raw) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw[len(raw)-8:])
}

func pruneUsageDatabase(database *bolt.DB, retentionDays int, now time.Time, force bool) error {
	return database.Update(func(transaction *bolt.Tx) error {
		return pruneUsageTransaction(transaction, retentionDays, now, force)
	})
}

func pruneUsageTransaction(transaction *bolt.Tx, retentionDays int, now time.Time, force bool) error {
	meta := transaction.Bucket(usageMetaBucket)
	today := now.UTC().Format("2006-01-02")
	if !force && string(meta.Get(usageLastPruneKey)) == today {
		return nil
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays)
	for _, name := range [][]byte{usageRequestsBucket, usageMinutesBucket} {
		bucket := transaction.Bucket(name)
		cursor := bucket.Cursor()
		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			if len(key) < 8 || int64(binary.BigEndian.Uint64(key[:8])) >= cutoff.UnixNano() {
				break
			}
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
	}
	return meta.Put(usageLastPruneKey, []byte(today))
}

func (store *usageStore) records(filter usageFilter) ([]storedUsageRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return nil, errors.New("usage database is closed")
	}
	records := make([]storedUsageRecord, 0)
	err := store.db.View(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(usageRequestsBucket)
		cursor := bucket.Cursor()
		var key, value []byte
		if filter.From.IsZero() {
			key, value = cursor.First()
		} else {
			key, value = cursor.Seek(timestampPrefix(filter.From))
		}
		for ; key != nil; key, value = cursor.Next() {
			if len(key) < 8 {
				continue
			}
			timestamp := time.Unix(0, int64(binary.BigEndian.Uint64(key[:8]))).UTC()
			if !filter.To.IsZero() && !timestamp.Before(filter.To) {
				break
			}
			var record storedUsageRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return fmt.Errorf("decode usage record: %w", err)
			}
			if filter.matches(record) {
				records = append(records, record)
			}
		}
		return nil
	})
	if err != nil {
		store.recordError(err)
	}
	return records, err
}

func (store *usageStore) ResetUsage() error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return errors.New("usage database is closed")
	}
	err := store.db.Update(func(transaction *bolt.Tx) error {
		for _, name := range [][]byte{usageRequestsBucket, usageMinutesBucket} {
			if err := transaction.DeleteBucket(name); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
				return err
			}
			if _, err := transaction.CreateBucket(name); err != nil {
				return err
			}
		}
		meta := transaction.Bucket(usageMetaBucket)
		if err := meta.Delete(usageSequenceKey); err != nil {
			return err
		}
		return meta.Delete(usageLastPruneKey)
	})
	if err != nil {
		store.recordError(err)
	}
	return err
}

func (store *usageStore) QueryPriceBook() (modelPriceBook, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return modelPriceBook{}, errors.New("usage database is closed")
	}
	book := emptyModelPriceBook()
	err := store.db.View(func(transaction *bolt.Tx) error {
		raw := transaction.Bucket(usageMetaBucket).Get(usagePricesKey)
		if len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, &book)
	})
	if book.Prices == nil {
		book.Prices = map[string]modelPrice{}
	}
	return book, err
}

func (store *usageStore) SavePriceBook(request saveModelPricesRequest, now time.Time) (modelPriceBook, error) {
	normalizedSettings, err := normalizePriceSyncSettings(request.SyncSettings)
	if err != nil {
		return modelPriceBook{}, err
	}
	normalizedPrices, err := normalizeModelPrices(request.Prices, now)
	if err != nil {
		return modelPriceBook{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return modelPriceBook{}, errors.New("usage database is closed")
	}
	result := modelPriceBook{}
	err = store.db.Update(func(transaction *bolt.Tx) error {
		current, err := priceBookFromTransaction(transaction)
		if err != nil {
			return err
		}
		if current.Revision != request.Revision {
			return errPriceRevisionConflict
		}
		for model, price := range normalizedPrices {
			if previous, exists := current.Prices[model]; exists && previous.Source == priceSourceModelsDev && sameBillablePrice(previous, price) {
				price.Source = previous.Source
				price.CatalogProvider = previous.CatalogProvider
				price.CatalogModel = previous.CatalogModel
				price.UpdatedAt = previous.UpdatedAt
			} else {
				price.Source = priceSourceManual
				price.CatalogProvider = ""
				price.CatalogModel = ""
				price.UpdatedAt = now.UTC()
			}
			normalizedPrices[model] = price
		}
		result = modelPriceBook{SchemaVersion: usageSchemaVersion, Revision: current.Revision + 1, Prices: normalizedPrices, SyncSettings: normalizedSettings, LastSync: current.LastSync}
		return putJSON(transaction.Bucket(usageMetaBucket), usagePricesKey, result)
	})
	return result, err
}

func (store *usageStore) ApplyPriceSync(prices map[string]modelPrice, settings priceSyncSettings, metadata priceSyncMetadata, revision uint64) (modelPriceBook, error) {
	now := time.Now().UTC()
	normalizedPrices, err := normalizeModelPrices(prices, now)
	if err != nil {
		return modelPriceBook{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return modelPriceBook{}, errors.New("usage database is closed")
	}
	result := modelPriceBook{}
	err = store.db.Update(func(transaction *bolt.Tx) error {
		current, err := priceBookFromTransaction(transaction)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return errPriceRevisionConflict
		}
		if current.Prices == nil {
			current.Prices = map[string]modelPrice{}
		}
		current.Prices, err = normalizeModelPrices(current.Prices, now)
		if err != nil {
			return err
		}
		existingByKey := make(map[string]string, len(current.Prices))
		for model := range current.Prices {
			existingByKey[routeKey(model)] = model
		}
		for model, price := range normalizedPrices {
			existingModel, exists := existingByKey[routeKey(model)]
			if exists && current.Prices[existingModel].Source == priceSourceManual {
				metadata.SkippedManual++
				continue
			}
			if exists {
				metadata.Updated++
				current.Prices[existingModel] = price
			} else {
				metadata.Created++
				existingByKey[routeKey(model)] = model
				current.Prices[model] = price
			}
		}
		current.Prices, err = normalizeModelPrices(current.Prices, now)
		if err != nil {
			return err
		}
		current.SchemaVersion = usageSchemaVersion
		current.Revision++
		current.SyncSettings = settings
		current.LastSync = &metadata
		result = current
		return putJSON(transaction.Bucket(usageMetaBucket), usagePricesKey, current)
	})
	return result, err
}

func priceBookFromTransaction(transaction *bolt.Tx) (modelPriceBook, error) {
	book := emptyModelPriceBook()
	if raw := transaction.Bucket(usageMetaBucket).Get(usagePricesKey); len(raw) > 0 {
		if err := json.Unmarshal(raw, &book); err != nil {
			return modelPriceBook{}, err
		}
	}
	if book.Prices == nil {
		book.Prices = map[string]modelPrice{}
	}
	return book, nil
}

func (store *usageStore) QueryPreferences() (dashboardPreferences, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	preferences := defaultDashboardPreferences()
	if store.db == nil {
		return dashboardPreferences{}, errors.New("usage database is closed")
	}
	err := store.db.View(func(transaction *bolt.Tx) error {
		if raw := transaction.Bucket(usageMetaBucket).Get(usagePreferencesKey); len(raw) > 0 {
			return json.Unmarshal(raw, &preferences)
		}
		return nil
	})
	return preferences, err
}

func (store *usageStore) SavePreferences(input dashboardPreferences) (dashboardPreferences, error) {
	preferences, err := normalizeDashboardPreferences(input)
	if err != nil {
		return dashboardPreferences{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return dashboardPreferences{}, errors.New("usage database is closed")
	}
	err = store.db.Update(func(transaction *bolt.Tx) error {
		return putJSON(transaction.Bucket(usageMetaBucket), usagePreferencesKey, preferences)
	})
	return preferences, err
}

func putJSON(bucket *bolt.Bucket, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, raw)
}

func (store *usageStore) RetainedSince(now time.Time) time.Time {
	store.mu.RLock()
	days := store.retentionDays
	store.mu.RUnlock()
	return now.UTC().AddDate(0, 0, -days)
}

func (store *usageStore) LastError() string {
	store.errorMu.RLock()
	defer store.errorMu.RUnlock()
	return store.lastError
}

func (store *usageStore) recordError(err error) {
	if err == nil {
		return
	}
	store.errorMu.Lock()
	store.lastError = err.Error()
	store.errorMu.Unlock()
}

func (store *usageStore) clearError() {
	store.errorMu.Lock()
	store.lastError = ""
	store.errorMu.Unlock()
}

func (store *usageStore) Close() error {
	store.mu.Lock()
	database := store.db
	store.db = nil
	store.mu.Unlock()
	if database == nil {
		return nil
	}
	return database.Close()
}

func storedRecordFromUsage(record pluginapi.UsageRecord, attribution attributionResult) storedUsageRecord {
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
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
		InputTokens:         nonnegativeUint64(record.Detail.InputTokens),
		OutputTokens:        nonnegativeUint64(record.Detail.OutputTokens),
		ReasoningTokens:     nonnegativeUint64(record.Detail.ReasoningTokens),
		CachedTokens:        nonnegativeUint64(record.Detail.CachedTokens),
		CacheReadTokens:     nonnegativeUint64(record.Detail.CacheReadTokens),
		CacheCreationTokens: nonnegativeUint64(record.Detail.CacheCreationTokens),
		TotalTokens:         nonnegativeUint64(record.Detail.TotalTokens),
	}
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
		case character == '-' || character == '_' || character == '.' || character == '+' || character == '/' || character == '=' || character == ':' || character == '@':
		default:
			return false
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
