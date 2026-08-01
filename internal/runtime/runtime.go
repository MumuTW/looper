package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MumuTW/looper/internal/bootstrap"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/domain"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/notify"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/loops"
	networkclient "github.com/MumuTW/looper/internal/network/client"
	"github.com/MumuTW/looper/internal/processidentity"
	"github.com/MumuTW/looper/internal/projects"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/webhookforward"
	"github.com/MumuTW/looper/internal/worker"
)

type OpenSQLiteCoordinatorFunc func(context.Context, string, storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error)

type SyncConfiguredProjectsFunc func(context.Context, *projects.Service, config.Config, time.Time) error

type RunSchedulerTickFunc func(context.Context, Services) error

type ReadProcessCommandFunc func(context.Context, int) (string, error)

type ReadProcessStartFunc func(context.Context, int) (int64, error)

type ReadProcessBootIDFunc func(context.Context, int) (string, error)

type SignalProcessFunc func(int, syscall.Signal) error

type RecoverySummary struct {
	StartedAt             string                     `json:"startedAt,omitempty"`
	CompletedAt           string                     `json:"completedAt,omitempty"`
	OrphanAgentCleanup    RecoveryOrphanAgentCleanup `json:"orphanAgentCleanup"`
	ExpiredLocksReleased  int64                      `json:"expiredLocksReleased"`
	InterruptedRunsMarked int64                      `json:"interruptedRunsMarked"`
	LoopsRequeued         int64                      `json:"loopsRequeued"`
	EventsWritten         int64                      `json:"eventsWritten"`
}

type StaleRunReconcileSummary struct {
	Mode                string `json:"mode"`
	StartedAt           string `json:"startedAt,omitempty"`
	CompletedAt         string `json:"completedAt,omitempty"`
	CandidateRuns       int64  `json:"candidateRuns"`
	InterruptedRuns     int64  `json:"interruptedRuns"`
	LoopsRequeued       int64  `json:"loopsRequeued"`
	QueueItemsRequeued  int64  `json:"queueItemsRequeued"`
	QueueItemsCancelled int64  `json:"queueItemsCancelled"`
	CleanedExecutions   int64  `json:"cleanedExecutions"`
	// QuarantinedExecutions counts executions parked via quarantineRecoveryEvidence
	// (still-running evidence + manual_intervention). Never report these as cleaned.
	QuarantinedExecutions int64    `json:"quarantinedExecutions"`
	SkippedUncertainRuns  int64    `json:"skippedUncertainRuns"`
	EventsWritten         int64    `json:"eventsWritten"`
	RunIDs                []string `json:"runIds,omitempty"`
	LoopIDs               []string `json:"loopIds,omitempty"`
	ExecutionIDs          []string `json:"executionIds,omitempty"`
	// QuarantinedLoopIDs names the loops parked by this pass, so a caller can
	// report them without re-deriving the set from event_logs.
	QuarantinedLoopIDs []string `json:"quarantinedLoopIds,omitempty"`
	// QuarantineSettlement reports the evidence retired this pass because an
	// operator already disposed of the loop behind it (#149 / #150).
	QuarantineSettlement QuarantineSettlementSummary `json:"quarantineSettlement"`
}

type staleRunReconcileMode string

const (
	staleRunReconcileModeStartup staleRunReconcileMode = "startup"
	staleRunReconcileModeLive    staleRunReconcileMode = "live"
)

type RecoveryOrphanAgentCleanup struct {
	Attempted        bool  `json:"attempted"`
	CleanedCount     int64 `json:"cleanedCount"`
	QuarantinedCount int64 `json:"quarantinedCount"`
	// Classification counts for active execution evidence (ADR-0015 R8 / #581).
	// ConfirmedDead includes durable terminal/current-daemon drain authority and
	// active rows are never promoted here from PID/lease evidence alone.
	ConfirmedDeadCount int64  `json:"confirmedDeadCount"`
	ObservedLiveCount  int64  `json:"observedLiveCount"`
	UncertainCount     int64  `json:"uncertainCount"`
	Warning            string `json:"warning,omitempty"`
}

type Options struct {
	Config config.Config
	// InitialConfig and ReloadConfig are supplied by bootstrap so hot reloads
	// replay the daemon's exact startup precedence (file, environment, and CLI).
	// Tests and embedders that omit ReloadConfig simply run without a watcher.
	InitialConfig        config.LoadedFileConfig
	ReloadConfig         func() (config.LoadedFileConfig, error)
	LoadConfigAt         func(string) (config.LoadedFileConfig, error)
	ConfigReloadInterval time.Duration
	// ConfigPath is the daemon-loaded config file path (from --config /
	// LOOPER_CONFIG resolution). Runtime config management patches this source;
	// trusted review submission receives a separate sanitized run snapshot.
	ConfigPath                  string
	Logger                      bootstrap.Logger
	Now                         func() time.Time
	ShutdownTimeout             time.Duration
	WorktreeCleanupInitialDelay time.Duration
	OpenSQLiteCoordinator       OpenSQLiteCoordinatorFunc
	SyncConfiguredProjects      SyncConfiguredProjectsFunc
	RunSchedulerTick            RunSchedulerTickFunc
	// RunSchedulerClaim overrides the claim pass the scheduler pump drives,
	// independently of RunSchedulerTick in both directions: either can be
	// injected while the other keeps its default catalog implementation.
	//
	// Blind spots, stated for reviewers of tests built on this seam: an
	// injected claim observes that the pump invoked a pass, not when the
	// pump chose to fire (ticker/trigger cadence regressions pass through),
	// and it bypasses the default claim's own internal admission gating,
	// which only the default implementation exercises.
	RunSchedulerClaim RunSchedulerTickFunc
	// WebhookForwarder overrides the webhook forwarder wired during startup.
	// Like RunSchedulerTick, an injected forwarder owns its own dependencies;
	// nil uses the default catalog-scheduler forwarder.
	WebhookForwarder   WebhookForwarder
	ReadProcessCommand ReadProcessCommandFunc
	ReadProcessStart   ReadProcessStartFunc
	ReadProcessBootID  ReadProcessBootIDFunc
	SignalProcess      SignalProcessFunc
	DeferRecovery      bool
}

type Services struct {
	Coordinator      *storage.SQLiteCoordinator
	Repositories     *storage.Repositories
	Projects         *projects.Service
	Loops            *loops.Service
	ActiveExecutions *ActiveExecutionRegistry
}

type WebhookForwarder interface {
	Forward(context.Context, webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error)
	Stats() webhookforward.Stats
	// CancelExecute aborts in-flight webhook discovery without waiting for drain.
	// BeginShutdown and MarkDegraded call this so admission-closed races cannot
	// still enqueue after a one-time AllowExecute pass.
	CancelExecute()
	Close()
}

type Runtime struct {
	config     config.Config
	configPath string
	logger     bootstrap.Logger
	now        func() time.Time

	configReloadMu       sync.Mutex
	configBoundary       sync.RWMutex
	loadedConfig         config.LoadedFileConfig
	reloadConfig         func() (config.LoadedFileConfig, error)
	loadConfigAt         func(string) (config.LoadedFileConfig, error)
	configReloadInterval time.Duration
	configReloadStop     chan struct{}
	configReloadDone     chan struct{}
	configReloadStatus   ConfigReloadStatus

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc
	runSchedulerTick       RunSchedulerTickFunc
	defaultSchedulerTick   RunSchedulerTickFunc
	defaultSchedulerClaim  RunSchedulerTickFunc
	customSchedulerTick    bool
	customSchedulerClaim   bool
	customWebhookForwarder bool
	readProcessCommand     ReadProcessCommandFunc
	readProcessStart       ReadProcessStartFunc
	readProcessBootID      ReadProcessBootIDFunc
	signalProcess          SignalProcessFunc
	shutdownTimeout        time.Duration
	deferRecovery          bool

	mu                          sync.RWMutex
	startedAt                   *time.Time
	recovery                    RecoverySummary
	stopped                     bool
	services                    Services
	startErr                    error
	startOnce                   sync.Once
	shutdownOnce                sync.Once
	shutdownCh                  chan struct{}
	schedulerStop               chan struct{}
	schedulerDone               chan struct{}
	schedulerWake               chan struct{}
	schedulerClaimWake          chan struct{}
	schedulerCancel             context.CancelFunc
	schedulerTasks              *schedulerTaskTracker
	worktreeCleanupStop         chan struct{}
	worktreeCleanupDone         chan struct{}
	worktreeCleanupWake         chan struct{}
	worktreeCleanupCancel       context.CancelFunc
	worktreeCleanupRunning      bool
	worktreeCleanupInitialDelay time.Duration
	worktreeCleanupStatus       WorktreeCleanupStatus
	// workProducerJoinStarted latches the first BeginDrain producer join for
	// the process lifetime. Never clear it: a second POST must not overwrite
	// residual done-channels after the first join timed out.
	workProducerJoinStarted atomic.Bool
	// workProducerJoinActive is true only while the join goroutine is inside stop*.
	workProducerJoinActive atomic.Bool
	// workProducerJoinDone is closed when the drain join goroutine finishes
	// stop* and registers residual done-channels. Stop waits on this first.
	workProducerJoinDone chan struct{}
	// drainResidualDones holds producer done-channels still open after a
	// bounded stop* timeout so DrainSnapshot keeps WorkProducersActive>0.
	drainResidualDones       []<-chan struct{}
	projectDiscovery         *projectDiscoveryRunner
	resumeProjectDiscoveries func(context.Context, *projects.Service) error
	recoveryCancel           context.CancelFunc
	recoveryDone             chan struct{}
	activeExecutions         *ActiveExecutionRegistry
	projectCatalog           *projects.Catalog
	githubGateway            *githubinfra.Gateway
	webhook                  *webhookRuntime
	databaseDaemonLock       *storage.DatabaseLock
	webhookForwarder         WebhookForwarder
	notificationGateways     *schedulerNotificationGatewayFactory
	networkManager           runtimeNetworkManager
	schedulerDisabled        bool
	startupReadyOnce         sync.Once
	startupReadyErr          error
	// ownershipAcquired remains true after CompleteStartup succeeds so stop
	// still writes looperd.stopped. Admission is the sole ready Authority;
	// this flag is not a mutation/claim gate.
	ownershipAcquired bool
	admission         *Admission
	// daemonBinary answers whether the executable file this daemon was launched
	// from still holds the image it is running (#154).
	daemonBinary *daemonBinaryWatcher

	// shutdownDrainErr is set by BeginShutdown when producer/handle drain fails.
	// Stop retains SQLite when non-nil (ADR-0015 / #577).
	shutdownDrainErr error
	// storageRetained is true when Stop skipped coordinator.Close after a
	// drain failure so undrained ownership is not closed under SQLite.
	storageRetained bool
}

type runtimeNetworkManager interface {
	Start(context.Context) error
	Stop()
	Status() networkclient.Status
	UpdateConfig(config.Config)
}

const reviewerRecoveryLoginTimeout = 3 * time.Second

func New(options Options) *Runtime {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	openSQLiteCoordinator := options.OpenSQLiteCoordinator
	if openSQLiteCoordinator == nil {
		openSQLiteCoordinator = storage.OpenSQLiteCoordinator
	}

	syncConfiguredProjects := options.SyncConfiguredProjects
	if syncConfiguredProjects == nil {
		syncConfiguredProjects = defaultSyncConfiguredProjects
	}

	runSchedulerTick := options.RunSchedulerTick
	customSchedulerTick := runSchedulerTick != nil
	customSchedulerClaim := options.RunSchedulerClaim != nil
	customWebhookForwarder := options.WebhookForwarder != nil

	readProcessCommand := options.ReadProcessCommand
	if readProcessCommand == nil {
		readProcessCommand = defaultReadProcessCommand
	}
	readProcessStart := options.ReadProcessStart
	if readProcessStart == nil {
		readProcessStart = defaultReadProcessStart
	}
	readProcessBootID := options.ReadProcessBootID
	if readProcessBootID == nil {
		readProcessBootID = defaultReadProcessBootID
	}

	signalProcess := options.SignalProcess
	if signalProcess == nil {
		signalProcess = defaultSignalProcess
	}

	shutdownTimeout := options.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Duration(options.Config.Daemon.ShutdownTimeoutMS) * time.Millisecond
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = time.Second
	}

	projectCatalog := projects.NewCatalog(options.Config)
	loadedConfig := options.InitialConfig
	if reflect.DeepEqual(loadedConfig.Config, config.Config{}) {
		loadedConfig.Config = options.Config
		loadedConfig.Metadata.ConfigPath = strings.TrimSpace(options.ConfigPath)
	}
	reloadInterval := options.ConfigReloadInterval
	if reloadInterval <= 0 {
		reloadInterval = time.Second
	}
	rt := &Runtime{
		config:                      options.Config,
		configPath:                  strings.TrimSpace(options.ConfigPath),
		logger:                      options.Logger,
		now:                         now,
		loadedConfig:                loadedConfig,
		reloadConfig:                options.ReloadConfig,
		loadConfigAt:                options.LoadConfigAt,
		configReloadInterval:        reloadInterval,
		openSQLiteCoordinator:       openSQLiteCoordinator,
		syncConfiguredProjects:      syncConfiguredProjects,
		runSchedulerTick:            runSchedulerTick,
		customSchedulerTick:         customSchedulerTick,
		defaultSchedulerClaim:       options.RunSchedulerClaim,
		customSchedulerClaim:        customSchedulerClaim,
		webhookForwarder:            options.WebhookForwarder,
		customWebhookForwarder:      customWebhookForwarder,
		readProcessCommand:          readProcessCommand,
		readProcessStart:            readProcessStart,
		readProcessBootID:           readProcessBootID,
		signalProcess:               signalProcess,
		shutdownTimeout:             shutdownTimeout,
		worktreeCleanupInitialDelay: options.WorktreeCleanupInitialDelay,
		projectDiscovery:            newProjectDiscoveryRunner(),
		deferRecovery:               options.DeferRecovery,
		recovery:                    createEmptyRecoverySummary(),
		shutdownCh:                  make(chan struct{}),
		activeExecutions:            NewActiveExecutionRegistry(),
		projectCatalog:              projectCatalog,
		webhook:                     newWebhookRuntime(options.Config, options.Logger, now),
		admission:                   NewAdmission(),
		daemonBinary:                newDaemonBinaryWatcher(options.Logger),
	}
	// Project daemon Admission onto agent spawn leases so cmd.Start is refused
	// while starting/stopping/degraded (#576 + #575).
	rt.activeExecutions.SetAllowSpawn(rt.AllowClaim)
	// Hard agent_executions observation failures close admission until process
	// restart (#578 / ADR-0015 R5). Prefer split-brain stop over silent continue.
	rt.activeExecutions.SetOnHardPersistFailure(func(err error) {
		reason := "agent execution persistence hard failure"
		if err != nil {
			reason = reason + ": " + err.Error()
		}
		if markErr := rt.MarkDegraded(reason); markErr != nil && rt.logger != nil {
			rt.logger.Warn("failed to mark admission degraded after persistence failure", map[string]any{
				"error":  markErr.Error(),
				"reason": reason,
			})
		} else if rt.logger != nil {
			rt.logger.Error("daemon admission degraded after agent execution persistence failure", map[string]any{
				"reason": reason,
			})
		}
	})
	if rt.webhook != nil {
		rt.webhook.forwarder = rt.WebhookForwarder
		// Tunnel listener is not behind the API mutation gate; consult the same
		// admission Authority before accepting deliveries (#583). Worker-side
		// discovery rechecks via webhookforward.Options.AllowExecute.
		rt.webhook.allowForward = rt.AllowMutations
	}
	if !customSchedulerTick {
		rt.runSchedulerTick = rt.executeDefaultSchedulerTick
	}
	return rt
}

func Start(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := New(Options{
		Config:        deps.Config,
		InitialConfig: deps.InitialConfig,
		ReloadConfig:  deps.ReloadConfig,
		LoadConfigAt:  deps.LoadConfigAt,
		Logger:        deps.Logger,
	})
	if err := rt.Start(ctx); err != nil {
		return nil, err
	}

	return rt, nil
}

func (r *Runtime) Start(ctx context.Context) error {
	r.startOnce.Do(func() {
		r.startErr = r.start(ctx)
		if r.startErr != nil {
			// Ownership of an injected forwarder transferred at construction;
			// a caller whose Start failed must not be required to Stop the
			// runtime just to release it.
			r.closeInjectedWebhookForwarder()
		}
	})

	return r.startErr
}

// closeInjectedWebhookForwarder releases a construction-injected forwarder
// exactly once: the field is cleared under the lock so a later Stop cannot
// double-close it.
func (r *Runtime) closeInjectedWebhookForwarder() {
	if !r.customWebhookForwarder {
		return
	}
	r.mu.Lock()
	forwarder := r.webhookForwarder
	r.webhookForwarder = nil
	r.mu.Unlock()
	if forwarder != nil {
		forwarder.Close()
	}
}

