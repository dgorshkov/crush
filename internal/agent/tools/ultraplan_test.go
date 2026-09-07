package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/stretchr/testify/require"
)

const (
	planFlowA = "flowchart TD\n    A --> B"
	planFlowB = "flowchart TD\n    A --> C"
)

// newPlanTestSessions builds a session service over a throwaway
// database. The connection pool behind it is process-global, so tests
// that use it must not run in parallel: another test's cleanup would
// close the connection out from under them.
func newPlanTestSessions(t *testing.T) session.Service {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return session.NewService(db.New(conn), conn)
}

// callUltraplan runs the tool in the background and hands back the
// review the user is expected to answer, so a test can drive one round
// of the loop.
func callUltraplan(
	t *testing.T,
	sessions session.Service,
	reviews ultraplan.Service,
	sessionID string,
	params UltraplanParams,
) (chan fantasy.ToolResponse, ultraplan.ReviewRequest) {
	t.Helper()

	tool := NewUltraplanTool(sessions, reviews)
	input, err := json.Marshal(params)
	require.NoError(t, err)

	events := reviews.Subscribe(t.Context())
	responses := make(chan fantasy.ToolResponse, 1)
	go func() {
		ctx := context.WithValue(t.Context(), SessionIDContextKey, sessionID)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: UltraplanToolName, Input: string(input)})
		require.NoError(t, err)
		responses <- resp
	}()

	select {
	case ev := <-events:
		return responses, ev.Payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the review request")
		return nil, ultraplan.ReviewRequest{}
	}
}

func awaitResponse(t *testing.T, responses chan fantasy.ToolResponse) fantasy.ToolResponse {
	t.Helper()
	select {
	case resp := <-responses:
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the tool to return")
		return fantasy.ToolResponse{}
	}
}

func metadataOf(t *testing.T, resp fantasy.ToolResponse) UltraplanResponseMetadata {
	t.Helper()
	var meta UltraplanResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	return meta
}

func TestUltraplanToolRejectsInvalidDiagramsWithoutAskingTheUser(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	tool := NewUltraplanTool(sessions, reviews)
	input, err := json.Marshal(UltraplanParams{
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: "flowchart TD\n    A[Start --> B"}},
	})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: UltraplanToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "unclosed square bracket")

	// A rejected round must not have touched stored state.
	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Nil(t, fetched.Plan)
}

func TestUltraplanToolStripsCodeFences(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	responses, review := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Goal:     "add a thing",
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: "```mermaid\n" + planFlowA + "\n```"}},
	})
	require.Equal(t, planFlowA, review.Diagrams[0].Source)
	require.Equal(t, "add a thing", review.Goal)
	require.Equal(t, 1, review.Round)

	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts:            []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: true,
	}))
	awaitResponse(t, responses)
}

func TestUltraplanToolFullLoop(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	// Round one: the user asks for changes on the second diagram.
	responses, review := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Goal:    "add a thing",
		Summary: "first pass",
		Diagrams: []UltraplanDiagram{
			{ID: "flow", Title: "Flow", Mermaid: planFlowA},
			{ID: "seq", Title: "Sequence", Mermaid: "sequenceDiagram\n    A->>B: hi"},
		},
	})
	require.Len(t, review.Diagrams, 2)

	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts: []ultraplan.DiagramVerdict{
			{ID: "flow", Accepted: true},
			{ID: "seq", Accepted: false, Feedback: "show the error path"},
		},
		Feedback: "close, but the failure case is missing",
	}))

	resp := awaitResponse(t, responses)
	require.False(t, resp.IsError)
	require.False(t, resp.StopTurn, "an open plan keeps the turn going so the agent can revise")
	require.Contains(t, resp.Content, "still open")
	require.Contains(t, resp.Content, "show the error path")
	require.Contains(t, resp.Content, "close, but the failure case is missing")

	meta := metadataOf(t, resp)
	require.Equal(t, ultraplan.StatusDrafting, meta.Status)
	require.Equal(t, 1, meta.Accepted)
	require.Equal(t, 2, meta.Total)

	// Round two: the agent revises and the user accepts everything,
	// editing one diagram on the way through.
	responses, review = callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Summary: "second pass",
		Diagrams: []UltraplanDiagram{
			{ID: "flow", Title: "Flow", Mermaid: planFlowA},
			{ID: "seq", Title: "Sequence", Mermaid: "sequenceDiagram\n    A->>B: hi\n    B->>A: error"},
		},
	})
	require.Equal(t, 2, review.Round)
	require.Equal(t, ultraplan.DiagramAccepted, review.Diagrams[0].Status, "an unchanged accepted diagram keeps its verdict")
	require.Equal(t, ultraplan.DiagramProposed, review.Diagrams[1].Status)

	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts: []ultraplan.DiagramVerdict{
			{ID: "flow", Accepted: true, Source: planFlowB},
			{ID: "seq", Accepted: true},
		},
		StartImplementation: true,
	}))

	resp = awaitResponse(t, responses)
	require.False(t, resp.IsError)
	require.False(t, resp.StopTurn, "the agent keeps going when the user asked to implement")
	require.Contains(t, resp.Content, "Plan accepted")
	require.Contains(t, resp.Content, "start implementing now")
	require.Contains(t, resp.Content, "edited these diagrams")

	stored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ultraplan.StatusAccepted, stored.Plan.Status)
	require.True(t, stored.Plan.Implementing)
	require.False(t, stored.Plan.Active(), "an accepted plan releases the mode gate")
	require.Equal(t, planFlowB, stored.Plan.Diagrams[0].Source)
	require.True(t, stored.Plan.Diagrams[0].EditedByUser)
}

