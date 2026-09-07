package dialog

import (
	"fmt"
	"image"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	fimage "github.com/charmbracelet/crush/internal/ui/image"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ultraplan"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// maxReviewHeight caps how much of the editor area a plan review may
// claim, so the conversation above it stays readable. Anything taller
// scrolls inside the component.
const maxReviewHeight = 22

// reviewStage is which of the review's screens is showing.
type reviewStage int

const (
	// stageList is the diagram list, where verdicts are given.
	stageList reviewStage = iota
	// stageNote is a full-pane editor for one piece of feedback.
	stageNote
	// stageImplement is the question asked once every diagram is
	// accepted: build it now, or not yet.
	stageImplement
	// stageSource is the Mermaid source editor, with live validation
	// and a live preview beside it.
	stageSource
)

// noteTargetPlan marks the note editor as editing the plan-wide
// feedback rather than one diagram's.
const noteTargetPlan = -1

// reviewLine is one rendered row, tagged with the selectable item it
// belongs to so the cursor can be kept on screen.
type reviewLine struct {
	text string
	item int // index of the owning item, or -1 for chrome
}

// UltraplanReview is the inline component for reviewing an Ultraplan
// diagram set. The user rules on each diagram — accepting it, asking
// for changes, or editing the Mermaid source in their own editor — and
// submits when done. Once every diagram is accepted the component asks
// whether to start implementing.
type UltraplanReview struct {
	Styles  *styles.Styles
	Request ultraplan.ReviewRequest

	verdicts []ultraplan.DiagramVerdict
	expanded []bool
	feedback string

	cursor       int // 0..n-1 diagrams, n = submit row
	stage        reviewStage
	noteTarget   int
	noteEditor   textarea.Model
	implementYes bool

	scrollOffset int
	focused      bool
	lastWidth    int
	heightDirty  bool

	// maxHeight caps the rows the review may claim. The UI sets it
	// from the terminal height so the source editor can use more room
	// than the list without swallowing the conversation.
	maxHeight int

	// Source editing.
	sourceEditor  textarea.Model
	sourceTarget  int
	sourceProblem string

	// Preview rendering.
	renderer       PreviewRenderer
	imgEnc         fimage.Encoding
	cellSize       fimage.CellSize
	isTmux         bool
	previews       map[string]previewState
	pendingRender  pendingRender
	renderGen      int
	renderInFlight bool
	// showSource makes an expanded diagram show its source even when a
	// picture is available.
	showSource bool

	keyUpDown     key.Binding
	keyAccept     key.Binding
	keyAcceptAll  key.Binding
	keyComment    key.Binding
	keyEdit       key.Binding
	keyExpand     key.Binding
	keyFeedback   key.Binding
	keySubmit     key.Binding
	keyClose      key.Binding
	keyLeftRight  key.Binding
	keySaveSource key.Binding
	keyExternal   key.Binding
	keyToggleView key.Binding

	// OnRespond is called with the completed review when the user
	// submits. The UI wires this to workspace submission.
	OnRespond func(resp ultraplan.ReviewResponse)

	// OnCancel is called when the user abandons the planning session.
	OnCancel func()

	// OnEdit is called to open a diagram's Mermaid source in the
	// user's editor. The UI wires this to an ExecProcess command that
	// sends the edited source back via ApplyEdit.
	OnEdit func(diagramID, title, source string) tea.Cmd
}

var _ InlineEditor = (*UltraplanReview)(nil)

// NewUltraplanReview creates a review component for a diagram set.
// Diagrams the user already accepted in an earlier round start
// accepted, so a round only asks about what actually changed.
func NewUltraplanReview(sty *styles.Styles, req ultraplan.ReviewRequest) *UltraplanReview {
	verdicts := make([]ultraplan.DiagramVerdict, len(req.Diagrams))
	expanded := make([]bool, len(req.Diagrams))
	for i, d := range req.Diagrams {
		verdicts[i] = ultraplan.DiagramVerdict{
			ID:       d.ID,
			Accepted: d.Accepted(),
			Feedback: d.Feedback,
		}
		// Open the diagrams that still need a decision; leave the
		// settled ones collapsed so the round reads as a diff.
		expanded[i] = !d.Accepted()
	}

	r := &UltraplanReview{
		Styles:        sty,
		Request:       req,
		verdicts:      verdicts,
		expanded:      expanded,
		noteTarget:    noteTargetPlan,
		keyUpDown:     key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "move")),
		keyAccept:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "accept")),
		keyAcceptAll:  key.NewBinding(key.WithKeys("A"), key.WithHelp("A", "accept all")),
		keyComment:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "comment")),
		keyEdit:       key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit source")),
		keyExpand:     key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "show source")),
		keyFeedback:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "note on plan")),
		keySubmit:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit")),
		keyClose:      CloseKey,
		keyLeftRight:  key.NewBinding(key.WithKeys("left", "right", "h", "l"), key.WithHelp("←/→", "switch")),
		keySaveSource: key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save")),
		keyExternal:   key.NewBinding(key.WithKeys("E"), key.WithHelp("E", "$EDITOR")),
		keyToggleView: key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "picture/source")),
		sourceTarget:  -1,
		previews:      make(map[string]previewState),
	}
	r.noteEditor = newQuestionTextarea(sty, "What needs to change?", 800)
	r.noteEditor.MaxHeight = 6
	r.sourceEditor = newSourceTextarea(sty)
	return r
}

