# Workspaces and login: task tracker

Design: docs/workspaces-auth-design.md. Thread: 2196ade6 in #agentchat.
Maya's decisions (2026-09-04) are section 13 of the design. A workspace is a room.
One file per task. Status is one of: todo, in-progress, review, deployed, done.
"done" means deployed to prod and verified in a real browser.
Every task ships on its own deploy. Never batch two tasks into one deploy.
Each deploy has its own rollback target (design section 7): 01 to 23, 03 to 24, 04 to 25.
"Deploy N" is the task 04 deploy (the backfill); "deploy N+1" is the task 08 deploy.
`AGENTCHAT_REGISTRATION_ENABLED=false` on the mini from the task 01 deploy until task 04
has run its backfill and verification queries.

| # | Task | Status |
|---|---|---|
| 01 | Auth provider interface, password provider, sessions | done |
| 02 | Login, registration and settings UI | done |
| 03 | Room users schema, session room entry, room creation requires login | done (a42c2ca) |
| 04 | User migration (humans to users, default password developer) | done |
| 05 | Workspace switcher | done |
| 06 | Skill (humans section), harness guides, cli.sh proof, fleet and prod docs | done |
| 07 | Final e2e pass, Clerk stub, completeness critic | done (9535abf) |
| 08 | Deploy N+1: retire legacy human tokens | done (84faafa) |
| 09 | One settings place: Workspace + Personal | done (b9cc1d6) |
| 10 | "Invite member" in the workspace menu | done (240c9f4) |
| 11 | Workspace avatars | done |
| 12 | Workspace rail (Discord style) | done |
| 13 | Per-workspace badges in the rail | done |
| 14 | Delete workspace | done |
| 15 | Kick members | done |
| 16 | Polish pass (rail, settings nav, slug from name, sidebar button) | done (5c8dc0c) |
| 20 | Channel rename (admins) with a system entry, boot state, tab title, sidebar polish, cli.sh --body-file | done |
| 25 | Delivery receipts + offline inbox (AgentRelay study) | done (3ed4173) |
| 26 | Workspace and channel TTL (AgentRelay study) | reversed (9d6fd3a shipped, removed same day, Maya) |
| 27 | Typed capability registry -> MCP surface (AgentRelay study) | done |
| 17 | Invite links replace invite codes | done |
| 18 | Rail order per user, unread counter badges, workspace mute, favicon + title badge | done |
| 19 | Agents belong to a human | done |
| 21 | Agents go offline and catch up on return | done |
| 22 | Reminders for agents (after 21) | done |
| 23 | Instant, whole workspace switching (after 18) | done |
| 24 | Icon design pass, shadcn/Lucide set, own deploy | done (3f85b30) |
| 28 | Channel sections: a real default section, drag order inside one, per human | review |
| 29 | Thread reply updates the root's reply footer live, no reload | review |
| 30 | Ack is a 👀 reaction, not a text line; watcher prints the ack command | review |
| 31 | Rename step 1: dual-read env vars, client dir and watcher hooks | done (555d4b1) |
| 33 | Rename step 2: hard cut to OpenChatter, no fallbacks, prod relabelled | review |
| 32 | Explicit acknowledgements: `ac ack`, per-recipient ack rows, watcher nag | review |

Tasks 09-15 are the feature queue Maya gave on 2026-09-04 (roots 948f9802 and dd69d0b2, process note 1a7ee18d
in #agentchat): one at a time, each with subagent review, browser e2e with screenshots, its own deploy and a
prod check, then a done line in thread 1a7ee18d.
