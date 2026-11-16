package server

import (
	"context"
	"log"
	"time"
)

// Expiry must scrub previews even when nobody uploads another file. Reuse the
// server maintenance wait group so shutdown waits for bounded SQL cancellation.
func (s *Server) runAssetImportCleanup(ctx context.Context) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	clean := func() {
		work, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, _, err := s.m.pg.CleanupImportBatchesContext(work); err != nil && ctx.Err() == nil {
			log.Printf("[asset-import] batch expiry cleanup failed; retrying on next maintenance tick")
		}
	}
	clean()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			clean()
		}
	}
}