// SetMaxHeight caps how many rows the review may claim.
func (r *UltraplanReview) SetMaxHeight(rows int) {
	if rows > 0 {
		r.maxHeight = rows
		r.heightDirty = true
	}
}

// diagramCount returns how many diagrams are under review.
func (r *UltraplanReview) diagramCount() int { return len(r.Request.Diagrams) }

// submitRow is the cursor index of the submit action.
func (r *UltraplanReview) submitRow() int { return r.diagramCount() }

// allAccepted reports whether every diagram currently carries an
// accepting verdict.
func (r *UltraplanReview) allAccepted() bool {
	if len(r.verdicts) == 0 {
		return false
	}
	for _, v := range r.verdicts {
		if !v.Accepted {
			return false
		}
	}
	return true
}

// acceptedCount returns how many diagrams are currently accepted.
func (r *UltraplanReview) acceptedCount() int {
	n := 0
	for _, v := range r.verdicts {
		if v.Accepted {
			n++
		}
	}
	return n
}

// ApplyEdit stores Mermaid source the user edited outside the TUI. An
// edit clears the diagram's acceptance: the user has just changed what
// they would be accepting, so they rule on it again.
func (r *UltraplanReview) ApplyEdit(diagramID, source string) {
	source = strings.TrimSpace(ultraplan.StripFence(source))
	if source == "" {
		return
	}
	for i := range r.Request.Diagrams {
		if r.Request.Diagrams[i].ID != diagramID {
			continue
		}
		if source == r.Request.Diagrams[i].Source {
			return
		}
		r.Request.Diagrams[i].Source = source
		r.Request.Diagrams[i].Kind = ultraplan.DetectKind(source)
		r.Request.Diagrams[i].EditedByUser = true
		r.Request.Diagrams[i].Problem = ""
		if err := ultraplan.ValidateMermaid(source); err != nil {
			r.Request.Diagrams[i].Problem = err.Error()
		}
		r.verdicts[i].Source = source
		r.verdicts[i].Accepted = false
		r.expanded[i] = true
		r.heightDirty = true
		return
	}
}

// HandleKey processes a key press. It returns true once the review has
// been submitted or abandoned.
func (r *UltraplanReview) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch r.stage {
	case stageNote:
		return r.handleNoteStageKey(msg)
	case stageImplement:
		return r.handleImplementStageKey(msg)
	case stageSource:
		return r.handleSourceStageKey(msg)
	default:
		return r.handleListKey(msg)
	}
}

