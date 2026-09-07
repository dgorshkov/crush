package ultraplan

import (
	"fmt"
	"strings"
)

// This is a structural validator, not a Mermaid parser. Mermaid's real
// grammar lives in a JavaScript runtime we do not ship, so instead we
// check the things that actually go wrong when a language model writes
// a diagram: a missing or misspelled header, an unclosed subgraph or
// block, unbalanced brackets and quotes, and the reserved-word traps
// that render as a broken diagram rather than an error.
//
// It is deliberately conservative. Checks that could reject valid
// Mermaid are scoped to the diagram types where the syntax in question
// is structural, so free-text-heavy types (sequence notes, gantt task
// names, mindmap nodes) are not policed for brackets they are allowed
// to contain.

// diagramType describes one Mermaid diagram type for validation.
type diagramType struct {
	// name is the canonical keyword, reported as the diagram kind.
	name string

	// beta reports whether Mermaid also accepts (or has accepted) a
	// "-beta" suffixed spelling. Mermaid graduates diagrams out of
	// beta on its own schedule and keeps the old spelling working, so
	// both forms are accepted for every type that ever had one.
	beta bool

	// brackets says which delimiters carry syntax in this diagram
	// type, and so which ones are worth balance-checking.
	brackets bracketMode

	// blockOpeners are keywords that open a block closed by "end".
	blockOpeners []string
}

// bracketMode selects which delimiters a diagram type is checked for.
//
// Balance checking is only safe where the delimiter is structural. A
// sequence note, a gantt task name and a state transition label all
// take arbitrary free text, so counting brackets there would reject
// valid diagrams. Entity-relationship cardinality is written with
// braces ("||--|{"), which rules out brace counting for erDiagram.
type bracketMode int

const (
	// bracketsNone skips delimiter checking entirely.
	bracketsNone bracketMode = iota

	// bracketsBraces checks only {} pairs, used where braces open a
	// block but square brackets and parentheses may appear in labels.
	bracketsBraces

	// bracketsAll checks [], () and {} pairs plus double quotes.
	bracketsAll
)

// diagramTypes lists the Mermaid diagram types this validator knows
// about, as of Mermaid 11.17. Both the bare and "-beta" spellings are
// accepted for every entry marked beta.
var diagramTypes = []diagramType{
	{name: "flowchart", brackets: bracketsAll, blockOpeners: []string{"subgraph"}},
	{name: "graph", brackets: bracketsAll, blockOpeners: []string{"subgraph"}},
	{name: "sequenceDiagram", blockOpeners: []string{"alt", "opt", "loop", "par", "critical", "rect", "box", "break"}},
	{name: "classDiagram", brackets: bracketsBraces},
	{name: "stateDiagram", brackets: bracketsBraces},
	{name: "erDiagram"},
	{name: "journey"},
	{name: "gantt"},
	{name: "pie"},
	{name: "quadrantChart"},
	{name: "requirementDiagram", brackets: bracketsBraces},
	{name: "gitGraph"},
	{name: "mindmap"},
	{name: "timeline"},
	{name: "zenuml"},
	{name: "sankey", beta: true},
	{name: "xychart", beta: true},
	{name: "block", beta: true, brackets: bracketsAll},
	{name: "packet", beta: true},
	{name: "kanban"},
	{name: "architecture", beta: true},
	{name: "radar", beta: true},
	{name: "treemap", beta: true},
	{name: "treeView", beta: true},
	{name: "swimlane", beta: true},
	{name: "eventModeling", beta: true},
	{name: "venn", beta: true},
	{name: "ishikawa", beta: true},
	{name: "wardley", beta: true},
	{name: "cynefin", beta: true},
	{name: "C4", brackets: bracketsAll},
	{name: "info"},
}

// aliases maps alternate spellings onto a canonical type name. Keys
// are lowercase; lookupType lowercases before consulting this map.
var aliases = map[string]string{
	"flowchart-v2":    "flowchart",
	"flowchart-elk":   "flowchart",
	"classdiagram-v2": "classDiagram",
	"statediagram-v2": "stateDiagram",
	"c4context":       "C4",
	"c4container":     "C4",
	"c4component":     "C4",
	"c4dynamic":       "C4",
	"c4deployment":    "C4",
	"swimlanes":       "swimlane",
	"requirement":     "requirementDiagram",
}

