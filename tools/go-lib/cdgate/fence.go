// Release fences: a deploy step runs only for the current release.
package cdgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdrelease"
)

// DispatchIDVariable is the pipeline variable a release dispatch carries.
const DispatchIDVariable = "PETREL_CD_DISPATCH_ID"

// ReleaseFenceExitCode is the CD skip exit code the CD wrapper script defines
// as CD_SKIP_EXIT_CODE. A wrapped step that exits with it ends skipped.
const ReleaseFenceExitCode = 79

// ReasonNotNewestRelease is the skip reason of a publish fenced out by a newer
// release.
const ReasonNotNewestRelease = "not-newest-release"

// ReasonNotCurrentRelease is the skip reason of a Lease-taking step fenced out
// because its pipeline is not the store's current release.
const ReasonNotCurrentRelease = "not-current-release"

// ErrNotCurrent means the caller's pipeline is not the claimed release: no
// release is claimed, or the claim names another pipeline or dispatch. The
// caller is fenced out and must neither write nor deploy.
var ErrNotCurrent = errors.New("pipeline is not the current release")

// Heartbeat stamps the current release with now. It refuses with ErrNotCurrent
// unless the release's pipeline and dispatch id both match. It returns a new
// state and never changes s.
func Heartbeat(s cdrelease.State, pipeline int64, dispatchID string, now time.Time) (cdrelease.State, error) {
	r := s.Release
	if r == nil || dispatchID == "" || r.Pipeline != pipeline || r.DispatchID != dispatchID {
		return s, ErrNotCurrent
	}
	next := *r
	next.Heartbeat = now.UTC().Format(time.RFC3339)
	s.Release = &next
	return s, nil
}

// ReleaseFence checks the queue store before a Lease-taking deploy. It proceeds
// only when the store's release is this pipeline and dispatch, and then writes
// the release heartbeat as one CAS. A fenced-out step writes nothing.
func ReleaseFence(store cdrelease.Store, pipeline int64, dispatchID string, now func() time.Time) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		_, err := store.Update(ctx, func(s cdrelease.State) (cdrelease.State, error) {
			return Heartbeat(s, pipeline, dispatchID, now())
		})
		if errors.Is(err, ErrNotCurrent) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("release heartbeat: %w", err)
		}
		return true, nil
	}
}

// RunHeartbeat is the heartbeat-only mode: one fence check and, when the
// pipeline is the current release, one heartbeat CAS. cd-release-heartbeat runs it
// before any leg. It deploys nothing and takes no Lease. A pipeline
// that is not the current release exits 0 and writes nothing; a store error
// exits 1.
func RunHeartbeat(ctx context.Context, fence func(context.Context) (bool, error), log io.Writer) int {
	current, err := fence(ctx)
	if err != nil {
		logf(log, "cd-release: heartbeat failed: %v", err)
		return 1
	}
	if !current {
		logf(log, "cd-release: not the current release; no heartbeat")
		return 0
	}
	logf(log, "cd-release: heartbeat written")
	return 0
}
