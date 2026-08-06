package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
)

const DefaultServerPort = 17310

const (
	DefaultReviewerAutoRecoveryMaxAttempts = 3
	DefaultReviewerRetryMaxDelayMS         = 300000
)

// DefaultLooperHome resolves the daemon's state directory. LOOPER_HOME overrides
// it ahead of HOME, mirroring how LOOPER_CONFIG overrides config discovery, so a
// second instance — or a test binary — can own its own worktree roots, database,
// and logs instead of writing into the operator's real ~/.looper.
func DefaultLooperHome() (string, error) {
	if override := strings.TrimSpace(os.Getenv("LOOPER_HOME")); override != "" {
		return override, nil
	}

	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		resolvedHomeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		homeDir = resolvedHomeDir
	}

	return filepath.Join(homeDir, ".looper"), nil
}

func DefaultConfigPath() (string, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(looperHome, "config.toml"), nil
}

func DefaultWorktreeRoot() (string, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(looperHome, "worktrees"), nil
}

// ErrUnisolatedWorktreeRoot is returned when a test binary resolves this
// fallback without an explicit LOOPER_HOME.
//
// This is the one fallback that creates directories in the operator's real
// ~/.looper. A fixture whose project record carries no worktreeRoot lands here,
// and the resulting directories are never registered in the worktrees table, so
// no cleanup pass has a record to plan from. They accumulate for as long as the
// suite keeps running — one container per distinct temporary repo path, which
// is one per test run.
//
// A doc comment asking test authors to call testenv.RunTestMain did not hold:
// the packages that reach this fallback are not the packages whose names
// suggest worktrees, so the ones that leak are exactly the ones nobody thought
// to annotate. Failing closed makes the invariant checkable instead of
// remembered — a leaking package now fails its own tests, in its own build,
// with the fix in the error text.
var ErrUnisolatedWorktreeRoot = fmt.Errorf(
	"refusing to resolve the default worktree root in a test binary without LOOPER_HOME: " +
		"add `func TestMain(m *testing.M) { os.Exit(testenv.RunTestMain(m)) }` to this package, " +
		"or set an explicit worktreeRoot on the project fixture")

func DefaultProjectWorktreeRoot(projectID string, repoIdentity string) (string, error) {
	if testing.Testing() && strings.TrimSpace(os.Getenv("LOOPER_HOME")) == "" {
		return "", ErrUnisolatedWorktreeRoot
	}

	worktreeRoot, err := DefaultWorktreeRoot()
	if err != nil {
		return "", err
	}

	return filepath.Join(worktreeRoot, ToRepoWorktreeDirectoryName(repoIdentity), ToProjectWorktreeDirectoryName(projectID)), nil
}

