//go:build !full && !qoder && !trae && !workbuddy

// Package platforms provides an assembly layer for build-tag-based platform registration.
// This file is compiled only when no platform tag is used, registering the
// generally shipped pair: WorkBuddy and Qoder (M4). Trae stays opt-in at
// build time (-tags full / trae), so the generally shipped artifact does not
// contain its code paths unless explicitly built for them.
package platforms

import (
	_ "all2api/internal/platform/qoder"
	_ "all2api/internal/platform/workbuddy"
)
