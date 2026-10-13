//go:build trae

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This file is compiled only when -tags trae is used, registering only Trae.
package platforms

import (
	_ "all2api/internal/platform/trae"
)
