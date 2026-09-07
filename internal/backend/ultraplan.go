package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/ultraplan"
)

// RespondUltraplanReview submits a completed diagram review. The
// returned bool reports whether this call resolved the pending review
// (true) or found it already resolved by another caller (false).
func (b *Backend) RespondUltraplanReview(workspaceID string, req proto.UltraplanReviewResponse) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}

	verdicts := make([]ultraplan.DiagramVerdict, len(req.Verdicts))
	for i, v := range req.Verdicts {
		verdicts[i] = ultraplan.DiagramVerdict{
			ID:       v.ID,
			Accepted: v.Accepted,
			Source:   v.Source,
			Feedback: v.Feedback,
		}
	}

	return ws.Reviews.Respond(ultraplan.ReviewResponse{
		RequestID:           req.RequestID,
		Verdicts:            verdicts,
		Feedback:            req.Feedback,
		StartImplementation: req.StartImplementation,
	}), nil
}

// CancelUltraplanReview abandons the pending review for a workspace.
// Returns true if a review was pending, false otherwise.
func (b *Backend) CancelUltraplanReview(workspaceID string) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	return ws.Reviews.Cancel(), nil
}

// StartUltraplan opens a planning session on a session, recording the
// goal the user wants planned. It is a no-op that reports false when a
// plan is already being negotiated.
func (b *Backend) StartUltraplan(ctx context.Context, workspaceID string, req proto.UltraplanStartRequest) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	if req.SessionID == "" {
		return false, fmt.Errorf("session id is required")
	}

	current, err := ws.Sessions.Get(ctx, req.SessionID)
	if err != nil {
		return false, err
	}
	if current.Plan.Active() {
		return false, nil
	}

	plan := &ultraplan.Plan{
		Status: ultraplan.StatusDrafting,
		Goal:   strings.TrimSpace(req.Goal),
	}
	if err := ws.Sessions.SavePlan(ctx, req.SessionID, plan); err != nil {
		return false, err
	}
	return true, nil
}

// AbandonUltraplan ends the planning session on a session without
// accepting the plan, releasing the mode gate. It reports false when
// no planning session was open.
func (b *Backend) AbandonUltraplan(ctx context.Context, workspaceID string, req proto.UltraplanAbandonRequest) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	if req.SessionID == "" {
		return false, fmt.Errorf("session id is required")
	}

	current, err := ws.Sessions.Get(ctx, req.SessionID)
	if err != nil {
		return false, err
	}
	if !current.Plan.Active() {
		return false, nil
	}

	// A review in flight has to be released too, or the agent stays
	// blocked on a plan that no longer exists.
	ws.Reviews.Cancel()

	current.Plan.Abandon()
	if err := ws.Sessions.SavePlan(ctx, req.SessionID, current.Plan); err != nil {
		return false, err
	}
	return true, nil
}
