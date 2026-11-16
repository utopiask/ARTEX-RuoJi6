package traffic

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const storageTTL = 5 * time.Minute
const storageRetry = 5 * time.Second

// StorageStats measures apparent regular-file lengths, not allocated blocks.
// Ready denotes a complete successful sample; failed scans retain the last sample.
type StorageStats struct {
	TotalBytes    int64  `json:"total_bytes"`
	IndexBytes    int64  `json:"index_bytes"`
	ExternalBytes int64  `json:"external_bytes"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	Refreshing    bool   `json:"refreshing"`
	Error         string `json:"error,omitempty"`
	Ready         bool   `json:"ready"`
	Stale         bool   `json:"stale"`
}

type storageCache struct {
	mu                sync.Mutex
	sample            StorageStats
	sampledAt         time.Time
	attemptedAt       time.Time
	generation        uint64
	sampledGeneration uint64
	running           bool
}

// StorageUsage never walks the filesystem on the caller's goroutine. A cold or
// stale read starts at most one throttled scan; later reads reuse its snapshot.
func (t *Traffic) StorageUsage() StorageStats {
	c := &t.storage
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	stale := !c.sample.Ready || c.sampledGeneration != c.generation || now.Sub(c.sampledAt) >= storageTTL || c.sample.Error != ""
	if stale && !c.running && !t.stopping() && (c.attemptedAt.IsZero() || now.Sub(c.attemptedAt) >= storageRetry) {
		c.running = true
		c.attemptedAt = now
		generation := c.generation
		// Close takes this same mutex before closing t.closed and waiting, so Add
		// cannot race a shutdown Wait while its counter is zero.
		t.reaping.Go(func() { t.refreshStorage(generation) })
	}
	out := c.sample
	out.Refreshing = c.running
	out.Stale = stale
	return out
}

func (t *Traffic) invalidateStorage() {
	t.storage.mu.Lock()
	t.storage.generation++
	// Let the first read after a mutation refresh immediately, unless one is
	// already running. Its generation check prevents publishing an older scan.
	if !t.storage.running {
		t.storage.attemptedAt = time.Time{}
	}
	t.storage.mu.Unlock()
}

func (t *Traffic) refreshStorage(generation uint64) {
	sample, err := t.measureStorage()
	c := &t.storage
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	if t.stopping() {
		return
	}
	if generation != c.generation {
		c.attemptedAt = time.Time{}
		return
	}
	if err != nil {
		// Avoid exposing local paths in API errors and never publish a partial sum.
		c.sample.Error = "traffic storage measurement failed"
		return
	}
	c.sampledAt = time.Now()
	c.sampledGeneration = generation
	sample.Ready = true
	sample.UpdatedAt = c.sampledAt.UTC().Format(time.RFC3339Nano)
	c.sample = sample
}

func (t *Traffic) measureStorage() (StorageStats, error) {
	var out StorageStats
	indexDir := filepath.Join(t.dir, "_index")
	st, err := os.Lstat(indexDir)
	if err != nil {
		return out, err
	}
	if !st.IsDir() {
		return out, errors.New("invalid index directory")
	}
	base := filepath.Join(indexDir, "index.sqlite")
	for i, path := range []string{base, base + "-wal", base + "-shm"} {
		st, err := os.Lstat(path)
		if err != nil {
			if i > 0 && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return out, err
		}
		if i > 0 && st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !st.Mode().IsRegular() {
			return out, errors.New("invalid index file")
		}
		out.IndexBytes += st.Size()
	}
	// A queue and batched ReadDir avoid materializing huge directory listings.
	// Symlinks are never followed. Staged deletions remain counted until unlinked.
	dirs := []string{t.dir}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		if err := t.storagePause(); err != nil {
			return out, err
		}
		f, err := os.Open(dir)
		if err != nil {
			if dir != t.dir && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return out, err
		}
		for {
			entries, readErr := f.ReadDir(128)
			for _, entry := range entries {
				if dir == t.dir && (entry.Name() == "_index" || entry.Name() == "_ca") {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				if entry.IsDir() {
					dirs = append(dirs, path)
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				info, err := entry.Info()
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					f.Close()
					return out, err
				}
				if info.Mode().IsRegular() {
					out.ExternalBytes += info.Size()
				}
			}
			if readErr != nil {
				f.Close()
				if readErr != io.EOF {
					return out, readErr
				}
				break
			}
			if err := t.storagePause(); err != nil {
				f.Close()
				return out, err
			}
		}
	}
	out.TotalBytes = out.IndexBytes + out.ExternalBytes
	return out, nil
}

func (t *Traffic) storagePause() error {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-t.closed:
		return errors.New("traffic closed")
	case <-timer.C:
		return nil
	}
}
