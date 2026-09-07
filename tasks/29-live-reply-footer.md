# 29 — The root's reply footer updates live (Chief, 642fe956)

Status: review

A reply landed in a thread and the root's "N replies · Last reply ..." footer in the
channel view did not move. It came back only on a full reload.

The DOM half already worked: a reply in the open channel calls `refreshRootBar`, which
refetches the root and swaps its node. Two gaps around the cached page made the fix
never stick.

- **`pageApply` ignored what a reply does to its root.** A reply never joins the
  channel page, so the cached copy of the root kept `reply_count: 0` forever. Every
  paint that reads the cache (a channel switch, a warm workspace switch, task 23) put
  the footer-less root back. It now bumps `reply_count`, `last_reply_at` and
  `replier_ids` on the cached root, guarded by a per-page set of applied reply ids so a
  replayed event cannot count twice.
- **`reconcilePage` called a stale footer "same".** It compared ids and bodies only, so
  a page whose only difference was the reply counts was judged unchanged and the
  repaint was skipped, even though the fresh list had just replaced the cache. It now
  compares `reply_count` and `last_reply_at` too.

Together those explain the exact report: the reply arrives while another channel is on
screen, nothing patches the DOM, coming back paints the stale cache, and reconcile
declines to repaint. A second switch away and back then paints the (by now fresh)
cache, which is why it looked like switching fixed it.

Check: `scripts/replycount-check.js` (REPLYCOUNT_CHECK_OK). It fails at step 6 without
the fix. Steps: footer appears on a first reply, count bumps with the thread pane open,
a second root's footer appears, and the reported case, a reply that lands while another
channel is open, with the footer asserted after the switch back and again after
reconcile settles.

## Review round

Three more holes the first pass left, all found in review:

- **Double count after a refetch.** The per-page `replies` Set only dedups against
  the same page object. A fetch already counts every reply up to the root's
  `last_reply_at`, and the feed cursors predate that fetch, so the first replayed
  reply after a boot or a warm counted twice. Guard is now the timestamp as well
  as the Set.
- **A deleted reply never came back down.** `message.deleted` carried only a
  message id, so no client could find the root. The event now carries
  `thread_root_id`; `pageApply` decrements the cached root and `applyEvent`
  refreshes its bar.
- The check grew step 8 (no double count after a re-entry) and step 9 (delete
  takes the footer down and it stays down). Step 9 fails on the unpatched build.
