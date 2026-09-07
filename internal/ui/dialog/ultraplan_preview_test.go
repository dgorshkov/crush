package dialog

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ultraplan"
	"github.com/stretchr/testify/require"
)

// tinyPNG encodes a small solid image, standing in for a rendered
// diagram.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for x := range 8 {
		for y := range 4 {
			img.Set(x, y, color.RGBA{R: 20, G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// previewDiagrams returns the standard pair with sources made unique
// to this test. The terminal image cache is a package-level global
// keyed by diagram id, source hash and size, so tests sharing an
// identical source would share cache entries and see each other's
// pictures. A Mermaid comment keeps the source valid while making the
// hash distinct.
func previewDiagrams(t *testing.T) []ultraplan.Diagram {
	t.Helper()
	diagrams := twoDiagrams()
	for i := range diagrams {
		diagrams[i].Source += "\n    %% " + t.Name()
	}
	return diagrams
}

// runCmd runs a command when there is one. HandleRendered returns nil
// when the picture is already on the terminal, which is not a failure.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// pressCtrl sends a control-modified key.
func pressCtrl(t *testing.T, r *UltraplanReview, c rune) bool {
	t.Helper()
	done, _ := r.HandleKey(tea.KeyPressMsg{Code: c, Mod: tea.ModCtrl})
	return done
}

// typeRune sends a single printable key.
func typeRune(t *testing.T, r *UltraplanReview, c rune) tea.Cmd {
	t.Helper()
	_, cmd := r.HandleKey(tea.KeyPressMsg{Code: c, Text: string(c)})
	return cmd
}

// recordingRenderer returns a renderer that hands back the given image
// and counts how many times it ran.
func recordingRenderer(data []byte, err error) (PreviewRenderer, *atomic.Int64) {
	var calls atomic.Int64
	return func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return data, err
	}, &calls
}

func TestSourceEditorOpensSavesAndDiscards(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "e")
	require.Equal(t, stageSource, review.stage)
	require.Equal(t, 0, review.sourceTarget)
	require.Equal(t, reviewFlow, review.sourceEditor.Value())

	review.sourceEditor.SetValue("flowchart LR\n    A --> C")
	review.revalidateSource()
	require.False(t, pressCtrl(t, review, 's'))

	require.Equal(t, stageList, review.stage)
	require.Equal(t, "flowchart LR\n    A --> C", review.Request.Diagrams[0].Source)
	require.True(t, review.Request.Diagrams[0].EditedByUser)
}

func TestSourceEditorDiscardKeepsTheOriginal(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "e")
	review.sourceEditor.SetValue("flowchart LR\n    A --> C")
	press(t, review, "esc")

	require.Equal(t, stageList, review.stage)
	require.Equal(t, reviewFlow, review.Request.Diagrams[0].Source, "escape discards the edit")
	require.False(t, review.Request.Diagrams[0].EditedByUser)
}

func TestSourceEditorValidatesAsYouType(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "e")
	require.Empty(t, review.sourceProblem)

	// Break it: an unclosed bracket.
	review.sourceEditor.SetValue("flowchart TD\n    A[Start --> B")
	review.revalidateSource()
	require.Contains(t, review.sourceProblem, "unclosed square bracket")

	// Repair it.
	review.sourceEditor.SetValue(reviewFlow)
	review.revalidateSource()
	require.Empty(t, review.sourceProblem)

	// Emptying it is a problem too, rather than silently valid.
	review.sourceEditor.SetValue("   ")
	review.revalidateSource()
	require.Contains(t, review.sourceProblem, "empty")
}

func TestSourceEditorKeystrokeRevalidates(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "e")
	review.sourceEditor.SetValue("flowchart TD\n    A --> B")
	review.sourceEditor.MoveToEnd()

	// A stray bracket at the end breaks the diagram, and the editor
	// should say so without waiting for a save.
	typeRune(t, review, '[')
	require.NotEmpty(t, review.sourceProblem)
}

func TestPreviewsDisabledWithoutARenderer(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	require.False(t, review.previewsEnabled())
	require.Nil(t, review.requestPreview(0, reviewFlow))
	require.Nil(t, review.InitialCmd())

	sty := review.Styles
	require.Contains(t, review.previewStatus(sty, "flow", 120), "Mermaid CLI")
}

