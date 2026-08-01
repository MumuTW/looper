package api

import (
	"fmt"
	"net/http"

	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

// upgradeDrainRuntime is deliberately narrower than RuntimeState. Upgrade
// drain owns no second lifecycle state: it asks Runtime Admission to close new
// work and reads the Supervisor-owned work set that Runtime exposes.
type upgradeDrainRuntime interface {
	BeginDrain(reason string) error
	AdmissionState() looperdruntime.AdmissionState
	DrainSnapshot() looperdruntime.DrainSnapshot
}

type upgradeDrainResponse struct {
	AdmissionState string                       `json:"admissionState"`
	Snapshot       looperdruntime.DrainSnapshot `json:"snapshot"`
	Drained        bool                         `json:"drained"`
}

func (h *Handler) upgradeDrainStatus(begin bool) (upgradeDrainResponse, error) {
	if h == nil || h.context.Runtime == nil {
		return upgradeDrainResponse{}, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Upgrade drain is not available on this daemon"}
	}
	rt, ok := any(h.context.Runtime).(upgradeDrainRuntime)
	if !ok {
		return upgradeDrainResponse{}, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Upgrade drain is not available on this daemon"}
	}
	if begin {
		if err := rt.BeginDrain("upgrade"); err != nil {
			return upgradeDrainResponse{}, apiError{code: pkgapi.ErrorCodeServiceUnavailable, status: http.StatusServiceUnavailable, message: fmt.Sprintf("begin upgrade drain: %v", err)}
		}
	}
	snapshot := rt.DrainSnapshot()
	return upgradeDrainResponse{AdmissionState: string(rt.AdmissionState()), Snapshot: snapshot, Drained: snapshot.Drained()}, nil
}
