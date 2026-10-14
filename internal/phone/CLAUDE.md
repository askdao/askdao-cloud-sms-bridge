# internal/phone/
> L2 | parent: ../../CLAUDE.md

Members
client.go: Client, the libgm subset the bridge uses, and NewLibgmClient (the production implementation)
state.go: Tracker deriving ok / phone_offline / browser_inactive / logged_out with a detail; Subscribe for status consumers
manager.go: Manager, connects with the saved session, maps libgm events to the Tracker and the inbound worker, caches conversations and SIMs, saves the session, reconnects after non-credential failures
pair.go: Pair, the Google-account pairing flow that writes the session file
inbound.go: Inbound and the inbox rules deciding which message events are new messages from someone else (memory only, run in libgm's callback)
outbound.go: Manager.Send / React through a per-target serial queue that waits for the phone's echo; number targets resolved to one-to-one conversations
number.go: NormalizeNumber, the E.164 rule for senders and number targets
fake_test.go: fakeClient / fakeFactory (no network) and test helpers
state_test.go: status transitions and notifications
manager_test.go: connection lifecycle, logged-out paths, reconnects, no take-back, session saving
pair_test.go: pairing success, failures, existing session
inbound_test.go: every inbound rule, re-sends, dedup beyond libgm's window, non-blocking callback, frame fields, number normalization
outbound_test.go: per-conversation order and echo wait, failures, request shape, number targets, 👍 in RCS and MMS conversations

[PROTOCOL]: Update this header on change, then check CLAUDE.md
