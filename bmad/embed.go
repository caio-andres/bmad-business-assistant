// Package bmadassets embeds the vendored BMad Method installation (bmad/dist).
//
// The bundle is the exact output of the official installer, pinned by version
// (see bmad/VERSION and scripts/update-bmad.sh). BMad Method is MIT-licensed;
// see bmad/LICENSE-BMAD-METHOD. "BMad" is a trademark of BMad Code, LLC; this
// project is an independent tool compatible with BMad Method.
package bmadassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the bundle root, containing "_bmad" and ".claude/skills".
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