// flowchartOrientations are the direction tokens a flowchart or graph
// header may carry.
var flowchartOrientations = []string{"tb", "td", "bt", "rl", "lr", "v", "^"}

// lookupType resolves a header keyword to a known diagram type,
// accepting the "-beta" spelling for types that have one and a small
// set of alternate spellings.
func lookupType(keyword string) (diagramType, bool) {
	lower := strings.ToLower(keyword)
	if canonical, ok := aliases[lower]; ok {
		lower = strings.ToLower(canonical)
	}
	base := strings.TrimSuffix(lower, "-beta")
	for _, dt := range diagramTypes {
		name := strings.ToLower(dt.name)
		if lower == name {
			return dt, true
		}
		if dt.beta && base == name {
			return dt, true
		}
	}
	return diagramType{}, false
}

// DetectKind returns the canonical Mermaid diagram type of a source,
// or an empty string when the header is missing or unrecognized.
func DetectKind(source string) string {
	header, _, err := splitHeader(source)
	if err != nil {
		return ""
	}
	dt, ok := lookupType(headerKeyword(header))
	if !ok {
		return ""
	}
	return dt.name
}

// StripFence removes a surrounding Markdown code fence from a Mermaid
// source. Models habitually wrap diagrams in ```mermaid even when told
// not to, and a fence is never part of the diagram, so stripping it is
// always the right call.
func StripFence(source string) string {
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")

	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[start]), "```") {
		return source
	}

	end := len(lines) - 1
	for end > start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end <= start || strings.TrimSpace(lines[end]) != "```" {
		return source
	}
	return strings.Join(lines[start+1:end], "\n")
}

// ValidateMermaid checks that a Mermaid source is structurally sound.
// A nil return does not promise Mermaid will render it, only that none
// of the failures this validator knows how to spot are present.
func ValidateMermaid(source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("source is empty")
	}
	if strings.Contains(source, "```") {
		return fmt.Errorf("source contains a Markdown code fence: pass the Mermaid source on its own, without ```mermaid")
	}

	header, body, err := splitHeader(source)
	if err != nil {
		return err
	}

	keyword := headerKeyword(header)
	dt, ok := lookupType(keyword)
	if !ok {
		return fmt.Errorf(
			"first line must declare a diagram type, got %q; use one of: %s",
			keyword, strings.Join(typeNames(), ", "),
		)
	}

	if len(body) == 0 {
		return fmt.Errorf("%s declares no content: add at least one statement below the header", dt.name)
	}

	if err := checkHeaderArgs(dt, header); err != nil {
		return err
	}
	if err := checkBlocks(dt, body); err != nil {
		return err
	}
	if dt.brackets == bracketsAll {
		if err := checkQuotes(body); err != nil {
			return err
		}
	}
	if err := checkBrackets(dt.brackets, body); err != nil {
		return err
	}
	if dt.name == "flowchart" || dt.name == "graph" {
		if err := checkFlowchartReservedWords(body); err != nil {
			return err
		}
	}
	return nil
}

// sourceLine is a body line paired with its 1-based number in the
// original source, so errors can point at the right place.
type sourceLine struct {
	num  int
	text string
}

// splitHeader returns the diagram header line and the significant body
// lines, having skipped YAML front matter, init directives, comments
// and blank lines.
func splitHeader(source string) (header string, body []sourceLine, err error) {
	raw := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")

	i := 0
	// Skip YAML front matter, which carries title and config.
	for i < len(raw) && strings.TrimSpace(raw[i]) == "" {
		i++
	}
	if i < len(raw) && strings.TrimSpace(raw[i]) == "---" {
		i++
		closed := false
		for ; i < len(raw); i++ {
			if strings.TrimSpace(raw[i]) == "---" {
				i++
				closed = true
				break
			}
		}
		if !closed {
			return "", nil, fmt.Errorf("front matter opened with --- is never closed")
		}
	}

	for ; i < len(raw); i++ {
		line := strings.TrimSpace(raw[i])
		if line == "" || isComment(line) {
			continue
		}
		header = line
		i++
		break
	}
	if header == "" {
		return "", nil, fmt.Errorf("source has no diagram declaration")
	}

	for ; i < len(raw); i++ {
		line := strings.TrimSpace(raw[i])
		if line == "" || isComment(line) {
			continue
		}
		body = append(body, sourceLine{num: i + 1, text: line})
	}
	return header, body, nil
}