func (r *Runtime) Stop(reason string) {
	r.shutdownOnce.Do(func() {
		if r.logger != nil {
			r.logger.Info("looperd runtime stopping", map[string]any{"reason": reason})
		}

		// If BeginDrain already started a background producer join, wait for it
		// before we stop again or close storage. Concurrent stop* on the same
		// loops would race, and closing SQLite under a join-owned wait is unsafe.
		r.waitForDrainProducerJoin()

		// Close admission and cancel work-producing contexts before draining
		// producers (ADR-0015 shutdown order). Use BeginShutdown so direct
		// Runtime.Stop matches the daemon path: scheduler, deferred recovery,
		// and in-flight webhook discovery are canceled before any waits.
		r.BeginShutdown(reason)

		r.stopConfigReloadLoop()
		r.stopDeferredReviewerRecovery()
		r.stopWorktreeCleanupLoop()
		r.stopSchedulerLoop()
		r.stopProjectDiscovery()
		r.stopWebhookRuntime()

		// Re-collect non-agent containment drain failures reported while
		// producers finished after BeginShutdown (shell validation / trusted
		// review cancel paths). Agent failures were already joined above.
		if r.activeExecutions != nil {
			if err := r.activeExecutions.NonAgentDrainErr(); err != nil {
				r.mu.Lock()
				r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, err)
				r.mu.Unlock()
			}
		}

		r.mu.Lock()
		r.stopped = true
		forwarder := r.webhookForwarder
		r.webhookForwarder = nil
		networkManager := r.networkManager
		r.networkManager = nil
		coordinator := r.services.Coordinator
		repositories := r.services.Repositories
		ownershipAcquired := r.ownershipAcquired
		drainErr := r.shutdownDrainErr
		r.mu.Unlock()

		if ownershipAcquired && repositories != nil {
			if err := r.appendStoppedEvent(context.Background(), repositories, reason); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime stop event failed", map[string]any{"error": err.Error()})
			}
		}

		// Independent infra (forwarder/network) still stop on drain failure;
		// they are not Supervisor domain and must not block retain-storage.
		if forwarder != nil {
			forwarder.Close()
		}
		if networkManager != nil {
			networkManager.Stop()
		}

		// #577: retain SQLite when containment drain failed. Never report
		// graceful success with undrained ownership under a closed DB.
		if drainErr != nil {
			r.mu.Lock()
			r.storageRetained = true
			r.mu.Unlock()
			close(r.shutdownCh)
			if r.logger != nil {
				r.logger.Error("looperd runtime stop retained storage after drain failure", map[string]any{
					"reason": reason,
					"error":  drainErr.Error(),
				})
			}
			return
		}

		r.mu.Lock()
		r.services = Services{}
		r.mu.Unlock()

		if coordinator != nil {
			if err := coordinator.Close(); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime close failed", map[string]any{"error": err.Error()})
			}
		}
		r.releaseDatabaseDaemonLock()

		close(r.shutdownCh)

		if r.logger != nil {
			r.logger.Info("looperd runtime stopped", map[string]any{"reason": reason})
		}
	})
}

// BeginShutdown transitions admission to stopping without closing storage.
// Daemon stop drains HTTP ingress after this so mutations/claims stop first.
// Also cancels the scheduler context so an in-flight full tick observes
// cancellation during the HTTP drain window before Runtime.Stop closes the
// loop; work-producing lanes still recheck AllowClaim as the Authority.
// Cancels deferred reviewer recovery so a post-ready recovery goroutine cannot
// still requeue loops/queue items after admission is already stopping; the
// wait for recovery exit remains in Runtime.Stop via stopDeferredReviewerRecovery.
// Cancels webhook-forward discovery so process exit can abort in-flight
// CreateOrGetActiveByDedupe promptly (sticky MarkDegraded does not cancel
// webhook execute — accepted/202 deliveries must still complete).
//
// Shutdown order (ADR-0015 / #577): close admission → cancel producers →
// confirmed-drain handles (agents + tracked non-agent shell/trusted-review).
// Producer cancel must run before ActiveExecutionRegistry.BeginShutdown waits
// on tracked non-agent handles: validation shell.Run only enters Kill after its
// owner ctx is canceled. Waiting first would burn the full kill budget then
// force-kill instead of cancel/drain promptly.
// SQLite close happens only in Stop after drain succeeds; drain failure is
// recorded for retain-storage. Non-agent Kill/Drain failures that finish after
// this returns are re-collected in Stop via NonAgentDrainErr.
func (r *Runtime) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	// Snapshot cancel targets under r.mu first, then flip stopping + invoke
	// cancels under admission.mu. Do not take r.mu while holding admission.mu
	// (other paths lock r.mu then read admission — that order would deadlock).
	cancels := r.snapshotWorkProducerCancels()
	_ = r.admission.BeginShutdownThen(reason, cancels.invokeForShutdown)
	// Close agent spawn admission and confirmed-drain live handles — agents and
	// tracked Supervisor-owned non-agents (#576/#577). Agent leases cancel via
	// registry; non-agent owners were canceled above.
	if r.activeExecutions != nil {
		if err := r.activeExecutions.BeginShutdown(reason); err != nil {
			r.mu.Lock()
			r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, err)
			r.mu.Unlock()
		}
	}
}

// workProducerCancels is a lock-free snapshot of cancel targets so
// MarkDegraded/BeginShutdown can invoke them while holding admission.mu
// without re-entering r.mu (lock-order safety).
type workProducerCancels struct {
	scheduler        context.CancelFunc
	recovery         context.CancelFunc
	cleanup          context.CancelFunc
	projectDiscovery *projectDiscoveryRunner
	forwarder        interface{ CancelExecute() }
}

// invokeForDegrade cancels sticky-degrade producers but leaves webhook execute
// live: Forward may already have returned accepted/202 for queued discovery,
// and GitHub will not retry that delivery while the daemon stays up.
func (c workProducerCancels) invokeForDegrade() {
	if c.scheduler != nil {
		c.scheduler()
	}
	if c.recovery != nil {
		c.recovery()
	}
	if c.cleanup != nil {
		c.cleanup()
	}
	if c.projectDiscovery != nil {
		c.projectDiscovery.Cancel()
	}
}

// invokeForShutdown cancels all work producers including webhook discovery so
// process exit can abort in-flight CreateOrGetActiveByDedupe promptly.
func (c workProducerCancels) invokeForShutdown() {
	c.invokeForDegrade()
	if c.forwarder != nil {
		c.forwarder.CancelExecute()
	}
}

func (r *Runtime) snapshotWorkProducerCancels() workProducerCancels {
	if r == nil {
		return workProducerCancels{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return workProducerCancels{
		scheduler:        r.schedulerCancel,
		recovery:         r.recoveryCancel,
		cleanup:          r.worktreeCleanupCancel,
		projectDiscovery: r.projectDiscovery,
		forwarder:        r.webhookForwarder,
	}
}

// StorageRetained reports whether Stop skipped SQLite close after a drain
// failure (ADR-0015 / #577). Operators must not treat stop as graceful success.
func (r *Runtime) StorageRetained() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.storageRetained
}

// ShutdownDrainError returns the drain failure recorded during BeginShutdown,
// if any.
func (r *Runtime) ShutdownDrainError() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shutdownDrainErr
}

// AdmissionState returns the authoritative live admission state.
func (r *Runtime) AdmissionState() AdmissionState {
	if r == nil || r.admission == nil {
		return AdmissionStopping
	}
	return r.admission.State()
}

// AllowMutations is the HTTP mutation readiness projection of admission.

// BeginDrain closes new-work admission without canceling active agent
// processes. Scheduler/recovery/cleanup/project-discovery producers are
// stopped and joined in the background so DrainSnapshot stays non-empty until
// they can no longer mutate storage; agents and tracked non-agent shells remain
// owned by DrainSnapshot until they exit. Accepted webhook deliveries keep
// running (no CancelExecute) but count toward WorkProducersActive via Queued/InFlight.
func (r *Runtime) BeginDrain(reason string) error {
	if r == nil || r.admission == nil {
		return ErrAdmissionNotReady
	}
	if err := r.admission.BeginDrain(reason); err != nil {
		return err
	}
	// Process-lifetime single shot: never re-run stop* after the first join
	// started, even if residuals remain and workProducerJoinActive is false.
	// A second join would snapshot nil producer fields and wipe residual dones.
	if r.workProducerJoinStarted.CompareAndSwap(false, true) {
		r.workProducerJoinActive.Store(true)
		r.mu.Lock()
		r.workProducerJoinDone = make(chan struct{})
		r.mu.Unlock()
		go r.joinWorkProducersForDrain()
	}
	return nil
}

// joinWorkProducersForDrain stops the same producer set sticky degrade cancels,
// then waits for each loop to exit. Timed-out producers remain visible via
// drainResidualDones so DrainSnapshot does not report drained early.
func (r *Runtime) joinWorkProducersForDrain() {
	if r == nil {
		return
	}
	// Snapshot done channels before stop* nils the runtime fields, so a
	// shutdownTimeout still leaves residual producers countable.
	r.mu.RLock()
	residual := make([]<-chan struct{}, 0, 3)
	for _, done := range []<-chan struct{}{r.schedulerDone, r.recoveryDone, r.worktreeCleanupDone} {
		if done != nil {
			residual = append(residual, done)
		}
	}
	r.mu.RUnlock()

	// stop* helpers cancel contexts, close stop channels, and wait (bounded by
	// shutdownTimeout) so in-flight producer work is joined, not only canceled.
	r.stopSchedulerLoop()
	r.stopDeferredReviewerRecovery()
	r.stopWorktreeCleanupLoop()
	// Drain join must not record discovery wait timeouts on shutdownDrainErr;
	// that flag retains SQLite on later Stop even after discovery eventually exits.
	r.waitProjectDiscoveryForDrain()

	stillOpen := make([]<-chan struct{}, 0, len(residual))
	for _, done := range residual {
		if channelStillOpen(done) {
			stillOpen = append(stillOpen, done)
		}
	}
	r.mu.Lock()
	r.drainResidualDones = stillOpen
	joinDone := r.workProducerJoinDone
	r.mu.Unlock()
	r.workProducerJoinActive.Store(false)
	if joinDone != nil {
		close(joinDone)
	}
	// Keep watching residual producers so WorkProducersActive clears when they exit.
	for _, done := range stillOpen {
		go r.watchResidualProducerDone(done)
	}
}

func (r *Runtime) watchResidualProducerDone(done <-chan struct{}) {
	if r == nil || done == nil {
		return
	}
	<-done
	r.mu.Lock()
	next := r.drainResidualDones[:0]
	for _, ch := range r.drainResidualDones {
		if ch != done && channelStillOpen(ch) {
			next = append(next, ch)
		}
	}
	r.drainResidualDones = next
	r.mu.Unlock()
}

func (r *Runtime) waitForDrainProducerJoin() {
	if r == nil {
		return
	}
	r.mu.RLock()
	joinDone := r.workProducerJoinDone
	active := r.workProducerJoinActive.Load()
	r.mu.RUnlock()
	if joinDone == nil && !active {
		return
	}
	if joinDone != nil {
		<-joinDone
	}
	// Join finished registering residuals; wait a bounded time for residuals
	// that stop* already timed out on, without blocking forever.
	deadline := time.Now().Add(r.shutdownTimeout)
	if r.shutdownTimeout <= 0 {
		deadline = time.Now().Add(20 * time.Second)
	}
	for time.Now().Before(deadline) {
		r.mu.RLock()
		n := len(r.drainResidualDones)
		r.mu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// DrainSnapshot reports Supervisor-owned work still in flight after BeginDrain.
// It includes work producers still being joined so cutover cannot proceed while
// discovery/scheduler/recovery/accepted-webhook work still mutate storage.
func (r *Runtime) DrainSnapshot() DrainSnapshot {
	if r == nil {
		return DrainSnapshot{}
	}
	snapshot := DrainSnapshot{}
	if r.activeExecutions != nil {
		snapshot = r.activeExecutions.DrainSnapshot()
	}
	snapshot.WorkProducersActive = r.countActiveWorkProducers()
	return snapshot
}

func (r *Runtime) countActiveWorkProducers() int {
	if r == nil {
		return 0
	}
	active := 0
	if r.workProducerJoinActive.Load() {
		active++
	}
	r.mu.RLock()
	active += len(r.drainResidualDones)
	discovery := r.projectDiscovery
	forwarder := r.webhookForwarder
	r.mu.RUnlock()
	if discovery != nil && discovery.Busy() {
		active++
	}
	// Accepted webhook deliveries deliberately keep running after drain; count
	// their outstanding work so final backup cannot race their storage writes.
	if forwarder != nil {
		stats := forwarder.Stats()
		active += stats.Queued + stats.InFlight
	}
	return active
}

// channelStillOpen reports whether a done channel has not yet been closed.
func channelStillOpen(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// WaitForDrain blocks until DrainSnapshot reports no in-flight work or ctx ends.
func (r *Runtime) WaitForDrain(ctx context.Context, interval time.Duration) (DrainSnapshot, error) {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snapshot := r.DrainSnapshot()
		if snapshot.Drained() {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return r.DrainSnapshot(), ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *Runtime) AllowMutations() error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.AllowMutations()
}

// AllowClaim is the scheduler work-producing projection of admission
// (full tick + durable claims).
func (r *Runtime) AllowClaim() error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.AllowClaim()
}

// WithAllowClaim runs fn only while claim admission is open, holding the
// admission mutex for the full duration of fn so MarkDegraded/BeginShutdown
// cannot interleave with the critical section (webhook accept + enqueue).
func (r *Runtime) WithAllowClaim(fn func()) error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	return r.admission.WithAllowWork(fn)
}

// MarkDegraded sticks admission until process restart and cancels work-producing
// contexts (scheduler, recovery, cleanup) so new discovery/claims/cleanup that
// already passed AllowClaim cannot complete after the transition. Unlike
// BeginShutdown, this does not drain active agent handles and does not
// CancelExecute webhook workers: Forward may already have returned accepted/202
// for in-memory queue entries, and sticky degrade leaves the daemon up with no
// GitHub retry. New webhook accepts are still refused via AllowExecute /
// AllowExecuteWhile. There is no clear-and-resume path: canceled producer
// contexts are not recreated; operators must restart looperd.
func (r *Runtime) MarkDegraded(reason string) error {
	if r == nil || r.admission == nil {
		return ErrAdmissionStopping
	}
	// Snapshot cancel targets under r.mu, then hold admission.mu across the
	// degraded transition + cancel invoke so there is no window where admission
	// is already closed but cleanup/scheduler contexts are still live. A
	// point-in-time AllowClaim winner must either start remove while still
	// ready, or observe cancel after close — never start git worktree remove
	// after close with a live context. Webhook execCtx stays live so accepted
	// deliveries can still finish CreateOrGetActiveByDedupe.
	cancels := r.snapshotWorkProducerCancels()
	return r.admission.TransitionThen(AdmissionDegraded, reason, cancels.invokeForDegrade)
}

func (r *Runtime) WaitForShutdown() {
	<-r.shutdownCh
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (r *Runtime) Services() Services {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.services
}

// Config returns the current runtime configuration with Projects materialized
// from the authoritative Project Catalog.
func (r *Runtime) Config() config.Config {
	if r.projectCatalog != nil {
		return r.projectCatalog.Snapshot()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config
}

func (r *Runtime) StartedAt() (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.startedAt == nil {
		return time.Time{}, false
	}

	return *r.startedAt, true
}

func (r *Runtime) WebhookForwarder() WebhookForwarder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.webhookForwarder
}

func (r *Runtime) NetworkStatus() networkclient.Status {
	r.mu.RLock()
	manager := r.networkManager
	r.mu.RUnlock()
	if manager == nil {
		return networkclient.Status{}
	}
	return manager.Status()
}

func runtimeHomeDirOrEmpty() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return homeDir
}

func (r *Runtime) RecoverySummary() RecoverySummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.recovery
}

func (r *Runtime) WebhookStatus() WebhookStatus {
	r.mu.RLock()
	webhook := r.webhook
	forwarder := r.webhookForwarder
	r.mu.RUnlock()
	if webhook == nil {
		return WebhookStatus{}
	}
	status := webhook.Status()
	if forwarder == nil {
		return status
	}
	stats := forwarder.Stats()
	status.Queue.Pending = stats.Queued
	status.Queue.Capacity = stats.QueueCapacity
	status.Queue.ActiveWorkers = stats.InFlight
	status.Counters.DeliveriesReceived = int(stats.DeliveriesReceived)
	status.Counters.Coalesced = int(stats.QueueCoalesced)
	status.Counters.Dropped = int(stats.DeliveriesDeduped + stats.DeliveriesIgnored + stats.QueueRejected)
	status.Counters.Queued = int(stats.QueueEnqueued)
	status.Counters.Processed = int(stats.ExecutionsSucceeded)
	status.Counters.Failed = int(stats.ExecutionsFailed)
	status.RecentOutcomes = webhookRecentOutcomesFromStats(stats.RecentOutcomes)
	return status
}

func (r *Runtime) RecordWebhookDelivery(eventType, deliveryID string) {
	r.mu.RLock()
	webhook := r.webhook
	r.mu.RUnlock()
	if webhook != nil {
		webhook.RecordDelivery(eventType, deliveryID)
		r.TriggerSchedulerTick()
	}
}

func webhookRecentOutcomesFromStats(outcomes []webhookforward.Outcome) []WebhookRecentOutcome {
	if len(outcomes) == 0 {
		return []WebhookRecentOutcome{}
	}
	recent := make([]WebhookRecentOutcome, 0, len(outcomes))
	for _, outcome := range outcomes {
		recent = append(recent, WebhookRecentOutcome{
			At:      outcome.At,
			Outcome: outcome.Status,
			Message: formatWebhookOutcomeMessage(outcome),
		})
	}
	return recent
}

