package dialog

import (
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/stretchr/testify/require"
)

const (
	reviewFlow = "flowchart TD\n    A --> B"
	reviewSeq  = "sequenceDiagram\n    A->>B: hi"
)

// newTestReview builds a review over the given diagrams and returns it
// alongside a flag set when the user abandons the planning session.
// Tests that care about the submitted response install their own
// OnRespond.
func newTestReview(t *testing.T, diagrams []ultraplan.Diagram) (*UltraplanReview, *bool) {
	t.Helper()

	sty := styles.CharmtonePantera()
	review := NewUltraplanReview(&sty, ultraplan.ReviewRequest{
		ID:       "req-1",
		Round:    1,
		Summary:  "first pass",
		Diagrams: diagrams,
	})

	cancelled := false
	review.OnCancel = func() { cancelled = true }
	return review, &cancelled
}

// press sends a key to the review and returns whether it finished.
func press(t *testing.T, r *UltraplanReview, keys string) bool {
	t.Helper()
	done, _ := r.HandleKey(tea.KeyPressMsg{Code: keyCodeFor(keys), Text: textFor(keys)})
	return done
}

// keyCodeFor maps the small set of keys these tests press.
func keyCodeFor(k string) rune {
	switch k {
	case "enter":
		return tea.KeyEnter
	case "esc":
		return tea.KeyEscape
	case "up":
		return tea.KeyUp
	case "down":
		return tea.KeyDown
	case "space":
		return tea.KeySpace
	default:
		return rune(k[0])
	}
}

func textFor(k string) string {
	switch k {
	case "enter", "esc", "up", "down":
		return ""
	case "space":
		return " "
	default:
		return k
	}
}

func twoDiagrams() []ultraplan.Diagram {
	return []ultraplan.Diagram{
		{ID: "flow", Title: "Flow", Kind: "flowchart", Source: reviewFlow, Status: ultraplan.DiagramProposed},
		{ID: "seq", Title: "Sequence", Kind: "sequenceDiagram", Source: reviewSeq, Status: ultraplan.DiagramProposed},
	}
}

func TestReviewSeedsVerdictsFromPriorRounds(t *testing.T) {
	t.Parallel()

	diagrams := twoDiagrams()
	diagrams[0].Status = ultraplan.DiagramAccepted
	review, _ := newTestReview(t, diagrams)

	require.True(t, review.verdicts[0].Accepted, "a diagram accepted last round starts accepted")
	require.False(t, review.verdicts[1].Accepted)
	require.False(t, review.expanded[0], "settled diagrams start collapsed")
	require.True(t, review.expanded[1], "diagrams still needing a decision start open")
	require.Equal(t, 1, review.acceptedCount())
}

func TestReviewAcceptAndSubmitPending(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	// Accept the first, leave the second.
	require.False(t, press(t, review, "a"))
	require.True(t, review.verdicts[0].Accepted)

	// Move to the submit row and send the round back.
	require.False(t, press(t, review, "down"))
	require.False(t, press(t, review, "down"))
	require.Equal(t, review.submitRow(), review.cursor)
	require.True(t, press(t, review, "enter"))

	require.NotNil(t, got)
	require.Equal(t, "req-1", got.RequestID)
	require.Len(t, got.Verdicts, 2)
	require.True(t, got.Verdicts[0].Accepted)
	require.False(t, got.Verdicts[1].Accepted)
	require.False(t, got.StartImplementation, "implementation cannot start on a partly accepted plan")
}

func TestReviewAcceptAllThenImplement(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	require.False(t, press(t, review, "A"))
	require.True(t, review.allAccepted())

	// Submitting an all-accepted plan asks about implementation
	// instead of returning straight away.
	require.False(t, press(t, review, "enter"))
	require.Equal(t, stageImplement, review.stage)
	require.Nil(t, got)

	require.True(t, press(t, review, "y"))
	require.NotNil(t, got)
	require.True(t, got.StartImplementation)
}

func TestReviewDeclineImplementation(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	press(t, review, "A")
	press(t, review, "enter")
	require.True(t, press(t, review, "n"))

	require.NotNil(t, got)
	require.True(t, got.Verdicts[0].Accepted)
	require.False(t, got.StartImplementation)
}

func TestReviewEscapeFromImplementReturnsToTheList(t *testing.T) {
	t.Parallel()

	review, cancelled := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	press(t, review, "A")
	press(t, review, "enter")
	require.Equal(t, stageImplement, review.stage)

	require.False(t, press(t, review, "esc"))
	require.Equal(t, stageList, review.stage)
	require.Nil(t, got, "backing out of the implementation question submits nothing")
	require.False(t, *cancelled, "backing out does not abandon the plan")
}

func TestReviewCommentWithdrawsAcceptance(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())

	press(t, review, "a")
	require.True(t, review.verdicts[0].Accepted)

	press(t, review, "c")
	require.Equal(t, stageNote, review.stage)
	review.noteEditor.SetValue("split this in two")
	press(t, review, "enter")

	require.Equal(t, stageList, review.stage)
	require.Equal(t, "split this in two", review.verdicts[0].Feedback)
	require.False(t, review.verdicts[0].Accepted, "asking for a change withdraws acceptance")
}

