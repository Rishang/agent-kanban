---
name: kanban-plan
description: Write the implementation plan for a coding task as markdown/MDX, attach it to its task on the agent-kanban board with set_plan, and wait for a human to approve it on the board before writing code. Use when the user is about to start a feature, refactor, migration or larger bug fix and wants a plan, spec or design first, or says "plan this out", "spec before we code", "plan first", "don't start coding yet". Works with the kanban skill; not for non-software plans.
---

# Kanban plans: approve before building

For a feature, refactor, migration or any change the user would want to see before code is written ("plan this", "spec first", or more than a few files), attach an implementation plan to the task and get it approved on the board. Plans are markdown or MDX; the board renders mermaid and code blocks, keeps versions and shows the status on the card. If your tools don't include `set_plan` (older server), post the plan as a `start_task` note and ask for approval in chat instead.

**Who writes what.** The plan's *content* is a file you write with your own tools; its *state* (status, approval, version) belongs to the board and changes only through MCP. `get_board` tells you where plans live:

- `plans: repo files in .agent-kanban/plans/` → the `.mdx` in the repo is the spec, synced both ways. Edit it directly; the board picks up every change as a new version. Call `set_plan id path=…` once to link a new file to its task. Never send `body=`, and never edit the front matter `status:` line: the board writes it.
- `plans: on the board` → write the file anywhere (`.agent-kanban/plans/` is a good home) and send it with `set_plan id path=…` after every revision; the board keeps its own copy.

