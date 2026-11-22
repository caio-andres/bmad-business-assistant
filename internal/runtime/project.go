package runtime

import (
	"github.com/caio-andres/bmad-business-assistant/internal/bmad"
	"github.com/caio-andres/bmad-business-assistant/internal/vfs"
)

// ProjectInput assembles the virtual project of one conversation.
type ProjectInput struct {
	Bundle    *bmad.Bundle
	Allowed   []string
	Overlay   bmad.OverlayInput
	Workspace vfs.Backend // writable documents (_bmad-output)
	Docs      vfs.Backend // product context (docs/), may be nil
}

// NewProjectFS builds the mounts described in the bmad-runtime spec.
func NewProjectFS(in ProjectInput) *vfs.FS {
	overlay := map[string][]byte{
		"config.user.toml": []byte(bmad.UserConfigTOML(in.Overlay)),
	}
	for name, content := range bmad.CustomOverrides(in.Bundle, in.Allowed, in.Overlay) {
		overlay[name] = []byte(content)
	}
	docs := in.Docs
	if docs == nil {
		docs = vfs.NewMap(nil)
	}
	ws := in.Workspace
	if ws == nil {
		ws = vfs.NewMap(nil)
	}
	return vfs.New(
		vfs.Mount{Prefix: "_bmad", ReadOnly: true, Backend: &vfs.Layered{Layers: []vfs.Backend{
			vfs.NewMap(overlay), &vfs.FSBackend{FS: in.Bundle.FS, Base: "_bmad"},
		}}},
		vfs.Mount{Prefix: bmad.SkillsDir, ReadOnly: true, Backend: &vfs.FSBackend{FS: in.Bundle.FS, Base: bmad.SkillsDir}},
		vfs.Mount{Prefix: OutputDir, Backend: ws},
		vfs.Mount{Prefix: "docs", ReadOnly: true, Backend: docs},
	)
}
