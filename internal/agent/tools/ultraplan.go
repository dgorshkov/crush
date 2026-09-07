package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ultraplan"
)

const UltraplanToolName = "ultraplan"

//go:embed ultraplan.md
var ultraplanDescription string

// UltraplanParams is the tool input: the complete diagram set as it
// now stands, not a delta against the previous round.
type UltraplanParams struct {
	Summary  string             `json:"summary" description:"One or two sentences on what changed in this round and what you want the user to look at"`
	Goal     string             `json:"goal,omitempty" description:"The user's underlying objective, set on the first round"`
	Diagrams []UltraplanDiagram `json:"diagrams" description:"The complete diagram set. Diagrams you leave out are dropped from the plan"`
}

// UltraplanDiagram is one proposed diagram.
type UltraplanDiagram struct {
	ID      string `json:"id" description:"Short stable slug, e.g. \"request-flow\". Reuse it to revise a diagram; use a new one to add a diagram"`
	Title   string `json:"title" description:"Human-readable name shown in the review"`
	Mermaid string `json:"mermaid" description:"Mermaid source on its own, with no Markdown code fence around it"`
	Intent  string `json:"intent,omitempty" description:"What decision this diagram is asking the user to confirm"`
}

// UltraplanResponseMetadata is attached to the tool result so the UI
// can render the plan without re-reading the session.
type UltraplanResponseMetadata struct {
	Status       ultraplan.Status    `json:"status"`
	Round        int                 `json:"round"`
	Summary      string              `json:"summary,omitempty"`
	Diagrams     []ultraplan.Diagram `json:"diagrams"`
	Feedback     string              `json:"feedback,omitempty"`
	Accepted     int                 `json:"accepted"`
	Total        int                 `json:"total"`
	Implementing bool                `json:"implementing,omitempty"`
	Cancelled    bool                `json:"cancelled,omitempty"`
}

// NewUltraplanTool creates the ultraplan tool. It persists the diagram
// set on the session and blocks until the user has ruled on it, so a
// planning session is one tool call per review round.
func NewUltraplanTool(sessions session.Service, reviews ultraplan.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		UltraplanToolName,
		ultraplanDescription,
		func(ctx context.Context, params UltraplanParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for planning")
			}

			current, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			proposed := make([]ultraplan.Diagram, len(params.Diagrams))
			for i, d := range params.Diagrams {
				proposed[i] = ultraplan.Diagram{
					ID:     strings.TrimSpace(d.ID),
					Title:  strings.TrimSpace(d.Title),
					Source: strings.TrimSpace(ultraplan.StripFence(d.Mermaid)),
					Intent: strings.TrimSpace(d.Intent),
				}
			}

			// Validate before touching stored state so a rejected
			// round leaves the last good plan in place.
			if err := ultraplan.ValidateSet(proposed); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(params.Summary) > ultraplan.MaxSummaryLength {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"summary exceeds %d characters (got %d)",
					ultraplan.MaxSummaryLength, len(params.Summary),
				)), nil
			}

			plan := current.Plan
			if plan == nil {
				plan = &ultraplan.Plan{}
			}
			if goal := strings.TrimSpace(params.Goal); goal != "" {
				plan.Goal = goal
			}
			plan.Propose(strings.TrimSpace(params.Summary), proposed)

			if err := sessions.SavePlan(ctx, sessionID, plan); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save plan: %w", err)
			}

			resp, err := reviews.Review(ctx, ultraplan.ReviewRequest{
				SessionID:  sessionID,
				ToolCallID: call.ID,
				Goal:       plan.Goal,
				Summary:    plan.Summary,
				Round:      plan.Round,
				Diagrams:   plan.Diagrams,
			})
			if err != nil {
				if errors.Is(err, ultraplan.ErrCancelled) {
					plan.Abandon()
					if saveErr := sessions.SavePlan(ctx, sessionID, plan); saveErr != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("failed to save plan: %w", saveErr)
					}
					out := fantasy.NewTextErrorResponse(
						"The user ended the planning session without accepting the plan. Stop planning and wait for them.",
					)
					out.StopTurn = true
					return fantasy.WithResponseMetadata(out, planMetadata(plan, "", true)), nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			plan.ApplyResponse(resp)
			if err := sessions.SavePlan(ctx, sessionID, plan); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save plan: %w", err)
			}

			out := fantasy.NewTextResponse(formatReviewOutcome(plan, resp))
			// A plan the user accepted but does not want built yet ends
			// the turn: there is nothing further to do until they say so.
			out.StopTurn = plan.Status == ultraplan.StatusAccepted && !plan.Implementing
			return fantasy.WithResponseMetadata(out, planMetadata(plan, resp.Feedback, false)), nil
		},
	)
}

