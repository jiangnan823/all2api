//go:build full

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This file is compiled only when -tags full is used, registering all available platforms.
// Without this layer, main.go would need direct imports of each platform package,
// breaking the cropping mechanism where excluded tags = no code compiled = no init() called.
package platforms

import (
	_ "all2api/internal/platform/qoder"
	_ "all2api/internal/platform/trae"
	_ "all2api/internal/platform/workbuddy"
)
