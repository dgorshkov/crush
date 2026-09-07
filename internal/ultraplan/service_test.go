package ultraplan

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testRequest() ReviewRequest {
	return ReviewRequest{
		SessionID: "session-1",
		Summary:   "first pass",
		Round:     1,
		Diagrams:  []Diagram{{ID: "flow", Title: "Flow", Source: flowA}},
	}
}

func TestReviewBlocksUntilRespond(t *testing.T) {
	t.Parallel()

	svc := NewService()
	events := svc.Subscribe(t.Context())

	type result struct {
		resp ReviewResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := svc.Review(t.Context(), testRequest())
		done <- result{resp, err}
	}()

	var published ReviewRequest
	select {
	case ev := <-events:
		published = ev.Payload
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the review request")
	}
	require.NotEmpty(t, published.ID)
	require.Equal(t, "session-1", published.SessionID)

	require.True(t, svc.Respond(ReviewResponse{
		Verdicts:            []DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: true,
	}))

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, published.ID, got.resp.RequestID)
		require.True(t, got.resp.Accepted())
		require.True(t, got.resp.StartImplementation)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Review to return")
	}
}

func TestReviewCancel(t *testing.T) {
	t.Parallel()

	svc := NewService()
	events := svc.Subscribe(t.Context())
	notifications := svc.SubscribeNotifications(t.Context())

	errs := make(chan error, 1)
	go func() {
		_, err := svc.Review(t.Context(), testRequest())
		errs <- err
	}()

	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the review request")
	}

	require.True(t, svc.Cancel())

	select {
	case err := <-errs:
		require.ErrorIs(t, err, ErrCancelled)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Review to return")
	}

	select {
	case n := <-notifications:
		require.NotEmpty(t, n.Payload.RequestID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the resolution notification")
	}
}

func TestRespondAndCancelWithNothingPending(t *testing.T) {
	t.Parallel()

	svc := NewService()
	require.False(t, svc.Respond(ReviewResponse{}))
	require.False(t, svc.Cancel())
}

func TestReviewRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	svc := NewService()

	_, err := svc.Review(t.Context(), ReviewRequest{Diagrams: []Diagram{{ID: "a", Title: "A", Source: flowA}}})
	require.ErrorContains(t, err, "session id is required")

	_, err = svc.Review(t.Context(), ReviewRequest{SessionID: "s"})
	require.ErrorContains(t, err, "at least one diagram")

	_, err = svc.Review(t.Context(), ReviewRequest{
		SessionID: "s",
		Diagrams:  []Diagram{{ID: "a", Title: "A", Source: "flowchart TD\n    A[oops --> B"}},
	})
	require.ErrorContains(t, err, "unclosed square bracket")
}

func TestReviewHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	svc := NewService()
	ctx, cancel := context.WithCancel(t.Context())
	events := svc.Subscribe(t.Context())

	errs := make(chan error, 1)
	go func() {
		_, err := svc.Review(ctx, testRequest())
		errs <- err
	}()

	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the review request")
	}
	cancel()

	select {
	case err := <-errs:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Review to return")
	}
}

func TestNormalizeResponseStripsFencesAndTrims(t *testing.T) {
	t.Parallel()

	resp := normalizeResponse(ReviewResponse{
		Feedback: "  needs an error path  ",
		Verdicts: []DiagramVerdict{
			{ID: "flow", Source: "```mermaid\nflowchart TD\n    A --> B\n```", Feedback: "  tidy  "},
		},
	})
	require.Equal(t, "needs an error path", resp.Feedback)
	require.Equal(t, "flowchart TD\n    A --> B", resp.Verdicts[0].Source)
	require.Equal(t, "tidy", resp.Verdicts[0].Feedback)
}

func TestResponseAcceptedRequiresVerdicts(t *testing.T) {
	t.Parallel()

	require.False(t, ReviewResponse{}.Accepted())
	require.True(t, ReviewResponse{Verdicts: []DiagramVerdict{{Accepted: true}}}.Accepted())
	require.False(t, ReviewResponse{Verdicts: []DiagramVerdict{{Accepted: true}, {}}}.Accepted())
}