func (r *UltraplanReview) handleListKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, r.keyClose):
		if r.OnCancel != nil {
			r.OnCancel()
		}
		return true, nil

	case key.Matches(msg, r.keyUpDown):
		if msg.String() == "up" {
			r.cursor = max(0, r.cursor-1)
		} else {
			r.cursor = min(r.submitRow(), r.cursor+1)
		}
		r.heightDirty = true
		return false, r.previewForCursor()

	case key.Matches(msg, r.keySubmit):
		if r.cursor == r.submitRow() || r.allAccepted() {
			return r.submit()
		}
		// On a diagram row, enter is the obvious "yes, this one".
		r.toggleAccept()
		return false, nil

	case key.Matches(msg, r.keyAccept):
		r.toggleAccept()
		return false, nil

	case key.Matches(msg, r.keyAcceptAll):
		for i := range r.verdicts {
			// A diagram that does not parse is left out: the plan only
			// completes when every diagram is valid, so accepting one
			// here would only be overruled downstream.
			if r.Request.Diagrams[i].Problem != "" {
				r.expanded[i] = true
				continue
			}
			r.verdicts[i].Accepted = true
		}
		r.heightDirty = true
		return false, nil

	case key.Matches(msg, r.keyExpand):
		if r.cursor < r.diagramCount() {
			r.expanded[r.cursor] = !r.expanded[r.cursor]
			r.heightDirty = true
			if r.expanded[r.cursor] {
				return false, r.previewForCursor()
			}
		}
		return false, nil

	case key.Matches(msg, r.keyToggleView):
		if r.previewsEnabled() {
			r.showSource = !r.showSource
			r.heightDirty = true
		}
		return false, nil

	case key.Matches(msg, r.keyComment):
		if r.cursor < r.diagramCount() {
			return false, r.openNoteEditor(r.cursor)
		}
		return false, nil

	case key.Matches(msg, r.keyFeedback):
		return false, r.openNoteEditor(noteTargetPlan)

	case key.Matches(msg, r.keyEdit):
		if r.cursor < r.diagramCount() {
			return false, r.openSourceEditor(r.cursor)
		}
		return false, nil

	case key.Matches(msg, r.keyExternal):
		if r.cursor < r.diagramCount() && r.OnEdit != nil {
			d := r.Request.Diagrams[r.cursor]
			return false, r.OnEdit(d.ID, d.Title, d.Source)
		}
		return false, nil
	}
	return false, nil
}

func (r *UltraplanReview) handleNoteStageKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, r.keyClose):
		r.closeNoteEditor(false)
		return false, nil
	case key.Matches(msg, r.keySubmit):
		r.closeNoteEditor(true)
		return false, nil
	}
	var cmd tea.Cmd
	r.noteEditor, cmd = r.noteEditor.Update(msg)
	r.heightDirty = true
	return false, cmd
}

func (r *UltraplanReview) handleImplementStageKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, r.keyClose):
		// Backing out returns to the list rather than abandoning the
		// plan: the diagrams are accepted, only the next step is
		// unsettled.
		r.stage = stageList
		r.heightDirty = true
		return false, nil
	case key.Matches(msg, r.keyLeftRight):
		r.implementYes = !r.implementYes
		return false, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("y", "Y"))):
		r.implementYes = true
		return r.respond()
	case key.Matches(msg, key.NewBinding(key.WithKeys("n", "N"))):
		r.implementYes = false
		return r.respond()
	case key.Matches(msg, r.keySubmit):
		return r.respond()
	}
	return false, nil
}

// previewForCursor asks for a picture of the diagram the cursor is on,
// so moving through the list brings each one up without the user
// having to ask.
func (r *UltraplanReview) previewForCursor() tea.Cmd {
	if r.cursor >= r.diagramCount() {
		return nil
	}
	return r.requestPreview(r.cursor, r.Request.Diagrams[r.cursor].Source)
}

// InitialCmd asks for a picture of the first diagram so the review
// opens with something drawn rather than waiting for a keystroke.
func (r *UltraplanReview) InitialCmd() tea.Cmd {
	return r.previewForCursor()
}

// toggleAccept flips the verdict on the diagram under the cursor. A
// diagram whose source does not parse cannot be accepted, so the plan
// never completes on a broken diagram.
func (r *UltraplanReview) toggleAccept() {
	if r.cursor >= r.diagramCount() {
		return
	}
	if r.Request.Diagrams[r.cursor].Problem != "" {
		r.verdicts[r.cursor].Accepted = false
		r.expanded[r.cursor] = true
		r.heightDirty = true
		return
	}
	r.verdicts[r.cursor].Accepted = !r.verdicts[r.cursor].Accepted
	r.heightDirty = true
}

