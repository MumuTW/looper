package version

const (
	defaultVersion       = "0.0.0-dev"
	defaultVersionSource = "internal/version/version.go"
	defaultChannel       = "dev"
	defaultAPIVersion    = "v1"
)

// These variables are shared by all Go binaries and can be overridden at build
// time with -ldflags.
var (
	Value          = defaultVersion
	VersionSource  = defaultVersionSource
	Channel        = defaultChannel
	APIVersion     = defaultAPIVersion
	GitCommitSHA   = ""
	BuildTimestamp = ""
)

type BuildMetadata struct {
	VersionSource  string  `json:"versionSource"`
	Channel        string  `json:"channel"`
	APIVersion     string  `json:"apiVersion"`
	GitCommitSHA   *string `json:"gitCommitSha"`
	BuildTimestamp *string `json:"buildTimestamp"`
}

type Info struct {
	Version  string        `json:"version"`
	Metadata BuildMetadata `json:"metadata"`
}

func Current() Info {
	return Info{
		Version: Value,
		Metadata: BuildMetadata{
			VersionSource:  VersionSource,
			Channel:        Channel,
			APIVersion:     APIVersion,
			GitCommitSHA:   stringPtrOrNil(GitCommitSHA),
			BuildTimestamp: stringPtrOrNil(BuildTimestamp),
		},
	}
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}
