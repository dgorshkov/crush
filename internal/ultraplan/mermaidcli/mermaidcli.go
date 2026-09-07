// Package mermaidcli renders Mermaid diagrams to PNG by shelling out
// to the Mermaid CLI (mmdc).
//
// Rendering is optional. Mermaid's renderer is a browser engine, so
// there is no way to draw a faithful diagram from Go alone, and crush
// ships as a single binary with no Node runtime. Where mmdc happens to
// be installed the review can show the real picture; where it is not,
// callers fall back to the diagram source. Nothing here is on a path
// that must succeed.
package mermaidcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBinary is the command the Mermaid CLI installs.
	DefaultBinary = "mmdc"

	// BinaryEnv overrides which binary is run, for installations that
	// wrap or rename mmdc.
	BinaryEnv = "CRUSH_MERMAID_CLI"

	// PuppeteerConfigEnv points at a Puppeteer config file. Container
	// and CI environments generally need one carrying --no-sandbox.
	PuppeteerConfigEnv = "CRUSH_MERMAID_PUPPETEER_CONFIG"

	// DefaultTimeout bounds a single render. mmdc starts a headless
	// browser, so a cold first run is slow, but a preview that never
	// returns is worse than one that gives up.
	DefaultTimeout = 20 * time.Second

	// maxStderr caps how much CLI output is quoted back in an error.
	maxStderr = 600

	// waitDelay bounds how long a killed CLI may keep its output pipe
	// open before the pipe is closed out from under it.
	waitDelay = 2 * time.Second
)

// ErrUnavailable is returned when no Mermaid CLI can be found.
var ErrUnavailable = errors.New("the Mermaid CLI (mmdc) is not installed")

// lookPath is indirected so tests can control binary discovery.
var lookPath = exec.LookPath

// Binary returns the Mermaid CLI command to run, honouring the
// override environment variable.
func Binary() string {
	if custom := strings.TrimSpace(os.Getenv(BinaryEnv)); custom != "" {
		return custom
	}
	return DefaultBinary
}

// Available reports whether a Mermaid CLI can be found on PATH. It is
// called when a review opens rather than per keystroke, so a PATH scan
// each time is cheap enough and picks up an install made mid-session.
func Available() bool {
	_, err := lookPath(Binary())
	return err == nil
}

// Options controls how a diagram is rendered.
type Options struct {
	// Theme is a Mermaid theme name: "dark", "default", "forest",
	// "neutral". Callers pass the one matching the terminal.
	Theme string

	// Background is a CSS colour or "transparent".
	Background string

	// Width is the render width in pixels. Height follows the
	// diagram's own aspect ratio.
	Width int

	// Scale multiplies the output resolution. Terminal cells are
	// coarse, so rendering above the display size and letting the
	// image code downsample reads better than rendering at size.
	Scale int

	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
}

// withDefaults fills in the options a caller left unset.
func (o Options) withDefaults() Options {
	if o.Theme == "" {
		o.Theme = "dark"
	}
	if o.Background == "" {
		o.Background = "transparent"
	}
	if o.Width <= 0 {
		o.Width = 1200
	}
	if o.Scale <= 0 {
		o.Scale = 2
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	return o
}

// Render turns Mermaid source into PNG bytes.
//
// It returns ErrUnavailable when no CLI is installed, so callers can
// tell "not set up" apart from "this diagram would not draw".
func Render(ctx context.Context, source string, opts Options) ([]byte, error) {
	if strings.TrimSpace(source) == "" {
		return nil, errors.New("source is empty")
	}

	binary := Binary()
	resolved, err := lookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %q not found on PATH", ErrUnavailable, binary)
	}

	opts = opts.withDefaults()

	dir, err := os.MkdirTemp("", "crush-mermaid-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create a temporary directory: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck

	input := filepath.Join(dir, "diagram.mmd")
	output := filepath.Join(dir, "diagram.png")
	if err := os.WriteFile(input, []byte(source), 0o600); err != nil {
		return nil, fmt.Errorf("failed to write the diagram source: %w", err)
	}

	args := []string{
		"--input", input,
		"--output", output,
		"--outputFormat", "png",
		"--theme", opts.Theme,
		"--backgroundColor", opts.Background,
		"--width", strconv.Itoa(opts.Width),
		"--scale", strconv.Itoa(opts.Scale),
		"--quiet",
	}
	if cfg := strings.TrimSpace(os.Getenv(PuppeteerConfigEnv)); cfg != "" {
		args = append(args, "--puppeteerConfigFile", cfg)
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, resolved, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Killing the CLI on the deadline is not enough on its own: Run
	// also waits for the stderr pipe to close, and mmdc's headless
	// browser is a grandchild holding that pipe open. Without a wait
	// delay a timed-out preview blocks for as long as the browser
	// lives, which is the hang the timeout exists to prevent.
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("the Mermaid CLI did not finish within %s", opts.Timeout)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the Mermaid CLI failed: %w%s", err, quoteStderr(stderr.String()))
	}

	png, err := os.ReadFile(output)
	if err != nil {
		return nil, fmt.Errorf("the Mermaid CLI wrote no image%s", quoteStderr(stderr.String()))
	}
	if len(png) == 0 {
		return nil, fmt.Errorf("the Mermaid CLI wrote an empty image%s", quoteStderr(stderr.String()))
	}
	return png, nil
}

// quoteStderr formats CLI output for an error message, trimmed and
// capped so a stack trace cannot swamp the UI.
func quoteStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxStderr {
		s = s[:maxStderr] + "…"
	}
	// Collapse to one line: this lands in a single-line status row.
	s = strings.Join(strings.Fields(s), " ")
	return ": " + s
}
