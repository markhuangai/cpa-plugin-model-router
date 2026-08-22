package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestLegacyBboltMigrationPreservesUsageAndSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	wantBytes, wantRecords, wantBook, wantPreferences := writeLegacyUsageDatabase(t, path)

	store, err := openUsageStore(path, 3650)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if kind, err := usageDatabaseKind(path); err != nil || kind != usageDatabaseSQLite {
		t.Fatalf("migrated database kind = %s, %v", kind, err)
	}
	backupBytes, err := os.ReadFile(path + ".bbolt-v1.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backupBytes) != string(wantBytes) {
		t.Fatal("legacy backup does not match the source database")
	}
	if _, err := os.Stat(path + ".migration-v1.json"); !os.IsNotExist(err) {
		t.Fatalf("migration marker still exists: %v", err)
	}
	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}

	page, err := store.Requests(usageFilter{From: wantRecords[0].RequestedAt.Add(-time.Minute), To: wantRecords[1].RequestedAt.Add(time.Minute)}, "time", "asc", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != len(wantRecords) || len(page.Items) != len(wantRecords) {
		t.Fatalf("migrated requests = %#v, want %d", page, len(wantRecords))
	}
	for index, record := range wantRecords {
		if page.Items[index].Sequence != record.Sequence || page.Items[index].ProviderModel != record.ProviderModel || page.Items[index].TotalTokens != record.TotalTokens {
			t.Fatalf("migrated request %d = %#v, want %#v", index, page.Items[index], record)
		}
	}
	if got, err := store.QueryPriceBook(); err != nil || got.Revision != wantBook.Revision || got.Prices["legacy/model"].Input != wantBook.Prices["legacy/model"].Input {
		t.Fatalf("migrated price book = %#v, %v; want %#v", got, err, wantBook)
	}
	if got, err := store.QueryPreferences(); err != nil || got.RequestPageSize != wantPreferences.RequestPageSize || !slices.Equal(got.HiddenGroupColumns, wantPreferences.HiddenGroupColumns) {
		t.Fatalf("migrated preferences = %#v, %v; want %#v", got, err, wantPreferences)
	}

	next := wantRecords[1].RequestedAt.Add(time.Minute)
	if err := store.Record(storedUsageRecord{RequestedAt: next, Attribution: attributionDirect, ProviderModel: "after-migration", TotalTokens: 1}); err != nil {
		t.Fatal(err)
	}
	page, err = store.Requests(usageFilter{From: next, To: next.Add(time.Minute)}, "time", "asc", 0, 10)
	if err != nil || page.Total != 1 || page.Items[0].Sequence != 43 {
		t.Fatalf("post-migration sequence = %#v, %v; want sequence 43", page, err)
	}
}

func TestLegacyMigrationRecoversAfterPrimaryRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	wantBytes, wantRecords, _, _ := writeLegacyUsageDatabase(t, path)
	temporaryPath := path + ".sqlite-v1.migrating"
	backupPath := path + ".bbolt-v1.bak"
	markerPath := path + ".migration-v1.json"

	marker := legacyMigrationMarker{Version: 1, Phase: "copying"}
	if err := setMigrationSourceFingerprint(path, &marker); err != nil {
		t.Fatal(err)
	}
	stats, err := importLegacyBbolt(path, temporaryPath)
	if err != nil {
		t.Fatal(err)
	}
	marker.applyStats(stats)
	marker.Phase = "ready"
	if err := writeMigrationMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, backupPath); err != nil {
		t.Fatal(err)
	}

	store, err := openUsageStore(path, 3650)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	page, err := store.Requests(usageFilter{From: wantRecords[0].RequestedAt.Add(-time.Minute), To: wantRecords[1].RequestedAt.Add(time.Minute)}, "time", "asc", 0, 10)
	if err != nil || page.Total != len(wantRecords) {
		t.Fatalf("recovered requests = %#v, %v", page, err)
	}
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupBytes) != string(wantBytes) {
		t.Fatal("recovered legacy backup does not match the source database")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("recovery marker still exists: %v", err)
	}
}

func TestLegacyMigrationRejectsChangedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	writeLegacyUsageDatabase(t, path)
	marker := legacyMigrationMarker{Version: 1, Phase: "copying"}
	if err := setMigrationSourceFingerprint(path, &marker); err != nil {
		t.Fatal(err)
	}
	if err := writeMigrationMarker(path+".migration-v1.json", marker); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("changed")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := openUsageStore(path, 3650); err == nil {
		t.Fatal("changed legacy source was accepted")
	}
	if _, err := os.Stat(path + ".migration-v1.json"); err != nil {
		t.Fatalf("migration marker was not retained after source mismatch: %v", err)
	}
}

