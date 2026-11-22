package fsmode

import (
	"io/fs"
	"testing"
)

func TestOr(t *testing.T) {
	def := Modes{Dir: DefaultDir, File: DefaultFile}
	tests := []struct {
		name string
		m    Modes
		def  Modes
		want Modes
	}{
		{name: "zero keeps the default", m: Modes{}, def: def, want: def},
		{name: "both set", m: Modes{Dir: 0o750, File: 0o640}, def: def, want: Modes{Dir: 0o750, File: 0o640}},
		{name: "dir only", m: Modes{Dir: 0o750}, def: def, want: Modes{Dir: 0o750, File: DefaultFile}},
		{name: "file only", m: Modes{File: 0o640}, def: def, want: Modes{Dir: DefaultDir, File: 0o640}},
		{name: "loosening is allowed", m: Modes{Dir: 0o777, File: 0o666}, def: def, want: Modes{Dir: 0o777, File: 0o666}},
		{name: "other bits dropped", m: Modes{Dir: fs.ModeDir | 0o750, File: fs.ModeSetuid | 0o640}, def: def, want: Modes{Dir: 0o750, File: 0o640}},
		{name: "only other bits is zero", m: Modes{Dir: fs.ModeDir, File: fs.ModeSticky}, def: def, want: def},
		{name: "another default", m: Modes{File: 0o640}, def: Modes{Dir: 0o710, File: 0o400}, want: Modes{Dir: 0o710, File: 0o640}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Or(tt.def); got != tt.want {
				t.Errorf("%+v.Or(%+v) = %+v, want %+v", tt.m, tt.def, got, tt.want)
			}
		})
	}
}
