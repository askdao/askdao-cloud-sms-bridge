# internal/ws/
> L2 | parent: ../../CLAUDE.md

Members
server.go: Server, the WebSocket endpoint: token auth as the first frame, one connection (newest wins, old closed with 4000), status / message frames out, send / react to the Backend
frames.go: the wire shapes: message frame (media always empty, reply_fallback only when set), status frame (status always "connected", phone state in state / detail), error frame, command
server_test.go: auth and refusals, status frames, message frame fields, commands, newest connection wins, delivery without conductor

[PROTOCOL]: Update this header on change, then check CLAUDE.md
