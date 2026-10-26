package launcher

import "embed"

// embeddedFS holds the engram binary built from third_party/engram via build-engram.ps1.
// The build target writes ./embedded/engram or ./embedded/engram.exe; both are matched.
//
// If embedded/ contains only the .gitkeep README, the embed succeeds with that single
// file and Spawn surfaces a clear "binary missing" error.
//
//go:embed all:embedded
var embeddedFS embed.FS