func TestUltraplanToolStopsTheTurnWhenImplementationIsDeferred(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	responses, _ := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: planFlowA}},
	})
	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts:            []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: false,
	}))

	resp := awaitResponse(t, responses)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "does NOT want implementation to start yet")

	stored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ultraplan.StatusAccepted, stored.Plan.Status)
	require.False(t, stored.Plan.Implementing)
}

func TestUltraplanToolHoldsThePlanOpenOnAnInvalidUserEdit(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	responses, _ := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: planFlowA}},
	})
	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts: []ultraplan.DiagramVerdict{{
			ID:       "flow",
			Accepted: true,
			Source:   "flowchart TD\n    A[Start --> B",
		}},
		StartImplementation: true,
	}))

	resp := awaitResponse(t, responses)
	require.False(t, resp.StopTurn)
	require.Contains(t, resp.Content, "does not parse")

	stored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ultraplan.StatusDrafting, stored.Plan.Status)
	require.False(t, stored.Plan.Implementing)
}

func TestUltraplanToolCancellationAbandonsThePlan(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	responses, _ := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: planFlowA}},
	})
	require.True(t, reviews.Cancel())

	resp := awaitResponse(t, responses)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "without accepting")
	require.True(t, metadataOf(t, resp).Cancelled)

	stored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ultraplan.StatusAbandoned, stored.Plan.Status)
	require.False(t, stored.Plan.Active(), "abandoning a plan releases the mode gate")
}

func TestUltraplanToolDoesNotClobberConcurrentSessionWrites(t *testing.T) {
	sessions := newPlanTestSessions(t)
	reviews := ultraplan.NewService()
	created, err := sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	responses, _ := callUltraplan(t, sessions, reviews, created.ID, UltraplanParams{
		Summary:  "first pass",
		Diagrams: []UltraplanDiagram{{ID: "flow", Title: "Flow", Mermaid: planFlowA}},
	})

	// A review blocks for as long as the user takes, and title
	// generation lands on the same row meanwhile. Saving the whole
	// session struct fetched before the review would roll this back.
	require.NoError(t, sessions.Rename(t.Context(), created.ID, "Add a rate limiter"))
	require.NoError(t, sessions.UpdateTitleAndUsage(t.Context(), created.ID, "Add a rate limiter", 1200, 340, 0.05))

	require.True(t, reviews.Respond(ultraplan.ReviewResponse{
		Verdicts:            []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: true,
	}))
	awaitResponse(t, responses)

	stored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "Add a rate limiter", stored.Title, "the generated title must survive the plan save")
	require.Equal(t, int64(1200), stored.PromptTokens, "usage accounting must survive the plan save")
	require.Equal(t, int64(340), stored.CompletionTokens)
	require.InDelta(t, 0.05, stored.Cost, 1e-9)
	require.Equal(t, ultraplan.StatusAccepted, stored.Plan.Status)
}

func TestUltraplanToolRequiresASession(t *testing.T) {
	tool := NewUltraplanTool(newPlanTestSessions(t), ultraplan.NewService())
	_, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: UltraplanToolName, Input: "{}"})
	require.ErrorContains(t, err, "session ID is required")
}
