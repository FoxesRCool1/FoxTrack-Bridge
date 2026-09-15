---
description: Turn a plan into a local-model batch, run it, verify, report once
argument-hint: <path to plan markdown>
allowed-tools: Read, Write, Edit, Bash, Grep, Glob
---

Plan source: $ARGUMENTS

Run this end to end. Do not check in with the user between steps.

## 1. Orient cheaply

Read `.llm/map.md` and `.llm/recipe.md` if they exist.

For anything you still need to know about the codebase, use `/ask-local`.
Do not read files yourself to explore. Read a file directly only when you are
about to write a spec that depends on its exact contents, and then read only
that file.

## 2. Build the batch

Write `.llm/plan.json` following `.llm/plan.example.json`.

- One task per target file, ordered by dependency.
- Each spec states exact signatures, exact field names and types, and numbered
  acceptance criteria. Conventions belong in the recipe, never repeated here.
- `context` is 2 to 4 files, and must include one that demonstrates the pattern
  being followed. A pattern example beats three paragraphs of prose.
- `check` is the narrowest command that actually proves the task.
- Anything on the recipe's hard-boundary list you write yourself. Keep it out
  of the plan entirely.

Self-check before running: if a spec is longer than the file it describes, that
task is a bad delegation candidate. Pull it out and write it yourself.

## 3. Run it

```
git add -A
python3 ~/.claude/scripts/llm-run.py .llm/plan.json
```

Do not interrupt it. It handles retries, gate failures and critique locally.

## 4. Read the digest, not the diffs

- **green**: accept. Do not open the diff. The gates, the check command and the
  critic already covered it. Opening green diffs is the single biggest way to
  waste what this workflow exists to save.
- **amber**: `llm-run.py --show <id>` and review that one properly.
- **red**: the file was reverted. Read `history` and `last_feedback`. Fix the
  spec and rerun with `--only <id>`, or implement it yourself. Do not run the
  same plan a third time hoping for a different result.

## 5. Verify the whole

Run the project's full gate once over everything. Fix what breaks. This catches
cross-file problems the per-task checks cannot see.

## 6. Report

Ten lines maximum: what was built by file, green/amber/red counts and wall time,
anything you implemented yourself and why, anything the user should look at, and
what you would change about the recipe next time. No code, no diffs.