func formatWebhookOutcomeMessage(outcome webhookforward.Outcome) string {
	parts := make([]string, 0, 4)
	if repo := strings.TrimSpace(outcome.Repo); repo != "" {
		parts = append(parts, repo)
	}
	if objectType := strings.TrimSpace(outcome.ObjectType); objectType != "" && outcome.Number > 0 {
		parts = append(parts, fmt.Sprintf("%s #%d", objectType, outcome.Number))
	}
	if eventType := strings.TrimSpace(outcome.EventType); eventType != "" {
		parts = append(parts, eventType)
	}
	message := strings.Join(parts, " · ")
	if err := strings.TrimSpace(outcome.Error); err != "" {
		if message == "" {
			return err
		}
		return message + ": " + err
	}
	return message
}

func (r *Runtime) ReconcileWebhookForwarders() {
	r.mu.RLock()
	webhook := r.webhook
	repositories := r.services.Repositories
	r.mu.RUnlock()
	if webhook != nil {
		if err := webhook.Reconcile(repositories); err != nil && r.logger != nil {
			r.logger.Warn("webhook.reconcile_failed", map[string]any{"error": err.Error()})
		}
	}
}

func (r *Runtime) RefreshWebhookForwarders() error {
	r.ReconcileWebhookForwarders()
	return nil
}

func (r *Runtime) stopWebhookRuntime() {
	r.mu.RLock()
	webhook := r.webhook
	r.mu.RUnlock()
	if webhook != nil {
		webhook.Stop()
	}
}

func (r *Runtime) releaseDatabaseDaemonLock() {
	r.mu.RLock()
	lock := r.databaseDaemonLock
	r.mu.RUnlock()
	if lock != nil {
		_ = lock.Release()
		r.mu.Lock()
		if r.databaseDaemonLock == lock {
			r.databaseDaemonLock = nil
		}
		r.mu.Unlock()
	}
}

func (r *Runtime) start(ctx context.Context) error {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.mu.RUnlock()

	// Record which executable this process actually loaded before anything can
	// replace it. Later checks compare against this, not against a re-read.
	if r.daemonBinary != nil {
		r.daemonBinary.record()
	}

	backupDir := ""
	if r.config.Storage.BackupDir != nil {
		backupDir = *r.config.Storage.BackupDir
	}

	lock, err := storage.AcquireDatabaseLock(r.config.Storage.DBPath, storage.DatabaseLockExclusive)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("runtime.database.lock_failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	if r.logger != nil {
		r.logger.Info("runtime.database.lock_acquired", map[string]any{"mode": storage.DatabaseLockExclusive})
	}

	coordinator, err := r.openSQLiteCoordinator(ctx, r.config.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
		Now:       r.now,
	})
	if err != nil {
		if lock != nil {
			_ = lock.Release()
		}
		return err
	}

	started := false
	resourcesPublished := false
	defer func() {
		if !started && !resourcesPublished {
			r.stopProjectDiscovery()
			if lock != nil {
				_ = lock.Release()
			}
			_ = coordinator.Close()
		}
	}()

	// Validate schema compatibility on every boot, regardless of whether
	// migration application is enabled, so a downgraded or mixed-version binary
	// cannot initialize repositories and run against a newer schema it cannot
	// prove it understands. Migration application remains conditional below.
	if err := coordinator.MigrationRunner().ValidateCompatibility(ctx); err != nil {
		return err
	}

	if r.config.Package.AutoMigrateOnStartup {
		_, err = coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{
			RequireBackup: r.config.Package.RequireBackupBeforeMigrate,
		})
		if err != nil {
			return err
		}
		if err := lock.Downgrade(); err != nil {
			return fmt.Errorf("downgrade database migration lock to shared: %w", err)
		}
	}

	repositories := storage.NewRepositories(coordinator.DB())
	gitGateway := gitinfra.New(gitinfra.Options{GitPath: derefString(r.config.Tools.GitPath), Repos: repositories, Now: r.now})
	// Every project is GitHub, so the gateway is always needed. GHPath may be
	// empty here; startup validation is what reports a missing gh, not a nil
	// gateway.
	githubGateway := githubinfra.New(githubinfra.Options{GHPath: derefString(r.config.Tools.GHPath), Env: config.DaemonGitHubCredentialEnv(r.config), Now: r.now, DiscoveryCacheTTL: time.Duration(r.config.Scheduler.DiscoveryCacheTTLSeconds) * time.Second})
	projectService := &projects.Service{
		DB:             coordinator.DB(),
		Repos:          repositories,
		Logger:         r.logger,
		Config:         r.config,
		ConfigSource:   r.projectCatalog,
		ConfigBoundary: &r.configBoundary,
		Now:            r.now,
		DetectRepo: func(ctx context.Context, repoPath string) (projects.DetectedRepo, error) {
			return detectProjectRepo(ctx, gitGateway, r.projectCatalog.View(), repoPath)
		},
		GetRepositorySettings: func(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
			if githubGateway == nil {
				return githubinfra.RepositorySettings{}, fmt.Errorf("github gateway is not configured")
			}
			return githubGateway.GetRepositorySettings(ctx, input)
		},
		GetBranchProtection: func(ctx context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
			if githubGateway == nil {
				return githubinfra.BranchProtection{}, fmt.Errorf("github gateway is not configured")
			}
			return githubGateway.GetBranchProtection(ctx, input)
		},
		ListWorktrees: func(ctx context.Context, repoPath string) ([]projects.WorktreeListEntry, error) {
			worktrees, err := gitGateway.ListWorktrees(ctx, repoPath)
			if err != nil {
				return nil, err
			}
			items := make([]projects.WorktreeListEntry, 0, len(worktrees))
			for _, worktree := range worktrees {
				items = append(items, projects.WorktreeListEntry{Path: worktree.Path, Branch: worktree.Branch, HeadSHA: worktree.HeadSHA, Bare: worktree.Bare})
			}
			return items, nil
		},
		ListOpenPullRequests: func(ctx context.Context, input projects.ListOpenPullRequestsInput) ([]projects.PullRequestSummary, error) {
			if githubGateway == nil {
				return nil, fmt.Errorf("github gateway is not configured")
			}
			pullRequests, err := githubGateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Timeout: input.Timeout})
			if err != nil {
				return nil, err
			}
			items := make([]projects.PullRequestSummary, 0, len(pullRequests))
			for _, pullRequest := range pullRequests {
				items = append(items, projects.PullRequestSummary{Number: pullRequest.Number, State: pullRequest.State, IsDraft: pullRequest.IsDraft})
			}
			return items, nil
		},
		CapturePullRequestSnapshot: func(ctx context.Context, input projects.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
			if githubGateway == nil {
				return storage.PullRequestSnapshotRecord{}, fmt.Errorf("github gateway is not configured")
			}
			cfg := r.Config()
			return captureProjectPullRequestSnapshot(ctx, &cfg, githubGateway, input)
		},
		AsyncSnapshotQueueEnabled: func() bool {
			return asyncSnapshotQueueEnabled(r.customSchedulerTick, r.Config())
		},
		PublishProjects: func(projects []config.ProjectRefConfig) {
			r.publishProjectsSnapshot(projects)
		},
		AfterPublishProjects: r.afterProjectsPublished,
		DiscoveryContext:     r.projectDiscoveryContext,
		ScheduleDiscovery:    r.scheduleProjectDiscovery,
	}
	loopService := &loops.Service{DB: coordinator.DB(), Repos: repositories, Now: r.now}
	startedAt := r.now().UTC()
	if err := r.syncConfiguredProjects(ctx, projectService, r.config, startedAt); err != nil {
		return err
	}
	// Project import is already committed. Materialize that durable state even
	// when the startup request is canceled; CompleteStartup still observes ctx.
	if err := r.reloadProjectCatalog(context.Background(), repositories); err != nil {
		return err
	}
	r.config = r.projectCatalog.Snapshot()
	// Fail loud, not silent: without a credential the daemon's own gh children
	// fall back to anonymous requests that GitHub rate-limits per IP, which
	// surfaces later as unexplained transient forge failures.
	if readiness := ForgeCredentialReadinessFor(r.config); readiness.Degraded() && r.logger != nil {
		r.logger.Warn("daemon-internal GitHub calls have no credential", map[string]any{
			"reason":         readiness.Reason,
			"degradedReason": ForgeCredentialDegradedReason,
		})
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.startedAt = &startedAt
	r.services = Services{
		Coordinator:      coordinator,
		Repositories:     repositories,
		Projects:         projectService,
		Loops:            loopService,
		ActiveExecutions: r.activeExecutions,
	}
	schedulerDisabled := false
	if !r.customSchedulerTick || !r.customSchedulerClaim {
		handlers := buildCatalogSchedulerHandlers(r.projectCatalog, &r.configBoundary, r.configPath, r.logger, coordinator, repositories, gitGateway, githubGateway, r.activeExecutions, func() schedulerAsyncRunner {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return r.schedulerTasks
		}, r.TriggerSchedulerClaim, r.now, r.reconcileLiveStaleRunningRuns, r.AllowClaim, r.WithAllowClaim)
		if !r.customSchedulerTick {
			r.defaultSchedulerTick = handlers.tick
			if !r.customWebhookForwarder {
				r.webhookForwarder = handlers.webhook
			} else if handlers.webhook != nil {
				// An injected forwarder replaces the default; close the one
				// the handler bundle constructed so its workers do not leak.
				handlers.webhook.Close()
			}
			r.notificationGateways = handlers.notificationGateways
			schedulerDisabled = !defaultSchedulerAgentsConfigured(r.config)
		} else if handlers.webhook != nil {
			// The bundle was built only for its claim (or not at all for an
			// injected one); close the eagerly constructed webhook forwarder
			// it carries so its workers do not leak.
			handlers.webhook.Close()
		}
		if !r.customSchedulerClaim {
			r.defaultSchedulerClaim = handlers.claim
		}
	}
	r.githubGateway = githubGateway
	r.networkManager = networkclient.NewManager(filepath.Join(runtimeHomeDirOrEmpty(), ".looper", "network.json"), r.config, repositories, githubGateway)
	r.databaseDaemonLock = lock
	r.schedulerDisabled = schedulerDisabled
	r.mu.Unlock()
	resourcesPublished = true

	if r.deferRecovery {
		if r.networkManager != nil {
			_ = r.networkManager.Start(context.Background())
		}
		started = true
		return nil
	}

	if err := r.CompleteStartup(ctx); err != nil {
		return err
	}
	started = true
	return nil
}

func (r *Runtime) CompleteStartup(ctx context.Context) error {
	r.startupReadyOnce.Do(func() {
		r.mu.RLock()
		if r.stopped {
			r.mu.RUnlock()
			r.startupReadyErr = fmt.Errorf("runtime already stopped")
			return
		}
		startedAt := r.startedAt
		repositories := r.services.Repositories
		projectService := r.services.Projects
		githubGateway := r.githubGateway
		schedulerDisabled := r.schedulerDisabled
		r.mu.RUnlock()

		if startedAt == nil {
			r.startupReadyErr = fmt.Errorf("runtime has not been started")
			return
		}
		if repositories == nil {
			r.startupReadyErr = fmt.Errorf("runtime repositories are not configured")
			return
		}
		if err := r.validateCoordinatorDependencyGates(ctx, repositories, githubGateway); err != nil {
			r.startupReadyErr = err
			return
		}
		recoverySummary, err := r.runRecoveryPipeline(ctx, repositories, githubGateway, *startedAt)
		if err != nil {
			r.startupReadyErr = err
			return
		}
		if err := r.appendStartedEvent(context.Background(), *startedAt, recoverySummary); err != nil {
			r.startupReadyErr = err
			return
		}

		r.mu.Lock()
		r.recovery = recoverySummary
		r.ownershipAcquired = true
		r.mu.Unlock()
		if r.networkManager != nil {
			if err := r.networkManager.Start(ctx); err != nil {
				r.startupReadyErr = err
				return
			}
		}

		if r.webhook != nil {
			if err := r.webhook.Start(repositories); err != nil {
				r.startupReadyErr = err
				return
			}
		}
		if schedulerDisabled && r.logger != nil {
			r.logger.Warn("looperd scheduler waiting for agent configuration", map[string]any{"reason": "no coding role agent is configured"})
		}
		// The scheduler's handlers snapshot the current config for each operation.
		// Keep the loop alive even without an initial vendor so configuring one can
		// take effect without restarting looperd.
		r.startSchedulerLoop()
		// Always start the loop: daemon.worktreeCleanup.enabled is hot-editable,
		// and the loop itself gates each pass on the current value.
		r.startWorktreeCleanupLoop()
		r.startConfigReloadLoop()

		// Open admission only after recovery and producer loops are assembled.
		// HTTP mutations and scheduler claims are projections of this state.
		if err := r.admission.MarkReady("complete startup"); err != nil {
			r.startupReadyErr = err
			return
		}
		// Persisted discovery is runtime-owned work. Launch it only after
		// dependency validation, recovery, ownership, producer assembly, and
		// admission readiness have all succeeded.
		if projectService != nil {
			resume := r.resumeProjectDiscoveries
			if resume == nil {
				resume = func(ctx context.Context, service *projects.Service) error {
					return service.ResumeIncompleteDiscoveries(ctx)
				}
			}
			if err := resume(ctx, projectService); err != nil {
				_ = r.MarkDegraded("resume incomplete project discovery: " + err.Error())
				r.startupReadyErr = err
				return
			}
		}
		// Deferred reviewer recovery requeues failed loops without the scheduler
		// claim path; start it only after admission is ready, and only while
		// admission remains ready (startDeferredReviewerRecovery rechecks under
		// the shutdown race where BeginShutdown may have already missed a nil
		// recoveryCancel between MarkReady and registration).
		r.startDeferredReviewerRecovery(githubGateway)
		// startSchedulerLoop already fired an immediate full tick while admission
		// was still starting (gate no-op). Wake full + claim pumps now that
		// admission is ready so discovery/HITL do not wait a full poll interval.
		r.TriggerSchedulerTick()

		if r.logger != nil {
			catalog := r.Config()
			r.logger.Info("looperd runtime assembled", map[string]any{
				"dbPath":                 r.config.Storage.DBPath,
				"projectCount":           len(catalog.Projects),
				"autoMigrate":            r.config.Package.AutoMigrateOnStartup,
				"backupRequired":         r.config.Package.RequireBackupBeforeMigrate,
				"recoverySummary":        recoverySummary,
				"schedulerDefaultActive": !r.customSchedulerTick && !schedulerDisabled,
				"admissionState":         string(r.admission.State()),
			})
		}
	})

	if r.startupReadyErr != nil {
		// CompleteStartup owns every producer it starts. Roll back through the
		// normal shutdown path so callers never receive an error while scheduler,
		// cleanup, reload, webhook, or network resources remain live.
		r.Stop("runtime startup failed: " + r.startupReadyErr.Error())
	}
	return r.startupReadyErr
}

func (r *Runtime) validateCoordinatorDependencyGates(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway) error {
	if repositories == nil || repositories.Projects == nil || githubGateway == nil {
		return nil
	}
	projectsList, err := repositories.Projects.List(ctx)
	if err != nil {
		return err
	}
	catalog := r.Config()
	for _, project := range projectsList {
		if project.Archived {
			continue
		}
		roleCfg := config.ProjectRoleConfigs(catalog, project.ID).Coordinator
		if !roleCfg.Enabled || !roleCfg.Dependencies.Enabled {
			continue
		}
		repo := strings.TrimSpace(runtimeProjectRepo(project.MetadataJSON))
		if repo == "" {
			return fmt.Errorf("coordinator dependency gate enabled but repository metadata unavailable for project %s", project.ID)
		}
		issueNumber, err := r.firstDependencyProbeIssue(ctx, githubGateway, repo, project.RepoPath)
		if err != nil {
			return err
		}
		if issueNumber == 0 {
			continue
		}
		if err := r.probeDependencyAPI(ctx, githubGateway, repo, project.RepoPath, issueNumber, roleCfg.Dependencies); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) firstDependencyProbeIssue(ctx context.Context, githubGateway *githubinfra.Gateway, repo, cwd string) (int64, error) {
	return githubGateway.FindAnyIssueNumber(ctx, repo, cwd)
}

func (r *Runtime) probeDependencyAPI(ctx context.Context, githubGateway *githubinfra.Gateway, repo, cwd string, issueNumber int64, depsCfg config.CoordinatorDependenciesConfig) error {
	var lastErr error
	for attempt := 0; attempt < runtimeMaxDependencyAttempts(depsCfg.APIRetryAttempts); attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, runtimeDependencyTimeout(depsCfg.APITimeoutSeconds))
		_, err := githubGateway.ListIssueBlockedBy(callCtx, githubinfra.ListIssueBlockedByInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if githubinfra.IsNotFoundError(err) {
			return fmt.Errorf("coordinator dependency gate enabled but dependencies API unavailable on %s; disable roles.coordinator.dependencies.enabled or upgrade GHES", repo)
		}
		if !runtimeShouldRetryDependencyError(err) {
			return err
		}
	}
	return lastErr
}

func runtimeProjectRepo(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	value, _ := metadata["repo"].(string)
	return strings.TrimSpace(value)
}

// defaultSchedulerAgentsConfigured reports whether the default scheduler has at
// least one coding-role agent vendor (global or role/profile). This is the same
// gate as buildDefaultSchedulerHandlersWithOptions.
func defaultSchedulerAgentsConfigured(cfg config.Config) bool {
	return config.AnyCodingRoleAgentConfigured(cfg)
}

