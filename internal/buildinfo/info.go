package buildinfo

// These values are set by the release build with -ldflags. Empty values identify
// a local build, which deliberately does not participate in automatic updates.
var (
	Version     = "dev"
	Commit      = "unknown"
	Date        = "unknown"
	ReleaseRepo = ""
)

const LoaderProtocol = 1
