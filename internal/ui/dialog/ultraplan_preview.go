package dialog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	fimage "github.com/charmbracelet/crush/internal/ui/image"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ultraplan"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	// Registered for their side effect: the Mermaid CLI writes PNG, but
	// decoding stays tolerant of a CLI configured to emit something else.
	_ "image/jpeg"
	_ "image/png"
)

// renderDebounce is how long typing must pause before a preview is
// re-rendered. The Mermaid CLI starts a browser, so rendering on every
// keystroke would queue work far faster than it drains.
const renderDebounce = 450 * time.Millisecond

// PreviewRenderer turns Mermaid source into encoded image bytes. It is
// injected rather than called directly so the review can be tested
// without a Mermaid CLI, and so a caller with no renderer can pass nil.
type PreviewRenderer func(ctx context.Context, source string) ([]byte, error)

// UltraplanRenderTickMsg fires when the debounce interval elapses. The
// UI routes it back into the review, which decides whether the tick is
// still current.
type UltraplanRenderTickMsg struct {
	Gen int
}

// UltraplanRenderedMsg carries the outcome of one render attempt.
type UltraplanRenderedMsg struct {
	Gen        int
	DiagramID  string
	SourceHash string
	Image      []byte
	Err        error
}

// previewState is what the review knows about one diagram's picture.
type previewState struct {
	// sourceHash identifies which version of the source produced this
	// state, so an edit invalidates it.
	sourceHash string

	// img is the decoded picture, nil until a render succeeds.
	img image.Image

	// rendering reports whether a render for this source is in flight.
	rendering bool

	// err is why the last attempt failed, if it did.
	err string
}

// sourceHash fingerprints Mermaid source. It keys both the preview
// state and the terminal's image cache, so editing a diagram cannot
// redisplay the previous picture.
func sourceHash(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:8])
}

// SetPreviewRenderer installs the renderer used for diagram previews.
// Passing nil disables previews, and the review falls back to showing
// the diagram source.
func (r *UltraplanReview) SetPreviewRenderer(renderer PreviewRenderer) {
	r.renderer = renderer
}

// SetPreviewCapabilities tells the review how to put an image on this
// terminal. Without Kitty graphics the block encoding still draws a
// recognisable picture, so previews are not limited to fancy terminals.
func (r *UltraplanReview) SetPreviewCapabilities(kitty bool, cellWidth, cellHeight int, tmux bool) {
	if kitty {
		r.imgEnc = fimage.EncodingKitty
	} else {
		r.imgEnc = fimage.EncodingBlocks
	}
	r.cellSize = fimage.CellSize{Width: cellWidth, Height: cellHeight}
	r.isTmux = tmux
}

// previewsEnabled reports whether a picture can be produced at all.
func (r *UltraplanReview) previewsEnabled() bool { return r.renderer != nil }

// previewID is the terminal image cache key for a diagram version.
func previewID(diagramID, hash string) string {
	return "ultraplan-" + diagramID + "-" + hash
}

// requestPreview schedules a render for the diagram at index i, unless
// one is already current or in flight. The debounce is shared with
// editing, so holding a key down or moving the cursor quickly collapses
// into a single render.
func (r *UltraplanReview) requestPreview(i int, source string) tea.Cmd {
	if !r.previewsEnabled() || i < 0 || i >= r.diagramCount() {
		return nil
	}
	// A diagram that does not parse would only make the CLI fail, and
	// the parse error is the more useful thing to show anyway.
	if ultraplan.ValidateMermaid(source) != nil {
		return nil
	}

	hash := sourceHash(source)
	id := r.Request.Diagrams[i].ID
	if state, ok := r.previews[id]; ok && state.sourceHash == hash && (state.img != nil || state.rendering || state.err != "") {
		return nil
	}

	r.renderGen++
	gen := r.renderGen
	r.pendingRender = pendingRender{index: i, source: source, hash: hash, id: id}

	return tea.Tick(renderDebounce, func(time.Time) tea.Msg {
		return UltraplanRenderTickMsg{Gen: gen}
	})
}

