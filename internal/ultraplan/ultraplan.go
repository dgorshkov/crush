// Package ultraplan implements Ultraplan mode: a planning session in
// which the agent and the user align on a set of Mermaid diagrams
// before any code is written.
//
// The agent proposes a diagram set. The user reviews it, editing
// diagram sources directly or replying in natural language. The
// session ends successfully only when every diagram parses as valid
// Mermaid and the user has explicitly accepted all of them. The set
// itself is free to change between rounds: diagrams may be added,
// replaced, or dropped as the plan takes shape.
//
// Plans are stored on the session row as JSON, the same way todos
// are.
package ultraplan

import (
	"fmt"
	"strings"
	"time"
)

// Status describes where a plan is in its lifecycle.
type Status string

const (
	// StatusDrafting means the plan is still being negotiated. This is
	// what puts a session in Ultraplan mode: while a plan is drafting,
	// tools that change the workspace are refused.
	StatusDrafting Status = "drafting"

	// StatusAccepted means every diagram is valid and the user has
	// accepted all of them.
	StatusAccepted Status = "accepted"

	// StatusAbandoned means the user cancelled the planning session.
	StatusAbandoned Status = "abandoned"
)

// DiagramStatus is the user's verdict on a single diagram.
type DiagramStatus string

const (
	// DiagramProposed means the agent has offered the diagram and the
	// user has not ruled on this version of it yet.
	DiagramProposed DiagramStatus = "proposed"

	// DiagramAccepted means the user accepted this exact source.
	DiagramAccepted DiagramStatus = "accepted"

	// DiagramChangesRequested means the user wants the diagram
	// reworked, usually with feedback attached.
	DiagramChangesRequested DiagramStatus = "changes_requested"
)

// Limits on a diagram set. They exist to keep a review readable in a
// terminal and to keep the tool result inside a sane token budget.
const (
	MaxDiagrams      = 8
	MaxSourceLength  = 8000
	MaxTitleLength   = 80
	MaxSummaryLength = 600
)

// Diagram is one Mermaid diagram within a plan.
type Diagram struct {
	// ID is a short stable slug the agent chooses. It is how a
	// diagram is tracked across rounds, so reusing an ID means
	// "this is a new version of that diagram".
	ID string `json:"id"`

	// Title is the human-readable name shown in the review.
	Title string `json:"title"`

	// Kind is the Mermaid diagram type detected from the source, for
	// example "flowchart" or "sequenceDiagram".
	Kind string `json:"kind,omitempty"`

	// Source is the Mermaid source, without the ```mermaid fence.
	Source string `json:"source"`

	// Intent is the agent's note on what the diagram is claiming, so
	// the user reviews the decision and not just the drawing.
	Intent string `json:"intent,omitempty"`

	// Status is the user's verdict on this version of the source.
	Status DiagramStatus `json:"status"`

	// Feedback is what the user said when asking for changes.
	Feedback string `json:"feedback,omitempty"`

	// Problem is set when the source fails validation, which can only
	// happen after a user edit: the agent's own proposals are
	// validated before they ever reach the user.
	Problem string `json:"problem,omitempty"`

	// EditedByUser reports whether Source came from the user's own
	// edit rather than from the agent.
	EditedByUser bool `json:"edited_by_user,omitempty"`
}

// Accepted reports whether the user has accepted this diagram.
func (d Diagram) Accepted() bool { return d.Status == DiagramAccepted }

