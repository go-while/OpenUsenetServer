package admin

import (
	"encoding/json"
	"testing"
	"time"

	"openusenet/internal/archive"
	"openusenet/internal/config"
	"openusenet/internal/store"
)

// startImportJob and startArchiveJob return a copy of the new job, not the live one that
// their goroutine updates under jobsMu: the copy can be encoded without the lock and keeps
// its initial state. Written by ous-sol in the review of this change.
func TestJobStartReturnsSnapshot(t *testing.T) {
	for _, kind := range []string{"import", "export"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Archive.ExportDir = t.TempDir()
			p := New(cfg, store.NewMemory(), nil, nil, nil, nil, nil)
			var snap *store.ArchiveJob
			if kind == "import" {
				snap = p.startImportJob(nil, nil, t.TempDir(), archive.ImportOpts{})
			} else {
				snap = p.startArchiveJob("*")
			}
			end := time.Now().Add(5 * time.Second)
			for {
				if _, err := json.Marshal(snap); err != nil {
					t.Fatal(err)
				}
				p.jobsMu.Lock()
				live := p.jobs[snap.ID]
				aliased, finished := live == snap, live.Status != "running"
				p.jobsMu.Unlock()
				if aliased {
					t.Fatal("returned live mutable job")
				}
				if finished {
					break
				}
				if time.Now().After(end) {
					t.Fatal("job did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			if snap.Kind != kind || snap.Status != "running" || !snap.Finished.IsZero() {
				t.Fatalf("initial snapshot changed with live job: %+v", snap)
			}
		})
	}
}