// pendingRender is the render a debounce tick will start when it fires.
type pendingRender struct {
	index  int
	source string
	hash   string
	id     string
}

// HandleRenderTick starts the render a debounce tick was waiting on,
// dropping the tick if newer input has superseded it.
func (r *UltraplanReview) HandleRenderTick(gen int) tea.Cmd {
	if gen != r.renderGen || !r.previewsEnabled() {
		return nil
	}
	// One render at a time: each one starts a browser, and the user is
	// only looking at one diagram. A render finishing re-checks for
	// newer pending work.
	if r.renderInFlight {
		return nil
	}

	pending := r.pendingRender
	if pending.id == "" {
		return nil
	}
	if state, ok := r.previews[pending.id]; ok && state.sourceHash == pending.hash && state.img != nil {
		return nil
	}

	r.renderInFlight = true
	r.previews[pending.id] = previewState{sourceHash: pending.hash, rendering: true}
	r.heightDirty = true

	renderer := r.renderer
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), renderCommandTimeout)
		defer cancel()
		data, err := renderer(ctx, pending.source)
		return UltraplanRenderedMsg{
			Gen:        gen,
			DiagramID:  pending.id,
			SourceHash: pending.hash,
			Image:      data,
			Err:        err,
		}
	}
}

// renderCommandTimeout bounds a render started from the review. The
// renderer applies its own deadline too; this one guarantees the
// command returns even for a renderer that does not.
const renderCommandTimeout = 30 * time.Second

