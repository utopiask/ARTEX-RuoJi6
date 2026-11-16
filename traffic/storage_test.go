package traffic

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func awaitStorage(t *testing.T, tr *Traffic) StorageStats {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := tr.StorageUsage()
		if s.Ready && !s.Stale && !s.Refreshing {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("storage never ready: %+v", tr.StorageUsage())
	return StorageStats{}
}

func TestStorageUsageAsyncCachedAndInvalidated(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("example.com", "GET", "/", nil, []byte("body")))
	// These fixtures exercise both external layouts without reading their contents.
	for path, size := range map[string]int{
		"_blobs/sha256/aa/test.bin":             37,
		"legacy.example/path/request.http":      19,
		"_delete_staging/pending/response.http": 11,
		"_ca/excluded":                          1000,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink must neither be followed nor counted.
	if err := os.Symlink(filepath.Join(dir, "_ca"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	first := tr.StorageUsage()
	if first.Ready || !first.Refreshing || !first.Stale {
		t.Fatalf("cold sample: %+v", first)
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() { tr.StorageUsage() })
	}
	wg.Wait()
	s := awaitStorage(t, tr)
	if s.ExternalBytes != 67 || s.IndexBytes != tr.indexBytes() || s.TotalBytes != s.IndexBytes+67 {
		t.Fatalf("wrong totals: %+v", s)
	}
	cached := tr.StorageUsage()
	if cached.UpdatedAt != s.UpdatedAt || cached.Refreshing {
		t.Fatalf("not cached: %+v", cached)
	}
	if err := os.Remove(filepath.Join(dir, "_blobs/sha256/aa/test.bin")); err != nil {
		t.Fatal(err)
	}
	tr.invalidateStorage()
	stale := tr.StorageUsage()
	if !stale.Ready || !stale.Stale || !stale.Refreshing {
		t.Fatalf("invalidated sample: %+v", stale)
	}
	if got := awaitStorage(t, tr); got.ExternalBytes != 30 {
		t.Fatalf("delete not reflected: %+v", got)
	}
}

func TestStorageUsageRejectsObsoleteAndFailedSamples(t *testing.T) {
	tr, dir := openTraffic(t)
	prior := awaitStorage(t, tr)
	tr.invalidateStorage()
	// Directly exercise a refresh whose generation predates a deletion.
	tr.refreshStorage(0)
	tr.storage.mu.Lock()
	if tr.storage.sampledGeneration == tr.storage.generation {
		t.Error("obsolete sample published")
	}
	tr.storage.mu.Unlock()
	if tr.storage.sample != prior {
		t.Fatal("obsolete refresh replaced the last good sample")
	}
	prior = awaitStorage(t, tr)
	// Invalid index file reliably fails on every uid, unlike permission fixtures.
	base := filepath.Join(dir, "_index", "index.sqlite")
	if err := os.Rename(base, base+".saved"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(base+".saved", base)
	tr.invalidateStorage()
	tr.StorageUsage()
	tr.reaping.Wait()
	failed := tr.StorageUsage()
	if !failed.Ready || !failed.Stale || failed.Refreshing || failed.Error != "traffic storage measurement failed" ||
		failed.TotalBytes != prior.TotalBytes || failed.IndexBytes != prior.IndexBytes ||
		failed.ExternalBytes != prior.ExternalBytes || failed.UpdatedAt != prior.UpdatedAt {
		t.Fatalf("failed sample: %+v; prior: %+v", failed, prior)
	}
	tr.storage.mu.Lock()
	attemptedAt := tr.storage.attemptedAt
	tr.storage.mu.Unlock()
	for range 30 {
		if tr.StorageUsage().Refreshing {
			t.Fatal("failure retry was not throttled")
		}
	}
	tr.storage.mu.Lock()
	if tr.storage.attemptedAt != attemptedAt {
		t.Error("throttled read started another scan")
	}
	tr.storage.mu.Unlock()
	if err := os.Rename(base+".saved", base); err != nil {
		t.Fatal(err)
	}
	tr.storage.mu.Lock()
	tr.storage.attemptedAt = time.Now().Add(-storageRetry)
	tr.storage.mu.Unlock()
	if recovered := awaitStorage(t, tr); recovered.Error != "" {
		t.Fatalf("successful retry retained error: %+v", recovered)
	}
}

func TestStorageUsageDeletionInvalidates(t *testing.T) {
	for _, name := range []string{"exact", "substring", "all"} {
		t.Run(name, func(t *testing.T) {
			tr, _ := openTraffic(t)
			tr.record(newFlow("example.com", "GET", "/", nil, make([]byte, maxInlineBody+1)))
			before := awaitStorage(t, tr)
			if before.ExternalBytes == 0 {
				t.Fatal("missing spilled body")
			}
			var err error
			switch name {
			case "exact":
				_, err = tr.DeleteHostsExact([]string{"example.com"})
			case "substring":
				_, err = tr.DeleteHost("example")
			case "all":
				_, _, err = tr.DeleteAll()
			}
			if err != nil {
				t.Fatal(err)
			}
			tr.reaping.Wait()
			if !tr.StorageUsage().Stale {
				t.Fatal("deletion did not invalidate cache")
			}
			if after := awaitStorage(t, tr); after.ExternalBytes != 0 {
				t.Fatalf("blob retained in stats: %+v", after)
			}
		})
	}
}

func TestStorageUsageStopsOnClose(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.StorageUsage()
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			for range 30 {
				tr.StorageUsage()
			}
		})
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if tr.StorageUsage().Refreshing {
		t.Fatal("refresh restarted after close")
	}
}

func TestStorageUsageTTLAndSingleFlight(t *testing.T) {
	tr, _ := openTraffic(t)
	prior := awaitStorage(t, tr)
	tr.storage.mu.Lock()
	tr.storage.sampledAt = time.Now().Add(-storageTTL + time.Minute)
	tr.storage.mu.Unlock()
	if got := tr.StorageUsage(); got.Stale || got.Refreshing || got.UpdatedAt != prior.UpdatedAt {
		t.Fatalf("sample expired before TTL: %+v", got)
	}
	tr.storage.mu.Lock()
	tr.storage.sampledAt = time.Now().Add(-storageTTL)
	tr.storage.attemptedAt = tr.storage.sampledAt
	tr.storage.mu.Unlock()
	if got := tr.StorageUsage(); !got.Stale || !got.Refreshing || got.UpdatedAt != prior.UpdatedAt {
		t.Fatalf("expired sample not returned while refreshing: %+v", got)
	}
	tr.storage.mu.Lock()
	attemptedAt := tr.storage.attemptedAt
	tr.storage.mu.Unlock()
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() { tr.StorageUsage() })
	}
	wg.Wait()
	tr.reaping.Wait()
	tr.storage.mu.Lock()
	if tr.storage.attemptedAt != attemptedAt {
		t.Error("concurrent reads started another scan")
	}
	tr.storage.mu.Unlock()
	if got := awaitStorage(t, tr); got.UpdatedAt == prior.UpdatedAt {
		t.Fatal("TTL refresh did not publish a new sample")
	}
}

