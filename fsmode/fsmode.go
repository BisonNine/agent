/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package fsmode holds the one type the SDK's packages take for the modes of
// the directories and files they create. It imports only the standard
// library, so every package can use it.
package fsmode

import "io/fs"

// The modes every package defaults to: owner-only.
const (
	DefaultDir  fs.FileMode = 0o700
	DefaultFile fs.FileMode = 0o600
)

// Modes is the mode of the directories (Dir) and files (File) a package
// creates. Only the permission bits are used. A field that is zero, once the
// other bits are dropped, keeps the package's default, which is DefaultDir
// and DefaultFile unless the package documents another; so the zero Modes
// changes nothing.
type Modes struct {
	Dir  fs.FileMode
	File fs.FileMode
}

// Or returns the permission bits of m, with each field that is zero taken from
// def instead.
func (m Modes) Or(def Modes) Modes {
	out := Modes{Dir: m.Dir & fs.ModePerm, File: m.File & fs.ModePerm}
	if out.Dir == 0 {
		out.Dir = def.Dir
	}
	if out.File == 0 {
		out.File = def.File
	}
	return out
}
