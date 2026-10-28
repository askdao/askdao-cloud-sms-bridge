# cmd/sms-bridge/
> L2 | parent: ../../CLAUDE.md

Members
main.go: entry point, subcommands `pair -cookies <file>`, `enroll <code>` and `serve`, wires logx, session, phone, render, delivery, ws and the edge heartbeat (capabilities, health, build version)
env.go: .env loader (never overrides set variables, never echoes a bad line), the config struct (incl. the Chrome path, the delivery state file and replay window, observer token, edge URL and identity file) and serve's checks (token required, observer token differs, port, replay minutes)
env_test.go: .env parsing, config checks (incl. observer token), Chrome path default

[PROTOCOL]: Update this header on change, then check CLAUDE.md
