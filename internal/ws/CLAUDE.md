# internal/ws/
> L2 | parent: ../../CLAUDE.md

Members
server.go: Server, the WebSocket endpoint: token auth as the first frame, one conductor connection (a newcomer gets 4009 while the current one answers a ping; a silent one is closed with 4000 and replaced), read-only observers on BRIDGE_OBSERVER_TOKEN (every frame, commands answered read-only), status / message / ack frames out, send / react / send_image to the Backend, seq_ack (marked frames de-duplicated and acknowledged after the Backend queue, inbound held, replayed on hello, released on received)
frames.go: the wire shapes: message frame (media always empty, reply_fallback only when set), status frame (status always "connected", phone state in state / detail, features ["send_image", "seq_ack"]), ack frame, error frame, command (with the delivery mark, hello and received)
server_test.go: auth and refusals, status frames, message frame fields, commands, send_image frames (incl. large ones), live connection refuses a newcomer, silent one replaced (frames right after auth kept), observers, marked frames acked and replays skipped, inbound held without conductor and replayed on hello

[PROTOCOL]: Update this header on change, then check CLAUDE.md
