// Package buildinfo holds what the build stamps into both binaries.
package buildinfo

// Version is set at build time with -ldflags "-X ...buildinfo.Version=v1.2.3";
// a plain go build leaves "dev".
var Version = "dev"
