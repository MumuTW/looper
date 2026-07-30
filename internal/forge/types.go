package forge

import "github.com/nexu-io/looper/internal/config"

type ProviderKind = config.ProviderKind

const (
	ProviderKindGitHub  = config.ProviderKindGitHub
	ProviderKindForgejo = config.ProviderKindForgejo
)

type RepositoryRef struct {
	ProviderID string
	Kind       ProviderKind
	BaseURL    string
	Repo       string
}

type ReviewDiscoveryStrategy string
type ReviewPublishStrategy string
type ThreadResolutionStrategy string
type ReviewCommentResolutionStrategy string
type WorkerClaimStrategy string
type WebhookStrategy string

const (
	ReviewDiscoveryReviewRequest ReviewDiscoveryStrategy = "review_request"
	ReviewDiscoveryLabel         ReviewDiscoveryStrategy = "label"

	ReviewPublishNative      ReviewPublishStrategy = "native_review"
	ReviewPublishCommentOnly ReviewPublishStrategy = "comment_only"

	ThreadResolutionNative   ThreadResolutionStrategy = "native"
	ThreadResolutionDisabled ThreadResolutionStrategy = "disabled"

	ReviewCommentResolutionNative     ReviewCommentResolutionStrategy = "native"
	ReviewCommentResolutionManualOnly ReviewCommentResolutionStrategy = "manual_only"
	ReviewCommentResolutionDisabled   ReviewCommentResolutionStrategy = "disabled"

	WorkerClaimAssignSelf  WorkerClaimStrategy = "assign_self"
	WorkerClaimPreAssigned WorkerClaimStrategy = "pre_assigned"

	WebhookNative  WebhookStrategy = "native"
	WebhookPolling WebhookStrategy = "polling"
)

type Capabilities struct {
	Issues                  bool
	PullRequests            bool
	Labels                  bool
	Assignees               bool
	Comments                bool
	Identity                bool
	Diffs                   bool
	NativeReviews           bool
	ReviewRequests          bool
	AutoMerge               bool
	Webhooks                bool
	ReviewCommentResolution ReviewCommentResolutionStrategy
	Dependencies            bool

	ReviewDiscovery  ReviewDiscoveryStrategy
	ReviewPublish    ReviewPublishStrategy
	ThreadResolution ThreadResolutionStrategy
	WorkerClaim      WorkerClaimStrategy
	Webhook          WebhookStrategy

	// GitHubPullRequests records that pull-request lifecycle calls for this
	// task source are served by the GitHub code repository.
	GitHubPullRequests bool
	// GitHubIssues records that issue discovery and mutation are served by
	// GitHub.
	GitHubIssues bool
	// GitHubCLIPullRequestCreation records the legacy GitHub CLI requirement
	// for agent-created PRs. Other provider adapters own their own tooling.
	GitHubCLIPullRequestCreation bool
}

func StaticCapabilities(kind ProviderKind) (Capabilities, bool) {
	switch kind {
	case ProviderKindGitHub:
		return Capabilities{Issues: true, PullRequests: true, Labels: true, Assignees: true, Comments: true, Identity: true, Diffs: true, NativeReviews: true, ReviewRequests: true, AutoMerge: true, Webhooks: true, ReviewCommentResolution: ReviewCommentResolutionNative, ReviewDiscovery: ReviewDiscoveryReviewRequest, ReviewPublish: ReviewPublishNative, ThreadResolution: ThreadResolutionNative, WorkerClaim: WorkerClaimAssignSelf, Webhook: WebhookNative, GitHubPullRequests: true, GitHubIssues: true, GitHubCLIPullRequestCreation: true}, true
	case ProviderKindForgejo:
		return Capabilities{Issues: true, PullRequests: true, Labels: true, Assignees: true, Comments: true, Identity: true, Diffs: true, NativeReviews: true, ReviewRequests: true, AutoMerge: false, Webhooks: false, ReviewCommentResolution: ReviewCommentResolutionManualOnly, Dependencies: false, ReviewDiscovery: ReviewDiscoveryReviewRequest, ReviewPublish: ReviewPublishNative, ThreadResolution: ThreadResolutionDisabled, WorkerClaim: WorkerClaimPreAssigned, Webhook: WebhookPolling}, true
	default:
		return Capabilities{}, false
	}
}

type Identity struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}