func TestPreviewIsNotRequestedForBrokenSource(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	renderer, calls := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)

	require.Nil(t, review.requestPreview(0, "flowchart TD\n    A[Start --> B"),
		"a diagram that does not parse would only make the CLI fail")
	require.Zero(t, calls.Load())
}

func TestPreviewRenderRoundTrip(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, calls := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)
	review.SetPreviewCapabilities(false, 10, 20, false)

	// Requesting schedules a debounce tick rather than rendering now.
	cmd := review.requestPreview(0, review.Request.Diagrams[0].Source)
	require.NotNil(t, cmd)
	require.Zero(t, calls.Load(), "the render waits for the debounce")

	// The tick starts the render.
	renderCmd := review.HandleRenderTick(review.renderGen)
	require.NotNil(t, renderCmd)
	msg := renderCmd()
	require.Equal(t, int64(1), calls.Load())

	rendered, ok := msg.(UltraplanRenderedMsg)
	require.True(t, ok)
	require.NoError(t, rendered.Err)

	// Caches the decoded image so previewLines can draw it.
	runCmd(review.HandleRendered(rendered))

	state := review.previews["flow"]
	require.NotNil(t, state.img)
	require.Empty(t, state.err)
	require.False(t, review.renderInFlight)
	require.True(t, review.previewIsCurrent("flow", review.Request.Diagrams[0].Source))
	require.True(t, review.showsPicture(0), "the expanded diagram should draw the picture")
}

func TestStaleRenderTicksAreDropped(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, calls := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	stale := review.renderGen

	// More typing supersedes the first request.
	review.requestPreview(0, "flowchart TD\n    A --> C")
	require.NotEqual(t, stale, review.renderGen)

	require.Nil(t, review.HandleRenderTick(stale), "the superseded tick must not render")
	require.Zero(t, calls.Load())

	require.NotNil(t, review.HandleRenderTick(review.renderGen))
}

func TestOnlyOneRenderRunsAtATime(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	review.SetPreviewRenderer(func(context.Context, string) ([]byte, error) {
		return tinyPNG(t), nil
	})

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	require.NotNil(t, review.HandleRenderTick(review.renderGen))
	require.True(t, review.renderInFlight)

	// A second tick while one is in flight must not start a browser.
	review.requestPreview(1, review.Request.Diagrams[1].Source)
	require.Nil(t, review.HandleRenderTick(review.renderGen),
		"a render already running blocks the next one")
}

func TestPendingWorkResumesAfterARenderFinishes(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	review.SetPreviewRenderer(func(context.Context, string) ([]byte, error) {
		return tinyPNG(t), nil
	})

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	first := review.HandleRenderTick(review.renderGen)
	require.NotNil(t, first)

	// While that runs, the user moves to the other diagram.
	review.requestPreview(1, review.Request.Diagrams[1].Source)

	rendered, ok := first().(UltraplanRenderedMsg)
	require.True(t, ok)
	require.NotNil(t, review.HandleRendered(rendered),
		"finishing a render should pick the queued one back up")
	require.False(t, review.renderInFlight)
}

func TestFailedRenderIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, _ := recordingRenderer(nil, errors.New("mmdc exploded"))
	review.SetPreviewRenderer(renderer)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	msg := review.HandleRenderTick(review.renderGen)()
	review.HandleRendered(msg.(UltraplanRenderedMsg))

	state := review.previews["flow"]
	require.Nil(t, state.img)
	require.Contains(t, state.err, "mmdc exploded")
	require.Contains(t, review.previewStatus(review.Styles, "flow", 200), "mmdc exploded")
	require.Empty(t, review.previewLines("flow"), "a failed render draws no picture")
}

func TestUndecodableImageIsReported(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, _ := recordingRenderer([]byte("not an image"), nil)
	review.SetPreviewRenderer(renderer)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	msg := review.HandleRenderTick(review.renderGen)()
	review.HandleRendered(msg.(UltraplanRenderedMsg))

	require.Contains(t, review.previews["flow"].err, "could not be decoded")
}

func TestEmptyImageIsReported(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, _ := recordingRenderer(nil, nil)
	review.SetPreviewRenderer(renderer)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	msg := review.HandleRenderTick(review.renderGen)()
	review.HandleRendered(msg.(UltraplanRenderedMsg))

	require.Contains(t, review.previews["flow"].err, "no image")
}

