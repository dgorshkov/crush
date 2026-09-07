package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/stretchr/testify/require"
)

// stubParams is the empty input for the stub tools used below.
type stubParams struct{}

// newStubTool returns a tool that flips ran when it executes, so a test
// can tell a refusal from a pass-through.
func newStubTool(name string, ran *bool) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"stub",
		func(context.Context, stubParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			*ran = true
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

// newTestSessions builds a session service over a throwaway database.
// The connection pool behind it is process-global, so tests that use it
// must not run in parallel: another test's cleanup would close the
// connection out from under them.
func newTestSessions(t *testing.T) session.Service {
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

// sessionWithPlan creates a session carrying the given plan.
func sessionWithPlan(t *testing.T, sessions session.Service, plan *ultraplan.Plan) session.Session {
	t.Helper()
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.Plan = plan
	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	return saved
}

func draftingPlan() *ultraplan.Plan {
	plan := &ultraplan.Plan{Goal: "add a thing"}
	plan.Propose("first pass", []ultraplan.Diagram{{
		ID: "flow", Title: "Flow", Source: "flowchart TD\n    A --> B",
	}})
	return plan
}

func runGated(t *testing.T, sessions session.Service, sessionID, toolName, input string) (fantasy.ToolResponse, bool) {
	t.Helper()
	ran := false
	gated := wrapToolsWithUltraplanGate([]fantasy.AgentTool{newStubTool(toolName, &ran)}, sessions, false, true)
	require.Len(t, gated, 1)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sessionID)
	resp, err := gated[0].Run(ctx, fantasy.ToolCall{Name: toolName, Input: input})
	require.NoError(t, err)
	return resp, ran
}

func TestUltraplanGateRefusesMutationsWhileDrafting(t *testing.T) {
	sessions := newTestSessions(t)
	sess := sessionWithPlan(t, sessions, draftingPlan())

	for _, name := range []string{
		tools.EditToolName,
		tools.MultiEditToolName,
		tools.WriteToolName,
		tools.DownloadToolName,
		tools.RenameToolName,
		tools.ReplaceSymbolToolName,
	} {
		t.Run(name, func(t *testing.T) {
			resp, ran := runGated(t, sessions, sess.ID, name, "{}")
			require.False(t, ran, "the gated tool must not run")
			require.True(t, resp.IsError)
			require.Contains(t, resp.Content, "Ultraplan")
			require.Contains(t, resp.Content, tools.UltraplanToolName)
		})
	}
}

func TestUltraplanGateAllowsReadOnlyBashWhileDrafting(t *testing.T) {
	sessions := newTestSessions(t)
	sess := sessionWithPlan(t, sessions, draftingPlan())

	resp, ran := runGated(t, sessions, sess.ID, tools.BashToolName, `{"command":"git log --oneline -5"}`)
	require.True(t, ran, "read-only shell commands stay available during planning")
	require.False(t, resp.IsError)
}

func TestUltraplanGateRefusesWritingBashWhileDrafting(t *testing.T) {
	sessions := newTestSessions(t)
	sess := sessionWithPlan(t, sessions, draftingPlan())

	resp, ran := runGated(t, sessions, sess.ID, tools.BashToolName, `{"command":"rm -rf build"}`)
	require.False(t, ran)
	require.True(t, resp.IsError)

	// Chaining a write onto a safe command must not slip through.
	resp, ran = runGated(t, sessions, sess.ID, tools.BashToolName, `{"command":"git log && rm -rf build"}`)
	require.False(t, ran)
	require.True(t, resp.IsError)

	// Unparseable input is refused rather than assumed harmless.
	resp, ran = runGated(t, sessions, sess.ID, tools.BashToolName, `not json`)
	require.False(t, ran)
	require.True(t, resp.IsError)
}

func TestUltraplanGatePassesThroughWithoutAnActivePlan(t *testing.T) {
	sessions := newTestSessions(t)

	accepted := draftingPlan()
	accepted.ApplyResponse(ultraplan.ReviewResponse{
		Verdicts:            []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true}},
		StartImplementation: true,
	})

	for name, plan := range map[string]*ultraplan.Plan{
		"no plan":        nil,
		"accepted plan":  accepted,
		"abandoned plan": abandonedPlan(),
	} {
		t.Run(name, func(t *testing.T) {
			sess := sessionWithPlan(t, sessions, plan)
			_, ran := runGated(t, sessions, sess.ID, tools.EditToolName, "{}")
			require.True(t, ran, "edits are allowed when no plan is being negotiated")
		})
	}
}

