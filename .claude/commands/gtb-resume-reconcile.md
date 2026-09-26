---
description: Reconcile code against the GTB status/checkpoint (after an interrupted session), then resume and finish the phase
argument-hint: "[optional extra instructions]"
---

Resume GTB implementation from `spec/gtb-implementation.md`. The previous session may have been interrupted before it updated the status register or packet checkpoint, so reconcile before building anything new.

## 1. Reconcile

1. Read the resume pointer, phase status register and transition log, then open the active phase packet in `spec/phases/<id>.md` and its checkpoint. If the pointer looks stale, also check the packet of the next phase in dependency order.
2. Gather the actual state: `git status`, `git diff --stat`, untracked files, and `git log` since the last phase commit.
3. Walk each scope item and exit criterion in the packet and classify it:
   - **done and recorded**
   - **done but not recorded**: code, migration, contract or test exists but the checkpoint doesn't mention it
   - **recorded but not done or broken**: the checkpoint claims it but the tree doesn't support it
   - **partial**: started but incomplete
4. Don't trust recorded check results. Re-run the relevant checks (`make check`, plus the live checks the packet names) to establish the current truth.
5. Watch for applied migrations: if an unrecorded migration was already applied to the local database, don't edit it; make any fix in a new migration.
6. Update the packet checkpoint (and the status register, resume pointer and transition log if they are wrong) to reflect the reconciled state, citing only checks that actually ran. Summarize the discrepancies you found in one short list.

If the tree is ambiguous (for example, half-written work whose intent you can't infer from the packet), don't guess. Checkpoint what you know, then stop and ask.

## 2. Resume

1. Consult only the foundation sections the packet names.
2. Implement the next bounded item, run the relevant checks, and update the packet checkpoint. Repeat until every scope item and exit criterion in the packet is done.
3. Finish per the working protocol: phase walkthrough (commands run), evidence, completion handoff, status register, transition log, resume pointer.

You are expected to complete the entire phase (the whole packet file). Do not stop midway.

Exception: if at any point you are stuck or need clarification, do not assume. Checkpoint the packet, then stop and ask.

$ARGUMENTS
