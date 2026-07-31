package version

import (
	"strconv"
	"strings"
)

const (
	packageImportPath        = "github.com/MumuTW/looper/internal/version"
	buildVersionEnvVar       = "LOOPER_BUILD_VERSION"
	buildVersionSourceEnvVar = "LOOPER_BUILD_VERSION_SOURCE"
	buildChannelEnvVar       = "LOOPER_BUILD_CHANNEL"
	buildAPIVersionEnvVar    = "LOOPER_BUILD_API_VERSION"
	buildGitSHAEnvVar        = "LOOPER_BUILD_GIT_SHA"
	buildTimestampEnvVar     = "LOOPER_BUILD_TIMESTAMP"
)

type BuildOverrides struct {
	Version        string
	VersionSource  string
	Channel        string
	APIVersion     string
	GitCommitSHA   string
	BuildTimestamp string
	Dirty          *bool
}

func DefaultBuildOverrides() BuildOverrides {
	return BuildOverrides{
		Version:        defaultVersion,
		VersionSource:  defaultVersionSource,
		Channel:        defaultChannel,
		APIVersion:     defaultAPIVersion,
		GitCommitSHA:   "",
		BuildTimestamp: "",
		Dirty:          nil,
	}
}

func BuildOverridesFromEnv(lookupEnv func(string) string) BuildOverrides {
	overrides := DefaultBuildOverrides()
	if v := strings.TrimSpace(lookupEnv(buildVersionEnvVar)); v != "" {
		overrides.Version = v
	}
	if v := strings.TrimSpace(lookupEnv(buildVersionSourceEnvVar)); v != "" {
		overrides.VersionSource = v
	}
	if v := strings.TrimSpace(lookupEnv(buildChannelEnvVar)); v != "" {
		overrides.Channel = v
	}
	if v := strings.TrimSpace(lookupEnv(buildAPIVersionEnvVar)); v != "" {
		overrides.APIVersion = v
	}
	overrides.GitCommitSHA = strings.TrimSpace(lookupEnv(buildGitSHAEnvVar))
	overrides.BuildTimestamp = strings.TrimSpace(lookupEnv(buildTimestampEnvVar))
	return overrides
}

func LDFlags(overrides BuildOverrides) string {
	return strings.Join([]string{
		ldflagAssignment("Value", overrides.Version),
		ldflagAssignment("VersionSource", overrides.VersionSource),
		ldflagAssignment("Channel", overrides.Channel),
		ldflagAssignment("APIVersion", overrides.APIVersion),
		ldflagAssignment("GitCommitSHA", overrides.GitCommitSHA),
		ldflagAssignment("BuildTimestamp", overrides.BuildTimestamp),
		ldflagAssignment("BuildDirty", optionalBoolString(overrides.Dirty)),
	}, " ")
}

func optionalBoolString(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}

func ldflagAssignment(name string, value string) string {
	return "-X " + packageImportPath + "." + name + "=" + value
}
