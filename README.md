# askdao-cloud-sms-bridge

SMS/RCS bridge for AskDAO: it connects an Android phone's Google Messages to the AskDAO
conductor, so the AI bot can have a phone number and take part in "Text Message · RCS" and
MMS group chats that an email-only iMessage identity cannot join.

The bridge pairs with the phone through the phone's Google account (the same way
messages.google.com does), using libgm from [mautrix-gmessages](https://github.com/mautrix/gmessages).
It runs on a Mac mini as a launchd service.

```
group members ──carrier RCS / MMS──▶ Android phone (Google Messages, the bot's number)
                                          │  Google Messages for web protocol (libgm)
                                          ▼
                              Mac mini: sms-bridge serve (launchd)
```

## Requirements

- An Android phone with a US number, Google Messages as the default SMS app, RCS connected,
  signed in to a dedicated Google account. Keep it plugged in, on Wi-Fi, with battery
  optimization for Google Messages turned off.
- A Mac (the Mac mini) with Go 1.26 or newer. The service is a LaunchAgent, so the Mac must
  log in automatically after a reboot (System Settings → Users & Groups → Automatic login).

## Install on the Mac mini

1. Install Go with Homebrew and check the version (1.26 or newer):

   ```bash
   brew install go
   go version
   ```

2. Clone and build:

   ```bash
   mkdir -p ~/Services && cd ~/Services
   git clone https://github.com/askdao/askdao-cloud-sms-bridge.git
   cd askdao-cloud-sms-bridge
   go build -o bin/sms-bridge ./cmd/sms-bridge
   ```

3. Configure:

   ```bash
   cp .env.example .env
   chmod 600 .env
   ```

   Edit `.env` (see the comments in `.env.example`).

4. Pair with the phone (next section), then install the service:

   ```bash
   ./deploy/macos/setup.sh install
   ```

   The service starts at login and restarts 10 seconds after a crash or an error exit.

## Pair with the phone

Pairing is done once. QR-code pairing no longer exists (Google removed it), so the bridge
pairs through the phone's Google account: you give it the cookies of a browser signed in to
that account, it asks Google to pair, and the phone shows a few emoji; you tap the one the
bridge prints.

Before you start:

- On the phone, open Google Messages → Device pairing and sign out any old devices you no
  longer use. Make sure account pairing is allowed there.
- Unlock the phone and keep Google Messages open in the foreground until pairing finishes.
  The bridge gives up if the phone does not answer within about 20 seconds.

### 1. Get the cookies (Safari, private window)

Upstream's requirements: a private window is required (so the browser does not rotate the
cookies), do not navigate anywhere else in that window after signing in, and close the
window without signing out.

1. Safari → Settings → Advanced → turn on "Show features for web developers" (once).
2. File → New Private Window (⇧⌘N).
3. In that window open the Web Inspector (⌥⌘I) and select the Network tab *before* signing in.
4. Open this address and sign in to the phone's Google account:

   ```
   https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config
   ```

   The page ends at `messages.google.com/web/config` showing a block of unreadable text.
   That is expected.
5. In the Network tab, right-click the request named `config`
   (`https://messages.google.com/web/config`; reload with ⌘R if it is not listed) and choose
   "Copy as cURL".
6. In Terminal, in the install directory, save it and clear the clipboard:

   ```bash
   pbpaste > cookies.txt && chmod 600 cookies.txt && pbcopy < /dev/null
   ```

   Check it without printing any cookie (this shows only the address):

   ```bash
   grep -o "^curl '[^']*'" cookies.txt
   ```

7. Close the private window (⌘W). Do not click "Sign out": that invalidates the cookies.

If pairing later says the cURL text has no cookies or misses some, write the cookies as JSON
instead: in the Web Inspector's Storage tab → Cookies, find `SID`, `HSID`, `SSID`, `OSID`,
`APISID`, `SAPISID` (and `__Secure-1PSIDTS` if present), and put their values in
`cookies.txt` with a text editor (not `echo`, which leaves them in the shell history):

```json
{"SID": "", "HSID": "", "SSID": "", "OSID": "", "APISID": "", "SAPISID": "", "__Secure-1PSIDTS": ""}
```