// HandleRendered folds a finished render into the review and returns
// the command that puts the picture on the terminal.
func (r *UltraplanReview) HandleRendered(msg UltraplanRenderedMsg) tea.Cmd {
	r.renderInFlight = false
	r.heightDirty = true

	state := previewState{sourceHash: msg.SourceHash}
	switch {
	case msg.Err != nil:
		state.err = msg.Err.Error()
	case len(msg.Image) == 0:
		state.err = "the renderer returned no image"
	default:
		img, _, err := image.Decode(bytes.NewReader(msg.Image))
		if err != nil {
			state.err = "the rendered image could not be decoded: " + err.Error()
		} else {
			state.img = img
		}
	}
	r.previews[msg.DiagramID] = state

	var cmds []tea.Cmd
	if state.img != nil {
		cols, rows := r.previewSize()
		cmds = append(cmds, r.imgEnc.Transmit(
			previewID(msg.DiagramID, msg.SourceHash),
			state.img, r.cellSize, cols, rows, r.isTmux,
		))
	}
	// Newer input may have arrived while this render was running.
	if cmd := r.resumePendingRender(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// resumePendingRender restarts the debounce when the pending work is
// for a source no completed render covers.
func (r *UltraplanReview) resumePendingRender() tea.Cmd {
	pending := r.pendingRender
	if pending.id == "" {
		return nil
	}
	if state, ok := r.previews[pending.id]; ok && state.sourceHash == pending.hash {
		return nil
	}
	r.renderGen++
	gen := r.renderGen
	return tea.Tick(renderDebounce, func(time.Time) tea.Msg {
		return UltraplanRenderTickMsg{Gen: gen}
	})
}

// previewSize returns the picture size in terminal cells.
func (r *UltraplanReview) previewSize() (cols, rows int) {
	cols = max(r.contentWidth(r.lastWidth)-4, 20)
	return cols, r.previewRows()
}

// previewRows is how many rows a picture may occupy.
func (r *UltraplanReview) previewRows() int {
	budget := r.maxHeight
	if budget <= 0 {
		budget = maxReviewHeight
	}
	// Leave room for the surrounding chrome in whichever stage is
	// showing the picture.
	return max(min(budget/2, 14), 6)
}

// previewIsCurrent reports whether the stored picture was rendered
// from the source the diagram now carries. A picture that no longer
// matches its source is worse than no picture: it shows the user
// something they are not actually looking at.
func (r *UltraplanReview) previewIsCurrent(diagramID, source string) bool {
	state, ok := r.previews[diagramID]
	if !ok || state.img == nil {
		return false
	}
	return state.sourceHash == sourceHash(source)
}

// previewLines renders a diagram's picture into display rows, or
// returns nil when there is nothing to draw. Callers that display it
// beside a source must check previewIsCurrent first.
func (r *UltraplanReview) previewLines(diagramID string) []string {
	state, ok := r.previews[diagramID]
	if !ok || state.img == nil {
		return nil
	}
	cols, rows := r.previewSize()
	rendered := r.imgEnc.Render(previewID(diagramID, state.sourceHash), cols, rows)
	if rendered == "" {
		return nil
	}
	return strings.Split(rendered, "\n")
}

// previewStatus is the one-line explanation shown in place of, or
// beneath, a picture.
func (r *UltraplanReview) previewStatus(sty *styles.Styles, diagramID string, width int) string {
	if !r.previewsEnabled() {
		return sty.Editor.QuestionNote.Render(ansi.Truncate(
			"    no preview: install the Mermaid CLI (npm i -g @mermaid-js/mermaid-cli) to see diagrams drawn",
			width, "…",
		))
	}
	state, ok := r.previews[diagramID]
	switch {
	case !ok:
		return ""
	case state.rendering:
		return sty.Editor.QuestionNote.Render("    rendering…")
	case state.err != "":
		return sty.Tool.ErrorMessage.Render(ansi.Truncate("    preview failed: "+state.err, width, "…"))
	default:
		return ""
	}
}

// -----------------------------------------------------------------------------
// Source editing
// -----------------------------------------------------------------------------

// newSourceTextarea builds the editor used for Mermaid source. Unlike
// the question textareas it keeps newline insertion, since a diagram is
// inherently multi-line, and shows line numbers so a validation error
// naming a line points somewhere.
func newSourceTextarea(sty *styles.Styles) textarea.Model {
	ta := textarea.New()
	ta.SetStyles(sty.Editor.Textarea)
	ta.Placeholder = "Mermaid source"
	ta.ShowLineNumbers = true
	ta.CharLimit = ultraplan.MaxSourceLength
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = 4
	ta.MaxHeight = 14
	ta.SetHeight(6)
	ta.Blur()
	return ta
}

// openSourceEditor switches to the source pane for a diagram.
func (r *UltraplanReview) openSourceEditor(i int) tea.Cmd {
	if i < 0 || i >= r.diagramCount() {
		return nil
	}
	r.stage = stageSource
	r.sourceTarget = i
	r.sourceEditor.SetValue(r.Request.Diagrams[i].Source)
	r.sourceEditor.MoveToEnd()
	r.revalidateSource()
	r.heightDirty = true

	cmds := []tea.Cmd{r.sourceEditor.Focus()}
	if cmd := r.requestPreview(i, r.Request.Diagrams[i].Source); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// closeSourceEditor leaves the source pane, applying the edit when keep
// is set.
func (r *UltraplanReview) closeSourceEditor(keep bool) {
	if keep && r.sourceTarget >= 0 {
		r.ApplyEdit(r.Request.Diagrams[r.sourceTarget].ID, r.sourceEditor.Value())
	}
	r.sourceEditor.Blur()
	r.sourceEditor.Reset()
	r.stage = stageList
	r.sourceTarget = -1
	r.sourceProblem = ""
	r.heightDirty = true
}

// revalidateSource re-checks the text in the editor. Validation is pure
// string work, so running it on every keystroke costs nothing and the
// user learns a diagram is broken as they break it.
func (r *UltraplanReview) revalidateSource() {
	source := strings.TrimSpace(r.sourceEditor.Value())
	if source == "" {
		r.sourceProblem = "source is empty"
		return
	}
	if err := ultraplan.ValidateMermaid(source); err != nil {
		r.sourceProblem = err.Error()
		return
	}
	r.sourceProblem = ""
}

// handleSourceStageKey routes keys while the source editor is open.
// Enter inserts a newline, so saving is on its own binding.
func (r *UltraplanReview) handleSourceStageKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, r.keyClose):
		r.closeSourceEditor(false)
		return false, nil
	case key.Matches(msg, r.keySaveSource):
		r.closeSourceEditor(true)
		return false, nil
	}

	var cmd tea.Cmd
	r.sourceEditor, cmd = r.sourceEditor.Update(msg)
	r.revalidateSource()
	r.heightDirty = true

	cmds := []tea.Cmd{cmd}
	if r.sourceProblem == "" {
		if renderCmd := r.requestPreview(r.sourceTarget, strings.TrimSpace(r.sourceEditor.Value())); renderCmd != nil {
			cmds = append(cmds, renderCmd)
		}
	}
	return false, tea.Batch(cmds...)
}

