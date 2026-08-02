package webui

import (
	"context"
	"encoding/json"
	"time"

	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// Loader reads one render's worth of durable state.
type Loader func(context.Context) Input

// NewRepositoryLoader reads triage input straight from durable state. Every
// source degrades on its own: a source that fails narrows the board and leaves
// a notice on the page rather than failing the request, because an operator
// looking at a half-broken daemon still needs the half that works.
func NewRepositoryLoader(repos *storage.Repositories, collector *escalator.Collector, links escalator.Linker, now func() time.Time) Loader {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context) Input {
		input := Input{Now: now().UTC(), Links: links}
		if repos == nil {
			input.Notices = append(input.Notices, "Storage is not available.")
			return input
		}

		if repos.PullRequestSnapshots != nil {
			snapshots, err := repos.PullRequestSnapshots.List(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Pull request snapshots could not be read.")
			} else {
				input.Snapshots = snapshots
			}
		}
		if repos.Events != nil {
			reports, err := loadGateReports(ctx, repos.Events)
			if err != nil {
				input.Notices = append(input.Notices, "Merge gate reports could not be read.")
			} else {
				input.Reports = reports
			}
		}
		if repos.Loops != nil {
			loops, err := repos.Loops.List(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Loops could not be read.")
			} else {
				input.Loops = loops
			}
		}
		if repos.Queue != nil {
			queue, err := repos.Queue.List(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Queue items could not be read.")
			} else {
				input.Queue = queue
			}
		}
		if collector != nil {
			snapshot, err := collector.Collect(ctx)
			if err != nil {
				input.Notices = append(input.Notices, "Escalator digest could not be collected.")
			} else {
				input.Escalator = snapshot
			}
		}
		return input
	}
}

// loadGateReports projects the durable merge-gate events. Gatekeeper appends one
// report per evaluation, so the newest per pull request is the current verdict;
// Classify does that reduction because it also has to reconcile it with the
// snapshot.
func loadGateReports(ctx context.Context, events *storage.EventsRepository) ([]gatekeeper.Report, error) {
	records, err := events.ListByEntityTypeAndEventTypes(ctx, "pull_request", []string{gatekeeper.GateReportEventType})
	if err != nil {
		return nil, err
	}
	reports := make([]gatekeeper.Report, 0, len(records))
	for _, record := range records {
		var report gatekeeper.Report
		if json.Unmarshal([]byte(record.PayloadJSON), &report) != nil {
			// A payload this surface cannot read is one report missing from the
			// board, not a reason to drop every other report with it.
			continue
		}
		reports = append(reports, report)
	}
	return reports, nil
}