// openNoteEditor switches to the note pane for a diagram, or for the
// plan as a whole when target is noteTargetPlan.
func (r *UltraplanReview) openNoteEditor(target int) tea.Cmd {
	r.stage = stageNote
	r.noteTarget = target
	if target == noteTargetPlan {
		r.noteEditor.Placeholder = "Anything about the plan as a whole?"
		r.noteEditor.SetValue(r.feedback)
	} else {
		r.noteEditor.Placeholder = "What needs to change?"
		r.noteEditor.SetValue(r.verdicts[target].Feedback)
	}
	r.noteEditor.MoveToEnd()
	r.heightDirty = true
	return r.noteEditor.Focus()
}

// closeNoteEditor leaves the note pane, saving the text when keep is
// set. Saving a note on a diagram withdraws its acceptance: asking for
// a change and accepting it are contradictory.
func (r *UltraplanReview) closeNoteEditor(keep bool) {
	if keep {
		text := strings.TrimSpace(r.noteEditor.Value())
		if r.noteTarget == noteTargetPlan {
			r.feedback = text
		} else {
			r.verdicts[r.noteTarget].Feedback = text
			if text != "" {
				r.verdicts[r.noteTarget].Accepted = false
			}
		}
	}
	r.noteEditor.Blur()
	r.noteEditor.Reset()
	r.stage = stageList
	r.noteTarget = noteTargetPlan
	r.heightDirty = true
}

// submit either asks the implementation question, when every diagram
// is accepted, or sends the round back to the agent for revision.
func (r *UltraplanReview) submit() (bool, tea.Cmd) {
	if r.allAccepted() {
		r.stage = stageImplement
		r.implementYes = true
		r.heightDirty = true
		return false, nil
	}
	return r.respond()
}

// respond hands the completed review to the UI.
func (r *UltraplanReview) respond() (bool, tea.Cmd) {
	if r.OnRespond != nil {
		r.OnRespond(ultraplan.ReviewResponse{
			RequestID:           r.Request.ID,
			Verdicts:            r.verdicts,
			Feedback:            r.feedback,
			StartImplementation: r.allAccepted() && r.implementYes,
		})
	}
	return true, nil
}

// ShortHelp returns the key bindings shown in the status bar.
func (r *UltraplanReview) ShortHelp() []key.Binding {
	switch r.stage {
	case stageNote:
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "save")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "discard")),
		}
	case stageImplement:
		return []key.Binding{r.keyLeftRight, r.keySubmit, r.keyClose}
	case stageSource:
		return []key.Binding{r.keySaveSource, r.keyClose}
	default:
		bindings := []key.Binding{r.keyUpDown, r.keyAccept, r.keyAcceptAll, r.keyComment, r.keyEdit}
		if r.OnEdit != nil {
			bindings = append(bindings, r.keyExternal)
		}
		bindings = append(bindings, r.keyExpand)
		if r.previewsEnabled() {
			bindings = append(bindings, r.keyToggleView)
		}
		return append(bindings, r.keyFeedback, r.keySubmit)
	}
}

// Height returns the number of content lines the review needs at the
// given width, capped so the transcript above stays visible.
func (r *UltraplanReview) Height(width int) int {
	switch r.stage {
	case stageNote:
		return r.noteHeight(r.contentWidth(width))
	case stageSource:
		return min(r.sourceHeight(r.contentWidth(width)), r.heightBudget())
	}
	return min(len(r.buildLines(r.contentWidth(width))), r.heightBudget())
}

// heightBudget is the most rows the review may claim.
func (r *UltraplanReview) heightBudget() int {
	if r.maxHeight > 0 {
		return max(r.maxHeight, maxReviewHeight)
	}
	return maxReviewHeight
}

// noteHeight is the height of the note pane: a header, a blank line,
// the textarea, and a trailing blank.
func (r *UltraplanReview) noteHeight(width int) int {
	r.noteEditor.SetWidth(width)
	return 3 + r.noteEditor.Height()
}

// HeightChanged reports whether an interaction may have changed the
// rendered height since the last layout pass.
func (r *UltraplanReview) HeightChanged() bool {
	changed := r.heightDirty
	r.heightDirty = false
	return changed
}

// SetFocused records whether the editor area holds focus.
func (r *UltraplanReview) SetFocused(focused bool) { r.focused = focused }