// isComment reports whether a trimmed line is a Mermaid comment or an
// init directive.
func isComment(line string) bool {
	return strings.HasPrefix(line, "%%")
}

// headerKeyword returns the first token of the header line, with any
// trailing punctuation removed. Flowcharts may write "flowchart TD" or
// "graph LR:", and mindmaps may write the keyword alone.
func headerKeyword(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimRight(fields[0], ":;")
}

// checkHeaderArgs validates the arguments a diagram header may carry.
// Only flowcharts have an argument worth checking: a bad orientation
// silently renders top-down instead of failing.
func checkHeaderArgs(dt diagramType, header string) error {
	if dt.name != "flowchart" && dt.name != "graph" {
		return nil
	}
	fields := strings.Fields(header)
	if len(fields) < 2 {
		return nil
	}
	orientation := strings.ToLower(strings.TrimRight(fields[1], ":;"))
	for _, valid := range flowchartOrientations {
		if orientation == valid {
			return nil
		}
	}
	return fmt.Errorf(
		"%s orientation %q is not valid: use TB, TD, BT, RL or LR",
		dt.name, fields[1],
	)
}

// checkBlocks verifies that every block opened in the body is closed
// by a matching "end".
func checkBlocks(dt diagramType, body []sourceLine) error {
	if len(dt.blockOpeners) == 0 {
		return nil
	}

	var open []sourceLine
	for _, line := range body {
		fields := strings.Fields(line.text)
		if len(fields) == 0 {
			continue
		}
		word := strings.ToLower(strings.TrimRight(fields[0], ":"))
		if word == "end" && len(fields) == 1 {
			if len(open) == 0 {
				return fmt.Errorf("line %d: \"end\" closes a block that was never opened", line.num)
			}
			open = open[:len(open)-1]
			continue
		}
		for _, opener := range dt.blockOpeners {
			if word == strings.ToLower(opener) {
				open = append(open, line)
				break
			}
		}
	}

	if len(open) > 0 {
		last := open[len(open)-1]
		return fmt.Errorf(
			"line %d: %q is never closed; add a matching \"end\" (%d block(s) still open)",
			last.num, firstWord(last.text), len(open),
		)
	}
	return nil
}

// checkQuotes reports a line holding an odd number of double quotes.
func checkQuotes(body []sourceLine) error {
	for _, line := range body {
		if strings.Count(line.text, `"`)%2 != 0 {
			return fmt.Errorf("line %d: unbalanced double quote in %q", line.num, truncate(line.text, 60))
		}
	}
	return nil
}

// bracketPair describes one delimiter pair for balance checking.
type bracketPair struct {
	open, close rune
	name        string
}

var (
	bracePair    = bracketPair{'{', '}', "brace"}
	allPairs     = []bracketPair{{'[', ']', "square bracket"}, {'(', ')', "parenthesis"}, bracePair}
	bracesOnly   = []bracketPair{bracePair}
	noPairsAtAll []bracketPair
)

// pairsFor returns the delimiter pairs to check for a bracket mode.
func pairsFor(mode bracketMode) []bracketPair {
	switch mode {
	case bracketsAll:
		return allPairs
	case bracketsBraces:
		return bracesOnly
	default:
		return noPairsAtAll
	}
}

