---
name: kanban
description: Track and update work on the project's local kanban board (agent-kanban MCP server). Use when starting, progressing, finishing or planning tasks, when the user mentions the board, tickets, a task id like KB-12, backlog, todo or "what should I work on", or asks to create, move, comment on or close a task.
---

# Local Kanban workflow

Boards for every project live in one local database served by `agent-kanban`; you reach the current project's board through the `kanban` MCP tools (the MCP URL's `?project=` picks the project). Humans watch it live in the browser, so keep it truthful.

## The loop: use the board proactively

For any non-trivial coding work, keep the board in step with what you are doing. Four calls cover it:

1. **`next_task`** (or `my_tasks` when resuming): what to work on and why. If the user asks for work that has no task yet, create it first.
2. **`start_task id note`**: assigns you, moves it to in progress, posts your plan. Call it *before* you start coding.
3. **`update_progress id done note add`**: tick checklist items (by index or text), add new ones, post a short note on decisions or blockers.
4. **`finish_task id summary link`**: what changed and how it was verified, plus the PR link; the task goes to review for a human. Use `done: true` only if `get_board` lists `+finish` under permissions.

A task that should not be done at all (duplicate, obsolete, out of scope, won't fix) is **closed**, not finished. If your tools include `close_task` and `get_board` lists `+close`, call `close_task id reason`: it posts the reason and moves the task to the closed column (`role=closed`). Otherwise add a comment with your reasoning and leave the decision to a human; don't move it to done. Done and closed tasks both count as no longer open.

**Sub-tasks** (if `create_task` accepts `parent`): break a larger task into sub-tasks one level deep with `parent: KB-20`; a sub-task can't have its own. `list_tasks` marks sub-tasks `↳KB-20` and parents `{sub-tasks 1/4}`, and `get_task` lists them. Work on the sub-tasks, not the parent: `next_task` skips a parent while any sub-task is open, and you can't finish it until they are all done or closed.

**Plans** (if your tools include `set_plan`): use the `kanban-plan` skill, if installed, for any planning; it has the full workflow and outline. For work the user wants to review before code is written, attach an implementation plan with `set_plan` (a `.md`/`.mdx` file in the project, or `body`). It waits as `awaiting` until a human approves it on the board; build only when `get_plan` says `approved`. You can't approve your own plan. `list_plans` lists and searches them (`q` ranked by BM25, `regex`, `status`). When `get_board` says plans are repo files, edit the `.mdx` directly and change status only with `set_plan`.

`get_board` once per session tells you the columns (with roles and WIP limits), labels, members and your permissions. Inputs are forgiving: columns and labels by name in any case, ids as `12` or `KB-12`, assignee `me`. If a call fails, the error lists the valid values; fix and retry once.

Due dates are set by humans in the UI; the tools have no due field. `link` (set by `finish_task`, changed by `update_task`) is a related http(s) URL: PR, issue, doc. Write tools reply with a one-line status, not the whole task. Call `get_task` only when you need the description or comments; `list_tasks` is one line per task.

## Creating tasks

- `create_task` (or `create_tasks` to break a feature into 2–8 steps) with a short imperative `title`, a markdown `description` (goal, approach, acceptance criteria) and optional `checklist` items.
- Use only existing labels. If you need a new one, say so in a comment; humans manage labels in Settings.
- Search first (`list_tasks q=...`) to avoid duplicates.
- Descriptions and comments are GitHub-flavoured markdown: fenced code blocks are syntax-highlighted, ```` ```mermaid ```` renders diagrams and ```` ```svg ```` renders an image. Use them when a diagram explains more than prose.

## Human-only tasks and links

- Tasks a human marked **human-only** are not yours: `list_tasks` hides them (it reports how many) and every write to them fails. Don't work around it.
- Tool results include the task url (e.g. `http://127.0.0.1:7777/#my-app/KB-12`); `list_tasks` gives the pattern once as `task url = …<id>`. Include it when you tell the user about a task.

## Etiquette

- One task at a time in progress per agent; respect column WIP limits shown in `get_board`.
- Don't rewrite a human's description; add a comment instead.
- If a call returns "permission denied", don't retry. Tell the user what you wanted to do.
- `delete_task` only when the user explicitly asks.
