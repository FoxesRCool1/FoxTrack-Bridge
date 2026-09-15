---
description: Build a mental model of this repo using the local model, cheaply
argument-hint: <optional focus area>
allowed-tools: Bash, Read, Grep, Glob
---

Orient yourself in this codebase. Focus: $ARGUMENTS

Use `/ask-local` for the reading. Do not read files yourself during this phase
unless ask-local's answer is ambiguous and one targeted read resolves it.

Ask it, in separate calls:
1. purpose, stack, entry points, how to build and run
2. top directories, what each contains, the most important file in each
3. anything specific to the focus area above

Then write `.llm/map.md`: a compact map of the repo, no more than 40 lines.
This file is the thing you re-read at the start of future sessions instead of
re-exploring. Report three lines to the user, not the map.