// asyncSnapshotQueueEnabled reports whether project import can enqueue async PR
// snapshots for the scheduler to process. Role-only agent installs count as
// scheduler-enabled so import does not fall back to full synchronous capture.
func asyncSnapshotQueueEnabled(customSchedulerTick bool, cfg config.Config) bool {
	return customSchedulerTick || defaultSchedulerAgentsConfigured(cfg)
}

func runtimeProjectBinding(cfg config.Config, projectID string) (config.ProjectRefConfig, bool) {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return project, true
		}
	}
	return config.ProjectRefConfig{}, false
}

func detectProjectRepo(ctx context.Context, gitGateway *gitinfra.Gateway, view projects.OperationView, repoPath string) (projects.DetectedRepo, error) {
	if gitGateway == nil {
		return projects.DetectedRepo{}, fmt.Errorf("git gateway is not configured")
	}
	remote, err := gitGateway.DetectOriginRemote(ctx, repoPath)
	if err != nil {
		return projects.DetectedRepo{}, err
	}
	if strings.TrimSpace(remote.Repo) == "" {
		return projects.DetectedRepo{}, nil
	}
	if remote.Host == "github.com" || strings.HasSuffix(remote.Host, ".github.com") {
		return projects.DetectedRepo{Repo: remote.Repo}, nil
	}
	// Preserve the non-GitHub origin classification so AddProject can reject a
	// provider-bound checkout at its authority boundary. Returning a detection
	// error here would only produce a best-effort warning and allow an inert
	// API-owned project to be registered.
	return projects.DetectedRepo{Repo: remote.Repo, Provider: strings.TrimSpace(remote.Host)}, nil
}

func (r *Runtime) reloadProjectCatalog(ctx context.Context, repos *storage.Repositories) error {
	if r.projectCatalog == nil || repos == nil || repos.Projects == nil {
		return fmt.Errorf("project catalog dependencies are not configured")
	}
	records, err := repos.Projects.List(ctx)
	if err != nil {
		return fmt.Errorf("list projects for runtime catalog: %w", err)
	}
	global := r.projectCatalog.Snapshot()
	materialized, err := projects.MaterializeCatalog(global, records)
	if err != nil {
		return fmt.Errorf("materialize runtime project catalog: %w", err)
	}
	materialized, err = projects.ValidateStoredCatalogValidationPolicies(global, records, materialized)
	if err != nil {
		return fmt.Errorf("validate runtime project catalog: %w", err)
	}
	r.publishProjects(materialized)
	return nil
}

func (r *Runtime) publishProjects(materialized []config.ProjectRefConfig) {
	r.publishProjectsSnapshot(materialized)
	r.afterProjectsPublished()
}

// publishProjectsSnapshot is the atomic in-memory half of project
// publication. Project mutations call it while holding configBoundary so a
// concurrent config reload cannot validate or publish a mixed catalog.
func (r *Runtime) publishProjectsSnapshot(materialized []config.ProjectRefConfig) {
	if r == nil || r.projectCatalog == nil {
		return
	}
	r.projectCatalog.Publish(materialized)
	r.publishCatalogConsumers(r.projectCatalog.Snapshot())
}

// publishCatalogConsumers keeps long-lived consumers on the same coherent
// globals-plus-projects snapshot used for new scheduler claims.
func (r *Runtime) publishCatalogConsumers(next config.Config) {
	r.mu.RLock()
	webhook := r.webhook
	networkManager := r.networkManager
	r.mu.RUnlock()
	if webhook != nil {
		webhook.updateConfig(next)
	}
	if networkManager != nil {
		networkManager.UpdateConfig(next)
	}
}

// afterProjectsPublished deliberately runs outside configBoundary. Webhook
// reconciliation can invoke gh and other external operations; those must not
// block config publication or new claim snapshots.
func (r *Runtime) afterProjectsPublished() {
	if r == nil {
		return
	}
	r.mu.RLock()
	started := r.startedAt != nil
	r.mu.RUnlock()
	if started {
		r.TriggerSchedulerTick()
		r.TriggerSchedulerClaim()
		r.ReconcileWebhookForwarders()
	}
}

func runtimeDependencyTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func runtimeMaxDependencyAttempts(attempts int) int {
	if attempts <= 0 {
		return 1
	}
	return attempts
}

func runtimeShouldRetryDependencyError(err error) bool {
	if githubinfra.IsTransientError(err) {
		return true
	}
	message := strings.ToLower(githubinfra.ErrorMessage(err))
	return strings.Contains(message, "timed out") || strings.Contains(message, "context deadline exceeded")
}

func (r *Runtime) startSchedulerLoop() {
	pollInterval := schedulerFullPollInterval(r.config)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	wakeCh := make(chan struct{}, 1)
	claimWakeCh := make(chan struct{}, 1)
	schedulerCtx, schedulerCancel := context.WithCancel(context.Background())
	taskTracker := &schedulerTaskTracker{}

	r.mu.Lock()
	r.schedulerStop = stopCh
	r.schedulerDone = doneCh
	r.schedulerWake = wakeCh
	r.schedulerClaimWake = claimWakeCh
	r.schedulerCancel = schedulerCancel
	r.schedulerTasks = taskTracker
	r.mu.Unlock()

	if r.defaultSchedulerClaim != nil {
		taskTracker.Go(func() {
			r.runSchedulerClaimLoop(schedulerCtx, stopCh, claimWakeCh)
		})
	}

	go func() {
		defer close(doneCh)
		defer taskTracker.Wait()

		r.executeSchedulerTick(schedulerCtx)
		if pollInterval <= 0 {
			for {
				select {
				case <-stopCh:
					return
				case <-wakeCh:
					r.executeSchedulerTick(schedulerCtx)
				}
			}
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-wakeCh:
				r.executeSchedulerTick(schedulerCtx)
			case <-ticker.C:
				r.executeSchedulerTick(schedulerCtx)
			}
		}
	}()
}

func schedulerFullPollInterval(cfg config.Config) time.Duration {
	if cfg.Webhook.Enabled {
		return time.Duration(cfg.Webhook.FallbackPollIntervalSeconds) * time.Second
	}
	return time.Duration(cfg.Scheduler.PollIntervalSeconds) * time.Second
}

func (r *Runtime) stopSchedulerLoop() {
	r.mu.Lock()
	stopCh := r.schedulerStop
	doneCh := r.schedulerDone
	cancel := r.schedulerCancel
	taskTracker := r.schedulerTasks
	r.schedulerStop = nil
	r.schedulerDone = nil
	r.schedulerWake = nil
	r.schedulerClaimWake = nil
	r.schedulerCancel = nil
	r.mu.Unlock()

	if stopCh == nil || doneCh == nil {
		if cancel != nil {
			cancel()
		}
		return
	}

	if cancel != nil {
		cancel()
	}
	close(stopCh)
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-doneCh:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for scheduler loop", map[string]any{"timeoutMs": r.shutdownTimeout.Milliseconds()})
		}
	}

	r.mu.Lock()
	if r.schedulerTasks == taskTracker {
		r.schedulerTasks = nil
	}
	r.mu.Unlock()
}

func (r *Runtime) startDeferredReviewerRecovery(githubGateway *githubinfra.Gateway) {
	if githubGateway == nil {
		return
	}
	services := r.Services()
	if services.Repositories == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.mu.Lock()
	// Refuse to arm recovery once shutdown has begun or admission is no longer
	// ready. BeginShutdown may already have observed recoveryCancel==nil; starting
	// a live goroutine afterward would let requeue persist work while stopping
	// (and stopDeferredReviewerRecovery may already have passed).
	if r.stopped || r.admission == nil || r.admission.State() != AdmissionReady {
		r.mu.Unlock()
		cancel()
		return
	}
	r.recoveryCancel = cancel
	r.recoveryDone = done
	r.mu.Unlock()

	// Publish-then-recheck: if BeginShutdown raced between the ready check and
	// assigning recoveryCancel, it missed cancel. If admission is no longer
	// ready, cancel immediately so the goroutine is born canceled.
	if err := r.AllowClaim(); err != nil {
		cancel()
	}

	go func(repositories *storage.Repositories) {
		defer close(done)
		if err := ctx.Err(); err != nil {
			return
		}
		// Deferred recovery requeues without AllowClaim on the scheduler path;
		// refuse when admission already closed after MarkReady/shutdown race.
		if err := r.AllowClaim(); err != nil {
			return
		}
		requeued, err := r.runDeferredReviewerRecovery(ctx, repositories, githubGateway, r.now().UTC())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if r.logger != nil {
				r.logger.Warn("looperd deferred reviewer recovery failed", map[string]any{"error": err.Error()})
			}
			return
		}
		if requeued > 0 && r.logger != nil {
			r.logger.Info("looperd deferred reviewer recovery completed", map[string]any{"loopsRequeued": requeued})
		}
	}(services.Repositories)
}

func (r *Runtime) stopDeferredReviewerRecovery() {
	r.mu.Lock()
	cancel := r.recoveryCancel
	done := r.recoveryDone
	r.recoveryCancel = nil
	r.recoveryDone = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for deferred reviewer recovery", map[string]any{"timeoutMs": r.shutdownTimeout.Milliseconds()})
		}
	}
}

// admissionRefusesDeferredRequeue reports whether deferred recovery must not
// persist queue/loop requeues. Stopping and degraded are hard refusals;
// starting is allowed only for direct unit-test helpers (production arms
// deferred recovery only after MarkReady via startDeferredReviewerRecovery).
func (r *Runtime) admissionRefusesDeferredRequeue() bool {
	if r == nil || r.admission == nil {
		return true
	}
	switch r.admission.State() {
	case AdmissionStopping, AdmissionDegraded:
		return true
	default:
		return false
	}
}

// WaitForDeferredReviewerRecovery blocks until the post-ready deferred
// reviewer recovery goroutine exits, or until ctx is canceled. It returns
// immediately when deferred recovery was never started (for example when no
// GitHub gateway is configured). Test fixtures call this after CompleteStartup
// so later inserts of terminal reviewer metadata cannot race
// normalizeTerminalReviewerLoopForRecovery.
func (r *Runtime) WaitForDeferredReviewerRecovery(ctx context.Context) error {
	r.mu.RLock()
	done := r.recoveryDone
	r.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) TriggerSchedulerTick() {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return
	}
	wakeCh := r.schedulerWake
	claimWakeCh := r.schedulerClaimWake
	r.mu.RUnlock()

	if wakeCh != nil {
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	}
	if claimWakeCh != nil {
		select {
		case claimWakeCh <- struct{}{}:
		default:
		}
	}
}

func (r *Runtime) TriggerSchedulerClaim() {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return
	}
	claimWakeCh := r.schedulerClaimWake
	r.mu.RUnlock()
	if claimWakeCh != nil {
		select {
		case claimWakeCh <- struct{}{}:
		default:
		}
	}
}

func (r *Runtime) executeSchedulerTick(ctx context.Context) {
	r.mu.RLock()
	services := r.services
	tick := r.runSchedulerTick
	r.mu.RUnlock()
	// The repositories guard protects the default tick, which cannot claim
	// without storage. An injected tick declares its own dependencies and
	// runs even before deferred recovery has populated services.
	if services.Repositories == nil && !r.customSchedulerTick {
		return
	}
	if tick == nil {
		return
	}

	// One stat per tick unless the file moved. Keeps a swap loud in the log
	// without waiting for an operator to run `looper status`.
	if r.daemonBinary != nil {
		r.daemonBinary.Status()
	}

	if err := tick(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler tick failed", map[string]any{"error": err.Error()})
	}
}

func (r *Runtime) executeDefaultSchedulerTick(ctx context.Context, services Services) error {
	r.mu.RLock()
	tick := r.defaultSchedulerTick
	r.mu.RUnlock()
	if tick == nil {
		return fmt.Errorf("default scheduler tick is not configured")
	}
	return tick(ctx, services)
}

func (r *Runtime) executeSchedulerClaimPass(ctx context.Context) {
	// Claim is a projection of admission — refuse before any durable claim.
	if err := r.AllowClaim(); err != nil {
		return
	}
	r.mu.RLock()
	services := r.services
	claim := r.defaultSchedulerClaim
	r.mu.RUnlock()
	if services.Repositories == nil || claim == nil {
		return
	}
	if err := claim(ctx, services); err != nil && r.logger != nil {
		r.logger.Warn("looperd scheduler claim pump failed", map[string]any{"error": err.Error()})
	}
}

func (r *Runtime) runSchedulerClaimLoop(ctx context.Context, stopCh <-chan struct{}, wakeCh <-chan struct{}) {
	const claimPumpInterval = time.Second
	r.executeSchedulerClaimPass(ctx)
	ticker := time.NewTicker(claimPumpInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-wakeCh:
			r.executeSchedulerClaimPass(ctx)
		case <-ticker.C:
			r.executeSchedulerClaimPass(ctx)
		}
	}
}

