//go:build workbuddy

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This file is compiled only when -tags workbuddy is used, registering only WorkBuddy.
package platforms

import (
	_ "all2api/internal/platform/workbuddy"
)
