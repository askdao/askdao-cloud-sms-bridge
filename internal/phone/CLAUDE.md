# internal/phone/
> L2 | parent: ../../CLAUDE.md

Members
client.go: Client, the libgm subset the bridge uses (texts, reactions, media uploads), and NewLibgmClient (the production implementation)
state.go: Tracker deriving ok / phone_offline / browser_inactive / logged_out with a detail; Subscribe for status consumers
manager.go: Manager, connects with the saved session, maps libgm events to the Tracker and the inbound worker, caches conversations and SIMs, saves the session, reconnects after non-credential failures
pair.go: Pair, the Google-account pairing flow that writes the session file
inbound.go: Inbound and the inbox rules deciding which message events are new messages from someone else (memory only, run in libgm's callback); strips the iPhone "Replied to a message:" line, drops iPhone reactions sent as text (tapback_fallback)
outbound.go: Manager.Send / React / SendImage through a per-target serial queue that waits for the phone's echo; SendImage is one job (render, upload, picture, caption, or fallback texts); number targets resolved to one-to-one conversations
number.go: NormalizeNumber, the E.164 rule for senders and number targets
fake_test.go: fakeClient / fakeFactory (no network, uploads recorded, per-request send results) and test helpers
state_test.go: status transitions and notifications
manager_test.go: connection lifecycle, logged-out paths, reconnects, no take-back, session saving
pair_test.go: pairing success, failures, existing session
inbound_test.go: every inbound rule, re-sends, dedup beyond libgm's window, non-blocking callback, frame fields, reply-line stripping, reactions sent as text, number normalization
outbound_test.go: per-conversation order and echo wait, failures, request shape, number targets, 👍 in RCS and MMS conversations; send_image picture then caption, fallbacks, caption failure, queue order

[PROTOCOL]: Update this header on change, then check CLAUDE.md