func (r *Runtime) runRecoveryPipeline(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, now time.Time) (RecoverySummary, error) {
	nowISO := formatJavaScriptISOString(now)
	eventsWritten := int64(0)
	summary := createEmptyRecoverySummary()
	summary.StartedAt = nowISO
	summary.OrphanAgentCleanup.Attempted = true
	uncertainAgentRunIDs := make(map[string]struct{})
	activeAgentRunIDs := make(map[string]struct{})
	quarantinedLoopIDs := make(map[string]struct{})
	if repositories.AgentExecutions != nil {
		activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
		if err != nil {
			return RecoverySummary{}, err
		}
		for _, execution := range activeExecutions {
			assessment, observedEvents, observeErr := r.observeExecutionLiveness(ctx, repositories, execution, now, "orphan_cleanup")
			if observeErr != nil {
				return RecoverySummary{}, observeErr
			}
			eventsWritten += observedEvents
			classification := assessment.Classification
			switch assessment.Disposition {
			case executionLivenessActive:
				summary.OrphanAgentCleanup.ObservedLiveCount += 1
				if execution.RunID != nil && strings.TrimSpace(*execution.RunID) != "" {
					activeAgentRunIDs[*execution.RunID] = struct{}{}
				}
			default:
				summary.OrphanAgentCleanup.UncertainCount += 1
			}

			switch assessment.Disposition {
			case executionLivenessActive, executionLivenessNeedsConfirmation:
				reason := "startup recovery: matching live process from previous daemon"
				if assessment.Disposition == executionLivenessNeedsConfirmation {
					reason = "needs confirmation: startup liveness evidence is not authoritative (" + classification.Reason + ")"
				}
				if execution.RunID != nil && strings.TrimSpace(*execution.RunID) != "" {
					if assessment.Disposition == executionLivenessNeedsConfirmation {
						uncertainAgentRunIDs[*execution.RunID] = struct{}{}
					}
				}
				quarantined, wrote, err := r.quarantineRecoveryEvidence(ctx, repositories, execution, nowISO, reason)
				if err != nil {
					return RecoverySummary{}, err
				}
				if quarantined {
					summary.OrphanAgentCleanup.QuarantinedCount += 1
					if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" {
						quarantinedLoopIDs[*execution.LoopID] = struct{}{}
					}
				}
				if wrote {
					eventsWritten += 1
				}
			}
		}
		if summary.OrphanAgentCleanup.QuarantinedCount > 0 {
			summary.OrphanAgentCleanup.Warning = "active executions were quarantined without process kill; containment is not confirmed"
		}
	}

	expiredLocks, err := repositories.Locks.ListExpired(ctx, nowISO)
	if err != nil {
		return RecoverySummary{}, err
	}
	for _, lock := range expiredLocks {
		if err := repositories.Locks.Release(ctx, lock.Key); err != nil {
			return RecoverySummary{}, err
		}
		summary.ExpiredLocksReleased += 1
		if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.lock_released",
			EntityType: stringPtr("lock"),
			EntityID:   stringPtr(lock.Key),
			CreatedAt:  nowISO,
		}, map[string]any{
			"owner":       lock.Owner,
			"expiredAt":   lock.ExpiresAt,
			"recoveredAt": nowISO,
		}); err != nil {
			return RecoverySummary{}, err
		}
		eventsWritten += 1
	}

	staleSummary, err := r.reconcileStaleRunningRunsWithMode(ctx, repositories, now, staleRunReconcileModeStartup)
	if err != nil {
		return RecoverySummary{}, err
	}
	summary.InterruptedRunsMarked += staleSummary.InterruptedRuns
	summary.LoopsRequeued += staleSummary.LoopsRequeued
	eventsWritten += staleSummary.EventsWritten
	// The stale-run pass can park work the execution sweep above did not, and
	// one recovery pass is one operator event regardless of which step parked it.
	for _, loopID := range staleSummary.QuarantinedLoopIDs {
		quarantinedLoopIDs[loopID] = struct{}{}
	}
	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	loopsByID := make(map[string]storage.LoopRecord, len(loops))
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
	}
	requeuedLoopIDs := make(map[string]struct{})
	terminalNormalizedLoopIDs := make(map[string]struct{})
	if staleSummary.LoopsRequeued > 0 {
		for _, loopID := range staleSummary.LoopIDs {
			loop, ok := loopsByID[loopID]
			if ok && loop.Status == "queued" {
				requeuedLoopIDs[loopID] = struct{}{}
			}
		}
	}
	for _, loop := range loops {
		if _, wasRequeued := requeuedLoopIDs[loop.ID]; wasRequeued {
			continue
		}
		// Quarantined work must not requeue or auto-recover as if cleaned.
		if _, quarantined := quarantinedLoopIDs[loop.ID]; quarantined {
			continue
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		if handled, wroteEvent, err := normalizeTerminalReviewerLoopForRecovery(ctx, repositories, loop, latestRun, nowISO); err != nil {
			return RecoverySummary{}, err
		} else if handled {
			if wroteEvent {
				eventsWritten += 1
			}
			terminalNormalizedLoopIDs[loop.ID] = struct{}{}
			continue
		}
		policy := r.reviewerRecoveryPolicyForProject(loop.ProjectID)
		if reviewerRecoveryNeedsFreshLogin(loop, latestRun, policy) {
			continue
		}
		if shouldAutoRecoverFailedReviewerLoop(loop, latestRun, latestQueue, policy) {
			// Share discard/retry exclusion with deferred recovery and discovery.
			didRequeue, recoveredQueueItems, err := requeueFailedReviewerWithSharedGuards(ctx, repositories, loop, latestQueue, nowISO, policy, latestRun)
			if err != nil {
				return RecoverySummary{}, err
			}
			if !didRequeue {
				continue
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			summary.LoopsRequeued += 1
			if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.reviewer_auto_recovered",
				LoopID:     stringPtr(loop.ID),
				EntityType: stringPtr("loop"),
				EntityID:   stringPtr(loop.ID),
				CreatedAt:  nowISO,
			}, map[string]any{
				"previousStatus":      loop.Status,
				"nextRunAt":           nowISO,
				"recoveredQueueItems": recoveredQueueItems,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
			terminalNormalizedLoopIDs[loop.ID] = struct{}{}
			continue
		}

		_, latestRunHasActiveAgent := activeAgentRunIDs[derefRunID(latestRun)]
		_, latestRunHasUncertainAgent := uncertainAgentRunIDs[derefRunID(latestRun)]
		if latestQueueIsManualIntervention(latestQueue) && (loop.Status == "running" || loop.Status == "queued") {
			if latestQueue.Status == "manual_intervention" {
				normalizedLoop := loop
				normalizedLoop.Status = normalizeStaleQueuedLoopStatus(loop, derefRunOrZero(latestRun))
				normalizedLoop.NextRunAt = nil
				if latestRun != nil {
					normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				normalizedLoop.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
					return RecoverySummary{}, err
				}
				terminalNormalizedLoopIDs[loop.ID] = struct{}{}
				latestRunStatus := ""
				if latestRun != nil {
					latestRunStatus = latestRun.Status
				}
				if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
					ID:         newRuntimeEventID(),
					EventType:  "looperd.recovery.loop_queue_normalized",
					LoopID:     stringPtr(loop.ID),
					EntityType: stringPtr("loop"),
					EntityID:   stringPtr(loop.ID),
					CreatedAt:  nowISO,
				}, map[string]any{
					"previousStatus":  loop.Status,
					"recoveredStatus": normalizedLoop.Status,
					"latestRunStatus": latestRunStatus,
				}); err != nil {
					return RecoverySummary{}, err
				}
				eventsWritten += 1
				continue
			}

			requeuedLoop := loop
			requeuedLoop.Status = "queued"
			requeuedLoop.NextRunAt = stringPtr(nowISO)
			if latestRun != nil {
				requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
			}
			requeuedLoop.UpdatedAt = nowISO
			if _, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO); err != nil {
				return RecoverySummary{}, err
			}
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.loop_manual_intervention_requeued",
				LoopID:     stringPtr(loop.ID),
				EntityType: stringPtr("loop"),
				EntityID:   stringPtr(loop.ID),
				CreatedAt:  nowISO,
			}, map[string]any{
				"previousStatus":  loop.Status,
				"recoveredStatus": requeuedLoop.Status,
				"queueItemId":     latestQueue.ID,
				"lastErrorKind":   derefString(latestQueue.LastErrorKind),
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
			continue
		}
		if shouldRequeueLoop(loop, latestRun, latestRunHasActiveAgent || latestRunHasUncertainAgent) {
			requeuedLoop := loop
			requeuedLoop.Status = "queued"
			requeuedLoop.NextRunAt = stringPtr(nowISO)
			if latestRun != nil {
				requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
			}
			requeuedLoop.UpdatedAt = nowISO
			if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
				return RecoverySummary{}, err
			}
			requeuedLoopIDs[loop.ID] = struct{}{}
			recoveredQueueItems, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO)
			if err != nil {
				return RecoverySummary{}, err
			}
			if recoveredQueueItems == 0 {
				if err := r.ensureRecoveryQueueItem(ctx, repositories, requeuedLoop, nowISO); err != nil {
					return RecoverySummary{}, err
				}
			}
			summary.LoopsRequeued += 1
			if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
				ID:         newRuntimeEventID(),
				EventType:  "looperd.recovery.loop_requeued",
				LoopID:     stringPtr(loop.ID),
				EntityType: stringPtr("loop"),
				EntityID:   stringPtr(loop.ID),
				CreatedAt:  nowISO,
			}, map[string]any{
				"previousStatus":      loop.Status,
				"nextRunAt":           nowISO,
				"recoveredQueueItems": recoveredQueueItems,
			}); err != nil {
				return RecoverySummary{}, err
			}
			eventsWritten += 1
		}
	}

	queueItems, err := repositories.Queue.List(ctx)
	if err != nil {
		return RecoverySummary{}, err
	}
	queuedLoopIDs := make(map[string]struct{})
	for _, item := range queueItems {
		if item.LoopID == nil {
			continue
		}
		if item.Status == "queued" || item.Status == "running" {
			queuedLoopIDs[*item.LoopID] = struct{}{}
		}
	}

	for _, loop := range loops {
		if _, normalized := terminalNormalizedLoopIDs[loop.ID]; normalized {
			continue
		}
		if _, exists := queuedLoopIDs[loop.ID]; exists {
			continue
		}

		if loop.Status != "queued" {
			continue
		}

		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return RecoverySummary{}, err
		}
		if latestRun == nil {
			continue
		}

		normalizedLoop := loop
		normalizedLoop.Status = normalizeStaleQueuedLoopStatus(loop, *latestRun)
		normalizedLoop.NextRunAt = nil
		normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
		normalizedLoop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
			return RecoverySummary{}, err
		}
		if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.loop_queue_normalized",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			CreatedAt:  nowISO,
		}, map[string]any{
			"previousStatus":  loop.Status,
			"recoveredStatus": normalizedLoop.Status,
			"latestRunStatus": latestRun.Status,
		}); err != nil {
			return RecoverySummary{}, err
		}
		eventsWritten += 1
	}

	summary.CompletedAt = nowISO
	if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.recovery.completed",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd-recovery"),
		CreatedAt:  nowISO,
	}, map[string]any{
		"expiredLocksReleased":  summary.ExpiredLocksReleased,
		"interruptedRunsMarked": summary.InterruptedRunsMarked,
		"loopsRequeued":         summary.LoopsRequeued,
		"orphanAgentCleanup":    summary.OrphanAgentCleanup,
	}); err != nil {
		return RecoverySummary{}, err
	}
	eventsWritten += 1
	summary.EventsWritten = eventsWritten

	// One operator event per recovery pass, after the pass has already
	// succeeded: the notification reports parked work, it does not gate it.
	r.notifyRecoveryQuarantine(ctx, repositories, quarantinedRecoveryRoster(quarantinedLoopIDs, loopsByID, nowISO), nowISO)

	return summary, nil
}

// quarantinedRecoveryRoster names the loops this recovery pass parked, in loop
// seq order. Loops whose record vanished mid-pass are reported by id alone.
func quarantinedRecoveryRoster(quarantinedLoopIDs map[string]struct{}, loopsByID map[string]storage.LoopRecord, nowISO string) []OutstandingQuarantinedLoop {
	if len(quarantinedLoopIDs) == 0 {
		return nil
	}
	roster := make([]OutstandingQuarantinedLoop, 0, len(quarantinedLoopIDs))
	for loopID := range quarantinedLoopIDs {
		entry := OutstandingQuarantinedLoop{LoopID: loopID, QuarantinedAt: nowISO}
		if loop, ok := loopsByID[loopID]; ok {
			entry.Seq = loop.Seq
			entry.Type = loop.Type
			entry.Target = loopForgeTarget(loop)
			entry.Status = loop.Status
		}
		roster = append(roster, entry)
	}
	sort.Slice(roster, func(i, j int) bool {
		if roster[i].Seq != roster[j].Seq {
			return roster[i].Seq < roster[j].Seq
		}
		return roster[i].LoopID < roster[j].LoopID
	})
	return roster
}

// notifyRecoveryQuarantine emits exactly one warn-level notification naming
// every loop this recovery pass parked. Delivery failures are logged: recovery
// already committed, and a report of it must not undo it.
func (r *Runtime) notifyRecoveryQuarantine(ctx context.Context, repositories *storage.Repositories, roster []OutstandingQuarantinedLoop, nowISO string) {
	if len(roster) == 0 || repositories == nil {
		return
	}
	r.mu.RLock()
	gateways := r.notificationGateways
	r.mu.RUnlock()
	if gateways == nil {
		// Recovery can run before/without the scheduler handlers (deferred
		// recovery, tests). Notification transport continuity is per-pass here.
		gateways = newSchedulerNotificationGatewayFactory()
	}
	cfg := r.Config()
	gateway := gateways.New(notify.Options{
		Config:        cfg.Notifications,
		OsascriptPath: derefString(cfg.Tools.OsascriptPath),
		LogFilePath:   filepath.Join(cfg.Daemon.LogDir, "looperd.log"),
		Repositories:  repositories,
		Now:           r.now,
	})
	records := gateway.Notify(ctx, notify.SystemNotificationPayload{
		Level:      "warn",
		Title:      "Looper Recovery Quarantined Work",
		Subtitle:   fmt.Sprintf("%s parked by startup recovery", pluralizeLoops(len(roster))),
		Body:       recoveryQuarantineNotificationBody(roster),
		EntityType: "recovery",
		EntityID:   "looperd-recovery",
		DedupeKey:  "runtime.recovery.quarantined:" + nowISO,
	})
	if len(records) == 0 && r.logger != nil {
		r.logger.Warn("looperd recovery quarantine notification was not delivered", map[string]any{
			"quarantinedLoops": len(roster),
		})
	}
}

func recoveryQuarantineNotificationBody(roster []OutstandingQuarantinedLoop) string {
	entries := make([]string, 0, len(roster))
	for _, loop := range roster {
		entries = append(entries, strings.Join(compactStrings([]string{
			fmt.Sprintf("loop %d", loop.Seq),
			loop.Type,
			loop.Target,
			"-> looper retry " + fmt.Sprintf("%d", loop.Seq),
		}), " "))
	}
	return fmt.Sprintf("Startup recovery quarantined %s without confirmed containment; they will not resume on their own. %s",
		pluralizeLoops(len(roster)), strings.Join(entries, "; "))
}

func pluralizeLoops(count int) string {
	if count == 1 {
		return "1 loop"
	}
	return fmt.Sprintf("%d loops", count)
}

func compactStrings(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func (r *Runtime) runDeferredReviewerRecovery(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, now time.Time) (int64, error) {
	if repositories == nil || githubGateway == nil {
		return 0, nil
	}
	// Refuse up front when admission already closed for shutdown/degraded.
	// (startDeferredReviewerRecovery only arms after ready; unit tests may call
	// this helper while still starting.)
	if r.admissionRefusesDeferredRequeue() {
		return 0, nil
	}
	nowISO := formatJavaScriptISOString(now)
	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return 0, err
	}
	reviewerLoginByProjectID := make(map[string]string)
	requeued := int64(0)
	for _, loop := range loops {
		if err := ctx.Err(); err != nil {
			return requeued, err
		}
		// Recheck before each durable requeue so BeginShutdown cannot leave a
		// still-running recovery path persisting queued loop/queue state.
		if r.admissionRefusesDeferredRequeue() {
			return requeued, nil
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		if handled, _, err := normalizeTerminalReviewerLoopForRecovery(ctx, repositories, loop, latestRun, nowISO); err != nil {
			return requeued, err
		} else if handled {
			continue
		}
		policy := r.reviewerRecoveryPolicyForProject(loop.ProjectID)
		if !reviewerRecoveryNeedsFreshLogin(loop, latestRun, policy) {
			continue
		}
		cachedLogin, cached := reviewerLoginByProjectID[loop.ProjectID]
		if !cached {
			login, ok := r.currentReviewerLoginForRecovery(ctx, repositories, githubGateway, loop, latestRun, policy)
			if !ok {
				continue
			}
			cachedLogin = login
			reviewerLoginByProjectID[loop.ProjectID] = cachedLogin
		}
		policy.currentLogin = cachedLogin
		if !shouldAutoRecoverFailedReviewerLoop(loop, latestRun, latestQueue, policy) {
			continue
		}
		currentLoop, err := repositories.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return requeued, err
		}
		if currentLoop == nil || !shouldAutoRecoverFailedReviewerLoop(*currentLoop, latestRun, latestQueue, policy) {
			continue
		}
		// Final admission gate immediately before lock+persist: matches the
		// shutdown race where cancel registration was missed after MarkReady.
		if r.admissionRefusesDeferredRequeue() {
			return requeued, nil
		}
		// Share discard/retry exclusion: recovery requeue of a PR reviewer must
		// not interleave with operator discard of a sibling loop on the same
		// managed worktree between preflight and git reset.
		didRequeue, recoveredQueueItems, err := requeueFailedReviewerWithSharedGuards(ctx, repositories, *currentLoop, latestQueue, nowISO, policy, latestRun)
		if err != nil {
			return requeued, err
		}
		if !didRequeue {
			continue
		}
		requeued += 1
		if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.reviewer_auto_recovered",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			CreatedAt:  nowISO,
		}, map[string]any{
			"previousStatus":      loop.Status,
			"nextRunAt":           nowISO,
			"recoveredQueueItems": recoveredQueueItems,
			"deferred":            true,
		}); err != nil {
			return requeued, err
		}
	}
	return requeued, nil
}

func (r *Runtime) appendStartedEvent(ctx context.Context, startedAt time.Time, recoverySummary RecoverySummary) error {
	services := r.Services()
	if services.Repositories == nil {
		return nil
	}

	return appendSystemEventWithPayload(ctx, services.Repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.started",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd"),
		CreatedAt:  formatJavaScriptISOString(startedAt.Add(time.Millisecond)),
	}, map[string]any{
		"daemonMode": r.config.Daemon.Mode,
		"host":       r.config.Server.Host,
		"port":       r.config.Server.Port,
		"recovery":   recoverySummary,
	})
}

func (r *Runtime) ExecutionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	return r.executionMatchesProcess(ctx, execution, pid)
}

func (r *Runtime) reconcileLiveStaleRunningRuns(ctx context.Context) (StaleRunReconcileSummary, error) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	r.mu.RUnlock()
	if repositories == nil {
		return StaleRunReconcileSummary{}, fmt.Errorf("storage is not configured")
	}
	if now == nil {
		now = time.Now
	}
	return r.reconcileStaleRunningRunsWithMode(ctx, repositories, now().UTC(), staleRunReconcileModeLive)
}

