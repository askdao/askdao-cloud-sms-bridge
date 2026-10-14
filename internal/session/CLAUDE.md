# internal/session/
> L2 | parent: ../../CLAUDE.md

Members
cookies.go: ParseCookies for "Copy as cURL" text or JSON, keeps the six required and one optional Google cookie; errors name cookies, never values
store.go: Load / Save of libgm AuthData; Save holds AuthData.CookiesLock while encoding and replaces the file atomically with mode 0600
cookies_test.go: both formats, missing-cookie errors, malformed inputs never echoed
store_test.go: round trip, file mode, broken files, cookie lock under -race

[PROTOCOL]: Update this header on change, then check CLAUDE.md
