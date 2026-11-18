// Package version holds build information, overridable via -ldflags.
package version

// Version is set at build time: -ldflags "-X llmgw/internal/version.Version=v1.2.3".
var Version = "dev"
