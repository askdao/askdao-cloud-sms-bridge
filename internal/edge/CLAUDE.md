# internal/edge/
> L2 | parent: ../../CLAUDE.md

Members
edge.go: Identity and its 0600 file (ReadIdentity, written through a temp file), Enroll (POST /api/v1/edge/enroll), Heartbeat (POST /api/v1/edge/heartbeat with the node token every interval conductor names; 401 stops, 409 and errors retry; failures logged once in ten)
edge_test.go: enrollment and the identity file, refused enrollment, beat payload and interval, 401 / 409 / 5xx, log throttling, the token never logged

[PROTOCOL]: Update this header on change, then check CLAUDE.md
