/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The capped log keeps the head and the most recent tail through many
// compactions, and since() maps logical offsets back onto the file.
func TestLogFileCompactionKeepsHeadAndTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test file
	l := newLogFile(path, f, 400)

	var full strings.Builder
	for i := range 500 {
		chunk := fmt.Sprintf("line %04d %s\n", i, strings.Repeat("x", i%17))
		full.WriteString(chunk)
		if _, werr := l.write([]byte(chunk)); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
	}
	all := full.String()
	if l.produced != int64(len(all)) || !l.compacted {
		t.Fatalf("produced %d (want %d), compacted %v", l.produced, len(all), l.compacted)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := all[:l.headKeep]
	marker := markerText(l.dropped)
	tail := all[l.produced-l.tailLen():]
	if string(data) != head+marker+tail {
		t.Fatalf("file is not head+marker+tail:\n%q", data)
	}
	if int64(len(head))+l.dropped+int64(len(tail)) != l.produced {
		t.Fatalf("head %d + dropped %d + tail %d != %d", len(head), l.dropped, len(tail), l.produced)
	}
	if l.tailLen() < l.tailKeep || l.tailLen() > l.tailKeep+l.slack {
		t.Fatalf("tail %d outside [%d, %d]", l.tailLen(), l.tailKeep, l.tailKeep+l.slack)
	}

	tailStart := l.produced - l.tailLen()
	for _, tc := range []struct {
		name        string
		from        int64
		want        string
		wantDropped int64
	}{
		{"from start", 0, head + marker + tail, l.dropped},
		{"inside head", 10, all[10:l.headKeep] + marker + tail, l.dropped},
		{"inside the dropped middle", tailStart - 7, markerText(7) + tail, 7},
		{"inside tail", tailStart + 5, all[tailStart+5:], 0},
		{"at end", l.produced, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pieces, dropped := l.since(tc.from)
			got, omitted, err := l.read(pieces, 0)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || dropped != tc.wantDropped || omitted != 0 {
				t.Fatalf("since(%d) = %q dropped %d omitted %d\nwant %q dropped %d", tc.from, got, dropped, omitted, tc.want, tc.wantDropped)
			}
		})
	}

	t.Run("bounded read keeps head and tail", func(t *testing.T) {
		pieces, _ := l.since(tailStart)
		got, omitted, err := l.read(pieces, 100)
		if err != nil {
			t.Fatal(err)
		}
		region := all[tailStart:]
		want := region[:30] + fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted) + region[len(region)-70:]
		if string(got) != want || omitted != int64(len(region)-100) {
			t.Fatalf("bounded read = %q omitted %d", got, omitted)
		}
	})
}
