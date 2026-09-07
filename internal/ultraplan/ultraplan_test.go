package ultraplan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	flowA = "flowchart TD\n    A --> B"
	flowB = "flowchart TD\n    A --> C"
	seqA  = "sequenceDiagram\n    A->>B: hi"
)

func TestProposeCarriesAcceptanceForUnchangedDiagrams(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("first pass", []Diagram{
		{ID: "flow", Title: "Flow", Source: flowA},
		{ID: "seq", Title: "Sequence", Source: seqA},
	})
	require.Equal(t, 1, p.Round)
	require.Equal(t, StatusDrafting, p.Status)
	require.Equal(t, "flowchart", p.Diagrams[0].Kind)
	require.False(t, p.AllAccepted())

	// User accepts the flowchart and asks for changes on the sequence.
	p.ApplyResponse(ReviewResponse{Verdicts: []DiagramVerdict{
		{ID: "flow", Accepted: true},
		{ID: "seq", Accepted: false, Feedback: "show the error path"},
	}})
	require.Equal(t, DiagramAccepted, p.Diagrams[0].Status)
	require.Equal(t, DiagramChangesRequested, p.Diagrams[1].Status)
	require.Equal(t, "show the error path", p.Diagrams[1].Feedback)
	require.Equal(t, StatusDrafting, p.Status)

	// Next round leaves the flowchart untouched and reworks the
	// sequence, so only the flowchart keeps its acceptance.
	p.Propose("second pass", []Diagram{
		{ID: "flow", Title: "Flow", Source: flowA},
		{ID: "seq", Title: "Sequence", Source: seqA + "\n    B->>A: error"},
	})
	require.Equal(t, 2, p.Round)
	require.Equal(t, DiagramAccepted, p.Diagrams[0].Status)
	require.Equal(t, DiagramProposed, p.Diagrams[1].Status)
	require.Empty(t, p.Diagrams[1].Feedback)
}

func TestProposeResetsAcceptanceWhenSourceChanges(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	p.ApplyResponse(ReviewResponse{Verdicts: []DiagramVerdict{{ID: "flow", Accepted: true}}})
	require.True(t, p.AllAccepted())

	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowB}})
	require.Equal(t, DiagramProposed, p.Diagrams[0].Status)
	require.False(t, p.AllAccepted())
}

func TestProposeIgnoresWhitespaceOnlyChanges(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	p.ApplyResponse(ReviewResponse{Verdicts: []DiagramVerdict{{ID: "flow", Accepted: true}}})

	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: "\n" + flowA + "   \n"}})
	require.Equal(t, DiagramAccepted, p.Diagrams[0].Status, "trailing whitespace should not revoke acceptance")
}

func TestProposeGrowsAndShrinksTheSet(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	require.Len(t, p.Diagrams, 1)

	p.Propose("", []Diagram{
		{ID: "flow", Title: "Flow", Source: flowA},
		{ID: "seq", Title: "Sequence", Source: seqA},
	})
	require.Len(t, p.Diagrams, 2)

	p.Propose("", []Diagram{{ID: "seq", Title: "Sequence", Source: seqA}})
	require.Len(t, p.Diagrams, 1)
	require.Equal(t, "seq", p.Diagrams[0].ID)
}

func TestApplyResponseAcceptsUserEdits(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	p.ApplyResponse(ReviewResponse{
		Verdicts:            []DiagramVerdict{{ID: "flow", Accepted: true, Source: flowB}},
		StartImplementation: true,
	})

	require.Equal(t, flowB, p.Diagrams[0].Source)
	require.True(t, p.Diagrams[0].EditedByUser)
	require.Equal(t, StatusAccepted, p.Status)
	require.True(t, p.Implementing)
	require.False(t, p.Active())
}

func TestApplyResponseHoldsPlanOpenWhenAnyDiagramIsRejected(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{
		{ID: "flow", Title: "Flow", Source: flowA},
		{ID: "seq", Title: "Sequence", Source: seqA},
	})
	p.ApplyResponse(ReviewResponse{
		Verdicts: []DiagramVerdict{
			{ID: "flow", Accepted: true},
			{ID: "seq", Accepted: false},
		},
		StartImplementation: true,
	})

	require.Equal(t, StatusDrafting, p.Status)
	require.False(t, p.Implementing, "implementation must not start on a partly accepted plan")
	require.True(t, p.Active())
	require.Len(t, p.Pending(), 1)
}

func TestApplyResponseRejectsAnAcceptedButInvalidUserEdit(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	p.ApplyResponse(ReviewResponse{
		Verdicts: []DiagramVerdict{{
			ID:       "flow",
			Accepted: true,
			Source:   "flowchart TD\n    A[Start --> B",
		}},
		StartImplementation: true,
	})

	require.Equal(t, DiagramChangesRequested, p.Diagrams[0].Status)
	require.Contains(t, p.Diagrams[0].Problem, "unclosed square bracket")
	require.Equal(t, StatusDrafting, p.Status)
	require.False(t, p.Implementing)
}

func TestAbandon(t *testing.T) {
	t.Parallel()

	p := &Plan{}
	p.Propose("", []Diagram{{ID: "flow", Title: "Flow", Source: flowA}})
	p.Abandon()
	require.Equal(t, StatusAbandoned, p.Status)
	require.False(t, p.Active())
	require.Len(t, p.Diagrams, 1, "abandoning keeps the diagrams visible")
}

func TestNilPlanIsInert(t *testing.T) {
	t.Parallel()

	var p *Plan
	require.False(t, p.Active())
	require.False(t, p.AllAccepted())
	require.Nil(t, p.Pending())
	_, ok := p.Diagram("flow")
	require.False(t, ok)
}

func TestValidateSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		diagrams []Diagram
		wantErr  string
	}{
		{
			name:    "empty set",
			wantErr: "at least one diagram",
		},
		{
			name:     "missing id",
			diagrams: []Diagram{{Title: "Flow", Source: flowA}},
			wantErr:  `"id" is required`,
		},
		{
			name: "duplicate id",
			diagrams: []Diagram{
				{ID: "flow", Title: "One", Source: flowA},
				{ID: "flow", Title: "Two", Source: seqA},
			},
			wantErr: "duplicate id",
		},
		{
			name:     "missing title",
			diagrams: []Diagram{{ID: "flow", Source: flowA}},
			wantErr:  `"title" is required`,
		},
		{
			name:     "invalid mermaid",
			diagrams: []Diagram{{ID: "flow", Title: "Flow", Source: "flowchart TD\n    A[Start --> B"}},
			wantErr:  "unclosed square bracket",
		},
		{
			name:     "valid",
			diagrams: []Diagram{{ID: "flow", Title: "Flow", Source: flowA}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSet(tt.diagrams)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateSetReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	err := ValidateSet([]Diagram{
		{ID: "", Title: "", Source: flowA},
		{ID: "b", Title: "B", Source: "nope"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "3 diagram problem(s)")
}

func TestValidateSetRejectsOversizedSet(t *testing.T) {
	t.Parallel()

	diagrams := make([]Diagram, MaxDiagrams+1)
	for i := range diagrams {
		diagrams[i] = Diagram{ID: string(rune('a' + i)), Title: "D", Source: flowA}
	}
	err := ValidateSet(diagrams)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at most")
}