Chrome also works if Device Bound Session Credentials are turned off in `chrome://flags`;
the steps are the same: private window, Network tab, "Copy as cURL" on the `config` request.

### 2. Run the pair command

```bash
./bin/sms-bridge pair -cookies cookies.txt
```

After a few seconds it prints `Tap this emoji on the phone: …`. Tap the same emoji in the
prompt Google Messages shows on the phone. On success it prints `Paired. Session saved to …`.
Then delete the cookie file:

```bash
rm -f cookies.txt
```

The session file (`session.json` by default, mode 0600) now holds the login, including the
Google account cookies, which the bridge keeps refreshed. Treat it like a signed-in browser:
never copy it into the repository or share it.

If pairing fails, the message says why: no phone found (the phone is signed in to another
account, or account pairing is off), the phone did not respond (keep Google Messages in the
foreground), the wrong emoji was tapped, pairing was cancelled or timed out. Fix it and run
the same command again; the cookie file stays valid until pairing succeeds.

## Start and stop

```bash
./deploy/macos/setup.sh install     # install (or reinstall) and start
./deploy/macos/setup.sh status      # loaded? running? last status change
./deploy/macos/setup.sh uninstall   # stop and remove the service

launchctl stop  com.askdao.sms-bridge                      # stop; stays stopped until started
launchctl start com.askdao.sms-bridge                      # start
launchctl kickstart -k gui/$(id -u)/com.askdao.sms-bridge  # restart
```

On a clean stop the bridge saves the session first.

To update: `git pull && go build -o bin/sms-bridge ./cmd/sms-bridge`, then restart.

## Logs

```bash
./deploy/macos/setup.sh logs        # last 50 lines of both files
tail -f logs/bridge.log logs/bridge.error.log
```

- `logs/bridge.log`: information; `logs/bridge.error.log`: warnings and errors.
- Every line starts with a UTC timestamp.
- No message text, no cookie, token or session content is ever logged. libgm's own log
  lines keep only an allowlist of fields (message, error, status codes, counters).

Every status change is one line, for example
`2026-01-01T12:00:00.000Z WRN status changed from=ok/ to=phone_offline/phone_not_responding`.

## Status

| state | detail | meaning | what to do |
|---|---|---|---|
| `ok` | | connected, the phone responds, the bridge is the phone's active web client | nothing |
| `phone_offline` | `connecting` | starting up, or waiting for the phone to make the bridge active | wait; it normally takes seconds |
| `phone_offline` | `connection_lost` | the connection to Google failed; libgm keeps retrying | check the Mac mini's network |
| `phone_offline` | `phone_not_responding` | the phone stopped answering libgm's pings (it checks every 20 minutes and reports after about 15 minutes without an answer) | check the phone: power, Wi-Fi, Google Messages running |
| `browser_inactive` | `another_browser` | Google Messages for web was opened elsewhere with the same account, and the phone switched to it | close that browser tab, then restart the service |
| `browser_inactive` | `timeout`, `inactivity` | the phone made the bridge inactive for its own reasons | restart the service |
| `logged_out` | `pair_revoked`, `google_logged_out`, `unpaired`, `credentials_rejected` | the pairing or the Google login is gone | pair again (below) |

The bridge never takes the phone back on its own after `browser_inactive`; that is left to
an operator. Note that libgm itself asks the phone to make the bridge active after every
connection and reconnection, and when it recovers from a phone that stopped answering pings,
so a restart (or such a recovery) does make the bridge active again.

After `logged_out` the process keeps running and stops talking to Google, so the state stays
visible; it does not delete the session file.

## Pair again

When the state is `logged_out`:

1. Stop the service: `launchctl stop com.askdao.sms-bridge`
2. On the phone: Google Messages → Device pairing → sign out this device if it is still listed.
3. Delete the session file: `rm -f session.json`
4. Get fresh cookies and run the pair command (section "Pair with the phone").
5. Delete the cookie file and start the service: `launchctl start com.askdao.sms-bridge`

When the state is `browser_inactive` / `another_browser`: find who has Google Messages for
web open with the bot's account, close it, then restart the service.

## License

AGPL-3.0, see [LICENSE](LICENSE). The bridge uses libgm from
[mautrix-gmessages](https://github.com/mautrix/gmessages), which is AGPL-3.0.
