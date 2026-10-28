# internal/delivery/
> L2 | parent: ../../CLAUDE.md

Members
ledger.go: Ledger, the seq_ack bookkeeping: per-conversation (seq, part) de-duplication of marked outbound frames, cumulative acks, inbound messages held until received with a replay window and a cap of 500, all saved in one JSON state file (temp file + rename, mode 0600)
ledger_test.go: de-duplication and acks, state surviving a restart, replay window and cap, unreadable state file

[PROTOCOL]: Update this header on change, then check CLAUDE.md
