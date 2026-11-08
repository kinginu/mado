package worker

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/proto"
)

// DefaultSpoolRetention keeps a dead session's spool for a week (DESIGN §14).
const DefaultSpoolRetention = 7 * 24 * time.Hour

// spoolSuffixes are the per-session files in spool_dir.
var spoolSuffixes = []string{".spool", ".token", ".token.tmp"}

// SpoolIDs lists session ids that have files in spoolDir, minus the excluded ones.
func SpoolIDs(spoolDir string, exclude func(id string) bool) []string {
	ents, err := os.ReadDir(spoolDir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range ents {
		for _, suf := range spoolSuffixes {
			id, ok := strings.CutSuffix(e.Name(), suf)
			if !ok || !proto.ValidSessionID(id) || seen[id] {
				continue
			}
			seen[id] = true
			if exclude == nil || !exclude(id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// CollectSpools deletes the files of the dead sessions whose newest file is older than
// retention. live is consulted per id at deletion time, so a session that came back
// (resumed) since the hint was sent is never touched. It returns the ids removed.
func CollectSpools(spoolDir string, dead []string, live func(id string) bool, retention time.Duration, now time.Time) []string {
	var removed []string
	for _, id := range dead {
		if !proto.ValidSessionID(id) || (live != nil && live(id)) {
			continue
		}
		var files []string
		newest := time.Time{}
		for _, suf := range spoolSuffixes {
			p := filepath.Join(spoolDir, id+suf)
			fi, err := os.Lstat(p)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			files = append(files, p)
			if fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if len(files) == 0 || now.Sub(newest) < retention {
			continue
		}
		for _, p := range files {
			_ = os.Remove(p)
		}
		removed = append(removed, id)
	}
	return removed
}
