# internal/phone/
> L2 | parent: ../../CLAUDE.md

Members
client.go: Client, the libgm subset the bridge uses, and NewLibgmClient (the production implementation)
state.go: Tracker deriving ok / phone_offline / browser_inactive / logged_out with a detail; Subscribe for status consumers
manager.go: Manager, connects with the saved session, maps libgm events to the Tracker, saves the session, reconnects after non-credential failures
pair.go: Pair, the Google-account pairing flow that writes the session file
fake_test.go: fakeClient / fakeFactory (no network) and test helpers
state_test.go: status transitions and notifications
manager_test.go: connection lifecycle, logged-out paths, reconnects, no take-back, session saving
pair_test.go: pairing success, failures, existing session

[PROTOCOL]: Update this header on change, then check CLAUDE.md