func DefaultConfig(cwd string) (Config, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return Config{}, err
	}

	backupDir := filepath.Join(looperHome, "backups")
	logDir := filepath.Join(looperHome, "logs")

	return Config{
		Server: ServerConfig{
			Host:     "127.0.0.1",
			Port:     DefaultServerPort,
			AuthMode: AuthModeNone,
		},
		Storage: StorageConfig{
			Mode:      "sqlite",
			DBPath:    filepath.Join(looperHome, "looper.sqlite"),
			BackupDir: stringPtr(backupDir),
		},
		Scheduler: SchedulerConfig{
			PollIntervalSeconds:         30,
			MaxConcurrentRuns:           3,
			RetryMaxAttempts:            -1,
			ConsecutiveFailureThreshold: 3,
			RetryBaseDelayMS:            5000,
			SlowLaneWarnThresholdMS:     5000,
			DiscoveryCacheTTLSeconds:    30,
		},
		Webhook: WebhookConfig{
			Enabled:                     false,
			Mode:                        WebhookModeGHForward,
			ListenPort:                  0,
			PublicBaseURL:               "",
			FallbackPollIntervalSeconds: 300,
		},
		Network: NetworkConfig{},
		Agent: AgentConfig{
			Params:       map[string]any{},
			Env:          map[string]string{},
			NativeResume: AgentNativeResumeConfig{Enabled: true},
			Timeouts: AgentTimeoutConfig{
				PlannerSeconds:  60 * 60,
				WorkerSeconds:   3 * 60 * 60,
				ReviewerSeconds: 90 * 60,
				FixerSeconds:    2 * 60 * 60,

				PlannerIdleTimeoutSeconds:  10 * 60,
				PlannerMaxRuntimeSeconds:   60 * 60,
				WorkerIdleTimeoutSeconds:   15 * 60,
				WorkerMaxRuntimeSeconds:    3 * 60 * 60,
				ReviewerIdleTimeoutSeconds: 10 * 60,
				ReviewerMaxRuntimeSeconds:  90 * 60,
				FixerIdleTimeoutSeconds:    10 * 60,
				FixerMaxRuntimeSeconds:     2 * 60 * 60,
			},
		},
		Logging: LoggingConfig{
			Level:     LogLevelInfo,
			MaxSizeMB: 10,
			MaxFiles:  5,
		},
		Notifications: NotificationConfig{
			InApp: true,
			Osascript: OsascriptNotificationConfig{
				Enabled: runtime.GOOS == "darwin",
				SoundForLevels: []NotificationSoundLevel{
					NotificationSoundLevelActionRequired,
					NotificationSoundLevelFailure,
				},
				ThrottleWindowSeconds: 60,
			},
			Webhook: WebhookNotificationConfig{
				Enabled:               false,
				Format:                "generic",
				ThrottleWindowSeconds: 60,
			},
		},
		Disclosure: DefaultDisclosureConfig(),
		Tools:      ToolPathsConfig{},
		Daemon: DaemonConfig{
			Mode:                   DaemonModeForeground,
			RestartPolicy:          DaemonRestartOnFailure,
			RestartThrottleSeconds: 10,
			LogDir:                 logDir,
			ShutdownTimeoutMS:      1000,
			WorkingDirectory:       cwd,
			Environment:            map[string]string{},
			WorktreeCleanup: WorktreeCleanupConfig{
				Enabled:       true,
				Interval:      "24h",
				RetentionDays: 7,
				MaxPerTick:    10,
				// Orphans -- records no loop, run, or queue item references -- are
				// included because the retention window already gates them: Plan
				// notes every candidate's own CreatedAt/UpdatedAt, so an orphan
				// ages like any other worktree. Excluding them made the sweeper
				// inert: the reference graph drops old worktrees as loops move on,
				// so nearly everything eligible by age is an orphan, and the pass
				// skipped it regardless of age.
				IncludeOrphans: true,
				DryRun:         false,
				// Removals are one RemoveAll each, so the budget is sized to
				// drain a real backlog rather than to pace git commands. The
				// backlog this has to survive is bursty, not steady: a single
				// day of agents re-running a test suite that resolves the real
				// worktree root can mint five figures of debris, and a budget
				// in the tens or hundreds would never catch up at a 24h
				// interval. It stays a cap, not a target — status reports what
				// the budget left behind.
				MaxDiskSweepPerTick: 2000,
			},
			ResourceGuard: ResourceGuardConfig{
				Enabled: true,
				// Disk is the load-bearing signal: running the state
				// filesystem dry does not merely slow looper down, it turns
				// SQLite write failures into corruption. 5% and 10 GB are
				// applied as the looser of the two: admission requires only one
				// to be satisfied, so the percentage keeps the guard satisfiable
				// on a filesystem smaller than the absolute floor (common for
				// containers and small dedicated volumes) and the absolute floor
				// caps percentage slack on a multi-terabyte filesystem.
				MinDiskFreePercent: 5,
				MinDiskFreeGB:      10,
				// Load is a coarse backstop, defaulted conservatively so it
				// does not refuse work on a healthy build machine. 2.0 per CPU
				// is the conventional "genuinely oversubscribed" line; an
				// operator who wants dispatch to back off as soon as the host
				// saturates should set this nearer 1.0.
				MaxLoadPerCPU: 2,
			},
		},
		Package: PackageConfig{
			Distribution:               "github-release",
			AutoMigrateOnStartup:       true,
			RequireBackupBeforeMigrate: false,
		},
		Defaults: DefaultsConfig{
			BaseBranch:         "main",
			AllowAutoCommit:    true,
			AllowAutoPush:      true,
			AllowAutoApprove:   true,
			AllowAutoMerge:     false,
			AllowRiskyFixes:    false,
			FixAllPullRequests: false,
			OpenPRStrategy:     OpenPRStrategyAllDone,
			AddSnapshotMode:    AddSnapshotModeAsync,
		},
		Instructions: InstructionsConfig{Enabled: true, MaxBytes: 8192},
		Roles: RoleConfigs{
			Triager: TriagerRoleConfig{
				Preset:      TriagerPresetLegacy,
				Classify:    true,
				AuthorTiers: map[string]TriagerAdmissionOutcome{},
				Legacy: TriagerLegacyPolicyConfig{
					AutoRouteConfidence:         0.8,
					MaxAutoRouteRisk:            "low",
					RequireInScope:              true,
					RequireNoMissingInformation: true,
					RequirePlanner:              true,
					RequireRationale:            true,
				},
			},
			Auditor: AuditorRoleConfig{Enabled: false, WindowMinutes: 60},
			Gatekeeper: GatekeeperRoleConfig{
				RequiredReviewChangedLines: 200,
			},
			Coordinator: CoordinatorRoleConfig{
				Enabled:      false,
				PollInterval: "5m",
				Triage: CoordinatorTriageConfig{
					TriagedLabel:    "triaged",
					MaxIssueAgeDays: 7,
					MaxPerTick:      5,
					Disposition: CoordinatorTriageDispositionConfig{
						OutOfScopeLabel:       "wontfix",
						UnclearLabel:          "needs-info",
						ReTriageOnAuthorReply: true,
					},
				},
				Dispatch: CoordinatorDispatchConfig{
					Mode: "human-gated",
					HumanGate: CoordinatorDispatchHumanGateConfig{
						SlashCommands: []string{"/plan", "/implement"},
						AllowedUsers:  []string{},
					},
					Autonomous: CoordinatorDispatchAutonomousConfig{
						DelayMinutes: 30,
						HoldLabel:    labels.HoldGlobal,
					},
					AssignTo: "",
				},
				Dependencies: CoordinatorDependenciesConfig{
					Enabled:           false,
					APITimeoutSeconds: 10,
					APIRetryAttempts:  3,
				},
				MergeWatch: CoordinatorMergeWatchConfig{
					TransientRetries:         3,
					MaxIndeterminateDuration: "15m",
				},
				MarkReady: CoordinatorMarkReadyConfig{
					Enabled: false,
					Scope:   CoordinatorMarkReadyScopeLooperOnly,
				},
			},
			Planner: PlannerRoleConfig{
				AutoDiscovery: true,
				Triggers: IssueRoleTriggersConfig{
					Labels:                     []string{labels.DefaultPlanTrigger},
					LabelMode:                  LabelModeAll,
					RequireAssigneeCurrentUser: true,
				},
			},
			Reviewer: ReviewerRoleConfig{
				Discovery: ReviewerRoleDiscoveryConfig{
					AutoDiscovery: true,
					Triggers: ReviewerRoleTriggersConfig{
						IncludeDrafts:        false,
						RequireReviewRequest: true,
						EnableSelfReview:     false,
						Labels:               []string{},
						LabelMode:            LabelModeAll,
					},
					SpecReview: ReviewerSpecReviewConfig{
						IncludeReviewingLabel: true,
						ReviewingLabel:        labels.SpecReviewing,
					},
				},
				Behavior: ReviewerConfig{
					Loop: ReviewerLoopConfig{
						EnabledByDefault:          true,
						QuietPeriodSeconds:        60,
						MinPublishIntervalSeconds: 300,
						MaxIterationsPerPR:        20,
						MaxIterationsPerHead:      1,
						MaxWallClockSeconds:       0,
						MaxConsecutiveFailures:    3,
						MaxAgentExecutionsPerPR:   25,
						StopOnApproved:            false,
						StopOnReadyLabel:          true,
						StopOnIdenticalOutput:     true,
					},
					Retry:                   DefaultReviewerRetryConfig(),
					Scope:                   ReviewerScopeChangedRanges,
					PublishMode:             ReviewerPublishModeSingleReview,
					ReviewEvents:            ReviewerReviewEventsConfig{Clean: ReviewerReviewEventApprove, Blocking: ReviewerReviewEventRequestChanges},
					DetectDuplicateFindings: true,
					NativeResume:            ReviewerNativeResumeConfig{OnHeadChange: false, ReReviewPromptOnHeadChange: false},
					ThreadResolution: ReviewerThreadResolutionConfig{
						Enabled:                     false,
						Mode:                        ReviewerThreadResolutionModeReportOnly,
						Scope:                       ReviewerThreadResolutionScopeLooperAuthoredOnly,
						AutoResolve:                 ReviewerThreadResolutionAutoResolveObjectiveOnly,
						RequireAuditComment:         true,
						RequireNewHeadSinceThread:   true,
						RequireCurrentReviewRequest: true,
						MaxThreadsPerRun:            10,
					},
				},
				AutoMerge: ReviewerAutoMergeConfig{
					Enabled:                 false,
					Strategy:                ReviewerAutoMergeStrategySquash,
					RequireBranchProtection: true,
					TransientRetries:        3,
					Scope:                   ReviewerAutoMergeScopeLooperOnly,
				},
			},
			Fixer: FixerRoleConfig{
				AutoDiscovery: true,
				Triggers: FixerRoleTriggersConfig{
					IncludeDrafts: false,
					AuthorFilter:  FixerAuthorFilterCurrentUser,
					Labels:        []string{},
					LabelMode:     LabelModeAll,
				},
			},
			Worker: WorkerRoleConfig{
				AutoDiscovery: true,
				Triggers: IssueRoleTriggersConfig{
					Labels:                     []string{labels.DefaultWorkerReadyTrigger},
					LabelMode:                  LabelModeAll,
					RequireAssigneeCurrentUser: true,
				},
			},
			Escalator: EscalatorRoleConfig{
				Enabled:               false,
				CadenceSeconds:        3600,
				RetryAttemptThreshold: 2,
				UnroutedAfterSeconds:  3600,
				StaleHeadAfterSeconds: 86400,
				MaxItems:              500,
			},
		},
		Projects: []ProjectRefConfig{},
	}, nil
}