func TestStorageMeasureCountsIndexFilesAndExcludesSymlinks(t *testing.T) {
	dir := t.TempDir()
	for path, size := range map[string]int{
		"_index/index.sqlite": 17, "_index/index.sqlite-wal": 11,
		"_index/index.sqlite-shm": 7, "_index/unrelated": 1000,
		"_ca/key": 1000, "legacy/response.http": 13,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tr := &Traffic{dir: dir}
	for _, name := range []string{"linked-file", "legacy/linked-file"} {
		if err := os.Symlink(filepath.Join(dir, "_ca/key"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	assertTotals := func(index int64) {
		t.Helper()
		got, err := tr.measureStorage()
		if err != nil || got.IndexBytes != index || got.ExternalBytes != 13 || got.TotalBytes != index+13 {
			t.Fatalf("measurement=%+v err=%v; want index=%d external=13", got, err, index)
		}
	}
	assertTotals(35)
	wal := filepath.Join(dir, "_index/index.sqlite-wal")
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "_ca/key"), wal); err != nil {
		t.Fatal(err)
	}
	assertTotals(24)
	if err := os.Remove(filepath.Join(dir, "_index/index.sqlite-shm")); err != nil {
		t.Fatal(err)
	}
	assertTotals(17)
	base := filepath.Join(dir, "_index/index.sqlite")
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "_ca/key"), base); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.measureStorage(); err == nil {
		t.Fatal("symlinked primary index accepted")
	}
	if err := os.Rename(filepath.Join(dir, "_index"), filepath.Join(dir, "saved-index")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "saved-index"), filepath.Join(dir, "_index")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.measureStorage(); err == nil {
		t.Fatal("symlinked index directory followed")
	}
}

func TestStorageUsageMaintenanceInvalidates(t *testing.T) {
	for _, name := range []string{"gc", "reap", "reclaim", "compact"} {
		t.Run(name, func(t *testing.T) {
			tr, dir := openTraffic(t)
			path := filepath.Join(dir, "_blobs/sha256/aa/orphan.bin")
			if name == "reap" {
				path = filepath.Join(dir, "_delete_staging/pending/response.http")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("orphan"), 0600); err != nil {
				t.Fatal(err)
			}
			prior := awaitStorage(t, tr)
			tr.wmu.Lock()
			var err error
			switch name {
			case "gc":
				err = tr.gcBlobs()
			case "reap":
				tr.reapStage(filepath.Dir(path))
			case "reclaim":
				tr.reclaim()
			case "compact":
				err = tr.compactIndex()
			}
			tr.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			tr.reaping.Wait()
			if !tr.StorageUsage().Stale {
				t.Fatal("maintenance did not invalidate cache")
			}
			after := awaitStorage(t, tr)
			if after.UpdatedAt == prior.UpdatedAt {
				t.Fatal("maintenance did not refresh sample")
			}
			if (name == "gc" || name == "reap") && after.ExternalBytes != 0 {
				t.Fatalf("removed file still counted: %+v", after)
			}
		})
	}
}

func TestStorageUsageImportInvalidates(t *testing.T) {
	source, _ := openTraffic(t)
	source.record(newFlow("archive.example", "GET", "/", nil, make([]byte, maxInlineBody+1)))
	archiveDir := t.TempDir()
	if n, err := source.ExportHosts([]string{"archive.example"}, archiveDir); err != nil || n != 1 {
		t.Fatalf("export count=%d err=%v", n, err)
	}
	tr, _ := openTraffic(t)
	prior := awaitStorage(t, tr)
	if n, err := tr.ImportArchive(archiveDir); err != nil || n != 1 {
		t.Fatalf("import count=%d err=%v", n, err)
	}
	if !tr.StorageUsage().Stale {
		t.Fatal("import did not invalidate cache")
	}
	if after := awaitStorage(t, tr); after.ExternalBytes <= prior.ExternalBytes {
		t.Fatalf("imported blob not reflected: before=%+v after=%+v", prior, after)
	}
}
