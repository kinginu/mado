package hub

import (
	"time"
)

// Housekeeping (DESIGN §17.11): every hour the hub deletes uploads that no stored
// message refers to after unsentFileTTL (attached in the composer, never sent), so
// they stop counting against the quota and their blobs go away.

const (
	housekeepingEvery = time.Hour
	unsentFileTTL     = 24 * time.Hour
)

func (h *Hub) startHousekeeping() {
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		first := time.NewTimer(time.Minute) // not in the way of the start
		defer first.Stop()
		t := time.NewTicker(housekeepingEvery)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-first.C:
			case <-t.C:
			}
			h.housekeep(time.Now())
		}
	}()
}

func (h *Hub) stopHousekeeping() {
	if h.stop != nil {
		close(h.stop)
		<-h.done
		h.stop = nil
	}
}

// housekeep runs one pass as of now.
func (h *Hub) housekeep(now time.Time) {
	h.inboxHousekeep(now)
	fs, err := h.store.UnreferencedFilesBefore(now.Add(-unsentFileTTL).UTC().Format(time.RFC3339Nano))
	if err != nil {
		h.log.Error("housekeeping: list files", "err", err)
		return
	}
	for i := range fs {
		orphan, err := h.store.DeleteFile(&fs[i])
		if err != nil {
			h.log.Error("housekeeping: delete file", "id", fs[i].ID, "err", err)
			continue
		}
		if orphan {
			if err := h.blobs.Remove(fs[i].SHA256); err != nil {
				h.log.Warn("housekeeping: remove blob", "err", err)
			}
		}
	}
	if len(fs) > 0 {
		h.log.Info("housekeeping: deleted unsent uploads", "files", len(fs))
	}
}
