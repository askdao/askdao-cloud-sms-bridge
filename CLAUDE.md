# askdao-cloud-sms-bridge - relays an Android phone's Google Messages (SMS/RCS) to the AskDAO conductor
Go 1.26 + libgm (go.mau.fi/mautrix-gmessages v0.2609.0) + zerolog; macOS launchd service; AGPL-3.0, public repository

<directory>
cmd/sms-bridge/ - the binary: `pair` (Google-account pairing) and `serve` (the service), .env loading
internal/session/ - cookie-file parsing and the session file (load, atomic 0600 save under libgm's cookie lock)
internal/phone/ - libgm seam (Client), connection Manager, status Tracker (ok / phone_offline / browser_inactive / logged_out), pairing flow
internal/logx/ - log line shaping: UTC timestamp first, info vs warn+ split, allowlisted fields for libgm
deploy/macos/ - launchd plist template and setup.sh (install / uninstall / status / logs)
</directory>

<config>
go.mod - module github.com/askdao/askdao-cloud-sms-bridge and pinned dependencies
.env.example - settings the service reads from .env in its working directory
.gitignore - keeps .env, session files, cookie files, bin/ and logs/ out of git
README.md - install, pairing, start/stop, logs, status, re-pairing
</config>

Public repository rules: no credentials, real phone numbers, emails, names or internal host names;
examples use +15550000000 and example.com. Logs never carry message text, cookies, tokens or session content.

[PROTOCOL]: Update this header on change, then check CLAUDE.md
