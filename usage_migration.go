package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	legacyMetaBucket     = []byte("meta")
	legacyRequestsBucket = []byte("requests")
	legacyMinutesBucket  = []byte("minutes")
	legacySchemaKey      = []byte("schema_version")
	legacySequenceKey    = []byte("next_sequence")
	legacyLastPruneKey   = []byte("last_prune_day")
	legacyPricesKey      = []byte("prices")
	legacyPreferencesKey = []byte("preferences")
)

const legacyBoltMagic uint32 = 0xED0CDAED

type usageDatabaseKindValue string

const (
	usageDatabaseMissing     usageDatabaseKindValue = "missing"
	usageDatabaseSQLite      usageDatabaseKindValue = "sqlite"
	usageDatabaseLegacyBbolt usageDatabaseKindValue = "bbolt"
	usageDatabaseUnknown     usageDatabaseKindValue = "unknown"
)

type legacyMigrationMarker struct {
	Version       int    `json:"version"`
	SourceSHA256  string `json:"source_sha256"`
	SourceSize    int64  `json:"source_size"`
	Phase         string `json:"phase"`
	RequestCount  int    `json:"request_count"`
	MinuteCount   int    `json:"minute_count"`
	NextSequence  uint64 `json:"next_sequence"`
	RequestDigest string `json:"request_digest"`
	MinuteDigest  string `json:"minute_digest"`
	MetaDigest    string `json:"meta_digest"`
}

type legacyMigrationStats struct {
	requestCount  int
	minuteCount   int
	nextSequence  uint64
	requestDigest string
	minuteDigest  string
	metaDigest    string
}

func usageDatabaseKind(path string) (usageDatabaseKindValue, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usageDatabaseMissing, nil
		}
		return usageDatabaseUnknown, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return usageDatabaseUnknown, err
	}
	if info.Size() == 0 {
		return usageDatabaseUnknown, nil
	}
	header := make([]byte, 16)
	if _, err := io.ReadFull(file, header); err != nil {
		return usageDatabaseUnknown, nil
	}
	if string(header) == "SQLite format 3\x00" {
		return usageDatabaseSQLite, nil
	}
	magic := make([]byte, 4)
	if _, err := file.ReadAt(magic, 16); err == nil && binary.LittleEndian.Uint32(magic) == legacyBoltMagic {
		return usageDatabaseLegacyBbolt, nil
	}
	return usageDatabaseUnknown, nil
}

func migrateLegacyBbolt(path string) error {
	markerPath := path + ".migration-v1.json"
	temporaryPath := path + ".sqlite-v1.migrating"
	backupPath := path + ".bbolt-v1.bak"
	marker, err := readMigrationMarker(markerPath)
	if err != nil {
		return err
	}
	if marker != nil {
		return recoverLegacyMigration(path, temporaryPath, backupPath, markerPath, *marker)
	}
	kind, err := usageDatabaseKind(path)
	if err != nil {
		return err
	}
	if kind != usageDatabaseLegacyBbolt {
		return fmt.Errorf("legacy migration expected bbolt source, got %s", kind)
	}
	if backupKind, err := usageDatabaseKind(backupPath); err != nil {
		return err
	} else if backupKind != usageDatabaseMissing {
		return fmt.Errorf("refusing to overwrite existing migration backup %q", backupPath)
	}
	if temporaryKind, err := usageDatabaseKind(temporaryPath); err != nil {
		return err
	} else if temporaryKind != usageDatabaseMissing {
		return fmt.Errorf("refusing to overwrite abandoned migration temporary file %q", temporaryPath)
	}
	marker = &legacyMigrationMarker{Version: 1, Phase: "copying"}
	if err := setMigrationSourceFingerprint(path, marker); err != nil {
		return err
	}
	if err := writeMigrationMarker(markerPath, *marker); err != nil {
		return err
	}
	stats, err := importLegacyBbolt(path, temporaryPath)
	if err != nil {
		return err
	}
	if err := verifyMigrationSourceFingerprint(path, *marker); err != nil {
		return err
	}
	marker.applyStats(stats)
	marker.Phase = "ready"
	if err := writeMigrationMarker(markerPath, *marker); err != nil {
		return err
	}
	return finishLegacyMigration(path, temporaryPath, backupPath, markerPath, *marker)
}

