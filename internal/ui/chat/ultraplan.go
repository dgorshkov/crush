package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/charmbracelet/x/ansi"
)

// -----------------------------------------------------------------------------
// Ultraplan Tool
// -----------------------------------------------------------------------------

// UltraplanToolMessageItem renders an ultraplan tool call in the
// transcript.
type UltraplanToolMessageItem struct {
	*baseToolMessageItem
}

var _ ToolMessageItem = (*UltraplanToolMessageItem)(nil)

// NewUltraplanToolMessageItem creates a new [UltraplanToolMessageItem].
func NewUltraplanToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &UltraplanToolRenderContext{}, canceled)
}

// UltraplanToolRenderContext renders ultraplan tool messages.
type UltraplanToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (u *UltraplanToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Plan", opts.Anim, opts.Compact)
	}

	var params tools.UltraplanParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	headerText := fmt.Sprintf("%d diagram(s)", len(params.Diagrams))
	var body string

	if opts.HasResult() && opts.Result.Metadata != "" {
		var meta tools.UltraplanResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
			headerText = ultraplanHeader(sty, meta)
			body = formatPlanDiagrams(sty, meta.Diagrams, cappedWidth)
			if meta.Feedback != "" {
				body = joinToolParts(body, sty.Tool.TodoStatusNote.Render(
					ansi.Truncate("note: "+meta.Feedback, cappedWidth, "…"),
				))
			}
		}
	}

	header := toolHeader(sty, opts.Status, "Plan", cappedWidth, opts, headerText)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if body == "" {
		return header
	}
	return joinToolParts(header, sty.Tool.Body.Render(body))
}

// ultraplanHeader summarizes the outcome of one review round.
func ultraplanHeader(sty *styles.Styles, meta tools.UltraplanResponseMetadata) string {
	ratio := sty.Tool.TodoRatio.Render(fmt.Sprintf("%d/%d", meta.Accepted, meta.Total))
	switch {
	case meta.Cancelled:
		return ratio + sty.Tool.TodoStatusNote.Render(" · planning cancelled")
	case meta.Status == ultraplan.StatusAccepted && meta.Implementing:
		return ratio + sty.Tool.TodoStatusNote.Render(" · plan accepted, implementing")
	case meta.Status == ultraplan.StatusAccepted:
		return ratio + sty.Tool.TodoStatusNote.Render(" · plan accepted, on hold")
	default:
		return ratio + sty.Tool.TodoStatusNote.Render(fmt.Sprintf(" · round %d, changes requested", meta.Round))
	}
}

// formatPlanDiagrams renders one line per diagram with its verdict.
func formatPlanDiagrams(sty *styles.Styles, diagrams []ultraplan.Diagram, width int) string {
	if len(diagrams) == 0 {
		return ""
	}
	lines := make([]string, 0, len(diagrams))
	for _, d := range diagrams {
		var prefix string
		switch d.Status {
		case ultraplan.DiagramAccepted:
			prefix = sty.Tool.TodoCompletedIcon.Render(styles.TodoCompletedIcon) + " "
		default:
			prefix = sty.Tool.TodoPendingIcon.Render(styles.TodoPendingIcon) + " "
		}

		text := d.Title
		if d.Kind != "" {
			text += " · " + d.Kind
		}
		if d.EditedByUser {
			text += " · edited by you"
		}
		switch {
		case d.Problem != "":
			text += " — does not parse"
		case d.Feedback != "":
			text += " — " + d.Feedback
		}
		lines = append(lines, ansi.Truncate(prefix+sty.Tool.TodoItem.Render(text), width, "…"))
	}
	return strings.Join(lines, "\n")
}
