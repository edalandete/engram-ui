package version

// Build-time injected via -ldflags "-X github.com/edalandete/engram-ui/internal/version.Version=..."
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// MinEngram is the minimum engram version this UI supports.
//
// "v0.0.0" means no requirement is declared and the check is skipped. Raise it
// only when the UI genuinely depends on something a newer engram added, and
// remember it makes boot fail for anyone on an older one.
//
// The version is read from the binary (`engram --version`), not from the HTTP
// /health endpoint: engram's handleHealth returns the string literal "0.1.0"
// regardless of what it was built from. A check against /health would compare
// MinEngram to that constant and reject every engram in existence.
// Bump on every release that uses a new engram endpoint or field.
const MinEngram = "v0.0.0"