// HandlePaste forwards a paste into whichever editor is open. Pasting
// a diagram into the source editor is the obvious way to bring one in
// from elsewhere, so it has to reach the right textarea.
func (r *UltraplanReview) HandlePaste(msg tea.PasteMsg) tea.Cmd {
	switch r.stage {
	case stageSource:
		return r.HandleSourcePaste(msg)
	case stageNote:
		var cmd tea.Cmd
		r.noteEditor, cmd = r.noteEditor.Update(msg)
		return cmd
	default:
		return nil
	}
}

// contentWidth clamps the drawing width to something readable.
func (r *UltraplanReview) contentWidth(width int) int {
	w := width
	if w <= 0 {
		w = r.lastWidth
	}
	if w <= 0 {
		w = choiceListMaxWidth
	}
	return min(w, choiceListMaxWidth)
}

// Draw renders the review and returns the cursor position when the
// note editor is open.
func (r *UltraplanReview) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	r.lastWidth = area.Dx()
	w := r.contentWidth(area.Dx())

	switch r.stage {
	case stageNote:
		return r.drawNote(scr, area, w)
	case stageSource:
		return r.drawSource(scr, area, w)
	}

	lines := r.buildLines(w)
	viewport := min(area.Dy(), r.heightBudget())
	r.clampScroll(lines, viewport)

	y := area.Min.Y
	for i := r.scrollOffset; i < len(lines) && y < area.Min.Y+viewport; i++ {
		uv.NewStyledString(lines[i].text).Draw(
			scr, image.Rect(area.Min.X, y, area.Max.X, y+1),
		)
		y++
	}
	return nil
}

