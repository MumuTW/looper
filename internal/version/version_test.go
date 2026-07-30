package version

import (
	"encoding/json"
	"testing"
)

func TestCurrentUsesSharedBuildMetadata(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp
	originalDirty := BuildDirty

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
		BuildDirty = originalDirty
	})

	Value = "1.2.3"
	VersionSource = "internal/version/version.go"
	Channel = "stable"
	APIVersion = "v1"
	GitCommitSHA = "abc123"
	BuildTimestamp = "2026-04-17T00:00:00Z"
	BuildDirty = "false"

	info := Current()

	if info.Version != "1.2.3" {
		t.Fatalf("Current().Version = %q, want %q", info.Version, "1.2.3")
	}

	if info.Metadata.VersionSource != "internal/version/version.go" {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, "internal/version/version.go")
	}

	if info.Metadata.Channel != "stable" {
		t.Fatalf("Current().Metadata.Channel = %q, want %q", info.Metadata.Channel, "stable")
	}

	if info.Metadata.APIVersion != "v1" {
		t.Fatalf("Current().Metadata.APIVersion = %q, want %q", info.Metadata.APIVersion, "v1")
	}

	if info.Metadata.GitCommitSHA == nil || *info.Metadata.GitCommitSHA != "abc123" {
		t.Fatalf("Current().Metadata.GitCommitSHA = %v, want %q", info.Metadata.GitCommitSHA, "abc123")
	}

	if info.Metadata.BuildTimestamp == nil || *info.Metadata.BuildTimestamp != "2026-04-17T00:00:00Z" {
		t.Fatalf("Current().Metadata.BuildTimestamp = %v, want %q", info.Metadata.BuildTimestamp, "2026-04-17T00:00:00Z")
	}
	if info.Metadata.Dirty == nil || *info.Metadata.Dirty {
		t.Fatalf("Current().Metadata.Dirty = %v, want false", info.Metadata.Dirty)
	}
}

func TestCurrentDefaultsToPackageVersionMetadata(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp
	originalDirty := BuildDirty

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
		BuildDirty = originalDirty
	})

	Value = defaultVersion
	VersionSource = defaultVersionSource
	Channel = defaultChannel
	APIVersion = defaultAPIVersion
	GitCommitSHA = ""
	BuildTimestamp = ""
	BuildDirty = ""

	info := Current()

	if info.Version != defaultVersion {
		t.Fatalf("Current().Version = %q, want %q", info.Version, defaultVersion)
	}

	if info.Metadata.VersionSource != defaultVersionSource {
		t.Fatalf("Current().Metadata.VersionSource = %q, want %q", info.Metadata.VersionSource, defaultVersionSource)
	}

	if info.Metadata.GitCommitSHA != nil {
		t.Fatalf("Current().Metadata.GitCommitSHA = %v, want nil", info.Metadata.GitCommitSHA)
	}

	if info.Metadata.BuildTimestamp != nil {
		t.Fatalf("Current().Metadata.BuildTimestamp = %v, want nil", info.Metadata.BuildTimestamp)
	}
	if info.Metadata.Dirty != nil {
		t.Fatalf("Current().Metadata.Dirty = %v, want nil", info.Metadata.Dirty)
	}

	if info.Metadata.Channel != defaultChannel {
		t.Fatalf("Current().Metadata.Channel = %q, want %q", info.Metadata.Channel, defaultChannel)
	}

	if info.Metadata.APIVersion != defaultAPIVersion {
		t.Fatalf("Current().Metadata.APIVersion = %q, want %q", info.Metadata.APIVersion, defaultAPIVersion)
	}
}

func TestCurrentJSONMatchesStatusMetadataShape(t *testing.T) {
	originalValue := Value
	originalSource := VersionSource
	originalChannel := Channel
	originalAPIVersion := APIVersion
	originalCommit := GitCommitSHA
	originalTimestamp := BuildTimestamp
	originalDirty := BuildDirty

	t.Cleanup(func() {
		Value = originalValue
		VersionSource = originalSource
		Channel = originalChannel
		APIVersion = originalAPIVersion
		GitCommitSHA = originalCommit
		BuildTimestamp = originalTimestamp
		BuildDirty = originalDirty
	})

	Value = defaultVersion
	VersionSource = defaultVersionSource
	Channel = defaultChannel
	APIVersion = defaultAPIVersion
	GitCommitSHA = ""
	BuildTimestamp = ""
	BuildDirty = ""

	encoded, err := json.Marshal(Current())
	if err != nil {
		t.Fatalf("json.Marshal(Current()) error = %v", err)
	}

	const want = `{"version":"0.0.0-dev","metadata":{"versionSource":"internal/version/version.go","channel":"dev","apiVersion":"v1","gitCommitSha":null,"buildTimestamp":null,"dirty":null}}`
	if string(encoded) != want {
		t.Fatalf("json.Marshal(Current()) = %s, want %s", encoded, want)
	}
}

func TestInfoSameBuildRequiresCompleteIdentity(t *testing.T) {
	clean := false
	base := Info{Version: "1.2.3", Metadata: BuildMetadata{
		VersionSource: "git-tag:v1.2.3",
		Channel:       "stable", APIVersion: "v1",
		GitCommitSHA: stringPtrOrNil("abc123"), BuildTimestamp: stringPtrOrNil("2026-04-17T00:00:00Z"), Dirty: &clean,
	}}
	if !base.SameBuild(base) {
		t.Fatal("identical build identities did not match")
	}
	if (Info{Version: "0.0.0-dev", Metadata: BuildMetadata{VersionSource: "source", Channel: "dev", APIVersion: "v1"}}).SameBuild(
		Info{Version: "0.0.0-dev", Metadata: BuildMetadata{VersionSource: "source", Channel: "dev", APIVersion: "v1"}},
	) {
		t.Fatal("incomplete identities were treated as proof of the same build")
	}
	dirty := base
	value := true
	dirty.Metadata.Dirty = &value
	if dirty.SameBuild(dirty) {
		t.Fatal("dirty identities were treated as proof of the same source build")
	}

	mutations := map[string]func(Info) Info{
		"version": func(info Info) Info { info.Version = "1.2.4"; return info },
		"source":  func(info Info) Info { info.Metadata.VersionSource = "git-tag:v1.2.4"; return info },
		"channel": func(info Info) Info { info.Metadata.Channel = "beta"; return info },
		"api":     func(info Info) Info { info.Metadata.APIVersion = "v2"; return info },
		"commit":  func(info Info) Info { info.Metadata.GitCommitSHA = stringPtrOrNil("def456"); return info },
		"timestamp": func(info Info) Info {
			info.Metadata.BuildTimestamp = stringPtrOrNil("2026-04-18T00:00:00Z")
			return info
		},
		"dirty": func(info Info) Info {
			dirty := true
			info.Metadata.Dirty = &dirty
			return info
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if base.SameBuild(mutate(base)) {
				t.Fatalf("SameBuild accepted mismatched %s", name)
			}
		})
	}
}
