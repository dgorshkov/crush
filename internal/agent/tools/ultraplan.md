Propose a plan as a set of Mermaid diagrams and block until the user has
ruled on every one of them. This is the tool that drives Ultraplan mode:
you and the user converge on a shared picture of the work before any of
it gets built.

## How it works

Each call submits the **complete diagram set**, not a delta. The set is
stored on the session and shown to the user, who can:

- accept a diagram as it stands,
- ask for changes in prose,
- edit the Mermaid source directly, or
- reply about the plan as a whole, including asking for a diagram that
  does not exist yet.

The call returns once they have finished reviewing. Read the result: it
says which diagrams are still open and why. Revise those, resubmit the
whole set, and repeat. A diagram you accepted last round stays accepted
as long as you resubmit its source byte for byte — change so much as a
node label and it goes back in front of the user.

The planning session ends only when every diagram is valid Mermaid **and**
the user has explicitly accepted all of them. At that point they are asked
whether to start implementing, and the result tells you their answer.

## Rules

- **Do not start implementing while a plan is open.** File edits are
  refused during a planning session, and the refusal is not a bug to work
  around.
- **Resubmit the full set every round.** A diagram you leave out is
  dropped from the plan. That is how you remove one deliberately.
- **Reuse an `id` to revise a diagram**; use a fresh `id` to add one. Ids
  are short, stable slugs like `request-flow` or `schema`.
- **Never wrap the source in a ```mermaid fence.** Pass the Mermaid source
  on its own.
- **Respect the user's edits.** If they rewrote a diagram, their version is
  authoritative. Build on it; do not revert it.
- Start small. One diagram that frames the shape of the work beats five
  that guess at detail nobody has agreed to yet. Add diagrams as the
  conversation earns them.
- At most 8 diagrams, 8000 characters of source each.

## Validation

Sources are checked before the user ever sees them. Invalid diagrams come
straight back to you and cost a round trip, so get these right:

- The first line must declare a diagram type: `flowchart`, `sequenceDiagram`,
  `classDiagram`, `stateDiagram-v2`, `erDiagram`, `gantt`, `mindmap`,
  `timeline`, `gitGraph`, `journey`, `quadrantChart`, `block-beta`,
  `architecture-beta`, and the rest of the Mermaid set.
- Brackets and quotes must balance. Quote any label containing brackets or
  punctuation: `A["fetch(url)"]`, not `A[fetch(url)]`.
- Every `subgraph` and every `alt`/`opt`/`loop`/`par` block needs its `end`.
- `end` is reserved in flowcharts. A node called `end` renders as a broken
  diagram, so call it `End` or `finish`.

The checker is structural, not a full Mermaid parser: it catches the
common breakages but does not guarantee a render. Keep diagrams simple and
conventional.

## What makes a good plan diagram

Diagrams are for the decisions, not the inventory. Each one should show
something a reader could disagree with.

- `flowchart` — control flow, the shape of a pipeline, decision points.
- `sequenceDiagram` — who calls whom, in what order, and what blocks.
- `erDiagram` / `classDiagram` — data model and ownership.
- `stateDiagram-v2` — lifecycle, and which transitions are legal.

Use `intent` on each diagram to say what you are asking the user to
confirm ("this puts validation in the tool, not the UI"). A diagram
without a claim is decoration.

## Example

```json
{
  "goal": "Add a rate limiter to the public API",
  "summary": "First cut: where the limiter sits and what happens on a rejection. The store choice is the open question.",
  "diagrams": [
    {
      "id": "request-path",
      "title": "Request path",
      "intent": "Limiter runs as middleware before auth, so unauthenticated floods are cheap to reject.",
      "mermaid": "flowchart LR\n    C[Client] --> L{Rate limit}\n    L -->|under limit| A[Auth]\n    L -->|over limit| R[\"429 + Retry-After\"]\n    A --> H[Handler]"
    }
  ]
}
```

## When to use

- The user asked to plan, design, or think something through first.
- The change is large or architectural enough that starting in the wrong
  place would be expensive.
- Ultraplan mode is already active for the session.

## When NOT to use

- Small, local, obvious changes. Just make them.
- Questions with a factual answer — read the code, or use the `question`
  tool for a genuine choice.
- Progress tracking during implementation. That is the `todos` tool.
