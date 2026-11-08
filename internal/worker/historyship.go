package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kinginu/mado/internal/history"
	"github.com/kinginu/mado/internal/proto"
)

// History import (DESIGN §14): when claude_history is on, the daemon ships Claude Code
// transcripts to the hub, read-only, incrementally. The hub sends history_offsets after
// hello (and for a file whose frame it rejected); the worker then sends only the tail
// past the hub's offset of each file whose size or mtime changed.

const (
	historyEvery      = 60 * time.Second
	historyBatchBytes = 1 << 20
	historyMaxLine    = 8 << 20
)

type histState struct {
	offset, size, mtime int64
}

type historyShipper struct {
	d    *Daemon
	wake chan struct{}

	mu      sync.Mutex
	full    *proto.HistoryOffsets // a complete offsets list from a new connection
	partial []proto.HistoryFileState

	known map[string]histState // loop goroutine only
	ready bool                 // offsets received since the last send failure
}

func newHistoryShipper(d *Daemon) *historyShipper {
	return &historyShipper{d: d, wake: make(chan struct{}, 1), known: map[string]histState{}}
}

func (h *historyShipper) poke() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *historyShipper) offsets(m *proto.HistoryOffsets) {
	h.mu.Lock()
	if m.Partial {
		h.partial = append(h.partial, m.Files...)
	} else {
		h.full, h.partial = m, nil
	}
	h.mu.Unlock()
	h.poke()
}

// takePending applies offsets frames that arrived; it reports whether a full list
// came in (which restarts a running scan).
func (h *historyShipper) takePending() (restart bool) {
	h.mu.Lock()
	full, partial := h.full, h.partial
	h.full, h.partial = nil, nil
	h.mu.Unlock()
	if full != nil {
		h.known = map[string]histState{}
		for _, f := range full.Files {
			h.known[f.Path] = histState{f.Offset, f.Size, f.MTime}
		}
		h.ready = true
		restart = true
	}
	for _, f := range partial {
		h.known[f.Path] = histState{f.Offset, f.Size, f.MTime}
	}
	return restart
}

// configDirs: ~/.claude and the per-profile CLAUDE_CONFIG_DIRs mado creates.
func (h *historyShipper) configDirs() []string {
	dirs := []string{h.d.cfg.ClaudeHome}
	if m, err := filepath.Glob(filepath.Join(h.d.cfg.ClaudeConfigBase, "claude-*")); err == nil {
		sort.Strings(m)
		dirs = append(dirs, m...)
	}
	return dirs
}

// run scans on every wake (offsets, history_rescan) and every minute, once offsets for
// the current connection have arrived.
func (h *historyShipper) run(ctx context.Context) {
	t := time.NewTicker(historyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-t.C:
		}
		h.takePending()
		for h.ready && h.scan(ctx) && h.takePending() {
			// a new full offsets list arrived during the scan: go again
		}
	}
}

type scanStats struct {
	files, lines, bad, oversize int
	bytes                       int64
}

// scan ships every changed file once. It returns false if it stopped early (send
// failure, shutdown, or a new full offsets list that needs a fresh pass).
func (h *historyShipper) scan(ctx context.Context) bool {
	start := time.Now()
	var st scanStats
	defaultDir := h.d.cfg.ClaudeHome
	for _, f := range history.Scan(h.configDirs()) {
		if ctx.Err() != nil {
			return false
		}
		h.mu.Lock()
		restart := h.full != nil
		h.mu.Unlock()
		if restart {
			return true // loop again with the new list
		}
		k, seen := h.known[f.Path]
		if seen && k.size == f.Size && k.mtime == f.MTime {
			continue
		}
		reset := seen && f.Size < k.offset
		from := k.offset
		if reset {
			from = 0
		}
		if err := h.ship(f, from, reset, f.ConfigDir == defaultDir, &st); err != nil {
			h.d.log.Warn("history send failed", "file", f.Path, "err", err)
			h.ready = false
			return false
		}
	}
	if st.files > 0 {
		h.d.log.Info("history shipped", "files", st.files, "lines", st.lines, "bytes", st.bytes,
			"unparsable_lines", st.bad, "oversize_lines", st.oversize, "took", time.Since(start).Round(time.Millisecond))
	}
	return true
}

func (h *historyShipper) ship(f history.File, from int64, reset, defaultConfig bool, st *scanStats) error {
	var meta history.Meta
	cwd := history.DecodeProjectDir(f.ProjectDir)
	sent := false
	var sendErr error
	frame := func(lines []history.Line, end int64) error {
		meta.Scan(lines)
		m := &proto.HistorySession{Path: f.Path, ConfigDir: f.ConfigDir, DefaultConfig: defaultConfig,
			ClaudeSessionID: f.SessionID, CWD: cwd, Title: meta.Title, TitleCustom: meta.TitleCustom,
			MTime: f.MTime, Size: f.Size, Offset: from, End: end, Reset: reset && !sent,
			Lines: make([]string, len(lines)), LineOffsets: make([]int64, len(lines))}
		if meta.CWD != "" {
			m.CWD = meta.CWD
		}
		for i, l := range lines {
			m.Lines[i], m.LineOffsets[i] = string(l.Raw), l.Off
			st.bytes += int64(len(l.Raw))
			if !json.Valid(l.Raw) {
				st.bad++
			} else if len(l.Raw) < 64 && isOversize(l.Raw) {
				st.oversize++
			}
		}
		st.lines += len(lines)
		if err := h.d.send(m); err != nil {
			sendErr = err
			return err
		}
		sent = true
		from = end
		return nil
	}
	end, err := history.ReadTail(f.Path, from, historyMaxLine, historyBatchBytes, frame)
	if sendErr != nil {
		return sendErr
	}
	if err != nil && !sent {
		// Unreadable (vanished, permissions): skip it this round.
		return nil
	}
	if !sent {
		// Nothing new (a partial line, or only the mtime changed): still record the new
		// size and mtime on the hub so the next scan can skip the file.
		if frame(nil, end) != nil {
			return sendErr
		}
	}
	st.files++
	h.known[f.Path] = histState{end, f.Size, f.MTime}
	return nil
}

func isOversize(b []byte) bool {
	var o struct{ Type string }
	return json.Unmarshal(b, &o) == nil && o.Type == "mado_oversize"
}