1. **Investigate** the code and earlier plans: `list_plans q=<topic>` (BM25-ranked, with each plan's one-line description) or `list_plans regex=…` finds related or superseded plans. Note real paths, symbols and consumers of anything you change. Ask the user at most two questions, only ones that change the plan's shape; everything else becomes a stated assumption.
2. **Write** `.agent-kanban/plans/<task-id>-<kebab-title>.mdx` using the outline below, then `set_plan id path=.agent-kanban/plans/…` (`body=` only if you can't write files). It becomes a version with status `awaiting`.
3. **Ask for review:** give the user the task url and a three-line summary (goal, approach, biggest risk). Don't paste the plan. A human approves it on the board (Approve button) or edits it there; you can't set `approved`.
4. **Wait.** Build nothing until `get_plan` says `approved`. If the user replies in chat, call `get_plan` again: they may have edited it, set it back to `draft`, or answered in a comment (`get_task`). If they approve in chat but the status isn't `approved`, ask them to click Approve.
5. **Revise** by editing the file (and, on the board mode, `set_plan path=` again). Any content change is a new version and waits for approval again.
6. **Build** from the approved plan; its steps can become checklist items or sub-tasks. When the code forces a departure, add a row to the plan's deviation log and right away call `set_plan id path=… status=implemented-deviations`: a build status sent with the change keeps the approval instead of asking for it again.
7. **Finish:** `set_plan id status=implemented` (or `implemented-deviations`), then `finish_task`. A never-approved plan can't be marked implemented: ask for approval first. Mark a plan that was replaced `superseded`.

If `set_plan` says the file changed in the repo, re-read the file and redo your edit.

Outline (drop sections that don't earn their place; keep the headings so reviewers can scan). The board's plan reader shows it like an html-plan-report page: plans rail, the plan, and an "On this page" rail built from the `##` headings.

````mdx
---
status: awaiting
---
# <Task title, as a plain-language goal>

<!-- The first paragraph is the lede: one or two sentences on what this gets the user. -->
Get … so that …

<Stats>
<Stat label="Overall size" value="M" note="5 steps" />
<Stat label="Files" value="7" note="2 new, 5 edited" />
<Stat label="Risk" value="Low" note="read path only" />
<Stat label="Breaking change" value="No" note="same API" />
</Stats>

<Decisions>
## Decisions for you

<Decision id="D1" title="Where do offline edits live?">
<Option label="SQLite queue" recommended>Survives restarts; one new dependency.</Option>
<Option label="AsyncStorage">Pick this if: edits stay small and rare.</Option>
</Decision>
</Decisions>

## Recommendation
<!-- 2–4 sentences: what to build and the biggest risk. -->

## Goal and non-goals

## Current state
<!-- How it works today with real paths; numbered findings marked (verified) or (inferred). -->

## Proposed approach
<!-- The approach, a ```mermaid diagram when it helps, rejected options with one-line reasons. -->
<Callout kind="weak" title="Weakest part of this plan:">…</Callout>

## Implementation steps
<Steps>
<Step id="S1" size="S" title="Imperative step title">What changes, files, and how to verify it.</Step>
<Step id="S2" size="M" title="…">…</Step>
</Steps>

## File change map
| File | Change | Step |

## Key changes
<!-- Contract changes: APIs, schema/migrations, shared types, config/env, CLI flags, and implicit contracts (log formats, error strings, ordering). Signatures, not bodies. -->

## Testing and verification
<!-- Each test names the step IDs it proves; every step appears. -->

## Risks and mitigations
| Risk | Likelihood | Impact | Steps |
|---|---|---|---|
| … | <Badge kind="ok">Low</Badge> | <Badge kind="warn">Medium</Badge> | S1 |

## Assumptions and open questions

## Deviation log
<!-- Added once building starts: what changed, why, which step. -->
````

**Components** the board renders (anything else in JSX is dropped; `import`/`export` lines and front matter are stripped):

| Component | Use |
|---|---|
| `<Stats>` with `<Stat label value note />` | The size/files/risk strip under the lede |
| `<Decisions>`, `<Decision id title decided?>`, `<Option label recommended?>` | The decisions card. Your pick first, with `recommended`; once the user picks, add `decided="Skip. Keep the default…"` |
| `<Callout kind title>` | `kind`: `changed` (what changed since the last version), `weak` (weakest part), `risk`, `ok`, `info` |
| `<Steps>` with `<Step id size title>` | Numbered implementation steps with S/M/L sizes |
| `<Badge kind>` | Inline `ok`, `warn`, `risk` or `info` pill, e.g. likelihood and impact |

Keep a blank line between a component's opening tag and any markdown block inside it. After a revision, add a `<Callout kind="changed" title="What changed since version N:">` under the stats.

**Decisions:** zero to four (more means split the task), each with 2–4 options, your pick first with `recommended`. Alternatives say "Pick this if: …". The user answers on the board (comment or edit), not in the terminal; record the answer with `decided=` as a new version.

**Small fixes** (about three files, no real design choice): keep only the lede, current vs expected behaviour, the fix as a ```` ```diff ````, tests (including unchanged behaviour) and risks. **Optional sections** when they earn their place: Phases, Rollout and rollback (migrations, flags, anything a revert won't undo), Sources (anything outside the repo), and a Changelog from version 2.

**HTML instead of MDX:** if the html-plan-report skill is installed and the user wants HTML pages, write the plan with that skill into `.agent-kanban/plans/` (not its default `.plan/`; say so in its `overwrite.md`). Run its `validate_plan.py` on the page and `update_index.py .agent-kanban/plans` (that writes the `_assets/` the board inlines), then `set_plan id path=.agent-kanban/plans/<task-id>-<task>.plan.html`. Skip its plan-mode check, terminal questions and go-ahead: the board's Approve button replaces them. In the page keep `data-status` at `awaiting` (or `draft`); the board ignores any later status a page claims. The board never rewrites an HTML page, so after every revision or deviation row call `set_plan path=…` again (with `status=implemented-deviations` for a deviation row). Change status alone with `set_plan id status=…` and no path: re-sending an edited page is a new version, which `implemented` refuses.

Write real paths and plain words, say which claims you verified, and include at least one risk the user wouldn't have thought of. Before asking for review, check that every decision is a real judgment call, non-goals are things someone might reasonably expect, steps are small enough to verify one at a time, and Key changes match the size of the change (signatures for a module; an impact map and at most three risky snippets for 15+ files).
