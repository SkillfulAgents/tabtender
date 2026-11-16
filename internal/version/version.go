// Package version exposes build metadata injected at link time.
package version

// Version is the tabtenderd release version. Release builds override it with
// -ldflags "-X github.com/SkillfulAgents/tabtender/internal/version.Version=...".
var Version = "dev"
