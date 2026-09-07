package ultraplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// ErrCancelled is returned by Review when the user abandons the
// planning session instead of ruling on the diagrams.
var ErrCancelled = errors.New("planning session cancelled by user")

// ErrReviewPending is returned by Review when another review is
// already in front of the user. Only one can be pending at a time,
// because a review owns the editor area until it resolves.
var ErrReviewPending = errors.New("a plan review is already awaiting a response")

// ReviewRequest is the envelope published to the UI when the agent
// puts a diagram set in front of the user.
type ReviewRequest struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`

	// Goal is the user's original request, shown as context.
	Goal string `json:"goal,omitempty"`

	// Summary is the agent's framing of this round.
	Summary string `json:"summary,omitempty"`

	// Round is the review round, counting from one.
	Round int `json:"round"`

	// Diagrams is the set under review, carrying the verdicts already
	// recorded in earlier rounds.
	Diagrams []Diagram `json:"diagrams"`
}

// DiagramVerdict is the user's ruling on one diagram.
type DiagramVerdict struct {
	ID string `json:"id"`

	// Accepted reports whether the user accepted the diagram as it now
	// stands, including any edit they made in this pass.
	Accepted bool `json:"accepted"`

	// Source carries the user's edited Mermaid source. Empty means
	// they left the agent's source alone.
	Source string `json:"source,omitempty"`

	// Feedback is what the user wants changed about this diagram.
	Feedback string `json:"feedback,omitempty"`
}

// ReviewResponse is the result of one review round.
type ReviewResponse struct {
	RequestID string           `json:"request_id"`
	Verdicts  []DiagramVerdict `json:"verdicts"`

	// Feedback is the user's remark on the plan as a whole, including
	// any request for a diagram that does not exist yet.
	Feedback string `json:"feedback,omitempty"`

	// StartImplementation is the user's answer to the question asked
	// once every diagram is accepted. It is meaningless when the plan
	// is not fully accepted.
	StartImplementation bool `json:"start_implementation"`
}

// Accepted reports whether the user accepted every diagram in the
// round. An empty verdict list is never an acceptance.
func (r ReviewResponse) Accepted() bool {
	if len(r.Verdicts) == 0 {
		return false
	}
	for _, v := range r.Verdicts {
		if !v.Accepted {
			return false
		}
	}
	return true
}

// Validate checks a request before it reaches the UI.
func (r ReviewRequest) Validate() error {
	if r.SessionID == "" {
		return fmt.Errorf("session id is required")
	}
	if len(r.Diagrams) == 0 {
		return fmt.Errorf("a review needs at least one diagram")
	}
	if len(r.Summary) > MaxSummaryLength {
		return fmt.Errorf("summary exceeds %d characters (got %d)", MaxSummaryLength, len(r.Summary))
	}
	return ValidateSet(r.Diagrams)
}

// Notification is published when a review is resolved so clients that
// did not answer can dismiss their open forms.
type Notification struct {
	RequestID string `json:"request_id"`
}

// Service manages the lifecycle of diagram reviews. Only one review
// can be pending at a time, because the tool blocks until the user
// rules on it.
type Service interface {
	pubsub.Subscriber[ReviewRequest]

	// SubscribeNotifications returns a channel of review resolution
	// notifications.
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[Notification]

	// Review publishes a diagram set and blocks until the user
	// responds or the context is cancelled. It returns
	// ErrReviewPending when another review is already awaiting a
	// response.
	Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error)

	// Respond resolves the pending review. It reports false when no
	// review is pending, or when the response carries a request ID
	// that is not the pending one.
	Respond(resp ReviewResponse) bool

	// Cancel abandons the pending review. It reports false when no
	// review is pending.
	Cancel() bool
}

type service struct {
	broker             *pubsub.Broker[ReviewRequest]
	notificationBroker *pubsub.Broker[Notification]

	mu        sync.Mutex
	pending   chan ReviewResponse
	cancelled chan struct{}
	pendingID string
}

// NewService creates a review service.
func NewService() Service {
	return &service{
		broker:             pubsub.NewBroker[ReviewRequest](),
		notificationBroker: pubsub.NewBroker[Notification](),
	}
}

// Subscribe returns a channel of review requests.
func (s *service) Subscribe(ctx context.Context) <-chan pubsub.Event[ReviewRequest] {
	return s.broker.Subscribe(ctx)
}

// SubscribeNotifications returns a channel of review resolutions.
func (s *service) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[Notification] {
	return s.notificationBroker.Subscribe(ctx)
}

// Review publishes a diagram set and blocks until the user responds.
func (s *service) Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	if req.ID == "" {
		req.ID = uuid.New().String()
	}
	if err := req.Validate(); err != nil {
		return ReviewResponse{}, err
	}

	s.mu.Lock()
	// Claiming the slot under the lock keeps a second reviewer from
	// stranding the first: without it the newcomer would overwrite the
	// channels the incumbent is blocked on, and the incumbent's
	// deferred cleanup would then wipe the newcomer's state.
	if s.pending != nil {
		s.mu.Unlock()
		return ReviewResponse{}, ErrReviewPending
	}
	pending := make(chan ReviewResponse, 1)
	cancelled := make(chan struct{})
	s.pending = pending
	s.cancelled = cancelled
	s.pendingID = req.ID
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only clear state this review still owns. Respond and Cancel
		// release the slot themselves, so by the time we get here a
		// later review may legitimately hold it.
		if s.pendingID == req.ID {
			s.pending = nil
			s.cancelled = nil
			s.pendingID = ""
		}
		s.mu.Unlock()
	}()

	s.broker.Publish(pubsub.CreatedEvent, req)

	// Read from the locals rather than the fields: the fields are
	// cleared by the deferred cleanup, and reading them here would
	// race with it.
	select {
	case <-ctx.Done():
		return ReviewResponse{}, ctx.Err()
	case <-cancelled:
		return ReviewResponse{}, ErrCancelled
	case resp := <-pending:
		resp.RequestID = req.ID
		return normalizeResponse(resp), nil
	}
}

// Respond resolves the pending review. A response carrying a request
// ID is only applied to that review, so a reply the user submitted
// against an earlier round cannot land on the current diagram set.
func (s *service) Respond(resp ReviewResponse) bool {
	s.mu.Lock()
	ch := s.pending
	requestID := s.pendingID
	if ch == nil || (resp.RequestID != "" && resp.RequestID != requestID) {
		s.mu.Unlock()
		return false
	}
	// Release the whole slot under the same lock that claimed it:
	// whoever resolves a review first wins, and a later Respond or
	// Cancel finds nothing pending.
	s.pending = nil
	s.cancelled = nil
	s.pendingID = ""
	s.mu.Unlock()

	ch <- resp

	if requestID != "" {
		s.notificationBroker.Publish(pubsub.CreatedEvent, Notification{RequestID: requestID})
	}
	return true
}

// Cancel abandons the pending review. It is safe to call from more
// than one place at once: ending a planning session cancels any review
// in flight, and the user may be pressing escape on that same review.
func (s *service) Cancel() bool {
	s.mu.Lock()
	cancelCh := s.cancelled
	requestID := s.pendingID
	if cancelCh == nil {
		s.mu.Unlock()
		return false
	}
	// Release the whole slot and close under the lock. Closing outside
	// it lets two callers reach the same channel and panic on the
	// second close.
	s.pending = nil
	s.cancelled = nil
	s.pendingID = ""
	close(cancelCh)
	s.mu.Unlock()

	if requestID != "" {
		s.notificationBroker.Publish(pubsub.CreatedEvent, Notification{RequestID: requestID})
	}
	return true
}

// normalizeResponse trims user-supplied text and strips a code fence
// from any edited source, so a diagram pasted back with its Markdown
// fence still validates.
func normalizeResponse(resp ReviewResponse) ReviewResponse {
	resp.Feedback = strings.TrimSpace(resp.Feedback)
	for i := range resp.Verdicts {
		resp.Verdicts[i].Feedback = strings.TrimSpace(resp.Verdicts[i].Feedback)
		if src := resp.Verdicts[i].Source; src != "" {
			resp.Verdicts[i].Source = strings.TrimSpace(StripFence(src))
		}
	}
	return resp
}
