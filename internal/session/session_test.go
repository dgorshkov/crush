package session

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}

func TestPlanRoundTrips(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	require.Nil(t, created.Plan, "a fresh session has no plan")

	plan := &ultraplan.Plan{Goal: "add ultraplan mode"}
	plan.Propose("first pass", []ultraplan.Diagram{{
		ID:     "flow",
		Title:  "Review loop",
		Source: "flowchart TD\n    A --> B",
	}})
	created.Plan = plan

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.NotNil(t, saved.Plan)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched.Plan)
	require.Equal(t, "add ultraplan mode", fetched.Plan.Goal)
	require.Equal(t, ultraplan.StatusDrafting, fetched.Plan.Status)
	require.True(t, fetched.Plan.Active())
	require.Len(t, fetched.Plan.Diagrams, 1)
	require.Equal(t, "flowchart", fetched.Plan.Diagrams[0].Kind)

	fetched.Plan.ApplyResponse(ultraplan.ReviewResponse{
		Verdicts:            []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: true,
	})
	_, err = sessions.Save(t.Context(), fetched)
	require.NoError(t, err)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ultraplan.StatusAccepted, refetched.Plan.Status)
	require.True(t, refetched.Plan.Implementing)
	require.False(t, refetched.Plan.Active())
}
