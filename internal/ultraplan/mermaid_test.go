package ultraplan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMermaidValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		kind   string
	}{
		{
			name: "flowchart",
			source: `flowchart TD
    A[Start] --> B{Ready?}
    B -->|yes| C[Ship it]
    B -->|no| A`,
			kind: "flowchart",
		},
		{
			name: "graph with lowercase orientation",
			source: `graph lr
    a --> b`,
			kind: "graph",
		},
		{
			name: "flowchart with subgraphs",
			source: `flowchart LR
    subgraph api [API layer]
        H[handler] --> S[service]
    end
    subgraph store [Storage]
        S --> DB[(SQLite)]
    end`,
			kind: "flowchart",
		},
		{
			name: "sequence diagram with blocks",
			source: `sequenceDiagram
    participant U as User
    participant A as Agent
    U->>A: propose plan
    alt accepted
        A->>U: start implementing
    else changes requested
        A->>U: revise diagrams
    end`,
			kind: "sequenceDiagram",
		},
		{
			name: "sequence note with unbalanced bracket in free text",
			source: `sequenceDiagram
    A->>B: hello
    Note over A,B: see foo(bar for the "why`,
			kind: "sequenceDiagram",
		},
		{
			name: "state diagram v2",
			source: `stateDiagram-v2
    [*] --> Drafting
    Drafting --> Accepted: all diagrams accepted
    Accepted --> [*]`,
			kind: "stateDiagram",
		},
		{
			name: "class diagram with body braces",
			source: `classDiagram
    class Plan {
        +Status status
        +Diagram[] diagrams
        +AllAccepted() bool
    }`,
			kind: "classDiagram",
		},
		{
			name: "er diagram",
			source: `erDiagram
    SESSION ||--o| PLAN : has
    PLAN ||--|{ DIAGRAM : contains`,
			kind: "erDiagram",
		},
		{
			name: "front matter and init directive",
			source: `---
title: Ultraplan flow
---
%%{init: {"theme": "dark"}}%%
flowchart TD
    A --> B`,
			kind: "flowchart",
		},
		{
			name: "comments are ignored",
			source: `flowchart TD
    %% this explains the next line
    A --> B`,
			kind: "flowchart",
		},
		{
			name: "beta suffix accepted",
			source: `architecture-beta
    group api(cloud)[API]`,
			kind: "architecture",
		},
		{
			name: "bare form of a beta type accepted",
			source: `packet
    0-15: "Source Port"`,
			kind: "packet",
		},
		{
			name: "quoted brackets do not unbalance",
			source: `flowchart TD
    A["array[0] lookup"] --> B["fn(x)"]`,
			kind: "flowchart",
		},
		{
			name: "mindmap free text",
			source: `mindmap
  root((plan))
    validity
    acceptance`,
			kind: "mindmap",
		},
		{
			name: "gantt with colons",
			source: `gantt
    title Rollout
    dateFormat YYYY-MM-DD
    section Build
    Validator :a1, 2026-01-01, 3d`,
			kind: "gantt",
		},
		{
			name: "asymmetric flag node shape",
			source: `flowchart TD
    A>flag] --> B
    C>another] --> D`,
			kind: "flowchart",
		},
		{
			name: "comparison inside a label is not a flag shape",
			source: `flowchart TD
    A[x >= y] --> B[count > 0]`,
			kind: "flowchart",
		},
		{
			name: "capitalized End node id is fine",
			source: `flowchart TD
    Start --> End`,
			kind: "flowchart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, ValidateMermaid(tt.source))
			require.Equal(t, tt.kind, DetectKind(tt.source))
		})
	}
}

func TestValidateMermaidInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{
			name:    "empty",
			source:  "   \n  ",
			wantErr: "source is empty",
		},
		{
			name:    "code fence",
			source:  "```mermaid\nflowchart TD\n  A --> B\n```",
			wantErr: "Markdown code fence",
		},
		{
			name:    "no diagram type",
			source:  "A[Start] --> B[End]",
			wantErr: "must declare a diagram type",
		},
		{
			name: "misspelled diagram type",
			source: `flowchrt TD
    A --> B`,
			wantErr: "must declare a diagram type",
		},
		{
			name:    "header only",
			source:  "flowchart TD",
			wantErr: "declares no content",
		},
		{
			name: "bad orientation",
			source: `flowchart SIDEWAYS
    A --> B`,
			wantErr: "orientation",
		},
		{
			name: "unclosed subgraph",
			source: `flowchart TD
    subgraph one
        A --> B`,
			wantErr: "never closed",
		},
		{
			name: "stray end",
			source: `flowchart TD
    A --> B
    end`,
			wantErr: "never opened",
		},
		{
			name: "unclosed sequence block",
			source: `sequenceDiagram
    A->>B: hi
    alt happy path
        B->>A: hello`,
			wantErr: "never closed",
		},
		{
			name: "unbalanced bracket",
			source: `flowchart TD
    A[Start --> B[End]`,
			wantErr: "unclosed square bracket",
		},
		{
			name: "stray closing bracket is still caught",
			source: `flowchart TD
    A --> B]`,
			wantErr: `closing "]" with no matching "["`,
		},
		{
			name: "unbalanced quote",
			source: `flowchart TD
    A["Start] --> B`,
			wantErr: "unbalanced double quote",
		},
		{
			name: "reserved end node id",
			source: `flowchart TD
    Start --> end`,
			wantErr: "reserved in flowcharts",
		},
		{
			name: "unterminated front matter",
			source: `---
title: nope
flowchart TD
    A --> B`,
			wantErr: "front matter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateMermaid(tt.source)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestStripFence(t *testing.T) {
	t.Parallel()

	fenced := "```mermaid\nflowchart TD\n    A --> B\n```"
	require.Equal(t, "flowchart TD\n    A --> B", StripFence(fenced))

	bare := "flowchart TD\n    A --> B"
	require.Equal(t, bare, StripFence(bare))

	// An unterminated fence is left alone so the validator can report
	// it rather than silently mangling the source.
	require.Equal(t, "```mermaid\nflowchart TD", StripFence("```mermaid\nflowchart TD"))
}

func TestDetectKindUnknown(t *testing.T) {
	t.Parallel()
	require.Empty(t, DetectKind("not a diagram"))
	require.Empty(t, DetectKind(""))
}