func recoverLegacyMigration(path, temporaryPath, backupPath, markerPath string, marker legacyMigrationMarker) error {
	if marker.Version != 1 {
		return fmt.Errorf("unsupported migration marker version %d", marker.Version)
	}
	sourceKind, err := usageDatabaseKind(path)
	if err != nil {
		return err
	}
	backupKind, err := usageDatabaseKind(backupPath)
	if err != nil {
		return err
	}
	temporaryKind, err := usageDatabaseKind(temporaryPath)
	if err != nil {
		return err
	}
	if sourceKind == usageDatabaseSQLite && backupKind == usageDatabaseLegacyBbolt && temporaryKind == usageDatabaseMissing {
		if err := verifyMigrationSourceFingerprint(backupPath, marker); err != nil {
			return err
		}
		if err := validateSQLiteMigration(path, marker); err != nil {
			return err
		}
		if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncParentDirectory(markerPath)
	}
	if backupKind == usageDatabaseLegacyBbolt && sourceKind == usageDatabaseMissing && temporaryKind == usageDatabaseSQLite {
		if err := verifyMigrationSourceFingerprint(backupPath, marker); err != nil {
			return err
		}
		if err := validateSQLiteMigration(temporaryPath, marker); err != nil {
			return err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("install recovered SQLite database: %w", err)
		}
		if err := syncParentDirectory(path); err != nil {
			return err
		}
		marker.Phase = "installed"
		if err := writeMigrationMarker(markerPath, marker); err != nil {
			return err
		}
		if err := validateSQLiteMigration(path, marker); err != nil {
			return err
		}
		if err := os.Remove(markerPath); err != nil {
			return err
		}
		return syncParentDirectory(markerPath)
	}
	if sourceKind == usageDatabaseLegacyBbolt && backupKind == usageDatabaseMissing {
		if err := verifyMigrationSourceFingerprint(path, marker); err != nil {
			return err
		}
		if temporaryKind == usageDatabaseMissing || marker.Phase == "copying" {
			stats, err := importLegacyBbolt(path, temporaryPath)
			if err != nil {
				return err
			}
			if err := verifyMigrationSourceFingerprint(path, marker); err != nil {
				return err
			}
			marker.applyStats(stats)
			marker.Phase = "ready"
			if err := writeMigrationMarker(markerPath, marker); err != nil {
				return err
			}
		} else if temporaryKind != usageDatabaseSQLite {
			return fmt.Errorf("migration recovery found unexpected temporary file kind %s", temporaryKind)
		} else if err := validateSQLiteMigration(temporaryPath, marker); err != nil {
			return err
		}
		return finishLegacyMigration(path, temporaryPath, backupPath, markerPath, marker)
	}
	return fmt.Errorf("migration recovery found ambiguous files: source=%s temporary=%s backup=%s", sourceKind, temporaryKind, backupKind)
}

func finishLegacyMigration(path, temporaryPath, backupPath, markerPath string, marker legacyMigrationMarker) error {
	if err := verifyMigrationSourceFingerprint(path, marker); err != nil {
		return err
	}
	if err := os.Rename(path, backupPath); err != nil {
		return fmt.Errorf("retain legacy bbolt backup: %w", err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return fmt.Errorf("restrict legacy bbolt backup permissions: %w", err)
	}
	if err := syncParentDirectory(backupPath); err != nil {
		return err
	}
	marker.Phase = "backed_up"
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install migrated SQLite database: %w", err)
	}
	if err := syncParentDirectory(path); err != nil {
		return err
	}
	marker.Phase = "installed"
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		return err
	}
	if err := validateSQLiteMigration(path, marker); err != nil {
		return err
	}
	if err := os.Remove(markerPath); err != nil {
		return err
	}
	return syncParentDirectory(markerPath)
}