func (r *Runtime) reconcileStaleRunningRunsWithMode(ctx context.Context, repositories *storage.Repositories, now time.Time, mode staleRunReconcileMode) (StaleRunReconcileSummary, error) {
	summary := StaleRunReconcileSummary{Mode: string(mode), StartedAt: formatJavaScriptISOString(now)}
	if repositories == nil || repositories.Runs == nil || repositories.Loops == nil {
		return summary, nil
	}
	nowISO := summary.StartedAt
	// Retire quarantine evidence the operator already resolved before scanning
	// running runs: settling frees the one-running-run-per-loop index the
	// replacement run would otherwise collide with.
	settlement, err := r.settleDisposedQuarantine(ctx, repositories, nowISO)
	if err != nil {
		return StaleRunReconcileSummary{}, err
	}
	summary.QuarantineSettlement = settlement
	summary.EventsWritten += settlement.EventsWritten

	runningRuns, err := repositories.Runs.ListByStatus(ctx, string(domain.RunStatusRunning))
	if err != nil {
		return StaleRunReconcileSummary{}, err
	}
	activeExecutionsByRunID := make(map[string][]storage.AgentExecutionRecord)
	if repositories.AgentExecutions != nil {
		activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		for _, execution := range activeExecutions {
			if execution.RunID == nil || strings.TrimSpace(*execution.RunID) == "" {
				continue
			}
			activeExecutionsByRunID[*execution.RunID] = append(activeExecutionsByRunID[*execution.RunID], execution)
		}
	}
	// Loops whose work was parked via quarantineRecoveryEvidence must not be
	// requeued by the post-interrupt repair pass or the later interrupted-loop
	// sweep while agent_executions remain running evidence (#575).
	quarantinedLoopIDs := make(map[string]struct{})
	for _, run := range runningRuns {
		if err := ctx.Err(); err != nil {
			return StaleRunReconcileSummary{}, err
		}
		loop, err := repositories.Loops.GetByID(ctx, run.LoopID)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		if loop == nil {
			continue
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, run.LoopID)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		decision, err := r.evaluateStaleRunCandidate(ctx, repositories, run, *loop, latestRun, activeExecutionsByRunID[run.ID], now, mode)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		if !decision.Candidate {
			continue
		}
		summary.CandidateRuns += 1
		if decision.Uncertain {
			summary.SkippedUncertainRuns += 1
			summary.EventsWritten += decision.EventsWritten
			for _, execution := range decision.ConfirmationExecutions {
				reason := "needs confirmation: execution liveness is not authoritative"
				quarantined, wrote, err := r.quarantineRecoveryEvidence(ctx, repositories, execution, nowISO, reason)
				if err != nil {
					return StaleRunReconcileSummary{}, err
				}
				if quarantined {
					summary.QuarantinedExecutions += 1
					summary.ExecutionIDs = append(summary.ExecutionIDs, execution.ID)
					if _, seen := quarantinedLoopIDs[run.LoopID]; !seen {
						quarantinedLoopIDs[run.LoopID] = struct{}{}
						summary.QuarantinedLoopIDs = append(summary.QuarantinedLoopIDs, run.LoopID)
					}
				}
				if wrote {
					summary.EventsWritten += 1
				}
			}
			continue
		}
		if !decision.Interrupt {
			continue
		}
		finalized, err := r.finalizeSuccessfulWorkerRunIfNeeded(ctx, repositories, *loop, &run, nowISO)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		if finalized {
			continue
		}
		if err := interruptRecoveryRun(ctx, repositories, run, *loop, nowISO, decision.Message); err != nil {
			return StaleRunReconcileSummary{}, err
		}
		summary.InterruptedRuns += 1
		summary.EventsWritten += 1
		summary.RunIDs = append(summary.RunIDs, run.ID)
		summary.LoopIDs = append(summary.LoopIDs, run.LoopID)

		latestRunBlocksRequeue := false
		if latestRun != nil && latestRun.ID != run.ID && latestRun.Status == string(domain.RunStatusRunning) {
			verification, err := r.verifyRunExecutionLiveness(ctx, repositories, activeExecutionsByRunID[latestRun.ID], now, string(mode)+"_latest_run")
			if err != nil {
				return StaleRunReconcileSummary{}, err
			}
			summary.EventsWritten += verification.EventsWritten
			latestRunBlocksRequeue = verification.Live || verification.Uncertain
		}
		queueRepair, err := r.repairStaleRunQueueState(ctx, repositories, *loop, latestRun, latestRunBlocksRequeue, nowISO)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		summary.LoopsRequeued += queueRepair.LoopsRequeued
		summary.QueueItemsRequeued += queueRepair.QueueItemsRequeued
		summary.QueueItemsCancelled += queueRepair.QueueItemsCancelled
		summary.EventsWritten += queueRepair.EventsWritten
	}
	if repositories.Queue != nil {
		loops, err := repositories.Loops.List(ctx)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		for _, loop := range loops {
			if _, quarantined := quarantinedLoopIDs[loop.ID]; quarantined {
				continue
			}
			queueRepair, err := r.repairInterruptedLoopQueueIfNeeded(ctx, repositories, loop, nowISO)
			if err != nil {
				return StaleRunReconcileSummary{}, err
			}
			summary.LoopsRequeued += queueRepair.LoopsRequeued
			summary.QueueItemsRequeued += queueRepair.QueueItemsRequeued
			summary.QueueItemsCancelled += queueRepair.QueueItemsCancelled
			summary.EventsWritten += queueRepair.EventsWritten
		}
	}
	summary.CompletedAt = nowISO
	return summary, nil
}

func (r *Runtime) appendStoppedEvent(ctx context.Context, repositories *storage.Repositories, reason string) error {
	return appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.stopped",
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd"),
		CreatedAt:  formatJavaScriptISOString(r.now()),
	}, map[string]any{
		"reason": reason,
	})
}

func defaultSyncConfiguredProjects(ctx context.Context, service *projects.Service, cfg config.Config, now time.Time) error {
	if service == nil {
		return fmt.Errorf("projects service is not configured")
	}
	return service.SyncConfigured(ctx, cfg, now)
}

func captureProjectPullRequestSnapshot(ctx context.Context, cfg *config.Config, gateway *githubinfra.Gateway, input projects.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	transportRepo, err := reviewThreadRepoForProject(cfg, input.ProjectID, input.Repo)
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	return gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{
		ProjectID: input.ProjectID, Repo: input.Repo, TransportRepo: transportRepo,
		PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt,
	})
}

type staleRunCandidateDecision struct {
	Candidate              bool
	Interrupt              bool
	Uncertain              bool
	Message                string
	ConfirmationExecutions []storage.AgentExecutionRecord
	EventsWritten          int64
}

type staleRunQueueRepairSummary struct {
	LoopsRequeued       int64
	QueueItemsRequeued  int64
	QueueItemsCancelled int64
	EventsWritten       int64
}

func (r *Runtime) evaluateStaleRunCandidate(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, latestRun *storage.RunRecord, activeExecutions []storage.AgentExecutionRecord, now time.Time, mode staleRunReconcileMode) (staleRunCandidateDecision, error) {
	decision := staleRunCandidateDecision{}
	if run.Status != string(domain.RunStatusRunning) {
		return decision, nil
	}
	if latestRun == nil {
		if mode == staleRunReconcileModeStartup {
			decision.Candidate = true
			decision.Interrupt = true
			decision.Message = "Interrupted stale/orphaned running run during looperd recovery"
		}
		return decision, nil
	}
	if mode != staleRunReconcileModeStartup && len(activeExecutions) == 0 && runHeartbeatIsRecent(run, now, executionLivenessLeaseTTL) {
		return decision, nil
	}
	if mode != staleRunReconcileModeStartup && len(activeExecutions) == 0 && latestRun.ID == run.ID && !isAgentBackedRunStep(loop, run) {
		return decision, nil
	}
	decision.Candidate = true
	if len(activeExecutions) > 0 {
		verification, err := r.verifyRunExecutionLiveness(ctx, repositories, activeExecutions, now, string(mode)+"_stale_run")
		if err != nil {
			return staleRunCandidateDecision{}, err
		}
		decision.EventsWritten += verification.EventsWritten
		if verification.Live {
			return staleRunCandidateDecision{}, nil
		}
		if verification.Uncertain {
			decision.Uncertain = true
			decision.ConfirmationExecutions = append(decision.ConfirmationExecutions, verification.ConfirmationExecutions...)
			return decision, nil
		}
	}
	decision.Interrupt = true
	if mode == staleRunReconcileModeStartup {
		decision.Message = "Interrupted stale/orphaned running run during looperd recovery"
	} else if latestRun.ID != run.ID {
		decision.Message = "Interrupted superseded stale running run during stale-run reconciliation"
	} else {
		decision.Message = "Interrupted stale running run during stale-run reconciliation"
	}
	return decision, nil
}

type executionLivenessResult struct {
	Live bool
	// Uncertain is true when any execution lacks the two-signal Authority needed
	// for stale recovery.
	Uncertain              bool
	ConfirmationExecutions []storage.AgentExecutionRecord
	EventsWritten          int64
}

func (r *Runtime) verifyRunExecutionLiveness(ctx context.Context, repositories *storage.Repositories, executions []storage.AgentExecutionRecord, now time.Time, scope string) (executionLivenessResult, error) {
	result := executionLivenessResult{}
	for _, execution := range executions {
		assessment, eventsWritten, err := r.observeExecutionLiveness(ctx, repositories, execution, now, scope)
		if err != nil {
			return executionLivenessResult{}, err
		}
		result.EventsWritten += eventsWritten
		switch assessment.Disposition {
		case executionLivenessActive:
			result.Live = true
			continue
		case executionLivenessNeedsConfirmation:
			result.Uncertain = true
			result.ConfirmationExecutions = append(result.ConfirmationExecutions, execution)
		}
	}
	return result, nil
}

func (r *Runtime) observeExecutionLiveness(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, now time.Time, scope string) (executionLivenessAssessment, int64, error) {
	assessment := executionLivenessAssessment{}
	if r.currentDaemonOwnsExecution(execution) {
		pid := 0
		if execution.PID != nil {
			pid = int(*execution.PID)
		}
		assessment = executionLivenessAssessment{
			Disposition: executionLivenessActive,
			Classification: ContainmentClassification{
				Class:  ContainmentObservedLive,
				Reason: "current_daemon_supervisor_handle",
				PID:    pid,
			},
		}
	} else {
		var err error
		assessment, err = r.assessExecutionLiveness(ctx, execution, now)
		if err != nil {
			return executionLivenessAssessment{}, 0, err
		}
	}

	nowISO := formatJavaScriptISOString(now)
	eventsWritten := int64(0)
	wrote, err := r.appendContainmentClassificationEvent(ctx, repositories, execution, assessment.Classification, scope, nowISO)
	if err != nil {
		return executionLivenessAssessment{}, 0, err
	}
	if wrote {
		eventsWritten++
	}
	wrote, err = r.appendExecutionLivenessEvent(ctx, repositories, execution, assessment, scope, nowISO)
	if err != nil {
		return executionLivenessAssessment{}, 0, err
	}
	if wrote {
		eventsWritten++
	}
	if assessment.Disposition == executionLivenessNeedsConfirmation &&
		(assessment.Classification.Reason == "process_probe_error" || assessment.Classification.Reason == "process_identity_mismatch") {
		if r.logger != nil && assessment.Classification.Reason == "process_probe_error" {
			r.logger.Warn("failed to verify agent execution identity", map[string]any{
				"executionId": execution.ID,
				"pid":         assessment.Classification.PID,
				"scope":       scope,
			})
		}
		wrote, err = r.appendUncertainProcessIdentityEvent(ctx, repositories, execution, assessment.Classification.PID, scope, nowISO)
		if err != nil {
			return executionLivenessAssessment{}, 0, err
		}
		if wrote {
			eventsWritten++
		}
	}
	return assessment, eventsWritten, nil
}

func (r *Runtime) currentDaemonOwnsExecution(execution storage.AgentExecutionRecord) bool {
	r.mu.RLock()
	registry := r.services.ActiveExecutions
	if registry == nil {
		registry = r.activeExecutions
	}
	r.mu.RUnlock()
	return registry != nil && registry.HasLiveHandle(
		strings.TrimSpace(derefString(execution.LoopID)),
		strings.TrimSpace(derefString(execution.RunID)),
		execution.ID,
	)
}

func (r *Runtime) repairStaleRunQueueState(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool, nowISO string) (staleRunQueueRepairSummary, error) {
	summary := staleRunQueueRepairSummary{}
	if repositories == nil || repositories.Queue == nil {
		return summary, nil
	}
	latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return staleRunQueueRepairSummary{}, err
	}
	if latestQueueIsManualIntervention(latestQueue) {
		if loop.Status == "running" || loop.Status == "queued" {
			if latestQueue.Status == "manual_intervention" {
				normalizedLoop := loop
				normalizedLoop.Status = normalizeStaleQueuedLoopStatus(loop, derefRunOrZero(latestRun))
				normalizedLoop.NextRunAt = nil
				if latestRun != nil {
					normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				normalizedLoop.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
					return staleRunQueueRepairSummary{}, err
				}
			} else if latestQueue.Status == "running" {
				requeuedLoop := loop
				requeuedLoop.Status = "queued"
				requeuedLoop.NextRunAt = stringPtr(nowISO)
				if latestRun != nil {
					requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				requeuedLoop.UpdatedAt = nowISO
				repair, err := r.requeueStaleRunLoop(ctx, storage.StaleRunRequeueInput{Loop: requeuedLoop, NowISO: nowISO})
				if err != nil {
					return staleRunQueueRepairSummary{}, err
				}
				if !repair.Applied {
					return summary, nil
				}
				summary.LoopsRequeued = 1
				summary.QueueItemsRequeued = repair.QueueItemsRequeued
				return summary, nil
			}
		}
	}
	if shouldRequeueLoop(loop, latestRun, latestRunHasLiveAgent) {
		requeuedLoop := loop
		requeuedLoop.Status = "queued"
		requeuedLoop.NextRunAt = stringPtr(nowISO)
		if latestRun != nil {
			requeuedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
		}
		requeuedLoop.UpdatedAt = nowISO
		repair, err := r.requeueStaleRunLoop(ctx, storage.StaleRunRequeueInput{
			Loop:             requeuedLoop,
			NowISO:           nowISO,
			Seed:             r.recoveryQueueItemSeed(requeuedLoop, nowISO),
			CancelDuplicates: true,
		})
		if err != nil {
			return staleRunQueueRepairSummary{}, err
		}
		if !repair.Applied {
			return summary, nil
		}
		summary.QueueItemsCancelled += repair.QueueItemsCancelled
		summary.LoopsRequeued = 1
		summary.QueueItemsRequeued = repair.QueueItemsRequeued
		if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.loop_requeued",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			CreatedAt:  nowISO,
		}, map[string]any{
			"previousStatus":      loop.Status,
			"nextRunAt":           nowISO,
			"recoveredQueueItems": repair.QueueItemsRequeued,
		}); err != nil {
			return staleRunQueueRepairSummary{}, err
		}
		summary.EventsWritten = 1
		return summary, nil
	}
	reason := "Cancelled stale queue items during stale-run reconciliation"
	cancelledCount, err := repositories.Queue.CancelByLoop(ctx, loop.ID, nowISO, &reason)
	if err != nil {
		return staleRunQueueRepairSummary{}, err
	}
	summary.QueueItemsCancelled = cancelledCount
	return summary, nil
}

func (r *Runtime) repairInterruptedLoopQueueIfNeeded(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string) (staleRunQueueRepairSummary, error) {
	if repositories == nil || repositories.Runs == nil || repositories.Queue == nil {
		return staleRunQueueRepairSummary{}, nil
	}
	latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || latestRun == nil {
		return staleRunQueueRepairSummary{}, err
	}
	if finalized, err := r.finalizeSuccessfulWorkerRunIfNeeded(ctx, repositories, loop, latestRun, nowISO); err != nil || finalized {
		return staleRunQueueRepairSummary{}, err
	}
	if latestRun.Status != string(domain.RunStatusInterrupted) {
		return staleRunQueueRepairSummary{}, nil
	}
	activeCount, err := repositories.Queue.CountActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return staleRunQueueRepairSummary{}, err
	}
	runningCount, err := repositories.Queue.CountByLoopIDAndStatus(ctx, loop.ID, "running")
	if err != nil {
		return staleRunQueueRepairSummary{}, err
	}
	if loop.Status == "queued" && runningCount == 0 && activeCount == 1 {
		return staleRunQueueRepairSummary{}, nil
	}
	if activeCount == 0 && !shouldRequeueLoop(loop, latestRun, false) {
		return staleRunQueueRepairSummary{}, nil
	}
	return r.repairStaleRunQueueState(ctx, repositories, loop, latestRun, false, nowISO)
}

func (r *Runtime) finalizeSuccessfulWorkerRunIfNeeded(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, run *storage.RunRecord, nowISO string) (bool, error) {
	if loop.Type != string(domain.LoopTypeWorker) || run == nil || !worker.IsSuccessfulClaimFinalizationCandidate(*run) || repositories == nil || repositories.Queue == nil {
		return false, nil
	}
	queueItem, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || queueItem == nil || !worker.CanFinalizeSuccessfulClaim(*queueItem, *run, loop.Status) {
		return false, err
	}
	r.mu.RLock()
	coordinator := r.services.Coordinator
	r.mu.RUnlock()
	if coordinator == nil {
		return false, fmt.Errorf("recover worker success finalization: sqlite coordinator is not configured")
	}
	completed := *run
	completed.Status = string(domain.RunStatusSuccess)
	completed.CurrentStep = nil
	completed.ErrorMessage = nil
	if completed.Summary == nil {
		completed.Summary = stringPtr("Recovered completed worker " + loop.ID)
	}
	completed.EndedAt = stringPtr(nowISO)
	completed.LastHeartbeatAt = stringPtr(nowISO)
	completed.UpdatedAt = nowISO
	if err := storage.FinalizeWorkerSuccess(ctx, coordinator.DB(), storage.WorkerSuccessFinalizationInput{Run: completed, QueueItemID: queueItem.ID, LoopID: loop.ID, LoopStatus: "completed", FinishedAt: nowISO}); err != nil {
		return false, err
	}
	return true, nil
}

func isAgentBackedRunStep(loop storage.LoopRecord, run storage.RunRecord) bool {
	if run.CurrentStep == nil {
		return false
	}
	step := strings.TrimSpace(*run.CurrentStep)
	switch loop.Type {
	case "planner":
		return step == "write-spec"
	case "reviewer":
		return step == "thread_resolution" || step == "review"
	case "fixer":
		return step == "repair"
	case "worker":
		return step == "execute"
	default:
		return false
	}
}

func runHeartbeatIsRecent(run storage.RunRecord, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	heartbeatAt := firstNonEmpty(stringOrEmpty(run.LastHeartbeatAt), run.UpdatedAt, run.StartedAt)
	if heartbeatAt == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(heartbeatAt))
	if err != nil {
		return false
	}
	return !parsed.UTC().Before(now.UTC().Add(-ttl))
}

