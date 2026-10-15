# cmd/sms-bridge/
> L2 | parent: ../../CLAUDE.md

Members
main.go: entry point, subcommands `pair -cookies <file>` and `serve`, wires logx, session and phone
env.go: .env loader (never overrides set variables, never echoes a bad line) and the config struct
env_test.go: .env parsing tests

[PROTOCOL]: Update this header on change, then check CLAUDE.md
