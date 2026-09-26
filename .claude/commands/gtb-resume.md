---
description: Resume the active GTB phase from its checkpoint and finish the whole phase
argument-hint: "[optional extra instructions]"
---

Resume GTB implementation from `spec/gtb-implementation.md`.

1. Read the resume pointer and phase status register, then open the active phase packet in `spec/phases/<id>.md` and its checkpoint.
2. Run `git status` and confirm the working tree matches what the checkpoint says. Trust the checkpoint; do not audit already-recorded work. If you see an obvious mismatch (uncommitted files the checkpoint doesn't mention, or a checkpoint claiming work that isn't in the tree), stop and recommend `/gtb-resume-reconcile` instead of guessing.
3. Consult only the foundation sections the packet names.
4. Implement the next bounded item, run the relevant checks, and update the packet checkpoint. Repeat until every scope item and exit criterion in the packet is done.
5. Finish per the working protocol: evidence, completion handoff, status register, transition log, resume pointer.

You are expected to complete the entire phase (the whole packet file). Do not stop midway.

Exception: if at any point you are stuck or need clarification, do not assume. Checkpoint the packet, then stop and ask.

$ARGUMENTS
