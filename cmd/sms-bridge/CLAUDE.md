# cmd/sms-bridge/
> L2 | parent: ../../CLAUDE.md

Members
main.go: entry point, subcommands `pair -cookies <file>` and `serve`, wires logx, session, phone, render, delivery and ws
env.go: .env loader (never overrides set variables, never echoes a bad line), the config struct (incl. the Chrome path, the delivery state file and replay window) and serve's checks (token required, port, replay minutes)
env_test.go: .env parsing, config checks, Chrome path default

[PROTOCOL]: Update this header on change, then check CLAUDE.md
