/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// logFile is a job's output file. Output is written to disk as it arrives.
// Once the data in the file passes the limit (plus some slack, so the file
// is not rewritten on every write) it is compacted in place to:
//
//	head (headKeep bytes) | marker "[... N bytes dropped ...]" | tail (tailKeep bytes)
//
// and later output is appended to the tail until the next compaction. The
// logical stream (every byte the job produced, numbered from zero) maps onto
// the file through headKeep, markerLen and the tail length.
//
// A logFile has no lock of its own: the job's mutex guards it.
type logFile struct {
	path string
	f    *os.File // open for writing while the job runs; nil afterwards

	headKeep, tailKeep, slack int64

	compacted bool
	markerLen int64 // length of the marker in the file (0 until compacted)
	size      int64 // bytes in the file
	produced  int64 // bytes the job has produced in all
	dropped   int64 // bytes removed from the middle
}

// minSlack bounds the marker's growth between compactions (its count can
// gain digits), which the in-place copy relies on.
const minSlack = 64

// copyChunk is the buffer size for moving the tail during compaction.
const copyChunk = 32 * 1024

func newLogFile(path string, f *os.File, limit int64) *logFile {
	l := &logFile{path: path, f: f}
	l.setLimit(limit)
	return l
}

func (l *logFile) setLimit(limit int64) {
	l.headKeep = limit / 2
	l.tailKeep = limit - l.headKeep
	l.slack = max(limit/4, minSlack)
}

// markerText is the line that stands for the dropped middle of the file.
func markerText(dropped int64) string {
	return fmt.Sprintf("\n[... %d bytes dropped ...]\n", dropped)
}

// tailLen is the number of output bytes after the head (and marker).
func (l *logFile) tailLen() int64 {
	if !l.compacted {
		return max(l.size-l.headKeep, 0)
	}
	return l.size - l.headKeep - l.markerLen
}

// write appends p and compacts when the file has grown past the limit. It
// reports whether it compacted, so the caller can persist the new layout.
func (l *logFile) write(p []byte) (bool, error) {
	n, err := l.f.WriteAt(p, l.size)
	l.size += int64(n)
	l.produced += int64(n)
	if err != nil {
		return false, fmt.Errorf("write output: %w", err)
	}
	if l.size-l.markerLen <= l.headKeep+l.tailKeep+l.slack {
		return false, nil
	}
	return true, l.compact()
}

// compact drops the oldest tail bytes so tailKeep remain, rewriting the
// marker. The tail moves towards the start of the file, so it is copied
// forward in chunks without reading it all into memory.
func (l *logFile) compact() error {
	drop := l.tailLen() - l.tailKeep
	if drop <= 0 {
		return nil
	}
	dropped := l.dropped + drop
	marker := markerText(dropped)
	src := l.headKeep + l.markerLen + drop
	dst := l.headKeep + int64(len(marker))
	if dst > src {
		// Cannot happen: drop is at least the slack, which exceeds any growth
		// of the marker. Refuse rather than corrupt the file.
		return errors.New("compact output: marker outgrew the dropped bytes")
	}
	buf := make([]byte, copyChunk)
	for done := int64(0); done < l.tailKeep; {
		n := min(int64(len(buf)), l.tailKeep-done)
		if _, err := l.f.ReadAt(buf[:n], src+done); err != nil {
			return fmt.Errorf("compact output: %w", err)
		}
		if _, err := l.f.WriteAt(buf[:n], dst+done); err != nil {
			return fmt.Errorf("compact output: %w", err)
		}
		done += n
	}
	if _, err := l.f.WriteAt([]byte(marker), l.headKeep); err != nil {
		return fmt.Errorf("compact output: %w", err)
	}
	if err := l.f.Truncate(dst + l.tailKeep); err != nil {
		return fmt.Errorf("compact output: %w", err)
	}
	l.size = dst + l.tailKeep
	l.markerLen = int64(len(marker))
	l.dropped = dropped
	l.compacted = true
	return nil
}

// piece is one run of the virtual output: a range of the file, or literal text.
type piece struct {
	off, n int64
	lit    []byte
}

func (p piece) length() int64 {
	if p.lit != nil {
		return int64(len(p.lit))
	}
	return p.n
}

// since describes the output from logical offset from to the end, and how
// many of those bytes were dropped from the file.
func (l *logFile) since(from int64) ([]piece, int64) {
	if from >= l.produced {
		return nil, 0
	}
	if !l.compacted {
		return []piece{{off: from, n: l.size - from}}, 0
	}
	tailFile := l.headKeep + l.markerLen
	tailStart := l.produced - l.tailLen()
	switch {
	case from < l.headKeep:
		return []piece{
			{off: from, n: l.headKeep - from},
			{off: l.headKeep, n: l.markerLen},
			{off: tailFile, n: l.size - tailFile},
		}, l.dropped
	case from < tailStart:
		gap := tailStart - from
		return []piece{
			{lit: []byte(markerText(gap))},
			{off: tailFile, n: l.size - tailFile},
		}, gap
	default:
		off := tailFile + from - tailStart
		return []piece{{off: off, n: l.size - off}}, 0
	}
}

// read returns the bytes of pieces. When max is positive and they are
// longer, it returns the first 30% and the last 70% of max with an
// "omitted" line between, and the number of bytes left out.
func (l *logFile) read(pieces []piece, maxBytes int) ([]byte, int64, error) {
	var total int64
	for _, p := range pieces {
		total += p.length()
	}
	if total == 0 {
		return nil, 0, nil
	}
	f, err := os.Open(l.path)
	if err != nil {
		return nil, 0, fmt.Errorf("read output: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor

	if maxBytes <= 0 || total <= int64(maxBytes) {
		out, rerr := readRange(f, pieces, 0, total)
		return out, 0, rerr
	}
	head := int64(maxBytes) * 3 / 10
	tail := int64(maxBytes) - head
	omitted := total - head - tail
	out, err := readRange(f, pieces, 0, head)
	if err != nil {
		return nil, 0, err
	}
	out = append(out, fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted)...)
	end, err := readRange(f, pieces, total-tail, tail)
	if err != nil {
		return nil, 0, err
	}
	return append(out, end...), omitted, nil
}

// readRange reads n bytes starting at virtual offset start of pieces.
func readRange(r io.ReaderAt, pieces []piece, start, n int64) ([]byte, error) {
	out := make([]byte, 0, n)
	pos := int64(0)
	for _, p := range pieces {
		plen := p.length()
		if n == 0 {
			break
		}
		if start >= pos+plen {
			pos += plen
			continue
		}
		skip := max(start-pos, 0)
		take := min(plen-skip, n)
		if p.lit != nil {
			out = append(out, p.lit[skip:skip+take]...)
		} else {
			buf := make([]byte, take)
			n, err := r.ReadAt(buf, p.off+skip)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("read output: %w", err)
			}
			out = append(out, buf[:n]...)
		}
		n -= take
		start += take
		pos += plen
	}
	return out, nil
}
