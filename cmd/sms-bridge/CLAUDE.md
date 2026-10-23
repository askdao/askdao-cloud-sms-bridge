# cmd/sms-bridge/
> L2 | parent: ../../CLAUDE.md

Members
main.go: entry point, subcommands `pair -cookies <file>` and `serve`, wires logx, session, phone, render and ws
env.go: .env loader (never overrides set variables, never echoes a bad line), the config struct (incl. the Chrome path) and serve's checks (token required, port)
env_test.go: .env parsing, config checks, Chrome path default

[PROTOCOL]: Update this header on change, then check CLAUDE.md