func (r *Runtime) appendUncertainProcessIdentityEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, pid int, scope string, nowISO string) (bool, error) {
	if r.logger != nil {
		r.logger.Warn("recovery skipped due to uncertain process identity", map[string]any{"executionId": execution.ID, "pid": pid, "scope": scope})
	}
	payloadJSON, err := marshalJSON(map[string]any{
		"pid":    pid,
		"reason": "command_mismatch",
		"scope":  scope,
	})
	if err != nil {
		return false, fmt.Errorf("marshal uncertain process identity event: %w", err)
	}
	if repositories != nil && repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == "looperd.recovery.process_identity_uncertain" && event.PayloadJSON == payloadJSON {
				return false, nil
			}
		}
	}
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   "looperd.recovery.process_identity_uncertain",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: payloadJSON,
		CreatedAt:   nowISO,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// appendContainmentClassificationEvent records confirmed_dead / observed_live /
// uncertain for an execution observation. Dedupes identical class+reason+scope.
func (r *Runtime) appendContainmentClassificationEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, classification ContainmentClassification, scope, nowISO string) (bool, error) {
	payload := map[string]any{
		"class":  string(classification.Class),
		"reason": classification.Reason,
		"scope":  scope,
	}
	if classification.PID > 0 {
		payload["pid"] = classification.PID
	}
	payloadJSON, err := marshalJSON(payload)
	if err != nil {
		return false, fmt.Errorf("marshal containment classification event: %w", err)
	}
	if repositories != nil && repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == "looperd.recovery.containment_classified" && event.PayloadJSON == payloadJSON {
				return false, nil
			}
		}
	}
	if r.logger != nil {
		r.logger.Info("startup recovery classified containment evidence", map[string]any{
			"executionId": execution.ID,
			"class":       string(classification.Class),
			"reason":      classification.Reason,
			"scope":       scope,
			"pid":         classification.PID,
		})
	}
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   "looperd.recovery.containment_classified",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: payloadJSON,
		CreatedAt:   nowISO,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Runtime) appendExecutionLivenessEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, assessment executionLivenessAssessment, scope, nowISO string) (bool, error) {
	eventType := "looperd.recovery.execution_confirmation_needed"
	switch assessment.Disposition {
	case executionLivenessActive:
		eventType = "looperd.recovery.execution_active"
	}
	payload := map[string]any{
		"status":         string(assessment.Disposition),
		"identityReason": assessment.Classification.Reason,
		"scope":          scope,
	}
	if assessment.LeaseHeartbeat != "" {
		payload["leaseHeartbeatAt"] = assessment.LeaseHeartbeat
	}
	if assessment.LeaseExpiresAt != "" {
		payload["leaseExpiresAt"] = assessment.LeaseExpiresAt
	}
	if assessment.Classification.PID > 0 {
		payload["pid"] = assessment.Classification.PID
	}
	payloadJSON, err := marshalJSON(payload)
	if err != nil {
		return false, fmt.Errorf("marshal execution liveness event: %w", err)
	}
	if repositories != nil && repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == eventType && event.PayloadJSON == payloadJSON {
				return false, nil
			}
		}
	}
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   eventType,
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: payloadJSON,
		CreatedAt:   nowISO,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// quarantineRecoveryEvidence parks work tied to uncertain/orphan execution
// evidence using existing manual_intervention / paused states. It does not
// signal processes or mark agent_executions terminal.
//
// Returns (didQuarantine, wroteEvent, err).
func (r *Runtime) quarantineRecoveryEvidence(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, nowISO, reason string) (bool, bool, error) {
	if repositories == nil {
		return false, false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "recovery quarantine: execution evidence without containment confirmation"
	}
	did := false
	wrote := false

	if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" && repositories.Loops != nil {
		loop, err := repositories.Loops.GetByID(ctx, *execution.LoopID)
		if err != nil {
			return false, false, err
		}
		if loop != nil {
			switch loop.Status {
			case "paused", "completed", "failed", "terminated", "stopped", "human_takeover":
				// Already non-actionable for claim/requeue. human_takeover is a
				// deliberate park (interactive handback); do not rewrite to paused.
			default:
				updated := *loop
				updated.Status = "paused"
				updated.NextRunAt = nil
				updated.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, updated); err != nil {
					return false, false, err
				}
				did = true
			}
		}
	}

	if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" && repositories.Queue != nil {
		var items []storage.QueueItemRecord
		if active, err := repositories.Queue.FindActiveByLoopID(ctx, *execution.LoopID); err != nil {
			return false, false, err
		} else if active != nil {
			items = append(items, *active)
		}
		if latest, err := repositories.Queue.GetLatestByLoopID(ctx, *execution.LoopID); err != nil {
			return false, false, err
		} else if latest != nil && (len(items) == 0 || items[0].ID != latest.ID) {
			items = append(items, *latest)
		}
		for _, item := range items {
			if item.Status != "queued" && item.Status != "running" {
				continue
			}
			msg := reason
			if err := repositories.Queue.Fail(ctx, storage.QueueFailInput{
				ID:           item.ID,
				FinishedAt:   nowISO,
				UpdatedAt:    nowISO,
				ErrorMessage: &msg,
				ErrorKind:    "manual_intervention",
			}); err != nil {
				return false, false, err
			}
			did = true
		}
	}

	payload := map[string]any{
		"reason":      reason,
		"recoveredAt": nowISO,
		"executionId": execution.ID,
		"status":      execution.Status,
	}
	if execution.PID != nil && *execution.PID > 0 {
		payload["pid"] = *execution.PID
	}
	if repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return did, false, err
		}
		for _, event := range events {
			if event.EventType != "looperd.recovery.execution_quarantined" {
				continue
			}
			var previous map[string]any
			if json.Unmarshal([]byte(event.PayloadJSON), &previous) == nil &&
				strings.TrimSpace(fmt.Sprint(previous["reason"])) == reason &&
				strings.TrimSpace(fmt.Sprint(previous["status"])) == execution.Status {
				return did, false, nil
			}
		}
	}
	if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  recoveryExecutionQuarantinedEventType,
		ProjectID:  execution.ProjectID,
		LoopID:     execution.LoopID,
		RunID:      execution.RunID,
		EntityType: stringPtr("agent_execution"),
		EntityID:   stringPtr(execution.ID),
		CreatedAt:  nowISO,
	}, payload); err != nil {
		return did, false, err
	}
	wrote = true
	if r.logger != nil {
		r.logger.Warn("recovery quarantined execution evidence without process kill", map[string]any{
			"executionId": execution.ID,
			"loopId":      execution.LoopID,
			"runId":       execution.RunID,
			"reason":      reason,
		})
	}
	return did || wrote, wrote, nil
}

// requeueStaleRunLoop commits stale-run reconciliation's loop requeue and the
// queue repair that belongs with it as one transaction, so the human-hold guard
// inside the loop write decides the whole repair.
func (r *Runtime) requeueStaleRunLoop(ctx context.Context, input storage.StaleRunRequeueInput) (storage.StaleRunRequeueResult, error) {
	r.mu.RLock()
	coordinator := r.services.Coordinator
	r.mu.RUnlock()
	if coordinator == nil {
		return storage.StaleRunRequeueResult{}, fmt.Errorf("recover stale run requeue: sqlite coordinator is not configured")
	}
	return storage.RequeueStaleRunLoop(ctx, coordinator.DB(), input)
}

// recoveryQueueItemSeed names the queue item recovery would publish for a loop
// that has nothing claimable left. The fallback stays deferred because a legacy
// loop can lack the fields needed to build it while its queue history is still
// complete enough to restore.
func (r *Runtime) recoveryQueueItemSeed(loop storage.LoopRecord, nowISO string) storage.RecoveryQueueItemSeed {
	maxAttempts := int64(r.Config().Scheduler.RetryMaxAttempts)
	return storage.RecoveryQueueItemSeed{
		DerivedID: newRuntimeEventID(),
		Fallback: func() (storage.QueueItemRecord, bool, error) {
			return buildRecoveryQueueItem(loop, nowISO, maxAttempts)
		},
	}
}

func (r *Runtime) ensureRecoveryQueueItem(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string) error {
	return storage.EnsureActiveQueueItem(ctx, repositories, loop.ID, r.recoveryQueueItemSeed(loop, nowISO), nowISO)
}

func interruptRecoveryRun(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, nowISO string, message string) error {
	interrupted := run
	interrupted.Status = string(domain.RunStatusInterrupted)
	if interrupted.ErrorMessage == nil {
		interrupted.ErrorMessage = stringPtr(message)
	}
	interrupted.EndedAt = stringPtr(nowISO)
	interrupted.LastHeartbeatAt = stringPtr(nowISO)
	interrupted.UpdatedAt = nowISO
	if err := repositories.Runs.Upsert(ctx, interrupted); err != nil {
		return err
	}
	return appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  "looperd.recovery.run_interrupted",
		ProjectID:  stringPtr(loop.ProjectID),
		LoopID:     stringPtr(loop.ID),
		RunID:      stringPtr(run.ID),
		EntityType: stringPtr("run"),
		EntityID:   stringPtr(run.ID),
		CreatedAt:  nowISO,
	}, map[string]any{
		"previousStatus":  "running",
		"recoveredStatus": "interrupted",
	})
}

func buildRecoveryQueueItem(loop storage.LoopRecord, nowISO string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	queueType := domain.LoopType(loop.Type)
	if queueType != domain.LoopTypePlanner && queueType != domain.LoopTypeReviewer && queueType != domain.LoopTypeFixer && queueType != domain.LoopTypeWorker {
		return storage.QueueItemRecord{}, false, nil
	}

	projectID := loop.ProjectID
	loopID := loop.ID
	queueRecord := storage.QueueItemRecord{
		ID:          newRuntimeEventID(),
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        loop.Type,
		TargetType:  loop.TargetType,
		TargetID:    strings.TrimSpace(derefString(loop.TargetID)),
		Repo:        loop.Repo,
		PRNumber:    loop.PRNumber,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}

	switch queueType {
	case domain.LoopTypePlanner:
		repo := strings.TrimSpace(derefString(loop.Repo))
		issueNumber, err := parseIssueNumberFromTargetID(queueRecord.TargetID)
		if err != nil || repo == "" || loop.TargetType != string(domain.LoopTargetTypeIssue) {
			if err == nil {
				err = fmt.Errorf("planner loop requires repo and issue target")
			}
			return storage.QueueItemRecord{}, false, err
		}
		lockKey := storage.IssueLockKey(loop.ProjectID, repo, issueNumber)
		targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
		payload := map[string]any{"issueNumber": issueNumber}
		payloadJSON, err := marshalJSON(payload)
		if err != nil {
			return storage.QueueItemRecord{}, false, fmt.Errorf("marshal planner recovery queue payload: %w", err)
		}
		queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = nil
		queueRecord.DedupeKey = fmt.Sprintf("planner:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, issueNumber)
		queueRecord.Priority = storage.QueuePriorityPlanner
		queueRecord.LockKey = &lockKey
		queueRecord.PayloadJSON = &payloadJSON
	case domain.LoopTypeReviewer:
		repo := strings.TrimSpace(derefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("reviewer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("reviewer:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, prNumber)
		queueRecord.Priority = storage.QueuePriorityReviewer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeFixer:
		repo := strings.TrimSpace(derefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("fixer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("fixer:%s", loop.ID)
		queueRecord.Priority = storage.QueuePriorityFixer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeWorker:
		payloadJSON := buildRecoveryWorkerPayloadJSON(loop.MetadataJSON)
		if payloadJSON != nil {
			queueRecord.PayloadJSON = payloadJSON
		}
		queueRecord.Priority = storage.QueuePriorityWorker
		lockKey := fmt.Sprintf("worker:%s", loop.ID)
		queueRecord.DedupeKey = fmt.Sprintf("worker:%s", loop.ID)
		if loop.TargetType == string(domain.LoopTargetTypeIssue) {
			repo := strings.TrimSpace(derefString(loop.Repo))
			issueNumber, err := parseIssueNumberFromTargetID(queueRecord.TargetID)
			if err != nil || repo == "" {
				if err == nil {
					err = fmt.Errorf("worker loop requires repo and issue target")
				}
				return storage.QueueItemRecord{}, false, err
			}
			lockKey = storage.IssueLockKey(loop.ProjectID, repo, issueNumber)
			targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = nil
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, issueNumber)
		} else if loop.TargetType == string(domain.LoopTargetTypePullRequest) {
			repo := strings.TrimSpace(derefString(loop.Repo))
			if repo == "" || loop.PRNumber == nil {
				return storage.QueueItemRecord{}, false, fmt.Errorf("worker loop requires repo and prNumber")
			}
			prNumber := *loop.PRNumber
			lockKey = storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
			targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = &prNumber
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, prNumber)
		}
		queueRecord.LockKey = &lockKey
	}

	return queueRecord, true, nil
}

func buildRecoveryWorkerPayloadJSON(metadataJSON *string) *string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return nil
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return nil
	}
	workerMeta, ok := metadata["worker"].(map[string]any)
	if !ok || len(workerMeta) == 0 {
		return nil
	}
	encoded, err := json.Marshal(workerMeta)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

func parseIssueNumberFromTargetID(targetID string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(targetID), ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	var issueNumber int64
	if _, err := fmt.Sscanf(parts[2], "%d", &issueNumber); err != nil || issueNumber <= 0 {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	return issueNumber, nil
}

func formatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

func appendSystemEvent(ctx context.Context, repositories *storage.Repositories, record storage.EventLogRecord) error {
	if repositories == nil || repositories.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}

	record.ActorType = stringPtr("system")
	record.ActorID = stringPtr("looperd")
	record.ActorDisplayName = stringPtr("looperd")
	return repositories.Events.Append(ctx, record)
}

func appendSystemEventWithPayload(ctx context.Context, repositories *storage.Repositories, record storage.EventLogRecord, payload any) error {
	encoded, err := marshalJSON(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event payload: %w", record.EventType, err)
	}
	record.PayloadJSON = encoded
	return appendSystemEvent(ctx, repositories, record)
}

func (r *Runtime) executionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	processCommand, err := r.readProcessCommand(ctx, pid)
	if err != nil {
		return false, false, err
	}
	processCommand = strings.TrimSpace(processCommand)
	if processCommand == "" {
		return false, false, nil
	}
	expectedBirth, ok := executionProcessBirth(execution)
	if !ok {
		return false, true, fmt.Errorf("missing durable process start identity")
	}
	if processidentity.RequiresBootID() && expectedBirth.BootID == "" {
		return false, true, fmt.Errorf("missing durable Linux boot identity")
	}
	actualStart, err := r.readProcessStart(ctx, pid)
	if err != nil {
		return false, true, fmt.Errorf("read process start identity: %w", err)
	}
	if actualStart != expectedBirth.StartTime {
		return false, true, nil
	}
	if expectedBirth.BootID != "" {
		actualBootID, err := r.readProcessBootID(ctx, pid)
		if err != nil {
			return false, true, fmt.Errorf("read process boot identity: %w", err)
		}
		if strings.TrimSpace(actualBootID) != expectedBirth.BootID {
			return false, true, nil
		}
	}
	// The birth token, not argv, is process identity. A shebang wrapper can exec
	// the real CLI in place while retaining the same PID and birth token.
	return true, true, nil
}

func executionProcessBirth(execution storage.AgentExecutionRecord) (processidentity.Birth, bool) {
	if execution.MetadataJSON == nil || strings.TrimSpace(*execution.MetadataJSON) == "" {
		return processidentity.Birth{}, false
	}
	var payload struct {
		ProcessIdentity struct {
			StartTime int64  `json:"startTime"`
			BootID    string `json:"bootId"`
		} `json:"processIdentity"`
	}
	if json.Unmarshal([]byte(*execution.MetadataJSON), &payload) != nil || payload.ProcessIdentity.StartTime <= 0 {
		return processidentity.Birth{}, false
	}
	return processidentity.Birth{
		StartTime: payload.ProcessIdentity.StartTime,
		BootID:    strings.TrimSpace(payload.ProcessIdentity.BootID),
	}, true
}

func defaultReadProcessCommand(ctx context.Context, pid int) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		return "", fmt.Errorf("inspect process %d with ps: %w", pid, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func defaultReadProcessStart(ctx context.Context, pid int) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return processidentity.StartTime(pid)
}

func defaultReadProcessBootID(ctx context.Context, _ int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return processidentity.LinuxBootID()
}

func defaultSignalProcess(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}

func createEmptyRecoverySummary() RecoverySummary {
	return RecoverySummary{
		OrphanAgentCleanup: RecoveryOrphanAgentCleanup{
			Attempted:    false,
			CleanedCount: 0,
		},
		ExpiredLocksReleased:  0,
		InterruptedRunsMarked: 0,
		LoopsRequeued:         0,
		EventsWritten:         0,
	}
}

type runtimeReviewerCheckpoint struct {
	ResumePolicy string `json:"resumePolicy,omitempty"`
	Detail       *struct {
		State          string           `json:"state,omitempty"`
		IsDraft        bool             `json:"isDraft,omitempty"`
		ReviewDecision string           `json:"reviewDecision,omitempty"`
		Labels         []string         `json:"labels,omitempty"`
		HeadSHA        string           `json:"headSha,omitempty"`
		CurrentLogin   string           `json:"currentLogin,omitempty"`
		Reviews        []map[string]any `json:"reviews,omitempty"`
	} `json:"detail,omitempty"`
}

type runtimeReviewerRecoveryPolicy struct {
	includeDrafts    bool
	stopOnApproved   bool
	stopOnReadyLabel bool
	currentLogin     string
	retry            config.ReviewerRetryConfig
}

