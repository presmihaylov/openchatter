# 30 — The ack is a reaction, not a message (Chief, 332d9185)

Status: review

Pres's reason is the token budget: a one-line "Got it, starting on X" wakes every
participant in the thread, and the fleet posts one per ask.

- **The skill's "Acknowledge receipt when you are tagged" section** now says the ack is
  `ac react <id> 👀` at the front and `ac reactions <id> ✅` when done. Words only to
  refuse or to ask a question. The section is four lines where it was fourteen. The two
  other places that told agents to write a one-line ack (the reactions bullet, the
  threads-first list) now point at the reaction too.
- **The watcher template's REPLY-TO line carries the nudge**, so an agent reading the
  event never has to look the command up:

      REPLY-TO <root-id> in <channel>: <author>: <body> | ack: ac react <ask-id> 👀

  The id on the `ack:` half is the message that tagged you; the id on the `REPLY-TO`
  half is the thread root your answer goes in. They differ whenever the tag is a reply,
  and reacting on the root instead would put the 👀 on the wrong message.

Fleet agents pick the new line up when their watcher restarts; no coordination needed,
since the old line still works.

## Review round

- A third place still asked for a written ack (the Etiquette checklist), and the
  chat example still showed `ac reply <id> 'on it'`. Both rewritten, and
  `TestSkillDoc` now fails if either string comes back.
- `emit_hits` did not strip newlines from the body. Every consumer reads line by
  line, so a multi-line ask lost its ` | ack:` suffix entirely. The body now goes
  through `gsub("\n"; " ")`, like the REMINDER line already did.
- The bridge and injector prompts now name the ack command too; before, only
  AGENTS.md did.
