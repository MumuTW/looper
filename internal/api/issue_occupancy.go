package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// issueOccupant is a live loop already working an issue, whatever role it holds.
type issueOccupant struct {
	LoopID   string
	Seq      int64
	Type     string
	TargetID string
}

// findIssueOccupant reports the loop currently working repo#issueNumber, or nil.
//
// Looper already records this: a loop whose status is conflicting-active *is*
// the claim, and syncIssueClaim projects it onto the issue as a comment. What was
// missing is anyone asking. Dispatch decisions were made by looking at open pull
// requests instead, which only shows work that has already produced something —
// so an issue could look free for the entire time a worker was busy on it.
//
// The scan crosses roles, but not every role is a collision. Planner is the
// upstream of a worker — it writes the spec the worker implements, so a live
// planner loop is a handoff, not an occupant. Fixer and reviewer are collisions:
// they are already working the pull request that answers this issue, and adding
// a worker means two runs producing the same change.
//
// Limitation worth stating plainly: this sees only Looper's own loops. Work done
// by a person, or by another tool, is invisible here — for that, the open pull
// requests on the forge remain the only signal.
func findIssueOccupant(loops []storage.LoopRecord, projectID, repo string, issueNumber int64, excludeLoopID string) *issueOccupant {
	if issueNumber <= 0 || strings.TrimSpace(repo) == "" {
		return nil
	}
	targetKey := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
	for _, loop := range loops {
		if loop.ProjectID != projectID || loop.ID == excludeLoopID {
			continue
		}
		if !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(loop.Status)) {
			continue
		}
		if !occupyingLoopType(loop.Type) {
			continue
		}
		if !loopDeclaresIssue(loop, targetKey, issueNumber) {
			continue
		}
		occupant := issueOccupant{LoopID: loop.ID, Seq: loop.Seq, Type: loop.Type, TargetID: derefString(loop.TargetID)}
		return &occupant
	}
	return nil
}

// occupyingLoopType answers which roles working an issue conflict with starting
// a worker on it. Worker is absent because a second worker for the same issue is
// handled upstream by reuse, which resumes the existing run rather than adding one.
func occupyingLoopType(loopType string) bool {
	switch domain.LoopType(loopType) {
	case domain.LoopTypeFixer, domain.LoopTypeReviewer:
		return true
	default:
		return false
	}
}

// loopDeclaresIssue matches both ways a loop can name an issue: as its target,
// or — for a worker that has since retargeted onto its pull request — in the
// worker metadata it carries.
func loopDeclaresIssue(loop storage.LoopRecord, targetKey string, issueNumber int64) bool {
	if loop.TargetType == string(domain.LoopTargetTypeIssue) && derefString(loop.TargetID) == targetKey {
		return true
	}
	metadata := parseProjectMetadata(loop.MetadataJSON)
	worker, _ := metadata["worker"].(map[string]any)
	if worker == nil {
		return false
	}
	declared := int64MetadataPtr(worker, "issueNumber")
	return declared != nil && *declared == issueNumber
}

func issueOccupiedError(repo string, issueNumber int64, occupant issueOccupant) error {
	return apiError{
		code:   pkgapi.ErrorCodeLoopConflict,
		status: http.StatusConflict,
		message: fmt.Sprintf(
			"%s#%d is already being worked by %s loop %s (seq %d); pass force to dispatch anyway",
			repo, issueNumber, occupant.Type, occupant.LoopID, occupant.Seq,
		),
	}
}