func TestReviewDiscardedCommentChangesNothing(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "a")

	press(t, review, "c")
	review.noteEditor.SetValue("never mind")
	press(t, review, "esc")

	require.Equal(t, stageList, review.stage)
	require.Empty(t, review.verdicts[0].Feedback)
	require.True(t, review.verdicts[0].Accepted)
}

func TestReviewPlanFeedback(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	press(t, review, "f")
	require.Equal(t, stageNote, review.stage)
	require.Equal(t, noteTargetPlan, review.noteTarget)
	review.noteEditor.SetValue("add a diagram for the failure path")
	press(t, review, "enter")

	require.Equal(t, "add a diagram for the failure path", review.feedback)

	review.cursor = review.submitRow()
	require.True(t, press(t, review, "enter"))
	require.NotNil(t, got)
	require.Equal(t, "add a diagram for the failure path", got.Feedback)
}

func TestReviewApplyEditWithdrawsAcceptance(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "A")
	require.True(t, review.allAccepted())

	review.ApplyEdit("flow", "```mermaid\nflowchart LR\n    A --> C\n```")

	require.Equal(t, "flowchart LR\n    A --> C", review.Request.Diagrams[0].Source)
	require.True(t, review.Request.Diagrams[0].EditedByUser)
	require.Equal(t, "flowchart", review.Request.Diagrams[0].Kind)
	require.Empty(t, review.Request.Diagrams[0].Problem)
	require.False(t, review.verdicts[0].Accepted, "an edit has to be re-approved")
	require.Equal(t, "flowchart LR\n    A --> C", review.verdicts[0].Source)
}

func TestReviewCannotAcceptAnUnparseableEdit(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	review.ApplyEdit("flow", "flowchart TD\n    A[Start --> B")
	require.Contains(t, review.Request.Diagrams[0].Problem, "unclosed square bracket")

	review.cursor = 0
	press(t, review, "a")
	require.False(t, review.verdicts[0].Accepted, "a broken diagram cannot be accepted")

	press(t, review, "A")
	require.False(t, review.allAccepted(), "accept-all still leaves a broken diagram out")
}

func TestReviewApplyEditIgnoresNoOpAndEmptyEdits(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "A")

	review.ApplyEdit("flow", reviewFlow)
	require.True(t, review.verdicts[0].Accepted, "an unchanged edit is not a change")

	review.ApplyEdit("flow", "   \n  ")
	require.Equal(t, reviewFlow, review.Request.Diagrams[0].Source)

	review.ApplyEdit("nonexistent", "flowchart TD\n    X --> Y")
	require.Equal(t, reviewFlow, review.Request.Diagrams[0].Source)
}

func TestReviewEscapeAbandonsThePlan(t *testing.T) {
	t.Parallel()

	review, cancelled := newTestReview(t, twoDiagrams())
	var got *ultraplan.ReviewResponse
	review.OnRespond = func(resp ultraplan.ReviewResponse) { got = &resp }

	require.True(t, press(t, review, "esc"))
	require.True(t, *cancelled)
	require.Nil(t, got)
}

func TestReviewToggleSourceAndHeightStaysBounded(t *testing.T) {
	t.Parallel()

	long := make([]ultraplan.Diagram, 0, 4)
	for _, id := range []string{"a", "b", "c", "d"} {
		src := "flowchart TD\n"
		for i := range 20 {
			src += "    N" + string(rune('a'+i)) + " --> M\n"
		}
		long = append(long, ultraplan.Diagram{ID: id, Title: "Diagram " + id, Source: src})
	}

	review, _ := newTestReview(t, long)
	require.LessOrEqual(t, review.Height(80), maxReviewHeight)

	// Collapsing a diagram cannot grow the rendered height.
	before := len(review.buildLines(80))
	press(t, review, "space")
	require.Less(t, len(review.buildLines(80)), before)
}

func TestReviewNotePaneHeightMatchesItsDrawing(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	listHeight := review.Height(80)

	press(t, review, "c")
	require.Equal(t, stageNote, review.stage)

	noteHeight := review.Height(80)
	require.Positive(t, noteHeight)
	require.NotEqual(t, listHeight, noteHeight,
		"the note pane must size itself, not report the list's height")
	require.LessOrEqual(t, noteHeight, maxReviewHeight)

	press(t, review, "esc")
	require.Equal(t, listHeight, review.Height(80))
}

func TestReviewCursorStaysInRange(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	for range 10 {
		press(t, review, "down")
	}
	require.Equal(t, review.submitRow(), review.cursor)

	for range 10 {
		press(t, review, "up")
	}
	require.Equal(t, 0, review.cursor)
}

func TestReviewEditKeyIsOfferedOnlyWithAnEditor(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	require.NotContains(t, helpKeys(review.ShortHelp()), "e")

	review.OnEdit = func(string, string, string) tea.Cmd { return nil }
	require.Contains(t, helpKeys(review.ShortHelp()), "e")
}

func helpKeys(bindings []key.Binding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.Help().Key)
	}
	return out
}
