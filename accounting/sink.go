/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package accounting records every launch of an LLM or API call: one Record
// per launch, written as one JSON object per line.
//
// There is one sink per process. Start opens <dir>/accounting.log and Close
// drains and closes it. Report never drops a record: when the sink is not
// started, has been closed, or cannot write the file, the record goes to the
// shared logger instead, as an info event of component "accounting" whose
// field "accounting" holds the record's JSON. Report blocks while the queue is
// full, so a log file on a stalled device blocks the callers.
package accounting

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.bison9.com/agent/logger"
)

// FileName is the name of the accounting file in the directory given to
// Start.
const FileName = "accounting.log"

const component = "accounting"

// ErrAlreadyStarted reports a Start while the sink is running.
var ErrAlreadyStarted = errors.New("accounting: already started")

// bufferSize is the number of records queued for the writer before Report
// blocks. A variable so tests can make it small.
var bufferSize = 1024

var (
	// lifecycle serialises Start and Close. Close holds it until the sink
	// it closes has drained, so a second Close, or a Start, waits for that.
	// Report never takes it.
	lifecycle sync.Mutex
	// mu guards current. Report holds it for reading while it queues a
	// record, so Close (which takes it for writing) cannot close the queue
	// under a sender; the writer drains the queue without it, so a blocked
	// Report always gets room.
	mu      sync.RWMutex
	current *sink
)

type sink struct {
	path     string
	file     *os.File
	queue    chan Record
	done     chan struct{}
	closeErr error // set by the writer before done is closed
}

// Start creates dir if needed (mode 0700), opens dir/accounting.log for
// appending (mode 0600 when created) and starts the writer. It returns
// ErrAlreadyStarted while a sink is running; after Close it may be called
// again. While a Close is still draining, Start waits for it to finish.
func Start(dir string) error {
	if dir == "" {
		return errors.New("accounting: directory is required")
	}
	lifecycle.Lock()
	defer lifecycle.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if current != nil {
		return ErrAlreadyStarted
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("accounting: create directory: %w", err)
	}
	path := filepath.Join(dir, FileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the directory is the host's choice; the file name is fixed
	if err != nil {
		return fmt.Errorf("accounting: open file: %w", err)
	}
	s := &sink{
		path:  path,
		file:  f,
		queue: make(chan Record, bufferSize),
		done:  make(chan struct{}),
	}
	go s.run()
	current = s
	return nil
}

// Report records one launch. It is safe for concurrent use. It first sets
// the record's CostSource, estimating the cost from the pricing table when the
// call returned none (see SetPricing). When the queue
// is full it blocks until there is room; it never drops a record. Without a
// running sink the record goes to the logger.
func Report(r Record) {
	settleCost(&r)
	mu.RLock()
	s := current
	if s == nil {
		mu.RUnlock()
		toLogger(encode(r))
		return
	}
	s.queue <- r
	mu.RUnlock()
}

// Close stops the sink: records already queued are written, the file is
// closed and the writer has finished when it returns. Records reported after
// Close go to the logger. A Close that runs while another is draining waits
// for it, then does nothing; so does a Close without a running sink.
func Close() error {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	mu.Lock()
	s := current
	current = nil
	if s != nil {
		close(s.queue)
	}
	mu.Unlock()
	if s == nil {
		return nil
	}
	<-s.done
	if s.closeErr != nil {
		return fmt.Errorf("accounting: close file: %w", s.closeErr)
	}
	return nil
}

// run writes queued records until the queue is closed, then closes the file.
func (s *sink) run() {
	defer close(s.done)
	for r := range s.queue {
		line := encode(r)
		if _, err := s.file.Write(append(line, '\n')); err != nil {
			logger.ErrorCF(component, "accounting file write failed", map[string]any{
				"path":  s.path,
				"error": err.Error(),
			})
			toLogger(line)
		}
	}
	s.closeErr = s.file.Close()
}

// encode returns the record's JSON. A Record holds only strings, booleans
// and numbers that are always finite, so marshalling cannot fail; should it,
// the record is still kept, as its Go representation.
func encode(r Record) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		logger.ErrorCF(component, "accounting record not encodable", map[string]any{"error": err.Error()})
		return fmt.Appendf(nil, "%+v", r)
	}
	return b
}

func toLogger(line []byte) {
	logger.InfoCF(component, "accounting record", map[string]any{"accounting": string(line)})
}