// Plan is the full state of a planning session.
type Plan struct {
	// Status is the plan lifecycle state.
	Status Status `json:"status"`

	// Goal is the user's original request, captured when the planning
	// session starts so later rounds keep their bearings.
	Goal string `json:"goal,omitempty"`

	// Summary is the agent's framing of the most recent round.
	Summary string `json:"summary,omitempty"`

	// Diagrams is the current diagram set, in presentation order.
	Diagrams []Diagram `json:"diagrams"`

	// Round counts how many times the agent has submitted a set for
	// review.
	Round int `json:"round"`

	// Implementing records the user's answer to the "start
	// implementing?" question asked once the plan is accepted.
	Implementing bool `json:"implementing,omitempty"`

	// UpdatedAt is a Unix timestamp of the last change.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// Active reports whether the plan is still being negotiated, which is
// what puts the session in Ultraplan mode.
func (p *Plan) Active() bool { return p != nil && p.Status == StatusDrafting }

// AllAccepted reports whether the plan holds at least one diagram and
// the user has accepted every one of them.
func (p *Plan) AllAccepted() bool {
	if p == nil || len(p.Diagrams) == 0 {
		return false
	}
	for _, d := range p.Diagrams {
		if !d.Accepted() {
			return false
		}
	}
	return true
}

// Pending returns the diagrams the user has not accepted yet.
func (p *Plan) Pending() []Diagram {
	if p == nil {
		return nil
	}
	var out []Diagram
	for _, d := range p.Diagrams {
		if !d.Accepted() {
			out = append(out, d)
		}
	}
	return out
}

// Diagram returns the diagram with the given ID.
func (p *Plan) Diagram(id string) (Diagram, bool) {
	if p == nil {
		return Diagram{}, false
	}
	for _, d := range p.Diagrams {
		if d.ID == id {
			return d, true
		}
	}
	return Diagram{}, false
}

// Propose replaces the diagram set with the one the agent just
// submitted and returns the updated plan.
//
// Acceptance is carried over only for a diagram whose ID and source
// both match what the user already accepted: any edit to an accepted
// diagram puts it back in front of the user. Diagrams missing from
// the new set are dropped, which is how the set shrinks between
// rounds.
func (p *Plan) Propose(summary string, proposed []Diagram) {
	prior := make(map[string]Diagram, len(p.Diagrams))
	for _, d := range p.Diagrams {
		prior[d.ID] = d
	}

	next := make([]Diagram, 0, len(proposed))
	for _, d := range proposed {
		d.Status = DiagramProposed
		d.Feedback = ""
		d.Problem = ""
		d.EditedByUser = false
		if d.Kind == "" {
			d.Kind = DetectKind(d.Source)
		}
		if old, ok := prior[d.ID]; ok && old.Accepted() && sameSource(old.Source, d.Source) {
			d.Status = DiagramAccepted
			d.EditedByUser = old.EditedByUser
		}
		next = append(next, d)
	}

	p.Summary = summary
	p.Diagrams = next
	p.Round++
	p.Status = StatusDrafting
	p.UpdatedAt = time.Now().Unix()
}

// ApplyResponse folds a completed review back into the plan. User
// edits become the new source and stay accepted only if the user
// accepted them in the same pass.
//
// A diagram the user edited into something Mermaid cannot parse is
// never left accepted, whatever the verdict said: the plan only
// completes when every diagram is both valid and accepted, so an
// invalid edit goes back to the agent to repair.
func (p *Plan) ApplyResponse(resp ReviewResponse) {
	verdicts := make(map[string]DiagramVerdict, len(resp.Verdicts))
	for _, v := range resp.Verdicts {
		verdicts[v.ID] = v
	}

	for i := range p.Diagrams {
		v, ok := verdicts[p.Diagrams[i].ID]
		if !ok {
			continue
		}
		if v.Source != "" && !sameSource(v.Source, p.Diagrams[i].Source) {
			p.Diagrams[i].Source = v.Source
			p.Diagrams[i].Kind = DetectKind(v.Source)
			p.Diagrams[i].EditedByUser = true
		}
		p.Diagrams[i].Feedback = strings.TrimSpace(v.Feedback)

		// Validate whatever source the diagram now carries, whichever
		// way the verdict went. A user edit that does not parse is the
		// most useful thing we can tell the agent, and it is lost if
		// the check only runs on the accepting branch: the UI refuses
		// to accept a broken diagram, so that branch never sees one.
		p.Diagrams[i].Problem = ""
		if err := ValidateMermaid(p.Diagrams[i].Source); err != nil {
			p.Diagrams[i].Problem = err.Error()
		}

		if v.Accepted && p.Diagrams[i].Problem == "" {
			p.Diagrams[i].Status = DiagramAccepted
			continue
		}
		p.Diagrams[i].Status = DiagramChangesRequested
	}

	p.UpdatedAt = time.Now().Unix()
	if p.AllAccepted() {
		p.Status = StatusAccepted
		p.Implementing = resp.StartImplementation
	}
}

// Abandon marks the plan cancelled, leaving the diagrams in place so
// the user can see what was on the table.
func (p *Plan) Abandon() {
	p.Status = StatusAbandoned
	p.Implementing = false
	p.UpdatedAt = time.Now().Unix()
}

// ValidateSet checks a proposed diagram set: structural limits first,
// then Mermaid validity for each diagram. It returns one error
// describing every problem found, so the agent can fix them in a
// single pass rather than one round trip per mistake.
func ValidateSet(diagrams []Diagram) error {
	if len(diagrams) == 0 {
		return fmt.Errorf("a plan needs at least one diagram")
	}
	if len(diagrams) > MaxDiagrams {
		return fmt.Errorf("a plan holds at most %d diagrams (got %d): merge or drop some", MaxDiagrams, len(diagrams))
	}

	var problems []string
	seen := make(map[string]bool, len(diagrams))
	for i, d := range diagrams {
		label := diagramLabel(i, d)
		switch {
		case strings.TrimSpace(d.ID) == "":
			problems = append(problems, fmt.Sprintf("%s: \"id\" is required (a short stable slug, e.g. \"data-flow\")", label))
		case seen[d.ID]:
			problems = append(problems, fmt.Sprintf("%s: duplicate id %q; every diagram needs its own id", label, d.ID))
		default:
			seen[d.ID] = true
		}
		if strings.TrimSpace(d.Title) == "" {
			problems = append(problems, fmt.Sprintf("%s: \"title\" is required", label))
		} else if len(d.Title) > MaxTitleLength {
			problems = append(problems, fmt.Sprintf("%s: title exceeds %d characters (got %d)", label, MaxTitleLength, len(d.Title)))
		}
		if len(d.Source) > MaxSourceLength {
			problems = append(problems, fmt.Sprintf("%s: source exceeds %d characters (got %d): split it into two diagrams", label, MaxSourceLength, len(d.Source)))
			continue
		}
		if err := ValidateMermaid(d.Source); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", label, err))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%d diagram problem(s) to fix:\n- %s", len(problems), strings.Join(problems, "\n- "))
}

// diagramLabel names a diagram in an error message, preferring the id
// and falling back to the position in the set.
func diagramLabel(i int, d Diagram) string {
	if id := strings.TrimSpace(d.ID); id != "" {
		return fmt.Sprintf("diagram %q", id)
	}
	if title := strings.TrimSpace(d.Title); title != "" {
		return fmt.Sprintf("diagram %q", title)
	}
	return fmt.Sprintf("diagram %d", i+1)
}

// sameSource compares two Mermaid sources ignoring trailing
// whitespace and line ending differences, so a stray newline does not
// invalidate an acceptance.
func sameSource(a, b string) bool {
	return normalizeSource(a) == normalizeSource(b)
}

func normalizeSource(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
