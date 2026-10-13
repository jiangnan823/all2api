// Package qoder implements the Qoder platform adapter.
// The real implementation is gated by build tags (see adapter.go); this file is
// always compiled so "go build ./..." and "go vet ./..." never fail with
// "build constraints exclude all Go files" in variants where the platform is excluded.
package qoder
