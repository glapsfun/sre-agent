---
description: Answer a cloud cost question with the finops skill, with every number proved
argument-hint: <cost question>
---

Invoke the `finops` skill and start at step 1 (Scope) for the following question, exactly as the user
stated it:

$ARGUMENTS

If no question was provided, ask the user what cost question they want answered before doing anything
else. Follow the finops skill's steps and safety rules exactly. In particular: no billable call before the
budget is approved, and never change anything in a cloud or cluster.
