/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// record is the persisted form of a job: enough to list it after a restart
// and to detect that it was lost.
type record struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Command    string    `json:"command"`
	Dir        string    `json:"dir,omitempty"`
	State      State     `json:"state"`
	ExitCode   int       `json:"exit_code"`
	Signal     string    `json:"signal,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitzero"`
	PID        int       `json:"pid,omitempty"`
	Notify     bool      `json:"notify,omitempty"`
	Input      bool      `json:"input,omitempty"`
	// The log's layout, so its head, marker and tail can be told apart.
	OutputBytes  int64 `json:"output_bytes"`
	DroppedBytes int64 `json:"dropped_bytes,omitempty"`
	HeadBytes    int64 `json:"head_bytes,omitempty"`
	Compacted    bool  `json:"compacted,omitempty"`
}

// saveRecord writes the job's current record. Writes for one job are
// serialised and each writes the latest state, so an older snapshot never
// lands last; once the job's files are removed nothing is written.
func (m *Manager) saveRecord(j *job) {
	j.recMu.Lock()
	defer j.recMu.Unlock()
	j.mu.Lock()
	if j.removed {
		j.mu.Unlock()
		return
	}
	info := j.infoLocked()
	rec := record{
		ID:           info.ID,
		Owner:        info.Owner,
		Command:      info.Command,
		Dir:          info.Dir,
		State:        info.State,
		ExitCode:     info.ExitCode,
		Signal:       info.Signal,
		StopReason:   info.StopReason,
		Started:      info.Started,
		Ended:        info.Ended,
		PID:          j.pid,
		Notify:       info.Notify,
		Input:        info.Input,
		OutputBytes:  info.OutputBytes,
		DroppedBytes: info.DroppedBytes,
	}
	if j.out != nil && j.out.compacted {
		rec.HeadBytes = j.out.headKeep
		rec.Compacted = true
	}
	j.mu.Unlock()
	if err := m.writeRecord(rec); err != nil {
		m.log.Warn("shell job record not saved", "job", rec.ID, "error", err)
	}
}

func (m *Manager) writeRecord(rec record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	path := m.recordPath(rec.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, m.modes.File); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := os.Chmod(tmp, m.modes.File); err != nil {
		return errors.Join(fmt.Errorf("set record mode: %w", err), os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("replace record: %w", err), os.Remove(tmp))
	}
	return nil
}

// load reads every record in the directory as an ended job. A job recorded
// as running was lost with the process that ran it: it is marked lost and
// returned so New can announce it. Leftover temporary files are removed.
func (m *Manager) load() ([]Info, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, fmt.Errorf("proc: read log directory: %w", err)
	}
	var lost []Info
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".cwd") || strings.HasSuffix(name, ".json.tmp") {
			// Left by a process that died: nothing reads them now.
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil {
				m.log.Warn("leftover shell job file not removed", "file", name, "error", err)
			}
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, name)) //nolint:gosec // a file in the manager's own directory
		if err != nil {
			m.log.Warn("shell job record not read", "file", name, "error", err)
			continue
		}
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID != id {
			m.log.Warn("shell job record not valid", "file", name)
			continue
		}
		j := m.restore(rec)
		if rec.State == StateRunning {
			j.state = StateLost
			j.ended = m.clock.Now()
			m.saveRecord(j)
			lost = append(lost, j.info())
			m.log.Info("shell job lost in a restart", "job", id, "owner", rec.Owner)
		}
		m.jobs[id] = j
	}
	sortByStart(lost)
	return lost, nil
}

// restore rebuilds an ended job from its record and its log file's size.
func (m *Manager) restore(rec record) *job {
	j := &job{
		m:          m,
		id:         rec.ID,
		owner:      rec.Owner,
		command:    rec.Command,
		dir:        rec.Dir,
		input:      rec.Input,
		started:    rec.Started,
		pid:        rec.PID,
		done:       make(chan struct{}),
		state:      rec.State,
		notify:     rec.Notify,
		exitCode:   rec.ExitCode,
		signal:     rec.Signal,
		stopReason: rec.StopReason,
		ended:      rec.Ended,
		changed:    make(chan struct{}),
	}
	close(j.done)
	out := newLogFile(m.logPath(rec.ID), nil, m.limits.MaxOutputBytes)
	if st, err := os.Stat(out.path); err == nil {
		out.size = st.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("shell job log not found", "job", rec.ID, "error", err)
	}
	out.produced = out.size
	if rec.Compacted {
		out.compacted = true
		out.headKeep = rec.HeadBytes
		out.markerLen = int64(len(markerText(rec.DroppedBytes)))
		out.dropped = rec.DroppedBytes
		out.produced = out.size - out.markerLen + out.dropped
	}
	j.out = out
	return j
}
