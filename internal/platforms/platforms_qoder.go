//go:build qoder

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This file is compiled only when -tags qoder is used, registering only Qoder.
package platforms

import (
	_ "all2api/internal/platform/qoder"
)