// checkBrackets reports unbalanced delimiters across the body,
// ignoring anything inside double quotes. Counting spans the whole
// body rather than one line at a time because a brace block
// legitimately runs across lines.
func checkBrackets(mode bracketMode, body []sourceLine) error {
	pairs := pairsFor(mode)
	if len(pairs) == 0 {
		return nil
	}

	watched := make(map[rune]bracketPair, len(pairs)*2)
	for _, p := range pairs {
		watched[p.open] = p
		watched[p.close] = p
	}

	counts := map[rune]int{}
	lastOpen := map[rune]int{}
	for _, line := range body {
		inQuotes := false
		prev := rune(0)
		for _, r := range line.text {
			if r == '"' {
				inQuotes = !inQuotes
				prev = r
				continue
			}
			if inQuotes {
				prev = r
				continue
			}
			// Mermaid's asymmetric node shape opens with ">" and closes
			// with "]", as in "A>flag]". Recognise it only at bracket
			// depth zero and directly after an id character, so an
			// arrow ("-->") and a comparison inside a label
			// ("A[x >= y]") are both left alone.
			if mode == bracketsAll && r == '>' && counts['['] == 0 && isIDChar(prev) {
				counts['[']++
				lastOpen['['] = line.num
				prev = r
				continue
			}
			p, ok := watched[r]
			if !ok {
				prev = r
				continue
			}
			if r == p.open {
				counts[p.open]++
				lastOpen[p.open] = line.num
				prev = r
				continue
			}
			counts[p.open]--
			if counts[p.open] < 0 {
				return fmt.Errorf(
					"line %d: closing %q with no matching %q",
					line.num, string(p.close), string(p.open),
				)
			}
			prev = r
		}
	}

	for _, p := range pairs {
		if n := counts[p.open]; n > 0 {
			return fmt.Errorf(
				"line %d: %d unclosed %s(s); every %q needs a matching %q",
				lastOpen[p.open], n, p.name, string(p.open), string(p.close),
			)
		}
	}
	return nil
}

// flowchartArrows are the link operators that separate node ids in a
// flowchart statement. Order matters: splitOperands rewrites them one
// at a time, so a longer operator must come before any operator that
// is a prefix or substring of it.
var flowchartArrows = []string{
	"<-.->", "<-->", "<==>",
	"-.->", "<-.-", "<==", "==>", "<--", "-->",
	"~~~", "===", "---", "-.-",
	"--o", "--x", "o--", "x--",
}

// checkFlowchartReservedWords catches the lowercase "end" node id,
// which Mermaid silently mis-parses into a broken flowchart rather
// than reporting an error.
func checkFlowchartReservedWords(body []sourceLine) error {
	for _, line := range body {
		if !hasArrow(line.text) {
			continue
		}
		for _, operand := range splitOperands(line.text) {
			if operand == "end" {
				return fmt.Errorf(
					"line %d: \"end\" is reserved in flowcharts and breaks rendering when used as a node id; rename it (for example to \"End\" or \"finish\")",
					line.num,
				)
			}
		}
	}
	return nil
}

// hasArrow reports whether a flowchart line contains a link operator.
func hasArrow(line string) bool {
	for _, arrow := range flowchartArrows {
		if strings.Contains(line, arrow) {
			return true
		}
	}
	return false
}

// splitOperands returns the bare node ids on a flowchart statement,
// stripped of their shape brackets and edge labels.
func splitOperands(line string) []string {
	// Drop edge labels, which sit between pipes and may hold anything.
	var b strings.Builder
	inLabel := false
	for _, r := range line {
		if r == '|' {
			inLabel = !inLabel
			b.WriteRune(' ')
			continue
		}
		if !inLabel {
			b.WriteRune(r)
		}
	}
	work := b.String()

	for _, arrow := range flowchartArrows {
		work = strings.ReplaceAll(work, arrow, "\x00")
	}

	var ids []string
	for _, part := range strings.Split(work, "\x00") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Keep only the id, which precedes any shape bracket.
		if idx := strings.IndexAny(part, "[({"); idx >= 0 {
			part = part[:idx]
		}
		part = strings.TrimSpace(part)
		if part != "" {
			ids = append(ids, part)
		}
	}
	return ids
}

// typeNames returns the canonical diagram type names for use in error
// messages.
func typeNames() []string {
	names := make([]string, 0, len(diagramTypes))
	for _, dt := range diagramTypes {
		names = append(names, dt.name)
	}
	return names
}

// isIDChar reports whether r can appear in a Mermaid node id.
func isIDChar(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	return fields[0]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