// planMetadata builds the render payload for the UI.
func planMetadata(plan *ultraplan.Plan, feedback string, cancelled bool) UltraplanResponseMetadata {
	accepted := 0
	for _, d := range plan.Diagrams {
		if d.Accepted() {
			accepted++
		}
	}
	return UltraplanResponseMetadata{
		Status:       plan.Status,
		Round:        plan.Round,
		Summary:      plan.Summary,
		Diagrams:     plan.Diagrams,
		Feedback:     feedback,
		Accepted:     accepted,
		Total:        len(plan.Diagrams),
		Implementing: plan.Implementing,
		Cancelled:    cancelled,
	}
}

// formatReviewOutcome writes the tool result the model reads. It is
// explicit about what to do next, because the difference between "keep
// planning" and "start building" is the whole point of the mode.
func formatReviewOutcome(plan *ultraplan.Plan, resp ultraplan.ReviewResponse) string {
	var b strings.Builder

	if plan.Status == ultraplan.StatusAccepted {
		fmt.Fprintf(&b, "Plan accepted: the user approved all %d diagram(s) after %d round(s).\n", len(plan.Diagrams), plan.Round)
		if edited := editedIDs(plan); len(edited) > 0 {
			fmt.Fprintf(&b, "\nThe user edited these diagrams themselves, so treat their version as authoritative: %s.\n", strings.Join(edited, ", "))
		}
		if resp.Feedback != "" {
			fmt.Fprintf(&b, "\nClosing note from the user: %s\n", resp.Feedback)
		}
		if plan.Implementing {
			b.WriteString("\nThe user asked you to start implementing now. Work to the accepted diagrams; if you find you must depart from them, say so and call ultraplan again rather than quietly diverging.")
		} else {
			b.WriteString("\nThe user does NOT want implementation to start yet. Do not change any files. Acknowledge the accepted plan briefly and stop.")
		}
		return b.String()
	}

	pending := plan.Pending()
	fmt.Fprintf(&b, "Round %d reviewed: %d of %d diagram(s) accepted. The planning session is still open.\n",
		plan.Round, len(plan.Diagrams)-len(pending), len(plan.Diagrams))

	if resp.Feedback != "" {
		fmt.Fprintf(&b, "\nFeedback on the plan as a whole: %s\n", resp.Feedback)
	}

	if edited := editedIDs(plan); len(edited) > 0 {
		fmt.Fprintf(&b, "\nThe user edited these diagrams directly: %s. Their edits are already stored; build on them rather than reverting.\n", strings.Join(edited, ", "))
	}

	b.WriteString("\nStill open:\n")
	for _, d := range pending {
		fmt.Fprintf(&b, "- %s (%s)", d.ID, d.Title)
		switch {
		case d.Problem != "":
			fmt.Fprintf(&b, ": the user's edit does not parse — %s", d.Problem)
		case d.Feedback != "":
			fmt.Fprintf(&b, ": %s", d.Feedback)
		default:
			b.WriteString(": no specific feedback given")
		}
		b.WriteString("\n")
	}

	b.WriteString("\nRevise and call ultraplan again with the complete diagram set, including the diagrams already accepted so they carry over unchanged. Do not start implementing.")
	return b.String()
}

// editedIDs lists the diagrams whose current source came from the user.
func editedIDs(plan *ultraplan.Plan) []string {
	var ids []string
	for _, d := range plan.Diagrams {
		if d.EditedByUser {
			ids = append(ids, d.ID)
		}
	}
	return ids
}
