//go:build !full && !qoder && !trae && !workbuddy

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This is the default variant (no build tags): it registers WorkBuddy only.
// Qoder and Trae are opt-in at build time (-tags full / qoder / trae), so the
// generally shipped artifact does not contain their code paths unless explicitly built for them.
package platforms

import (
	_ "all2api/internal/platform/workbuddy"
)
