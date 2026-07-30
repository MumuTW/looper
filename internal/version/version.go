package version

import "strings"

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
	BuildDirty     = ""
)

type BuildMetadata struct {
	VersionSource  string  `json:"versionSource"`
	Channel        string  `json:"channel"`
	APIVersion     string  `json:"apiVersion"`
	GitCommitSHA   *string `json:"gitCommitSha"`
	BuildTimestamp *string `json:"buildTimestamp"`
	Dirty          *bool   `json:"dirty"`
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
			Dirty:          boolPtrOrNil(BuildDirty),
		},
	}
}

// SameBuild reports whether two binaries carry the same complete build
// identity. The build timestamp participates deliberately: release artifacts
// use one preparation timestamp, so equality proves they came from the same
// cut rather than merely the same source commit.
func (i Info) SameBuild(other Info) bool {
	return i.Complete() && other.Complete() &&
		i.Version == other.Version &&
		i.Metadata.VersionSource == other.Metadata.VersionSource &&
		i.Metadata.Channel == other.Metadata.Channel &&
		i.Metadata.APIVersion == other.Metadata.APIVersion &&
		equalOptionalString(i.Metadata.GitCommitSHA, other.Metadata.GitCommitSHA) &&
		equalOptionalString(i.Metadata.BuildTimestamp, other.Metadata.BuildTimestamp) &&
		equalOptionalBool(i.Metadata.Dirty, other.Metadata.Dirty)
}

// Complete reports whether the identity contains enough source evidence to
// prove equality. Two unknown fields being equal is not proof that two dev or
// source-archive binaries came from the same build.
func (i Info) Complete() bool {
	return strings.TrimSpace(i.Version) != "" &&
		strings.TrimSpace(i.Metadata.VersionSource) != "" &&
		strings.TrimSpace(i.Metadata.Channel) != "" &&
		strings.TrimSpace(i.Metadata.APIVersion) != "" &&
		i.Metadata.GitCommitSHA != nil && strings.TrimSpace(*i.Metadata.GitCommitSHA) != "" &&
		i.Metadata.BuildTimestamp != nil && strings.TrimSpace(*i.Metadata.BuildTimestamp) != "" &&
		i.Metadata.Dirty != nil && !*i.Metadata.Dirty
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

func boolPtrOrNil(value string) *bool {
	switch value {
	case "true":
		value := true
		return &value
	case "false":
		value := false
		return &value
	default:
		return nil
	}
}

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalOptionalBool(left, right *bool) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
