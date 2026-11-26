# Diagrams in plans

Read this before drawing a diagram in a plan. Use one only when a picture says it faster than prose, and pick the mermaid type from **what you need to explain**, not habit: a flowchart is the wrong picture for a schema, a schedule or a trade-off. Every type below renders on the board (mermaid 12), and large diagrams get an expand view.

## Pick the type

| To explain | Use | Put it in |
|---|---|---|
| Logic, branches, a pipeline | `flowchart LR` (`TD` when it is tall and narrow) | `## Flow` |
| Calls between services, agents, the board, over time | `sequenceDiagram` | `## Flow` |
| A status or lifecycle (plan status, job states) | `stateDiagram-v2` | `## Flow` |
| Tables, columns, relations, stored shapes (JSON, file formats) | `erDiagram` | `## Data model` |
| Types, interfaces and who implements them | `classDiagram` | `## Data model` or Key changes |
| Components and how they connect | `architecture-beta`; across systems and people: `C4Context` / `C4Container` | Proposed approach |
| Layout of parts (screen regions, modules side by side) | `block-beta` | Proposed approach |
| A binary or wire format, field by field | `packet-beta` | Key changes |
| Phases and their order or overlap | `gantt` (dates or relative days) or `timeline` | Phases, Rollout and rollback |
| Branches, releases, a migration across versions | `gitGraph` | Rollout and rollback |
| Breaking the scope down | `mindmap` | Goal and non-goals |
| Options placed on two axes (effort vs impact, risk vs value) | `quadrantChart` | Decisions or Risks |
| What a user goes through, step by step, and how it feels | `journey` | Goal or Testing |
| Requirements traced to the tests that verify them | `requirementDiagram` | Testing and verification |
| Where volume goes (requests, data, cost) | `sankey-beta` | Current state |
| Numbers: benchmarks, before and after | `xychart-beta` (bar or line) | Current state or Testing |

Not for plans: `pie` (a table is clearer), `kanban` (the board is the kanban), `radar-beta` and `treemap-beta` (rarely the clearest answer for a code change).

## Rules

- One idea per diagram, and only what the change touches. An ER diagram shows the touched entities and their direct neighbours; a sequence shows the participants of this flow, not the whole system.
- Mark what is new or changed: `"new"` on an ER or class field, a `classDef new` on flowchart nodes, `Note over` in a sequence.
- Keep labels to a few words. Long labels wrap into tall boxes; put the detail in the text under the diagram.
- Write one line under each diagram saying what to look at.
- Two small diagrams beat one that needs scrolling. Over about 15 nodes, split it.
- Leave styling to the board: no `%%{init}%%` themes or colours. The board colours nodes and entities itself, in light and dark.
- Quote labels that contain punctuation: `A["write id.yml"]`, `B{"still open?"}`.

## Examples

Short, working starting points. Copy the one you need and change the content.

**Flowchart** (logic and branches)

```mermaid
flowchart LR
  A[ticket change] --> B{files on?}
  B -->|no| C[nothing]
  B -->|yes| D["write id.yml"]:::new
  classDef new stroke-dasharray: 4 3
```

**Sequence** (calls over time)

```mermaid
sequenceDiagram
  participant U as Reviewer
  participant B as Board
  participant A as Agent
  U->>B: approve plan v3
  A->>B: get_plan
  B-->>A: approved
  Note over A: builds from v3
```

**State** (a lifecycle)

```mermaid
stateDiagram-v2
  [*] --> awaiting
  awaiting --> approved: human approves
  awaiting --> draft
  approved --> implemented
  approved --> awaiting: new version
  implemented --> [*]
```

**ER** (tables and stored shapes)

```mermaid
erDiagram
  TASKS ||--o{ COMMENTS : has
  TASKS {
    text id PK
    int plan_updated "new"
  }
  COMMENTS {
    int id PK
    text task_id FK
    text quote "new: passage anchor"
  }
```

**Class** (types and interfaces)

```mermaid
classDiagram
  class PlanSink {
    <<interface>>
    +Save(recs) error
    +Remove(rec) error
  }
  PlanSink <|.. supabaseSink
  PlanSink <|.. notionSink : later
```

**Architecture** (components)

```mermaid
architecture-beta
  group app(cloud)[agent kanban]
  service api(server)[MCP and API] in app
  service db(database)[kanban db] in app
  service sb(internet)[Supabase]
  api:R -- L:db
  api:B -- T:sb
```

**C4 context** (systems and people)

```mermaid
C4Context
  Person(dev, "Developer")
  System(board, "agent-kanban")
  System_Ext(agent, "Coding agent")
  Rel(dev, board, "reviews plans")
  Rel(agent, board, "MCP")
```

**Block** (layout of parts)

```mermaid
block-beta
  columns 3
  plans["Plans rail"] plan["Plan"]:1 toc["On this page"]
```

**Packet** (a wire format)

```mermaid
packet-beta
  0-15: "source port"
  16-31: "destination port"
  32-63: "sequence number"
```

**Gantt** (phases)

```mermaid
gantt
  dateFormat YYYY-MM-DD
  section Phase 1
  Export files :p1, 2026-12-01, 5d
  section Phase 2
  Read edits back :after p1, 7d
```

**Timeline** (order without dates)

```mermaid
timeline
  v1 : read-only export
  v2 : two-way edits
  v3 : conflict handling
```

**Git graph** (branches and releases)

```mermaid
gitGraph
  commit id: "v0.4"
  branch plans
  commit id: "plan mirror"
  commit id: "plan date"
  checkout main
  merge plans tag: "v0.5"
```

**Mindmap** (scope)

```mermaid
mindmap
  root((Ticket files))
    In scope
      YAML export
      Sub-tasks
    Later
      Two-way edits
    Out
      Ticket order
```

**Quadrant** (options on two axes)

```mermaid
quadrantChart
  title Options
  x-axis Low effort --> High effort
  y-axis Low value --> High value
  quadrant-1 Plan carefully
  quadrant-2 Do first
  quadrant-3 Skip
  quadrant-4 Avoid
  Read-only export: [0.3, 0.7]
  Two-way sync: [0.8, 0.8]
```

**Journey** (a user's steps)

```mermaid
journey
  title Reviewing a plan
  section Read
    Open the plan: 4: Reviewer
    Find the diagram: 3: Reviewer
  section Decide
    Comment on a step: 4: Reviewer
    Approve: 5: Reviewer
```

**Requirement** (traced to tests)

```mermaid
requirementDiagram
  requirement export {
    id: R1
    text: "every open ticket has a file"
    risk: medium
    verifymethod: test
  }
  element exportTest {
    type: "go test"
  }
  exportTest - verifies -> export
```

**Sankey** (where volume goes)

```mermaid
sankey-beta
Requests,Board UI,600
Requests,MCP,300
MCP,get_plan,120
```

**XY chart** (numbers)

```mermaid
xychart-beta
  title "Render time (ms)"
  x-axis [before, after]
  bar [420, 180]
```