func DefaultReviewerRetryConfig() ReviewerRetryConfig {
	return ReviewerRetryConfig{
		EnhancedTransientClassification: false,
		ExtraTransientErrorPatterns:     []string{},
		RecoverExistingMatchedFailures:  false,
		AutoRecoveryMaxAttempts:         DefaultReviewerAutoRecoveryMaxAttempts,
		MaxDelayMS:                      DefaultReviewerRetryMaxDelayMS,
	}
}

// DefaultReviewerConvergenceConfig returns the semantic loop defaults from
// issue #115. It is separate from DefaultConfig so the optional config
// pointer can remain nil in canonical output while runtime still has one
// documented default authority.
func DefaultReviewerConvergenceConfig() ReviewerConvergenceConfig {
	return ReviewerConvergenceConfig{
		MaxConsecutiveUnproductive: 3,
		MaxFixerAttemptsPerItem:    4,
		MaxTotalRounds:             40,
		SeverityFloor:              ReviewerSeverityFloorNonBlocking,
	}
}

func DefaultDisclosureConfig() DisclosureConfig {
	return DisclosureConfig{
		Enabled:      true,
		IncludeAgent: true,
		IncludeOS:    false,
		Channels: DisclosureChannelsConfig{
			GitCommit:            true,
			PullRequest:          true,
			IssueComment:         true,
			ReviewComment:        true,
			InlineCommentVisible: true,
		},
	}
}

func stringPtr(value string) *string {
	return &value
}