func abandonedPlan() *ultraplan.Plan {
	plan := draftingPlan()
	plan.Abandon()
	return plan
}

func TestUltraplanGateSkipsUngatedToolsAndSubAgents(t *testing.T) {
	sessions := newTestSessions(t)
	sess := sessionWithPlan(t, sessions, draftingPlan())

	// A read tool is never wrapped.
	_, ran := runGated(t, sessions, sess.ID, tools.ViewToolName, "{}")
	require.True(t, ran)

	// Sub-agents run read-only tool sets already, so they are not
	// wrapped at all.
	subRan := false
	subTools := wrapToolsWithUltraplanGate(
		[]fantasy.AgentTool{newStubTool(tools.EditToolName, &subRan)}, sessions, true, true,
	)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sess.ID)
	_, err := subTools[0].Run(ctx, fantasy.ToolCall{Name: tools.EditToolName, Input: "{}"})
	require.NoError(t, err)
	require.True(t, subRan)
}

func TestUltraplanGateWithoutASessionID(t *testing.T) {
	sessions := newTestSessions(t)
	_, ran := runGated(t, sessions, "", tools.EditToolName, "{}")
	require.True(t, ran, "a call with no session cannot be in a planning session")
}

func TestUltraplanGateIsNotInstalledWithoutTheTool(t *testing.T) {
	sessions := newTestSessions(t)
	sess := sessionWithPlan(t, sessions, draftingPlan())

	// A non-interactive run has no ultraplan tool in its schema, so
	// gating writes would lock the session with no way out.
	ran := false
	gated := wrapToolsWithUltraplanGate(
		[]fantasy.AgentTool{newStubTool(tools.EditToolName, &ran)}, sessions, false, false,
	)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sess.ID)
	_, err := gated[0].Run(ctx, fantasy.ToolCall{Name: tools.EditToolName, Input: "{}"})
	require.NoError(t, err)
	require.True(t, ran, "without the escape hatch the gate must not be installed")
}

func TestPlanningReadOnlyCommand(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"ls -la",
		"cat internal/agent/ultraplan.go",
		"rg --files-with-matches ultraplan",
		"grep -rn TODO internal",
		"git log --oneline -20",
		"git status",
		"find . -name '*.go'",
		"wc -l internal/ultraplan/mermaid.go",
	}
	for _, cmd := range allowed {
		require.True(t, planningReadOnlyCommand(cmd), "should be allowed: %s", cmd)
	}

	// Everything here reaches the workspace despite a harmless looking
	// first word. The bash tool's own allow-list says yes to several of
	// them, which is exactly why this gate does not reuse it.
	refused := []string{
		"echo pwned > /etc/passwd",
		"cat x >> y",
		"ls > listing.txt",
		"timeout 5 rm -rf build",
		"nice make install",
		"nohup ./deploy.sh",
		"env FOO=1 rm -rf build",
		"kill -9 1",
		"killall node",
		"git log && rm -rf build",
		"git status; rm -rf build",
		"ls | xargs rm",
		"echo $(rm -rf build)",
		"rm -rf build",
		"go build ./...",
		"grepfoo something",
		"",
		"   ",
	}
	for _, cmd := range refused {
		require.False(t, planningReadOnlyCommand(cmd), "should be refused: %s", cmd)
	}
}

func TestUltraplanReminder(t *testing.T) {
	t.Parallel()

	require.Empty(t, ultraplanReminder(nil))

	drafting := ultraplanReminder(draftingPlan())
	require.Contains(t, drafting, "Ultraplan mode is active")
	require.Contains(t, drafting, "add a thing")
	require.Contains(t, drafting, "flow")

	empty := ultraplanReminder(&ultraplan.Plan{Status: ultraplan.StatusDrafting, Goal: "start here"})
	require.Contains(t, empty, "No diagrams proposed yet")

	plan := draftingPlan()
	plan.ApplyResponse(ultraplan.ReviewResponse{
		Verdicts: []ultraplan.DiagramVerdict{{ID: "flow", Accepted: true, Source: "flowchart LR\n    A --> C"}},
	})
	accepted := ultraplanReminder(plan)
	require.Contains(t, accepted, "accepted Ultraplan plan")
	require.Contains(t, accepted, "not yet asked for implementation")
	require.Contains(t, accepted, "edited by the user")

	require.Empty(t, ultraplanReminder(abandonedPlan()))
}
