package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ultraplan"
)

// gatedTools are the tools refused while a planning session is open.
// The list is deliberately about changing the workspace, not about
// spending tokens: reading, searching and asking are all still fair
// game during planning, and are most of what good planning is.
var gatedTools = []string{
	tools.EditToolName,
	tools.MultiEditToolName,
	tools.WriteToolName,
	tools.DownloadToolName,
	tools.RenameToolName,
	tools.ReplaceSymbolToolName,
}

// ultraplanGate wraps a tool and refuses it while the session has a
// plan still under negotiation.
//
// Refusing here rather than dropping the tool from the schema is
// deliberate: Ultraplan mode turns on and off mid-session as plans open
// and close, and the tool set is built once per agent. A refusal also
// tells the model why, which a missing tool does not.
type ultraplanGate struct {
	inner    fantasy.AgentTool
	sessions session.Service
}

// wrapToolsWithUltraplanGate wraps the tools that change the workspace
// so they are refused during a planning session. Sub-agents are left
// alone: they run read-only tool sets of their own.
func wrapToolsWithUltraplanGate(agentTools []fantasy.AgentTool, sessions session.Service, isSubAgent bool) []fantasy.AgentTool {
	if sessions == nil || isSubAgent {
		return agentTools
	}
	out := make([]fantasy.AgentTool, len(agentTools))
	for i, tool := range agentTools {
		if isGatedTool(tool.Info().Name) {
			out[i] = &ultraplanGate{inner: tool, sessions: sessions}
			continue
		}
		out[i] = tool
	}
	return out
}

// isGatedTool reports whether a tool changes the workspace and so is
// refused during planning.
func isGatedTool(name string) bool {
	return name == tools.BashToolName || slices.Contains(gatedTools, name)
}

func (g *ultraplanGate) Info() fantasy.ToolInfo { return g.inner.Info() }

func (g *ultraplanGate) ProviderOptions() fantasy.ProviderOptions { return g.inner.ProviderOptions() }

func (g *ultraplanGate) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}

func (g *ultraplanGate) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if !g.planActive(ctx) {
		return g.inner.Run(ctx, call)
	}

	// Read-only shell commands are how an agent inspects a codebase, so
	// they stay available while planning.
	if call.Name == tools.BashToolName && bashCallIsReadOnly(call.Input) {
		return g.inner.Run(ctx, call)
	}

	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"%q is unavailable: this session has an open Ultraplan planning session, so nothing in the workspace may change yet. "+
			"Keep reading, searching and asking as needed, then call the %q tool to put the diagram set in front of the user. "+
			"Files can be changed once they have accepted the plan and asked you to start implementing.",
		call.Name, tools.UltraplanToolName,
	)), nil
}

// planActive reports whether the session on the context has a plan
// still being negotiated. A lookup failure never blocks the tool: a
// planning session is not worth breaking an agent over.
func (g *ultraplanGate) planActive(ctx context.Context) bool {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return false
	}
	current, err := g.sessions.Get(ctx, sessionID)
	if err != nil {
		slog.Debug("Failed to read session for the Ultraplan gate", "session_id", sessionID, "error", err)
		return false
	}
	return current.Plan.Active()
}

// bashCallIsReadOnly reports whether a bash tool call runs one of the
// known read-only commands. Input we cannot parse is treated as not
// read-only.
func bashCallIsReadOnly(input string) bool {
	var params tools.BashParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return false
	}
	return tools.IsSafeReadOnlyCommand(params.Command)
}

// ultraplanReminder returns the system reminder describing the state of
// the session's plan, or an empty string when there is nothing to say.
//
// The plan lives in the database rather than the transcript, so without
// this the model would have to infer the mode from its own earlier tool
// calls, which it does badly across a summarization boundary.
func ultraplanReminder(plan *ultraplan.Plan) string {
	switch {
	case plan.Active():
		var b strings.Builder
		b.WriteString("Ultraplan mode is active for this session. You are planning, not building: ")
		b.WriteString("tools that change the workspace are refused until the user accepts the plan and asks you to start.\n\n")
		if plan.Goal != "" {
			fmt.Fprintf(&b, "Goal: %s\n", plan.Goal)
		}
		if len(plan.Diagrams) == 0 {
			b.WriteString("\nNo diagrams proposed yet. Read whatever you need, then call the \"ultraplan\" tool with a first diagram set.")
			return b.String()
		}
		fmt.Fprintf(&b, "\nDiagram set after round %d:\n", plan.Round)
		for _, d := range plan.Diagrams {
			fmt.Fprintf(&b, "- %s (%s): %s", d.ID, d.Title, d.Status)
			switch {
			case d.Problem != "":
				fmt.Fprintf(&b, " — does not parse: %s", d.Problem)
			case d.Feedback != "":
				fmt.Fprintf(&b, " — %s", d.Feedback)
			}
			b.WriteString("\n")
		}
		b.WriteString("\nCall \"ultraplan\" with the complete set, including diagrams already accepted, to continue the review.")
		return b.String()

	case plan != nil && plan.Status == ultraplan.StatusAccepted:
		var b strings.Builder
		b.WriteString("This session has an accepted Ultraplan plan. Treat these diagrams as the agreed design")
		if !plan.Implementing {
			b.WriteString(", and note the user has not yet asked for implementation to start")
		}
		b.WriteString(":\n")
		for _, d := range plan.Diagrams {
			fmt.Fprintf(&b, "- %s (%s)", d.ID, d.Title)
			if d.EditedByUser {
				b.WriteString(" — edited by the user, so their version is authoritative")
			}
			b.WriteString("\n")
		}
		b.WriteString("\nIf the work needs to depart from the accepted diagrams, say so and call \"ultraplan\" again rather than diverging quietly.")
		return b.String()

	default:
		return ""
	}
}