func TestSQLiteUsageStoresAllowOverlappingGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	first, err := openUsageStore(path, 3650)
	if err != nil {
		t.Fatal(err)
	}
	second, err := openUsageStore(path, 3650)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})

	now := time.Now().UTC()
	errorsSeen := make(chan error, 100)
	var workers sync.WaitGroup
	for _, store := range []*usageStore{first, second} {
		store := store
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := 0; index < 50; index++ {
				if err := store.Record(storedUsageRecord{RequestedAt: now.Add(time.Duration(index) * time.Microsecond), Attribution: attributionDirect, ProviderModel: "overlap", TotalTokens: 1}); err != nil {
					errorsSeen <- err
				}
			}
		}()
	}
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	page, err := first.Requests(usageFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)}, "time", "asc", 0, 200)
	if err != nil || page.Total != 100 {
		t.Fatalf("overlapping generation requests = %#v, %v; want 100", page, err)
	}
}

func writeLegacyUsageDatabase(t *testing.T, path string) ([]byte, []storedUsageRecord, modelPriceBook, dashboardPreferences) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Minute)
	records := []storedUsageRecord{
		{Sequence: 41, RequestedAt: now.Add(-2 * time.Minute), Attribution: attributionRouted, RouterModel: "legacy-router", Provider: "legacy", ProviderModel: "legacy/model", TotalTokens: 11},
		{Sequence: 42, RequestedAt: now.Add(-time.Minute), Attribution: attributionDirect, Provider: "legacy", ProviderModel: "legacy/direct", TotalTokens: 7},
	}
	book := modelPriceBook{
		SchemaVersion: usageSchemaVersion,
		Revision:      9,
		Prices:        map[string]modelPrice{"legacy/model": {tokenRates: tokenRates{Input: 1.25, Output: 2.5}, Source: priceSourceManual}},
		SyncSettings:  defaultPriceSyncSettings(),
	}
	preferences := defaultDashboardPreferences()
	preferences.RequestPageSize = 25
	preferences.HiddenGroupColumns = []string{"result", "source"}

	database, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Update(func(transaction *bolt.Tx) error {
		meta, err := transaction.CreateBucket(legacyMetaBucket)
		if err != nil {
			return err
		}
		requests, err := transaction.CreateBucket(legacyRequestsBucket)
		if err != nil {
			return err
		}
		minutes, err := transaction.CreateBucket(legacyMinutesBucket)
		if err != nil {
			return err
		}
		var version [4]byte
		binary.BigEndian.PutUint32(version[:], usageSchemaVersion)
		if err := meta.Put(legacySchemaKey, version[:]); err != nil {
			return err
		}
		var sequence [8]byte
		binary.BigEndian.PutUint64(sequence[:], 42)
		if err := meta.Put(legacySequenceKey, sequence[:]); err != nil {
			return err
		}
		if err := meta.Put(legacyLastPruneKey, []byte("2026-08-22")); err != nil {
			return err
		}
		prices, err := json.Marshal(book)
		if err != nil {
			return err
		}
		if err := meta.Put(legacyPricesKey, prices); err != nil {
			return err
		}
		preferencesJSON, err := json.Marshal(preferences)
		if err != nil {
			return err
		}
		if err := meta.Put(legacyPreferencesKey, preferencesJSON); err != nil {
			return err
		}
		for _, record := range records {
			value, err := json.Marshal(record)
			if err != nil {
				return err
			}
			key := make([]byte, 16)
			binary.BigEndian.PutUint64(key[:8], uint64(record.RequestedAt.UnixNano()))
			binary.BigEndian.PutUint64(key[8:], record.Sequence)
			if err := requests.Put(key, value); err != nil {
				return err
			}
			dimensions, err := json.Marshal([]string{record.Attribution, record.RouterModel, record.ProviderModel, record.Provider, record.Source, record.ServiceTier, record.result()})
			if err != nil {
				return err
			}
			minute := record.RequestedAt.Truncate(time.Minute)
			minuteKey := append(timestampPrefix(minute), dimensions...)
			aggregate := minuteAggregate{Minute: minute, Attribution: record.Attribution, RouterModel: record.RouterModel, ProviderModel: record.ProviderModel, Provider: record.Provider, Result: record.result()}
			aggregate.usageCounters.add(record)
			aggregateJSON, err := json.Marshal(aggregate)
			if err != nil {
				return err
			}
			if err := minutes.Put(minuteKey, aggregateJSON); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw, records, book, preferences
}
