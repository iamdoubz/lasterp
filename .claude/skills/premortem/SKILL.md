---
name: premortem
description: Run a premortem on any plan, launch, product, hire, strategy, or decision. Imagines it failed 6 months from now, works backward to find every reason why, then produces a revised plan. Triggers include "premortem this", "premortem my", "what could kill this", "stress test this plan", "find the blind spots", "poke holes in this", "where will this break", "am I missing anything", "what could go wrong", "future-proof this", "devil's advocate this".
---

# Premortem

The opposite of a postmortem. Imagine the plan already failed and work backward to find why, before you start.

- Method: Gary Klein, *Harvard Business Review*, 2007.
- Kahneman called it his single most valuable decision-making technique.
- Used by Google, Goldman Sachs, P&G.
- Mechanism: "what could go wrong?" produces hedged, polite answers. "This already failed, explain why" puts the brain into narrative mode and generates specific, honest causes. Wharton/Cornell call this "prospective hindsight."

Why it matters for AI assistance: Claude defaults to agreeable. Asking "is this a good plan?" gets reasons it's good. The premortem reframe forces honest failure analysis instead of polite risk assessment.

## When NOT to apply

- Vague ideas with no concrete plan yet (help plan first, then premortem)
- Questions with one right answer (just answer)
- Creative feedback on a draft (that's editing)
- Decisions already made and irreversible (premortem only helps when course correction is possible)
- Requests for multi-perspective decision support (use LLM Council instead — different mechanism, different output)
- Simple feedback or factual questions

## Step 1 — Gather minimum context

A premortem is only as good as its input. You need three things:

1. **What is it?** — describe the plan in one sentence
2. **Who is it for?** — audience, customer, team, stakeholders
3. **What does success look like?** — failure is the inverse

Scan first, ask second:

- Read the current conversation for context already provided
- Glob + Read the workspace for `CLAUDE.md`, any `memory/` folder, project briefs (~30 seconds max)
- If all three are clear, proceed
- If not, ask for the most important missing piece. One question at a time. Conversational, not a form.

## Step 2 — Set the premortem frame

Tell the user, naming the actual plan (not a placeholder):

> *"OK, premortem time. It's 6 months from now. The [actual plan: workshop / launch / hire / pricing change / etc.] has failed. It's done. Let's look back and figure out why."*

The "this has already failed" framing is the active mechanism. Without it, the analysis collapses back into polite risk assessment.

## Step 3 — Generate failure reasons

Run a single comprehensive pass. No prescribed categories, no lenses.

> *"This plan has failed 6 months from now. Generate every genuine reason it could have died. Be specific. Ground each reason in the actual details of the plan. Don't pad with weak reasons. Don't stop early if there are more."*

Each reason should be specific to this plan, grounded in details the user provided, and a real threat (not minor inconvenience or extreme edge case). Use whatever count is real for this plan — could be 4, could be 9. Don't force a number.

## Step 4 — Spawn deep-dive agents in parallel

For each failure reason, spawn one Task sub-agent. All in parallel — sequential spawning lets earlier outputs influence later ones.

Pass each agent the prompt body below as its task. Substitute the angle-bracket values with actual content before sending — do not pass the brackets through.

> You are an investigator in a premortem analysis. You've been assigned one specific failure reason to analyse in depth.
>
> THE PLAN: \<full context: what it is, who it's for, what success looks like, plus relevant workspace context\>
>
> PREMORTEM FRAME: It is 6 months from now. This plan has failed.
>
> YOUR ASSIGNED FAILURE REASON: \<the specific failure reason from step 3\>
>
> Your job: go deep on this one failure. Write the story of how it played out. Use details from the plan. Make it feel like a case study of something that actually happened.
>
> Output three sections:
>
> 1. **The failure story** — 2-3 paragraph narrative. Specific moments where things went wrong and why.
> 2. **The underlying assumption** — the one thing the user took for granted that made this failure possible. One sentence.
> 3. **Early warning signs** — 1-2 concrete, observable signals the user could watch for. Things you can see or measure, not vague feelings.
>
> Keep total under 300 words. Direct. No hedging. No sugarcoating.

## Step 5 — Synthesise

Read every deep-dive. Produce:

1. **Most likely failure** — which scenario is most probable given what's known. The one to focus on first.
2. **Most dangerous failure** — which would cause the most damage if it happened, even if less likely. The one worth insuring against.
3. **Hidden assumption** — across all analyses, the single biggest thing the user is taking for granted. Often where the real value of the premortem lives.
4. **Revised plan** — concrete changes that make the plan more resilient. Each maps to a specific failure scenario. Not "consider testing your pricing" — *"run a $47 pilot with 20 people before committing to $297 publicly."*
5. **Pre-launch checklist** — 3-5 specific things to verify, test, or put in place. Each prevents or detects one identified failure mode.

## Step 6 — Output

Generate a single self-contained HTML file: `premortem-report-[timestamp].html`. Synthesis at the top (it's what gets read first), one card per failure reason below showing the story / assumption / warning signs. Save and open.

In the chat, give a 3-sentence summary: most likely failure, hidden assumption, single most important revision. The HTML has the full detail.

## Example

**User:** *"premortem this — I'm launching a $297 live workshop on Claude Cowork for marketing teams. 50 seats. Targeting marketing managers at 10-50 person companies."*

**Failure reasons surface:**
1. Marketing managers at this company size need approval to spend $297 — friction not budgeted
2. Tool-specific pitch in a market still asking whether AI is relevant
3. Real buyers may be solopreneurs, not team managers
4. Demo environments with realistic marketing data and multi-seat setups need 5 weeks of prep, not 2
5. Solopreneur attendees produce reviews that don't resonate with target buyer
6. Max revenue $14,850 may not justify prep time vs. other opportunities

**Synthesis:** Audience mismatch is most likely. Solopreneur testimonials drifting the cohort away from the actual target buyer is most dangerous. Hidden assumption: "marketing managers at 10-50 person companies" is reachable, but those people don't self-identify that way and don't hang out in shared places. Revised plan: $47 pilot for 20 people first, identify who actually buys, then build the full workshop for whoever shows up.
