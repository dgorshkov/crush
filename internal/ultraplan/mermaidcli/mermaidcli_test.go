package mermaidcli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const flowSource = "flowchart TD\n    A --> B"

// fakeCLI writes a script standing in for mmdc and puts it on PATH.
// body is shell run with the same arguments the real CLI would get.
func fakeCLI(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLI is a shell script")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, DefaultBinary)
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755))

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(BinaryEnv, "")
	t.Setenv(PuppeteerConfigEnv, "")
}

// argWriter is a script fragment that records its arguments and writes
// a believable PNG to the --output path.
const argWriter = `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
printf '%s\n' "$@" > "$(dirname "$out")/args.txt"
printf '\211PNG\r\n\032\n fake image bytes' > "$out"
`

func TestAvailable(t *testing.T) {
	fakeCLI(t, "exit 0")
	require.True(t, Available())

	// An empty PATH cannot resolve the binary.
	t.Setenv("PATH", t.TempDir())
	require.False(t, Available())
}

func TestBinaryHonoursTheOverride(t *testing.T) {
	t.Setenv(BinaryEnv, "")
	require.Equal(t, DefaultBinary, Binary())

	t.Setenv(BinaryEnv, "my-mermaid")
	require.Equal(t, "my-mermaid", Binary())

	t.Setenv(BinaryEnv, "  spaced  ")
	require.Equal(t, "spaced", Binary())
}

func TestRenderReturnsThePNG(t *testing.T) {
	fakeCLI(t, argWriter)

	png, err := Render(t.Context(), flowSource, Options{})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(png), "\x89PNG"), "expected PNG bytes, got %q", string(png))
}

func TestRenderPassesTheExpectedArguments(t *testing.T) {
	fakeCLI(t, argWriter+`
cp "$(dirname "$out")/args.txt" "$ARGS_COPY"
`)
	argsCopy := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("ARGS_COPY", argsCopy)

	_, err := Render(t.Context(), flowSource, Options{Theme: "forest", Background: "white", Width: 900, Scale: 3})
	require.NoError(t, err)

	recorded, err := os.ReadFile(argsCopy)
	require.NoError(t, err)
	args := string(recorded)

	require.Contains(t, args, "--theme\nforest")
	require.Contains(t, args, "--backgroundColor\nwhite")
	require.Contains(t, args, "--width\n900")
	require.Contains(t, args, "--scale\n3")
	require.Contains(t, args, "--outputFormat\npng")
	require.NotContains(t, args, "--puppeteerConfigFile", "no config file unless one is configured")
}

func TestRenderPassesThePuppeteerConfigWhenSet(t *testing.T) {
	fakeCLI(t, argWriter+`
cp "$(dirname "$out")/args.txt" "$ARGS_COPY"
`)
	argsCopy := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("ARGS_COPY", argsCopy)
	t.Setenv(PuppeteerConfigEnv, "/etc/puppeteer.json")

	_, err := Render(t.Context(), flowSource, Options{})
	require.NoError(t, err)

	recorded, err := os.ReadFile(argsCopy)
	require.NoError(t, err)
	require.Contains(t, string(recorded), "--puppeteerConfigFile\n/etc/puppeteer.json")
}

func TestRenderWritesTheSourceForTheCLI(t *testing.T) {
	fakeCLI(t, `
out=""
in=""
prev=""
for a in "$@"; do
  case "$prev" in
    --output) out="$a" ;;
    --input) in="$a" ;;
  esac
  prev="$a"
done
cp "$in" "$SOURCE_COPY"
printf '\211PNG\r\n\032\n' > "$out"
`)
	sourceCopy := filepath.Join(t.TempDir(), "diagram.mmd")
	t.Setenv("SOURCE_COPY", sourceCopy)

	_, err := Render(t.Context(), flowSource, Options{})
	require.NoError(t, err)

	written, err := os.ReadFile(sourceCopy)
	require.NoError(t, err)
	require.Equal(t, flowSource, string(written))
}

func TestRenderWithoutTheCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(BinaryEnv, "")

	_, err := Render(t.Context(), flowSource, Options{})
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestRenderReportsCLIFailureWithItsOutput(t *testing.T) {
	fakeCLI(t, `
echo "Parse error on line 3: expecting SEMI" >&2
exit 1
`)

	_, err := Render(t.Context(), flowSource, Options{})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnavailable, "a failing render is not a missing install")
	require.Contains(t, err.Error(), "Parse error on line 3")
}

func TestRenderReportsAMissingImage(t *testing.T) {
	// Exits cleanly but writes nothing, which mmdc has been known to do
	// when its browser dies quietly.
	fakeCLI(t, "exit 0")

	_, err := Render(t.Context(), flowSource, Options{})
	require.ErrorContains(t, err, "wrote no image")
}

func TestRenderReportsAnEmptyImage(t *testing.T) {
	fakeCLI(t, `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
: > "$out"
`)

	_, err := Render(t.Context(), flowSource, Options{})
	require.ErrorContains(t, err, "empty image")
}

func TestRenderTimesOut(t *testing.T) {
	fakeCLI(t, "sleep 5")

	start := time.Now()
	_, err := Render(t.Context(), flowSource, Options{Timeout: 150 * time.Millisecond})
	require.ErrorContains(t, err, "did not finish within")
	require.Less(t, time.Since(start), 3*time.Second, "the timeout should cut the run short")
}

func TestRenderHonoursCallerCancellation(t *testing.T) {
	fakeCLI(t, "sleep 5")

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := Render(ctx, flowSource, Options{})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

func TestRenderRejectsEmptySource(t *testing.T) {
	fakeCLI(t, argWriter)

	_, err := Render(t.Context(), "   \n  ", Options{})
	require.ErrorContains(t, err, "source is empty")
}

func TestRenderLeavesNoTemporaryFiles(t *testing.T) {
	fakeCLI(t, argWriter)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "crush-mermaid-*"))
	require.NoError(t, err)

	_, err = Render(t.Context(), flowSource, Options{})
	require.NoError(t, err)

	after, err := filepath.Glob(filepath.Join(os.TempDir(), "crush-mermaid-*"))
	require.NoError(t, err)
	require.Len(t, after, len(before), "the temporary render directory should be removed")
}

func TestQuoteStderr(t *testing.T) {
	t.Parallel()

	require.Empty(t, quoteStderr("   \n "))
	require.Equal(t, ": one line", quoteStderr("one\n   line\n"))
	require.Contains(t, quoteStderr(strings.Repeat("x", maxStderr+50)), "…")
}

// TestFakeCLIIsActuallyUsed guards the whole suite: if the fake were
// never invoked these tests would silently pass against a real mmdc,
// or against nothing at all.
func TestFakeCLIIsActuallyUsed(t *testing.T) {
	fakeCLI(t, argWriter)

	resolved, err := exec.LookPath(DefaultBinary)
	require.NoError(t, err)
	require.NotEmpty(t, resolved)

	info, err := os.Stat(resolved)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0o111, "the fake CLI must be executable")
}
