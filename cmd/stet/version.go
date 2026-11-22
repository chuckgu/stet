package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at build time with -ldflags "-X main.version=… -X main.commit=…".
var version, commit = "dev", ""

// versionLine is what stet version prints:
// "stet <version> (commit <commit>, <go version>)". Without a commit set at
// build time it uses the build's vcs.revision (12 characters), or "unknown".
func versionLine() string {
	c := commit
	if c == "" {
		c = "unknown"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			}
		}
	}
	return fmt.Sprintf("stet %s (commit %s, %s)", version, c, runtime.Version())
}