func TestEditingInvalidatesThePreview(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, _ := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)
	review.SetPreviewCapabilities(false, 10, 20, false)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	msg := review.HandleRenderTick(review.renderGen)()
	runCmd(review.HandleRendered(msg.(UltraplanRenderedMsg)))
	before := review.previews["flow"].sourceHash
	require.True(t, review.showsPicture(0))

	// The same source must not re-render.
	require.Nil(t, review.requestPreview(0, review.Request.Diagrams[0].Source),
		"an unchanged diagram is already drawn")

	// A different source must.
	review.ApplyEdit("flow", "flowchart LR\n    A --> C")
	require.NotNil(t, review.requestPreview(0, review.Request.Diagrams[0].Source))
	require.NotEqual(t, before, sourceHash(review.Request.Diagrams[0].Source))
	require.False(t, review.previewIsCurrent("flow", review.Request.Diagrams[0].Source),
		"the picture no longer matches the edited source")
	require.False(t, review.showsPicture(0),
		"the list must fall back to source rather than draw a stale picture")
}

func TestPreviewIDIsVersionedBySource(t *testing.T) {
	t.Parallel()

	a := previewID("flow", sourceHash(reviewFlow))
	b := previewID("flow", sourceHash("flowchart LR\n    A --> C"))
	require.NotEqual(t, a, b, "the image cache key must change with the source")
}

func TestExpandedDiagramPrefersThePicture(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, previewDiagrams(t))
	renderer, _ := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)
	review.SetPreviewCapabilities(false, 10, 20, false)

	review.requestPreview(0, review.Request.Diagrams[0].Source)
	msg := review.HandleRenderTick(review.renderGen)()
	runCmd(review.HandleRendered(msg.(UltraplanRenderedMsg)))

	require.True(t, review.expanded[0])
	require.True(t, review.showsPicture(0), "a freshly rendered diagram draws its picture")

	// Toggling to source swaps the picture for the text.
	press(t, review, "v")
	require.True(t, review.showSource)
	require.False(t, review.showsPicture(0), "the toggle puts the source back")

	// And the source is what the list then holds.
	var shown string
	for _, line := range review.buildLines(100) {
		shown += line.text + "\n"
	}
	require.Contains(t, shown, "flowchart TD")
}

func TestViewToggleDoesNothingWithoutARenderer(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	press(t, review, "v")
	require.False(t, review.showSource, "there is no picture to toggle away from")
}

func TestSourceStageHeightIsBounded(t *testing.T) {
	t.Parallel()

	review, _ := newTestReview(t, twoDiagrams())
	review.SetMaxHeight(30)
	press(t, review, "e")
	review.sourceEditor.SetValue("flowchart TD\n" + longBody(60))
	review.revalidateSource()

	require.LessOrEqual(t, review.Height(100), review.heightBudget())
	require.Positive(t, review.Height(100))
}

func longBody(lines int) string {
	var b bytes.Buffer
	for i := range lines {
		b.WriteString("    N")
		b.WriteByte(byte('a' + i%26))
		b.WriteString(" --> M\n")
	}
	return b.String()
}

func TestConcurrentRenderResultsDoNotRace(t *testing.T) {
	t.Parallel()

	// The render command runs off the UI goroutine, but its result is
	// folded in on the UI goroutine. This guards the shape of that
	// contract: results applied in sequence never leave the review in a
	// half-updated state.
	review, _ := newTestReview(t, twoDiagrams())
	renderer, _ := recordingRenderer(tinyPNG(t), nil)
	review.SetPreviewRenderer(renderer)
	review.SetPreviewCapabilities(false, 10, 20, false)

	var wg sync.WaitGroup
	results := make([]UltraplanRenderedMsg, 0, 4)
	for i := range 4 {
		review.requestPreview(0, reviewFlow)
		cmd := review.HandleRenderTick(review.renderGen)
		if cmd == nil {
			continue
		}
		wg.Add(1)
		go func(int) {
			defer wg.Done()
			msg := cmd().(UltraplanRenderedMsg)
			results = append(results, msg)
		}(i)
		wg.Wait()
		review.HandleRendered(results[len(results)-1])
	}
	require.NotNil(t, review.previews["flow"].img)
}