// HandleSourcePaste forwards a paste into the source editor.
func (r *UltraplanReview) HandleSourcePaste(msg tea.PasteMsg) tea.Cmd {
	var cmd tea.Cmd
	r.sourceEditor, cmd = r.sourceEditor.Update(msg)
	r.revalidateSource()
	r.heightDirty = true
	return cmd
}

// sourceHeight is the row count of the source pane.
func (r *UltraplanReview) sourceHeight(width int) int {
	r.sourceEditor.SetWidth(width)
	// Header, editor, status line, preview, trailing blank.
	h := 2 + max(r.sourceEditor.Height(), 1) + 1
	if lines := r.previewLines(r.currentDiagramID()); len(lines) > 0 {
		h += len(lines) + 1
	} else if r.previewStatus(r.Styles, r.currentDiagramID(), width) != "" {
		h += 2
	}
	return h + 1
}

// currentDiagramID names the diagram the source editor is on.
func (r *UltraplanReview) currentDiagramID() string {
	if r.sourceTarget < 0 || r.sourceTarget >= r.diagramCount() {
		return ""
	}
	return r.Request.Diagrams[r.sourceTarget].ID
}

// drawSource renders the source pane and returns the cursor position.
func (r *UltraplanReview) drawSource(scr uv.Screen, area uv.Rectangle, width int) *tea.Cursor {
	sty := r.Styles
	y := area.Min.Y

	title := "Editing " + r.Request.Diagrams[r.sourceTarget].Title
	status := sty.Editor.QuestionNote.Render("valid")
	if r.sourceProblem != "" {
		status = sty.Tool.ErrorMessage.Render(ansi.Truncate(r.sourceProblem, max(width/2, 20), "…"))
	}
	header := questionIconPrompt(sty, r.focused) +
		sty.Editor.QuestionUnselected.Render(ansi.Truncate(title, max(width/2, 10), "…")) +
		"  " + status
	y += drawStyledText(scr, image.Rect(area.Min.X, y, area.Max.X, area.Max.Y), header)
	y++

	r.sourceEditor.SetWidth(width)
	editorHeight := max(r.sourceEditor.Height(), 1)
	editorArea := image.Rect(area.Min.X, y, area.Max.X, min(area.Max.Y, y+editorHeight))
	uv.NewStyledString(r.sourceEditor.View()).Draw(scr, editorArea)

	cur := r.sourceEditor.Cursor()
	if cur != nil {
		cur.Y += y - area.Min.Y
	}
	y += editorHeight
	y++

	diagramID := r.currentDiagramID()
	lines := r.previewLines(diagramID)
	for _, line := range lines {
		if y >= area.Max.Y {
			break
		}
		uv.NewStyledString(line).Draw(scr, image.Rect(area.Min.X, y, area.Max.X, y+1))
		y++
	}

	// While typing, the picture on screen lags the text by a render.
	// Keeping it beats blanking the pane on every keystroke, but it has
	// to be labelled or the user reads it as current.
	statusLine := r.previewStatus(sty, diagramID, width)
	if statusLine == "" && len(lines) > 0 && !r.previewIsCurrent(diagramID, strings.TrimSpace(r.sourceEditor.Value())) {
		statusLine = sty.Editor.QuestionNote.Render("    picture is one edit behind…")
	}
	if statusLine != "" && y < area.Max.Y {
		drawStyledText(scr, image.Rect(area.Min.X, y, area.Max.X, area.Max.Y), statusLine)
	}

	return cur
}
