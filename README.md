<div align="center">

<img src="web/logo.svg" width="64" alt="local-kanban logo">

# local-kanban

**Hand your agent a ticket. Watch it ship.**

A local kanban board your coding agents work on alongside you: Claude Code, Codex, opencode, pi,
anything that speaks [MCP](https://modelcontextprotocol.io). One Go binary, one local database, zero cloud.

[Quick start](#quick-start) · [Demos](#see-it-work) · [▶ Full video](.github/videos/demo.mp4)

</div>

### 🤖 An agent takes the next task, end to end

![opencode picks SHOP-1, posts a plan, ticks the checklist live and moves it to Review](.github/images/agent.gif)

> One prompt: *"work on the next task from the board"*. opencode calls `next_task`, posts its plan,
> writes a failing test, fixes the bug, ticks each checklist item as it goes and hands the task to
> **Review** with a summary. You didn't touch the board; it updated live.

## Quick start

```sh
task build                       # → ./bin/local-kanban
cd ~/code/my-app && local-kanban # board at http://127.0.0.1:7777/#my-app
local-kanban skill install       # teaches Claude Code / Codex the board workflow in this project (--global: for all projects)
```

Connect an agent (inside the repo):

```sh
claude mcp add --transport http kanban "http://127.0.0.1:7777/mcp?agent=claude&project=my-app"
```

<details><summary>Codex, opencode, others</summary>

```toml
# ~/.codex/config.toml
[mcp_servers.kanban]
url = "http://127.0.0.1:7777/mcp?agent=codex&project=my-app"
```

```json
// opencode.json
{ "mcp": { "kanban": { "type": "remote", "url": "http://127.0.0.1:7777/mcp?agent=opencode&project=my-app" } } }
```

Anything else: `http://127.0.0.1:7777/mcp?agent=<name>&project=<project>`.
</details>

## See it work

### 🗂️ A board that reads like a doc

Drag cards between columns with WIP limits. Open one for markdown, syntax-highlighted code and
**mermaid diagrams**, right in the task.

![Drag a card, open a task with a mermaid diagram](.github/images/board.gif)

### 🔎 Find anything, across every project

`/` or `⌘K`. Ranked results with highlights, plus filters: `is:open` `@claude` `#bug` `prio:high` `project:docs`.

![Search across projects with filters](.github/images/search.gif)

### 📁 Every repo is a project

Run `local-kanban` in a repo and it joins the board, named after the git root. Switch projects in one click.

![Switching between projects](.github/images/projects.gif)

### 🎨 Make it yours, and set the rules

Light or dark, eleven accents, your own columns and labels. **Per-project permissions** decide what agents
may do, and tasks marked **human-only** are off limits to them.

![Themes, accents, members and agent permissions](.github/images/settings.gif)

<details><summary><b>How agents use it</b></summary>

The loop is four calls: `next_task` → `start_task` → `update_progress` → `finish_task`. Inputs are forgiving
(`"In Progress"`, `"#Bug"`, `12` or `SHOP-12`), and a wrong value comes back with the list of valid ones.
Every change lands in **Activity** with who did it and how (UI or MCP).
</details>

## Configuration

Everything lives in `~/.config/kanban/` (override with `LOCAL_KANBAN_CONFIG_PATH`):

| File | Holds |
|---|---|
| `kanban.db` | every project's board |
| `settings.json` | `addr` (default `127.0.0.1:7777`), `user`, `syncInterval` |
| `.env` | optional Turso Cloud sync: `LOCAL_KANBAN_TURSO_URL`, `LOCAL_KANBAN_TURSO_TOKEN` |

Precedence: flags > environment > `.env` > `settings.json`.

## Keyboard

`n` new task · `/` or `⌘K` search · `f` filter board · `p` projects · `,` settings · `⌘↵` save and close · `Esc` close

## Development

```sh
task dev    # server on :7788 with a throwaway config in ./tmp
task test   # also: task fmt, task lint, task build
```

The server listens on loopback only, rejects foreign `Host` headers and non-JSON posts, and sanitizes all markdown.
