package buildinfo

import "fmt"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
	BuiltBy = "source"
)

func String() string {
	return fmt.Sprintf("ccr %s (%s, %s, built by %s)", Version, Commit, Date, BuiltBy)
}