func importLegacyBbolt(sourcePath, temporaryPath string) (legacyMigrationStats, error) {
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return legacyMigrationStats{}, err
	}
	source, err := bolt.Open(sourcePath, 0o600, &bolt.Options{Timeout: 2 * time.Second, ReadOnly: true})
	if err != nil {
		return legacyMigrationStats{}, fmt.Errorf("open legacy bbolt source (it must be unloaded before migration): %w", err)
	}
	defer source.Close()
	destination, err := openSQLiteDatabase(temporaryPath, "DELETE")
	if err != nil {
		return legacyMigrationStats{}, err
	}
	defer destination.Close()
	transaction, err := destination.Begin()
	if err != nil {
		return legacyMigrationStats{}, err
	}
	stats, err := copyLegacyBuckets(transaction, source)
	if err != nil {
		_ = transaction.Rollback()
		return legacyMigrationStats{}, err
	}
	if err := transaction.Commit(); err != nil {
		return legacyMigrationStats{}, err
	}
	if err := syncFile(temporaryPath); err != nil {
		return legacyMigrationStats{}, err
	}
	return stats, nil
}

func copyLegacyBuckets(transaction *sql.Tx, source *bolt.DB) (legacyMigrationStats, error) {
	stats := legacyMigrationStats{}
	requestHash := sha256.New()
	minuteHash := sha256.New()
	metaHash := sha256.New()
	err := source.View(func(readTx *bolt.Tx) error {
		meta := readTx.Bucket(legacyMetaBucket)
		requests := readTx.Bucket(legacyRequestsBucket)
		minutes := readTx.Bucket(legacyMinutesBucket)
		if meta == nil || requests == nil || minutes == nil {
			return errors.New("legacy bbolt database is missing required buckets")
		}
		rawSchema := meta.Get(legacySchemaKey)
		if len(rawSchema) < 4 || binary.BigEndian.Uint32(rawSchema[len(rawSchema)-4:]) != usageSchemaVersion {
			return fmt.Errorf("legacy usage schema is unsupported")
		}
		stats.nextSequence = legacyDecodeUint64(meta.Get(legacySequenceKey))
		if stats.nextSequence > uint64(^uint64(0)>>1) {
			return errors.New("legacy usage sequence exceeds SQLite integer range")
		}
		maxRequestSequence := uint64(0)
		for _, key := range [][]byte{legacySequenceKey, legacyLastPruneKey, legacyPricesKey, legacyPreferencesKey} {
			writeDigestPair(metaHash, key, meta.Get(key))
		}
		if raw := meta.Get(legacyPricesKey); len(raw) > 0 && !json.Valid(raw) {
			return errors.New("legacy prices metadata is invalid JSON")
		}
		if raw := meta.Get(legacyPreferencesKey); len(raw) > 0 && !json.Valid(raw) {
			return errors.New("legacy preferences metadata is invalid JSON")
		}
		if _, err := transaction.Exec(`UPDATE store_state SET next_sequence = ?, last_prune_day = ?, prices_json = ?, preferences_json = ? WHERE id = 1`, int64(stats.nextSequence), string(meta.Get(legacyLastPruneKey)), meta.Get(legacyPricesKey), meta.Get(legacyPreferencesKey)); err != nil {
			return err
		}
		cursor := requests.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			if len(key) != 16 {
				return fmt.Errorf("legacy request key has length %d, want 16", len(key))
			}
			sequence := binary.BigEndian.Uint64(key[8:])
			if sequence == 0 || sequence > uint64(^uint64(0)>>1) {
				return errors.New("legacy request sequence is outside SQLite range")
			}
			if sequence > maxRequestSequence {
				maxRequestSequence = sequence
			}
			var record storedUsageRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return fmt.Errorf("decode legacy usage record: %w", err)
			}
			if record.Sequence != sequence || record.RequestedAt.UTC().UnixNano() != int64(binary.BigEndian.Uint64(key[:8])) {
				return errors.New("legacy request key and payload disagree")
			}
			if _, err := transaction.Exec(`INSERT INTO requests(sequence, requested_at_ns, payload) VALUES (?, ?, ?)`, int64(sequence), record.RequestedAt.UTC().UnixNano(), value); err != nil {
				return err
			}
			writeDigestPair(requestHash, key, value)
			stats.requestCount++
		}
		if stats.nextSequence < maxRequestSequence {
			return fmt.Errorf("legacy next sequence %d is behind request sequence %d", stats.nextSequence, maxRequestSequence)
		}
		cursor = minutes.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			if len(key) < 8 {
				return errors.New("legacy minute aggregate key is too short")
			}
			var aggregate minuteAggregate
			if err := json.Unmarshal(value, &aggregate); err != nil {
				return fmt.Errorf("decode legacy minute aggregate: %w", err)
			}
			if aggregate.Minute.UTC().UnixNano() != int64(binary.BigEndian.Uint64(key[:8])) {
				return errors.New("legacy minute key and payload disagree")
			}
			if _, err := transaction.Exec(`INSERT INTO minute_aggregates(key, minute_ns, payload) VALUES (?, ?, ?)`, key, aggregate.Minute.UTC().UnixNano(), value); err != nil {
				return err
			}
			writeDigestPair(minuteHash, key, value)
			stats.minuteCount++
		}
		return nil
	})
	if err != nil {
		return legacyMigrationStats{}, err
	}
	stats.requestDigest = hex.EncodeToString(requestHash.Sum(nil))
	stats.minuteDigest = hex.EncodeToString(minuteHash.Sum(nil))
	stats.metaDigest = hex.EncodeToString(metaHash.Sum(nil))
	return stats, nil
}

