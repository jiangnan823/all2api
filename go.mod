module all2api

// Declared as the minimum toolchain that can build and analyse this module; CI pins the
// exact patch (GO_VERSION=1.26.9 in .github/workflows/ci.yml) and runs with
// GOTOOLCHAIN=local. Do not raise this without raising the golangci-lint / gosec /
// govulncheck pins in the same commit — see docs/DEVOPS-M0-NOTES.md §9.16 for the
// measured compatibility matrix.
go 1.26.0