func (r *Runtime) reviewerRecoveryPolicyForProject(projectID string) runtimeReviewerRecoveryPolicy {
	cfg := r.Config()
	roles := config.ProjectRoleConfigs(cfg, projectID)
	includeDrafts := roles.Reviewer.Discovery.Triggers.IncludeDrafts
	if reviewerRole, ok := config.ProjectCodingRoleConfig(cfg, projectID, config.CodingRoleReviewer); ok {
		includeDrafts = reviewerRole.Discovery.IncludeDrafts
	}
	return runtimeReviewerRecoveryPolicy{
		includeDrafts:    includeDrafts,
		stopOnApproved:   roles.Reviewer.Behavior.Loop.StopOnApproved,
		stopOnReadyLabel: roles.Reviewer.Behavior.Loop.StopOnReadyLabel,
		retry:            config.NormalizeReviewerRetryConfig(roles.Reviewer.Behavior.Retry),
	}
}

func (r *Runtime) currentReviewerLoginForRecovery(ctx context.Context, repositories *storage.Repositories, githubGateway *githubinfra.Gateway, loop storage.LoopRecord, latestRun *storage.RunRecord, policy runtimeReviewerRecoveryPolicy) (string, bool) {
	if githubGateway == nil || !policy.stopOnApproved || latestRun == nil || strings.TrimSpace(loop.ProjectID) == "" {
		return "", false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	if checkpoint.Detail == nil || len(checkpoint.Detail.Reviews) == 0 {
		return "", false
	}
	project, err := repositories.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to load project for reviewer recovery login refresh", map[string]any{"loopId": loop.ID, "projectId": loop.ProjectID, "error": err.Error()})
		}
		return "", false
	}
	if project == nil || strings.TrimSpace(project.RepoPath) == "" {
		return "", false
	}
	loginCtx, cancel := context.WithTimeout(ctx, reviewerRecoveryLoginTimeout)
	defer cancel()
	repo := ""
	if loop.Repo != nil {
		repo = *loop.Repo
	}
	cfg := r.Config()
	login, err := roleCurrentUserLogin(loginCtx, &cfg, githubGateway, repo, project.RepoPath)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("failed to refresh reviewer login during recovery", map[string]any{"loopId": loop.ID, "projectId": loop.ProjectID, "error": err.Error()})
		}
		return "", false
	}
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return "", false
	}
	return login, true
}

func canRefreshReviewerLoginForRecovery(loop storage.LoopRecord, latestRun *storage.RunRecord) bool {
	return loop.Type == string(domain.LoopTypeReviewer) && loop.Status == "failed" && latestRun != nil && latestRun.Status == "failed"
}

func reviewerRecoveryNeedsFreshLogin(loop storage.LoopRecord, latestRun *storage.RunRecord, policy runtimeReviewerRecoveryPolicy) bool {
	if !canRefreshReviewerLoginForRecovery(loop, latestRun) || !policy.stopOnApproved || latestRun == nil {
		return false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	return checkpoint.Detail != nil && len(checkpoint.Detail.Reviews) > 0
}

func shouldAutoRecoverFailedReviewerLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestQueue *storage.QueueItemRecord, policy runtimeReviewerRecoveryPolicy) bool {
	if loop.Type != string(domain.LoopTypeReviewer) || loop.Status != "failed" || latestRun == nil || latestRun.Status != "failed" || latestQueue == nil || latestQueue.Status != "failed" {
		return false
	}
	meta := parseRuntimeJSONObject(loop.MetadataJSON)
	if manual, _ := meta["manual"].(bool); manual {
		return false
	}
	if !runtimeReviewerLoopEnabled(meta) {
		return false
	}
	loopMeta := runtimeReviewerLoopMetadata(meta)
	if reason, _ := runtimeStringFromAny(loopMeta["terminationReason"]); reason != "" && !isDeprecatedReviewerLoopBudgetReason(reason) {
		return false
	}
	policy.retry = config.NormalizeReviewerRetryConfig(policy.retry)
	if runtimeIntFromAny(loopMeta["autoRecoveryAttempts"]) >= policy.retry.AutoRecoveryMaxAttempts {
		return false
	}
	checkpoint := parseRuntimeReviewerCheckpoint(latestRun.CheckpointJSON)
	queueKind := derefString(latestQueue.LastErrorKind)
	queueMessage := derefString(latestQueue.LastError)
	resumePolicy := loops.NormalizeResumePolicy(queueKind, checkpoint.ResumePolicy)
	if loops.SuppressesAutonomousRecovery(queueKind, resumePolicy) {
		return false
	}
	if checkpoint.Detail == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(checkpoint.Detail.State)) != "open" {
		return false
	}
	if !policy.includeDrafts && checkpoint.Detail.IsDraft {
		return false
	}
	currentLogin := checkpoint.Detail.CurrentLogin
	if strings.TrimSpace(policy.currentLogin) != "" {
		currentLogin = policy.currentLogin
	}
	if policy.stopOnApproved && runtimeReviewerCheckpointApprovedForRecovery(checkpoint.Detail.Reviews, currentLogin, checkpoint.Detail.HeadSHA, checkpoint.Detail.ReviewDecision) {
		return false
	}
	if policy.stopOnReadyLabel && labels.Has(checkpoint.Detail.Labels, labels.SpecReady) {
		return false
	}
	failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage), queueMessage)
	return (queueKind == loops.FailureKindRetryableAfterResume && (resumePolicy == loops.ResumePolicyRestartFromDiscover || resumePolicy == "rerun_review")) || isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) || runtimeRecoverableEnhancedTransient(policy.retry, *latestQueue, failureSummary) || (isKnownReviewerRediscoveryGuardrail(failureSummary) && isRuntimeReviewerRediscoveryRunStep(latestRun))
}

func requeueFailedReviewerQueueItemForRecovery(ctx context.Context, repositories *storage.Repositories, loopID string, latestQueue *storage.QueueItemRecord, queuedAt string, policy runtimeReviewerRecoveryPolicy, matchedMessage string) (int64, error) {
	if latestQueue != nil && (isRuntimeRetryableTransientWithRemainingAttempts(*latestQueue) || runtimeRecoverableEnhancedTransient(policy.retry, *latestQueue, matchedMessage)) {
		return repositories.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, latestQueue.ID, queuedAt, latestQueue.Attempts)
	}
	if latestQueue == nil {
		return 0, nil
	}
	return repositories.Queue.RequeueFailedByID(ctx, loopID, latestQueue.ID, queuedAt)
}

// requeueFailedReviewerWithSharedGuards requeues a failed reviewer under the
// same per-loop + same-target locks as API discard+retry, so deferred/startup
// recovery cannot activate a PR worktree sibling between discard preflight and
// git reset/clean. didRequeue is false when eligibility no longer holds under
// the lock (no error).
func requeueFailedReviewerWithSharedGuards(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestQueue *storage.QueueItemRecord, nowISO string, policy runtimeReviewerRecoveryPolicy, latestRun *storage.RunRecord) (didRequeue bool, recoveredQueueItems int64, err error) {
	unlock := LockLoopRequeue(loop.ID)
	defer unlock()
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(loop))
	defer unlockTarget()

	// Re-check eligibility under the lock: discard/retry may have already
	// requeued or otherwise changed status while we waited.
	current, err := repositories.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return false, 0, err
	}
	if current == nil || !shouldAutoRecoverFailedReviewerLoop(*current, latestRun, latestQueue, policy) {
		return false, 0, nil
	}
	failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage), derefString(latestQueue.LastError))
	recoveredQueueItems, err = requeueFailedReviewerQueueItemForRecovery(ctx, repositories, loop.ID, latestQueue, nowISO, policy, failureSummary)
	if err != nil {
		return false, 0, err
	}
	if recoveredQueueItems == 0 {
		active, activeErr := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
		if activeErr != nil {
			return false, 0, activeErr
		}
		if active == nil {
			return false, 0, fmt.Errorf("reviewer recovery did not requeue failed queue item %s for loop %s", latestQueue.ID, loop.ID)
		}
	}
	requeuedLoop := autoRecoveredReviewerLoop(*current, nowISO)
	if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
		return false, 0, err
	}
	return true, recoveredQueueItems, nil
}

func isRuntimeRetryableTransientWithRemainingAttempts(queue storage.QueueItemRecord) bool {
	if derefString(queue.LastErrorKind) != "retryable_transient" {
		return false
	}
	return runtimeQueueHasRemainingAttempts(queue)
}

func runtimeRecoverableEnhancedTransient(policy config.ReviewerRetryConfig, queue storage.QueueItemRecord, message string) bool {
	policy = config.NormalizeReviewerRetryConfig(policy)
	return policy.RecoverExistingMatchedFailures && runtimeQueueHasRemainingAttempts(queue) && config.ReviewerRetryMessageMatches(policy, message)
}

func runtimeQueueHasRemainingAttempts(queue storage.QueueItemRecord) bool {
	return true
}

func autoRecoveredReviewerLoop(loop storage.LoopRecord, nowISO string) storage.LoopRecord {
	updated := loop
	updated.Status = "queued"
	updated.NextRunAt = stringPtr(nowISO)
	updated.UpdatedAt = nowISO
	meta := parseRuntimeJSONObject(updated.MetadataJSON)
	loopMeta := runtimeReviewerLoopMetadata(meta)
	loopMeta["status"] = "active"
	loopMeta["lastStatus"] = "auto_recovered"
	loopMeta["autoRecoveryAttempts"] = runtimeIntFromAny(loopMeta["autoRecoveryAttempts"]) + 1
	delete(loopMeta, "terminationReason")
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	if err == nil {
		text := string(encoded)
		updated.MetadataJSON = &text
	}
	return updated
}

func parseRuntimeReviewerCheckpoint(value *string) runtimeReviewerCheckpoint {
	if value == nil || strings.TrimSpace(*value) == "" {
		return runtimeReviewerCheckpoint{}
	}
	var checkpoint runtimeReviewerCheckpoint
	_ = json.Unmarshal([]byte(*value), &checkpoint)
	return checkpoint
}

func parseRuntimeJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(*value), &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

func runtimeReviewerLoopMetadata(meta map[string]any) map[string]any {
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	return loopMeta
}

func runtimeReviewerLoopEnabled(meta map[string]any) bool {
	if enabled, ok := meta["followUpdates"].(bool); ok {
		return enabled
	}
	if loopMeta, ok := meta["loop"].(map[string]any); ok {
		if enabled, ok := loopMeta["enabled"].(bool); ok {
			return enabled
		}
	}
	return false
}

func runtimeIntFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func runtimeStringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func runtimeHasApprovedReviewByAuthorForHead(reviews []map[string]any, login string, headSHA string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	headSHA = strings.TrimSpace(headSHA)
	if login == "" || headSHA == "" {
		return false
	}
	for _, review := range reviews {
		author, ok := review["author"].(map[string]any)
		if !ok {
			continue
		}
		authorLogin, ok := runtimeStringFromAny(author["login"])
		if !ok || strings.ToLower(strings.TrimSpace(authorLogin)) != login {
			continue
		}
		state, _ := runtimeStringFromAny(review["state"])
		if !strings.EqualFold(strings.TrimSpace(state), "APPROVED") {
			continue
		}
		commit, ok := review["commit"].(map[string]any)
		if !ok {
			continue
		}
		if oid, ok := runtimeStringFromAny(commit["oid"]); ok && strings.TrimSpace(oid) == headSHA {
			return true
		}
	}
	return false
}

func runtimeReviewerCheckpointApprovedForRecovery(reviews []map[string]any, login string, headSHA string, reviewDecision string) bool {
	if runtimeHasApprovedReviewByAuthorForHead(reviews, login, headSHA) {
		return true
	}
	return false
}

func isDeprecatedReviewerLoopBudgetReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "max_iterations_per_pr", "max_iterations_per_head", "max_wall_clock", "max_consecutive_failures", "max_agent_executions_per_pr":
		return true
	default:
		return false
	}
}

func removeDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	for _, key := range deprecatedReviewerLoopBudgetMetadataKeys {
		delete(loopMeta, key)
	}
	reason, _ := runtimeStringFromAny(loopMeta["terminationReason"])
	if isDeprecatedReviewerLoopBudgetReason(reason) {
		delete(loopMeta, "terminationReason")
		status, _ := runtimeStringFromAny(loopMeta["status"])
		if status == "failed" || status == "terminated" {
			loopMeta["status"] = "active"
		}
	}
}

var deprecatedReviewerLoopBudgetMetadataKeys = []string{
	"maxIterationsPerPR",
	"maxIterationsPerHead",
	"maxWallClockSeconds",
	"maxConsecutiveFailures",
	"maxAgentExecutionsPerPR",
}

func isKnownReviewerRediscoveryGuardrail(message string) bool {
	return strings.Contains(message, "PR head changed before publish") || strings.Contains(message, "review request removed before publish")
}

func isRuntimeReviewerRediscoveryRunStep(run *storage.RunRecord) bool {
	if run == nil || run.CurrentStep == nil {
		return false
	}
	switch strings.TrimSpace(*run.CurrentStep) {
	case "publish", "review", "thread_resolution":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func shouldRequeueLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool) bool {
	if terminalReviewerRecoveryMetadataStatus(loop) != "" {
		return false
	}
	// paused + awaiting_human + human_takeover are deliberately parked: a human
	// paused it, the agent is waiting for an answer, or a human is driving its
	// session via takeover. Recovery must NOT re-queue them (the killed run looks
	// "interrupted", but that's expected) — only an explicit resume does.
	if loop.Status == "paused" || loop.Status == string(domain.LoopStatusAwaitingHuman) || loop.Status == string(domain.LoopStatusHumanTakeover) {
		return false
	}
	if loop.Status == "completed" || loop.Status == "failed" || loop.Status == "terminated" || loop.Status == "stopped" {
		return false
	}
	if latestRun == nil {
		return loop.Status == "running"
	}
	if latestRun.Status == string(domain.RunStatusRunning) && latestRunHasLiveAgent {
		return false
	}

	return loop.Status == "running" || latestRun.Status == "interrupted"
}

func derefRunID(run *storage.RunRecord) string {
	if run == nil {
		return ""
	}
	return run.ID
}

func terminalReviewerRecoveryMetadataStatus(loop storage.LoopRecord) string {
	if loop.Type != string(domain.LoopTypeReviewer) || loop.MetadataJSON == nil || strings.TrimSpace(*loop.MetadataJSON) == "" {
		return ""
	}
	meta := parseRuntimeJSONObject(loop.MetadataJSON)
	loopMeta := runtimeReviewerLoopMetadata(meta)
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	status, _ := runtimeStringFromAny(loopMeta["status"])
	if status == "terminated" || status == "stopped" {
		return status
	}
	return ""
}

func normalizeTerminalReviewerLoopForRecovery(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestRun *storage.RunRecord, nowISO string) (bool, bool, error) {
	terminalReviewerStatus := terminalReviewerRecoveryMetadataStatus(loop)
	if terminalReviewerStatus == "" {
		return false, false, nil
	}
	cancelReason := "reviewer terminal metadata recovered during runtime recovery"
	cancelledQueueItems, err := repositories.Queue.CancelByLoop(ctx, loop.ID, nowISO, &cancelReason)
	if err != nil {
		return true, false, err
	}
	if loop.Status != terminalReviewerStatus || loop.NextRunAt != nil || cancelledQueueItems > 0 {
		normalizedLoop := loop
		normalizedLoop.Status = terminalReviewerStatus
		normalizedLoop.NextRunAt = nil
		if latestRun != nil {
			normalizedLoop.LastRunAt = coalesceString(latestRun.EndedAt, stringPtr(latestRun.StartedAt), loop.LastRunAt)
		}
		normalizedLoop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
			return true, false, err
		}
		latestRunStatus := ""
		if latestRun != nil {
			latestRunStatus = latestRun.Status
		}
		if err := appendSystemEventWithPayload(ctx, repositories, storage.EventLogRecord{
			ID:         newRuntimeEventID(),
			EventType:  "looperd.recovery.reviewer_terminal_metadata_normalized",
			LoopID:     stringPtr(loop.ID),
			EntityType: stringPtr("loop"),
			EntityID:   stringPtr(loop.ID),
			CreatedAt:  nowISO,
		}, map[string]any{
			"previousStatus":      loop.Status,
			"recoveredStatus":     normalizedLoop.Status,
			"latestRunStatus":     latestRunStatus,
			"cancelledQueueItems": cancelledQueueItems,
		}); err != nil {
			return true, false, err
		}
		return true, true, nil
	}
	return true, false, nil
}

func normalizeStaleQueuedLoopStatus(loop storage.LoopRecord, latestRun storage.RunRecord) string {
	if status := terminalReviewerRecoveryMetadataStatus(loop); status != "" {
		return status
	}

	switch latestRun.Status {
	case "success":
		return "completed"
	case "interrupted", "running":
		return "interrupted"
	default:
		return "paused"
	}
}

func latestQueueIsManualIntervention(queue *storage.QueueItemRecord) bool {
	return queue != nil && (queue.Status == "manual_intervention" || (queue.LastErrorKind != nil && *queue.LastErrorKind == "manual_intervention"))
}

func derefRunOrZero(run *storage.RunRecord) storage.RunRecord {
	if run == nil {
		return storage.RunRecord{}
	}
	return *run
}

func marshalJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func stringPtr(value string) *string {
	return &value
}

func coalesceString(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func newRuntimeEventID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("runtime_%d", time.Now().UTC().UnixNano())
	}
	return "runtime_" + hex.EncodeToString(raw)
}
