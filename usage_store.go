package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	usageStorageSchemaVersion = 1
	sqliteBusyTimeoutMS       = 5000
)

var errPriceRevisionConflict = errors.New("model price revision conflict")

type usageStore struct {
	mu            sync.RWMutex
	db            *sql.DB
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
		err := pruneSQLiteDatabase(store.db, retentionDays, time.Now().UTC(), true)
		store.mu.Unlock()
		if err != nil {
			store.recordError(err)
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create usage data directory: %w", err)
	}
	database, err := openUsageDatabase(path)
	if err != nil {
		return err
	}
	if err := pruneSQLiteDatabase(database, retentionDays, time.Now().UTC(), true); err != nil {
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

func openUsageDatabase(path string) (*sql.DB, error) {
	markerPath := path + ".migration-v1.json"
	if _, err := os.Stat(markerPath); err == nil {
		if err := migrateLegacyBbolt(path); err != nil {
			return nil, fmt.Errorf("recover legacy usage migration: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect usage migration marker: %w", err)
	}
	temporaryPath := path + ".sqlite-v1.migrating"
	if temporaryKind, err := usageDatabaseKind(temporaryPath); err != nil {
		return nil, err
	} else if temporaryKind != usageDatabaseMissing {
		return nil, fmt.Errorf("usage database has an abandoned migration temporary file %q", temporaryPath)
	}
	kind, err := usageDatabaseKind(path)
	if err != nil {
		return nil, err
	}
	if kind == usageDatabaseLegacyBbolt {
		if err := migrateLegacyBbolt(path); err != nil {
			return nil, fmt.Errorf("migrate legacy usage database: %w", err)
		}
	} else if kind == usageDatabaseUnknown {
		return nil, fmt.Errorf("usage database %q is neither SQLite nor a supported legacy bbolt database", path)
	} else if kind == usageDatabaseMissing {
		backupPath := path + ".bbolt-v1.bak"
		if backupKind, err := usageDatabaseKind(backupPath); err != nil {
			return nil, err
		} else if backupKind != usageDatabaseMissing {
			return nil, fmt.Errorf("usage database primary file is missing while legacy backup %q remains", backupPath)
		}
	}
	return openSQLiteDatabase(path, "WAL")
}

func sqliteDSN(path, journalMode string) string {
	fileURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(sqliteAbsolutePath(path))}
	query := url.Values{}
	query.Set("_busy_timeout", fmt.Sprintf("%d", sqliteBusyTimeoutMS))
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", journalMode)
	query.Set("_synchronous", "FULL")
	query.Set("_txlock", "immediate")
	fileURL.RawQuery = query.Encode()
	return fileURL.String()
}

func sqliteAbsolutePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		return absolute
	}
	return path
}