// clampScroll keeps the selected item inside the viewport.
func (r *UltraplanReview) clampScroll(lines []reviewLine, viewport int) {
	if viewport <= 0 || len(lines) <= viewport {
		r.scrollOffset = 0
		return
	}

	first, last := -1, -1
	for i, l := range lines {
		if l.item != r.cursor {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		r.scrollOffset = min(r.scrollOffset, len(lines)-viewport)
		return
	}

	if first < r.scrollOffset {
		r.scrollOffset = first
	}
	// Prefer showing the top of a tall item over its bottom: a
	// diagram's header carries the verdict.
	if last >= r.scrollOffset+viewport {
		r.scrollOffset = min(first, last-viewport+1)
	}
	r.scrollOffset = max(0, min(r.scrollOffset, len(lines)-viewport))
}

// drawNote renders the full-pane feedback editor.
func (r *UltraplanReview) drawNote(scr uv.Screen, area uv.Rectangle, width int) *tea.Cursor {
	sty := r.Styles
	y := area.Min.Y

	title := "Note on the plan"
	if r.noteTarget != noteTargetPlan {
		title = "Changes to " + r.Request.Diagrams[r.noteTarget].Title
	}
	header := questionIconPrompt(sty, r.focused) + sty.Editor.QuestionUnselected.Render(title)
	y += drawStyledText(scr, image.Rect(area.Min.X, y, area.Max.X, area.Max.Y), header)
	y++

	r.noteEditor.SetWidth(width)
	editorArea := image.Rect(area.Min.X, y, area.Max.X, min(area.Max.Y, y+max(r.noteEditor.Height(), 1)))
	uv.NewStyledString(r.noteEditor.View()).Draw(scr, editorArea)

	cur := r.noteEditor.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y += y - area.Min.Y
	return cur
}

// buildLines renders the whole review into rows. Height and Draw both
// go through it so they can never disagree.
func (r *UltraplanReview) buildLines(width int) []reviewLine {
	if r.stage == stageImplement {
		return r.buildImplementLines(width)
	}

	sty := r.Styles
	var lines []reviewLine
	push := func(text string, item int) {
		for part := range strings.SplitSeq(text, "\n") {
			lines = append(lines, reviewLine{text: part, item: item})
		}
	}

	// Header.
	header := fmt.Sprintf("Plan review · round %d · %d/%d accepted",
		r.Request.Round, r.acceptedCount(), r.diagramCount())
	push(questionIconPrompt(sty, r.focused)+sty.Editor.QuestionUnselected.Render(header), -1)
	if r.Request.Summary != "" {
		push(sty.Editor.QuestionBody.Render(wrapIndent(r.Request.Summary, width-2, "  ")), -1)
	}
	push("", -1)

	for i, d := range r.Request.Diagrams {
		r.pushDiagram(&lines, push, i, d, width)
	}

	if r.feedback != "" {
		push(sty.Editor.QuestionNote.Render(wrapIndent("note on the plan: "+r.feedback, width-2, "  ")), -1)
		push("", -1)
	}

	push(r.submitLabel(), r.submitRow())
	push("", -1)
	return lines
}

// pushDiagram appends the rows for one diagram.
func (r *UltraplanReview) pushDiagram(
	lines *[]reviewLine,
	push func(string, int),
	i int,
	d ultraplan.Diagram,
	width int,
) {
	sty := r.Styles
	selected := r.cursor == i
	bar := "  "
	if selected {
		bar = sty.Editor.QuestionCursorBar.Render("┃ ")
	}

	titleStyle := sty.Editor.QuestionUnselected
	if selected {
		titleStyle = sty.Editor.QuestionSelected
	}

	mark := sty.Editor.QuestionCheckOff.Render("○")
	if r.verdicts[i].Accepted {
		mark = sty.Editor.QuestionCheckOn.Render("●")
	}

	meta := d.Kind
	if meta == "" {
		meta = "mermaid"
	}
	if d.EditedByUser {
		meta += " · edited by you"
	}
	title := fmt.Sprintf("%s %s %s %s",
		bar, mark, titleStyle.Render(d.Title), sty.Editor.QuestionNote.Render("· "+meta))
	push(ansi.Truncate(title, width, "…"), i)

	if d.Intent != "" {
		push(sty.Editor.QuestionBody.Render(wrapIndent("    "+d.Intent, width-4, "    ")), i)
	}
	if d.Problem != "" {
		push(sty.Tool.ErrorMessage.Render(
			wrapIndent("    does not parse: "+d.Problem, width-4, "    "),
		), i)
	}
	if fb := r.verdicts[i].Feedback; fb != "" {
		push(sty.Editor.QuestionNote.Render(wrapIndent("    your note: "+fb, width-4, "    ")), i)
	}

	if r.expanded[i] {
		// The list is the record of what is being reviewed, so it only
		// shows a picture that matches the source it stands for.
		lines := r.previewLines(d.ID)
		if len(lines) > 0 && r.previewIsCurrent(d.ID, d.Source) && !r.showSource {
			for _, previewLine := range lines {
				push(previewLine, i)
			}
		} else {
			for _, srcLine := range strings.Split(d.Source, "\n") {
				push(sty.Tool.ContentCodeLine.Render(ansi.Truncate("    "+srcLine, width, "…")), i)
			}
		}
		if statusLine := r.previewStatus(sty, d.ID, width); statusLine != "" {
			push(statusLine, i)
		}
	}
	push("", i)
}

// submitLabel renders the submit row, whose wording depends on whether
// the plan is ready to close.
func (r *UltraplanReview) submitLabel() string {
	selected := r.cursor == r.submitRow()
	label := fmt.Sprintf("Send %d change request(s) back", r.diagramCount()-r.acceptedCount())
	if r.allAccepted() {
		label = "Accept the plan"
	}
	return "  " + common.ButtonGroup(r.Styles, []common.ButtonOpts{
		{Text: label, Selected: selected, Padding: 2},
	}, " ")
}

// buildImplementLines renders the question asked once the plan is
// accepted.
func (r *UltraplanReview) buildImplementLines(width int) []reviewLine {
	sty := r.Styles
	var lines []reviewLine
	push := func(text string) {
		for part := range strings.SplitSeq(text, "\n") {
			lines = append(lines, reviewLine{text: part, item: -1})
		}
	}

	head := fmt.Sprintf("Plan accepted — all %d diagram(s) signed off.", r.diagramCount())
	push(questionIconPrompt(sty, r.focused) + sty.Editor.QuestionUnselected.Render(head))
	push(sty.Editor.QuestionBody.Render(
		wrapIndent("Start implementing now? Saying no keeps the workspace untouched; you can pick it up later.", width-2, "  "),
	))
	push("")
	push("  " + common.ButtonGroup(sty, []common.ButtonOpts{
		{Text: "Start now", Selected: r.implementYes, Padding: 3, UnderlineIndex: 0},
		{Text: "Not yet", Selected: !r.implementYes, Padding: 3, UnderlineIndex: 0},
	}, " "))
	push("")
	return lines
}