func validateSQLiteMigration(path string, marker legacyMigrationMarker) error {
	if marker.NextSequence > uint64(^uint64(0)>>1) {
		return errors.New("migration marker sequence exceeds SQLite integer range")
	}
	database, err := sql.Open("sqlite3", sqliteReadOnlyDSN(path))
	if err != nil {
		return err
	}
	defer database.Close()
	var quickCheck string
	if err := database.QueryRow(`PRAGMA quick_check`).Scan(&quickCheck); err != nil {
		return fmt.Errorf("SQLite quick_check: %w", err)
	}
	if !strings.EqualFold(quickCheck, "ok") {
		return fmt.Errorf("SQLite quick_check returned %q", quickCheck)
	}
	var requestCount, minuteCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&requestCount); err != nil {
		return err
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM minute_aggregates`).Scan(&minuteCount); err != nil {
		return err
	}
	if requestCount != marker.RequestCount || minuteCount != marker.MinuteCount {
		return fmt.Errorf("migrated row counts are requests=%d/minutes=%d, want %d/%d", requestCount, minuteCount, marker.RequestCount, marker.MinuteCount)
	}
	var sequence int64
	if err := database.QueryRow(`SELECT next_sequence FROM store_state WHERE id = 1`).Scan(&sequence); err != nil {
		return err
	}
	if sequence != int64(marker.NextSequence) {
		return fmt.Errorf("migrated next sequence is %d, want %d", sequence, marker.NextSequence)
	}
	requestDigest, err := sqliteBucketDigest(database, `SELECT requested_at_ns, sequence, payload FROM requests ORDER BY requested_at_ns, sequence`, true)
	if err != nil {
		return err
	}
	minuteDigest, err := sqliteBucketDigest(database, `SELECT key, minute_ns, payload FROM minute_aggregates ORDER BY key`, false)
	if err != nil {
		return err
	}
	metaDigest, err := sqliteMetaDigest(database)
	if err != nil {
		return err
	}
	if requestDigest != marker.RequestDigest || minuteDigest != marker.MinuteDigest || metaDigest != marker.MetaDigest {
		return errors.New("migrated SQLite content digest does not match the legacy source")
	}
	return nil
}

func sqliteReadOnlyDSN(path string) string {
	fileURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(sqliteAbsolutePath(path))}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_busy_timeout", fmt.Sprintf("%d", sqliteBusyTimeoutMS))
	fileURL.RawQuery = query.Encode()
	return fileURL.String()
}

func sqliteBucketDigest(database *sql.DB, query string, requests bool) (string, error) {
	hash := sha256.New()
	rows, err := database.Query(query)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		if requests {
			var timestamp, sequence int64
			var value []byte
			if err := rows.Scan(&timestamp, &sequence, &value); err != nil {
				return "", err
			}
			key := make([]byte, 16)
			binary.BigEndian.PutUint64(key[:8], uint64(timestamp))
			binary.BigEndian.PutUint64(key[8:], uint64(sequence))
			writeDigestPair(hash, key, value)
		} else {
			var key []byte
			var minute int64
			var value []byte
			if err := rows.Scan(&key, &minute, &value); err != nil {
				return "", err
			}
			writeDigestPair(hash, key, value)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sqliteMetaDigest(database *sql.DB) (string, error) {
	var sequence int64
	var lastPrune sql.NullString
	var prices, preferences []byte
	if err := database.QueryRow(`SELECT next_sequence, last_prune_day, prices_json, preferences_json FROM store_state WHERE id = 1`).Scan(&sequence, &lastPrune, &prices, &preferences); err != nil {
		return "", err
	}
	hash := sha256.New()
	sequenceBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(sequenceBytes, uint64(sequence))
	writeDigestPair(hash, legacySequenceKey, sequenceBytes)
	lastPruneBytes := []byte(nil)
	if lastPrune.Valid {
		lastPruneBytes = []byte(lastPrune.String)
	}
	writeDigestPair(hash, legacyLastPruneKey, lastPruneBytes)
	writeDigestPair(hash, legacyPricesKey, prices)
	writeDigestPair(hash, legacyPreferencesKey, preferences)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeDigestPair(hash interface{ Write([]byte) (int, error) }, key, value []byte) {
	length := make([]byte, 8)
	binary.BigEndian.PutUint64(length, uint64(len(key)))
	_, _ = hash.Write(length)
	_, _ = hash.Write(key)
	binary.BigEndian.PutUint64(length, uint64(len(value)))
	_, _ = hash.Write(length)
	_, _ = hash.Write(value)
}

func setMigrationSourceFingerprint(path string, marker *legacyMigrationMarker) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	hash, err := sha256File(path)
	if err != nil {
		return err
	}
	marker.SourceSize = info.Size()
	marker.SourceSHA256 = hash
	return nil
}

func verifyMigrationSourceFingerprint(path string, marker legacyMigrationMarker) error {
	if marker.SourceSize < 0 || marker.SourceSHA256 == "" {
		return errors.New("migration marker is missing the legacy source fingerprint")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat migration source: %w", err)
	}
	if info.Size() != marker.SourceSize {
		return fmt.Errorf("migration source size changed from %d to %d bytes", marker.SourceSize, info.Size())
	}
	hash, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("hash migration source: %w", err)
	}
	if hash != marker.SourceSHA256 {
		return errors.New("migration source digest changed while migration was in progress")
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (marker *legacyMigrationMarker) applyStats(stats legacyMigrationStats) {
	marker.RequestCount = stats.requestCount
	marker.MinuteCount = stats.minuteCount
	marker.NextSequence = stats.nextSequence
	marker.RequestDigest = stats.requestDigest
	marker.MinuteDigest = stats.minuteDigest
	marker.MetaDigest = stats.metaDigest
}

func readMigrationMarker(path string) (*legacyMigrationMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var marker legacyMigrationMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("decode migration marker: %w", err)
	}
	return &marker, nil
}

func writeMigrationMarker(path string, marker legacyMigrationMarker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	temporaryPath := path + ".tmp"
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncParentDirectory(path)
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncParentDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if runtime.GOOS == "windows" && err != nil {
		return closeErr
	}
	if err != nil {
		return err
	}
	return closeErr
}

func legacyDecodeUint64(raw []byte) uint64 {
	if len(raw) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(raw[len(raw)-8:])
}