func openSQLiteDatabase(path string, journalMode string) (*sql.DB, error) {
	database, err := sql.Open("sqlite3", sqliteDSN(path, journalMode))
	if err != nil {
		return nil, fmt.Errorf("open SQLite usage database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("connect SQLite usage database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("restrict usage database permissions: %w", err)
	}
	if err := initializeSQLiteDatabase(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	if journalMode == "WAL" {
		var actual string
		if err := database.QueryRow("PRAGMA journal_mode").Scan(&actual); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("verify SQLite journal mode: %w", err)
		}
		if !strings.EqualFold(actual, "wal") {
			_ = database.Close()
			return nil, fmt.Errorf("SQLite journal mode is %q, want WAL", actual)
		}
	}
	if err := chmodSQLiteSidecars(path); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func initializeSQLiteDatabase(database *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS store_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			storage_schema_version INTEGER NOT NULL,
			next_sequence INTEGER NOT NULL DEFAULT 0,
			last_prune_day TEXT,
			prices_json BLOB,
			preferences_json BLOB
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			sequence INTEGER PRIMARY KEY,
			requested_at_ns INTEGER NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS requests_requested_at_sequence ON requests(requested_at_ns, sequence)`,
		`CREATE TABLE IF NOT EXISTS minute_aggregates (
			key BLOB PRIMARY KEY,
			minute_ns INTEGER NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS minute_aggregates_minute ON minute_aggregates(minute_ns)`,
		`INSERT OR IGNORE INTO store_state(id, storage_schema_version, next_sequence) VALUES (1, ?, 0)`,
	}
	for index, statement := range statements {
		if index == len(statements)-1 {
			if _, err := database.Exec(statement, usageStorageSchemaVersion); err != nil {
				return fmt.Errorf("initialize SQLite state: %w", err)
			}
			continue
		}
		if _, err := database.Exec(statement); err != nil {
			return fmt.Errorf("initialize SQLite schema: %w", err)
		}
	}
	var version int
	if err := database.QueryRow("SELECT storage_schema_version FROM store_state WHERE id = 1").Scan(&version); err != nil {
		return fmt.Errorf("read SQLite storage schema: %w", err)
	}
	if version > usageStorageSchemaVersion {
		return fmt.Errorf("usage database storage schema %d is newer than supported schema %d", version, usageStorageSchemaVersion)
	}
	if version < usageStorageSchemaVersion {
		return fmt.Errorf("usage database storage schema %d requires an unsupported migration", version)
	}
	return nil
}

func chmodSQLiteSidecars(path string) error {
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Chmod(sidecar, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restrict SQLite sidecar permissions: %w", err)
		}
	}
	return nil
}

func (store *usageStore) Record(record storedUsageRecord) error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return errors.New("usage database is closed")
	}
	err := func() error {
		transaction, err := store.db.Begin()
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = transaction.Rollback()
			}
		}()
		var sequence int64
		if err := transaction.QueryRow(`UPDATE store_state SET next_sequence = next_sequence + 1 WHERE id = 1 RETURNING next_sequence`).Scan(&sequence); err != nil {
			return err
		}
		if sequence < 1 {
			return errors.New("usage sequence overflow")
		}
		record.Sequence = uint64(sequence)
		value, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := transaction.Exec(`INSERT INTO requests(sequence, requested_at_ns, payload) VALUES (?, ?, ?)`, sequence, record.RequestedAt.UTC().UnixNano(), value); err != nil {
			return err
		}
		if err := updateMinuteAggregateSQLite(transaction, record); err != nil {
			return err
		}
		if err := pruneSQLiteTransaction(transaction, store.retentionDays, time.Now().UTC(), false); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return err
		}
		committed = true
		return nil
	}()
	if err != nil {
		store.recordError(err)
		log.Printf("model-router: persist usage: %v", err)
	} else {
		store.clearError()
	}
	return err
}

func updateMinuteAggregateSQLite(transaction *sql.Tx, record storedUsageRecord) error {
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
	var raw []byte
	err = transaction.QueryRow(`SELECT payload FROM minute_aggregates WHERE key = ?`, key).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(raw) > 0 {
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
	_, err = transaction.Exec(`INSERT INTO minute_aggregates(key, minute_ns, payload) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET payload = excluded.payload`, key, minute.UnixNano(), value)
	return err
}

func timestampPrefix(value time.Time) []byte {
	key := make([]byte, 8)
	putBigEndianUint64(key, uint64(value.UTC().UnixNano()))
	return key
}

func putBigEndianUint64(destination []byte, value uint64) {
	for index := 7; index >= 0; index-- {
		destination[index] = byte(value)
		value >>= 8
	}
}

func pruneSQLiteDatabase(database *sql.DB, retentionDays int, now time.Time, force bool) error {
	transaction, err := database.Begin()
	if err != nil {
		return err
	}
	if err := pruneSQLiteTransaction(transaction, retentionDays, now, force); err != nil {
		_ = transaction.Rollback()
		return err
	}
	return transaction.Commit()
}

func pruneSQLiteTransaction(transaction *sql.Tx, retentionDays int, now time.Time, force bool) error {
	today := now.UTC().Format("2006-01-02")
	var lastPrune sql.NullString
	if err := transaction.QueryRow(`SELECT last_prune_day FROM store_state WHERE id = 1`).Scan(&lastPrune); err != nil {
		return err
	}
	if !force && lastPrune.Valid && lastPrune.String == today {
		return nil
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays).UnixNano()
	if _, err := transaction.Exec(`DELETE FROM requests WHERE requested_at_ns < ?`, cutoff); err != nil {
		return err
	}
	if _, err := transaction.Exec(`DELETE FROM minute_aggregates WHERE minute_ns < ?`, cutoff); err != nil {
		return err
	}
	_, err := transaction.Exec(`UPDATE store_state SET last_prune_day = ? WHERE id = 1`, today)
	return err
}

func (store *usageStore) records(filter usageFilter) ([]storedUsageRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return nil, errors.New("usage database is closed")
	}
	query := `SELECT requested_at_ns, payload FROM requests`
	args := make([]any, 0, 2)
	conditions := make([]string, 0, 2)
	if !filter.From.IsZero() {
		conditions = append(conditions, "requested_at_ns >= ?")
		args = append(args, filter.From.UTC().UnixNano())
	}
	if !filter.To.IsZero() {
		conditions = append(conditions, "requested_at_ns < ?")
		args = append(args, filter.To.UTC().UnixNano())
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY requested_at_ns, sequence"
	rows, err := store.db.Query(query, args...)
	if err != nil {
		store.recordError(err)
		return nil, err
	}
	defer rows.Close()
	records := make([]storedUsageRecord, 0)
	for rows.Next() {
		var timestamp int64
		var value []byte
		if err := rows.Scan(&timestamp, &value); err != nil {
			return nil, err
		}
		var record storedUsageRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		if filter.matches(record) {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		store.recordError(err)
		return nil, err
	}
	return records, nil
}

func (store *usageStore) ResetUsage() error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return errors.New("usage database is closed")
	}
	transaction, err := store.db.Begin()
	if err == nil {
		_, err = transaction.Exec(`DELETE FROM requests`)
	}
	if err == nil {
		_, err = transaction.Exec(`DELETE FROM minute_aggregates`)
	}
	if err == nil {
		_, err = transaction.Exec(`UPDATE store_state SET next_sequence = 0, last_prune_day = NULL WHERE id = 1`)
	}
	if err != nil {
		if transaction != nil {
			_ = transaction.Rollback()
		}
		store.recordError(err)
		return err
	}
	if err := transaction.Commit(); err != nil {
		store.recordError(err)
		return err
	}
	store.clearError()
	return nil
}

func (store *usageStore) QueryPriceBook() (modelPriceBook, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.db == nil {
		return modelPriceBook{}, errors.New("usage database is closed")
	}
	book := emptyModelPriceBook()
	var raw []byte
	err := store.db.QueryRow(`SELECT prices_json FROM store_state WHERE id = 1`).Scan(&raw)
	if err == nil && len(raw) > 0 {
		err = json.Unmarshal(raw, &book)
	}
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
	transaction, err := store.db.Begin()
	if err != nil {
		return modelPriceBook{}, err
	}
	current, err := priceBookFromSQLiteTransaction(transaction)
	if err == nil && current.Revision != request.Revision {
		err = errPriceRevisionConflict
	}
	if err != nil {
		_ = transaction.Rollback()
		return modelPriceBook{}, err
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
	result := modelPriceBook{SchemaVersion: usageSchemaVersion, Revision: current.Revision + 1, Prices: normalizedPrices, SyncSettings: normalizedSettings, LastSync: current.LastSync}
	value, err := json.Marshal(result)
	if err == nil {
		_, err = transaction.Exec(`UPDATE store_state SET prices_json = ? WHERE id = 1`, value)
	}
	if err == nil {
		err = transaction.Commit()
	} else {
		_ = transaction.Rollback()
	}
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
	transaction, err := store.db.Begin()
	if err != nil {
		return modelPriceBook{}, err
	}
	current, err := priceBookFromSQLiteTransaction(transaction)
	if err == nil && current.Revision != revision {
		err = errPriceRevisionConflict
	}
	if err != nil {
		_ = transaction.Rollback()
		return modelPriceBook{}, err
	}
	if current.Prices == nil {
		current.Prices = map[string]modelPrice{}
	}
	current.Prices, err = normalizeModelPrices(current.Prices, now)
	if err != nil {
		_ = transaction.Rollback()
		return modelPriceBook{}, err
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
		_ = transaction.Rollback()
		return modelPriceBook{}, err
	}
	current.SchemaVersion = usageSchemaVersion
	current.Revision++
	current.SyncSettings = settings
	current.LastSync = &metadata
	result := current
	value, err := json.Marshal(current)
	if err == nil {
		_, err = transaction.Exec(`UPDATE store_state SET prices_json = ? WHERE id = 1`, value)
	}
	if err == nil {
		err = transaction.Commit()
	} else {
		_ = transaction.Rollback()
	}
	return result, err
}

func priceBookFromSQLiteTransaction(transaction *sql.Tx) (modelPriceBook, error) {
	book := emptyModelPriceBook()
	var raw []byte
	if err := transaction.QueryRow(`SELECT prices_json FROM store_state WHERE id = 1`).Scan(&raw); err != nil {
		return modelPriceBook{}, err
	}
	if len(raw) > 0 {
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
	var raw []byte
	err := store.db.QueryRow(`SELECT preferences_json FROM store_state WHERE id = 1`).Scan(&raw)
	if err == nil && len(raw) > 0 {
		err = json.Unmarshal(raw, &preferences)
	}
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
	value, err := json.Marshal(preferences)
	if err == nil {
		_, err = store.db.Exec(`UPDATE store_state SET preferences_json = ? WHERE id = 1`, value)
	}
	return preferences, err
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
