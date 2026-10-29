// Package buildinfo holds what the build stamps into both binaries.
package buildinfo

// Version is set at build time with -ldflags "-X ...buildinfo.Version=1.2.3"
// (bin/dev build: the tag without its v, or 0.0.0~dev+<commit>);
// a plain go build leaves "dev".
var Version = "dev"
