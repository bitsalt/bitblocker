# Project File Generator — Agent Instructions

These instructions tell you how to create a new project file for the BitSalt sprint tracking system in Obsidian. Read them fully before doing anything else.

---

## Your Job

You will produce a single Markdown file that fits into an existing sprint tracking system. The output file should be ready to save directly into the Obsidian vault at:

`/home/jeff/projects/[Project-Name].md`

Note: file names should use hyphens rather than spaces to be easily compatible with Linux systems 

---

## Step 1 — Establish the Build Plan

Two scenarios:

**If a finalized build plan has been provided:** Use it. Do not re-derive it. If anything is ambiguous, ask one clarifying question before proceeding.

**If a spec document has been provided but no plan:** Read the spec, then produce a proposed build plan before generating the project file. The plan should include:
- A short summary of what is being built and why
- A breakdown of work into logical phases or milestones
- An honest assessment of unknowns or dependencies that could affect sequencing

Present the plan and wait for approval or adjustments. Do not generate the project file until the plan is confirmed.

---

## Step 2 — Map the Plan to Sprints

Once the plan is confirmed, break it into two-week sprints using these rules:

- Each sprint should have a clear, achievable goal — one sentence describing what "done" looks like for that sprint
- Tasks within a sprint should be discrete and completable in a single sitting where possible
- If a task has a dependency on something outside the project (AWS setup, third-party API access, another person's work), flag it explicitly in the task row
- Do not over-pack sprints. It is better to have a lighter sprint and carry tasks forward than to set up a sprint that cannot realistically be completed in 10–20 hours of side work
- Carry-over between sprints is expected and normal — there is a carry-over log at the bottom of the file for that purpose

Sprint start dates follow the established schedule:
- Sprint 1: Apr 21
- Sprint 2: May 5
- Sprint 3: May 19
- Sprint 4: Jun 2
- Sprint 5: Jun 16
- (Continue in two-week increments as needed)

Pick up from the next available sprint start date unless told otherwise.

---

## Step 3 — Generate the Project File

Use the template below exactly. Do not add sections that aren't in the template. Do not remove sections. Fill in every field.

---

## Template

```markdown
# [Project Name]

> **Back to:** [[BitSalt Projects]]
> **Started:** [date]
> **Status:** 🟡 In progress
> **One-liner:** [Single sentence — what this project is and who it's for]

---

## Overview

[2–4 sentences. What is being built, why it matters, and what done looks like at the end of the project. Written for someone who hasn't read the spec.]

---

## Milestones

| Milestone | Target sprint | Status |
|---|---|---|
| [Milestone name] | Sprint X | ⬜ |

---

## Sprint [N] — [Start date] to [End date]

**Goal:** [One sentence — what does done look like for this sprint?]

| Task | Status | Notes |
|---|---|---|
| [Task] | ⬜ | [Dependency or note, if any] |

---

[Repeat sprint section for each sprint in the plan]

---

## Decisions Log

Use this section to record decisions made during the project and the reasoning behind them. Add entries as they happen.

| Date | Decision | Reasoning |
|---|---|---|
| | | |

---

## Open Questions

| Question | Owner | Status |
|---|---|---|
| | | |

---

## Carry-over Log

| Task | Original sprint | Moved to | Reason |
|---|---|---|---|
| | | | |

---

## Notes
```

---

## Status Key

Use these consistently throughout the file:

- ⬜ Not started
- 🟡 In progress
- 🔵 In review
- ✅ Done
- ⏸ Blocked

---

## After Generating the File

Tell the user:
1. The filename to use when saving (should match the project name exactly as it appears in `[[BitSalt Projects]]`)
2. Whether any sprint dates overlap with projects already in the system — if you have context on active projects, flag potential schedule conflicts
3. Any open questions or dependencies you identified that the user should resolve before Sprint 1 begins
