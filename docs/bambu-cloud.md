# Bambu Cloud mode: research and design notes

Researched September 2026. This is the reference for the "connect without LAN
mode" option for Bambu Lab printers.

## What is implemented (September 2026)

- `cloud/bambu.go`: sign-in (password, then the emailed code; TOTP accounts
  fall back to the emailed code too), paste-a-token, the device list, and the
  pacing rules: one password sign-in a minute, one code attempt every 10
  seconds, a 5-minute hold after a Cloudflare block, the device list cached
  for a minute, an honest `User-Agent: FoxTrack-Bridge/<version>`.
- `mqtt/cloud.go`: one MQTT connection per account for every cloud printer,
  verified TLS, a fixed client id per process, keepalive 30 s, one reconnect
  owner with 5 s to 5 min backoff plus jitter, a 30-minute hold after 10
  failures or 3 refused connections in a row (the ban heuristic), auth
  failures final, `pushall` at most once a minute per printer, `pushing.start`
  after 2 minutes of silence at most every 5 minutes. The LAN IP the printer
  reports (`print.net.info[].ip`) is stored so the LAN camera path works.
- `server_cloud.go`: `/api/cloud/{status,login,verify,token,devices,unlink,retry}`.
  `POST /api/printers` with `"connection":"cloud"` looks the serial up on the
  account and stores the LAN access code the cloud reports.
- Config: `bambu_cloud` block and `connection` per printer, both additive.
  The token is redacted from every response and survives settings saves.
- UI: Settings > Bambu Cloud, the "Bambu Lab (cloud)" printer type with a
  device picker, and cloud cards that show only the light and camera buttons.

The rest of this document is the research the implementation follows.

## Short answer

Cloud mode is possible for **telemetry only**, and it carries real risk.

- Third-party clients can still log in to Bambu Cloud and subscribe to
  `device/<serial>/report` on the cloud MQTT broker. Bambu's own wiki exempts
  "MQTT status pushes for tools like Home Assistant" from Authorization Control.
- Control does **not** work over cloud on current firmware. Every command except
  the light is rejected with `HMS_0500_0500_0001_0007 "MQTT Command verification
  failed"`. Newer firmware signs commands with a certificate provisioned by Bambu
  Studio. Do not copy those certificates; that is the "impersonation" Bambu is
  now enforcing against (see "Bambu's position").
- Camera does not come with cloud mode. The cloud camera path uses a proprietary
  P2P SDK. The device list does hand back the LAN access code, so the existing
  LAN camera path can be reused when the printer is on the same network.

## Why the March 2026 ban most likely happened

Bambu's only published rate rule (June 24 2024) is about **MQTT connection
churn**, not REST volume:

- More than 50 concurrent MQTT connections per account, or connect/disconnect
  cycles of about one minute, earns a 24 hour to 7 day ban.
- During the ban the account cannot use the cloud from official or third-party
  software. Repeat offences re-ban.

A week-long ban matches that policy exactly. The Bridge's LAN code uses its own
reconnect loop with `SetAutoReconnect(false)`. A cloud client built the same way,
one connection per printer with a fast retry loop, produces exactly the pattern
Bambu bans. Cloud REST polling is the less likely cause.

Source: https://forum.bambulab.com/t/bambu-lab-mqtt-limitations/83440

## Mechanics

### Cloud MQTT

| Item | Value |
| --- | --- |
| Broker | `us.mqtt.bambulab.com:8883` (global), `cn.mqtt.bambulab.com:8883` (China) |
| TLS | Public certificate. Verify it. No `InsecureSkipVerify`. |
| Username | `u_<uid>` |
| Password | the access token |
| Subscribe | `device/<serial>/report` |
| Publish | `device/<serial>/request` (light only on current firmware) |

Payloads are the same as LAN mode. X1 sends full state; P1 and A1 send deltas,
which the existing merge logic in `mqtt/mqtt.go` already handles. Send one
`pushall` per printer when the subscription opens. Printers rate-limit `pushall`
to about one per minute and drop extras. For silence recovery use
`{"pushing":{"command":"start"}}` after 60 seconds, as ha-bambulab does.

Getting the uid: a JWT token (password login) carries `username` = `u_<uid>` in
its payload. An opaque token (email-code login) needs
`GET /v1/design-user-service/my/preference`, then prefix `uid` with `u_`.

Sources:
- https://github.com/Doridian/OpenBambuAPI/blob/main/mqtt.md
- https://github.com/greghesp/ha-bambulab/blob/main/custom_components/bambu_lab/pybambu/bambu_client.py
- https://pkg.go.dev/github.com/polarn/waybar-modules/pkg/bambu

### Login

Base URL `https://api.bambulab.com` (or `api.bambulab.cn`).

1. `POST /v1/user-service/user/login` with `{"account","password","apiError":""}`.
   The reply is one of: `accessToken`; `loginType:"verifyCode"`; or
   `loginType:"tfa"` with `tfaKey`.
2. Email code (the normal case since late 2024):
   `POST /v1/user-service/user/sendemail/code` with `{"email","type":"codeLogin"}`,
   then `POST .../login` again with `{"account","code"}`.
3. TOTP: `GET https://bambulab.com/api/csrf`, then
   `POST https://bambulab.com/api/sign-in/tfa` with `{"tfaKey","tfaCode"}` and
   the `x-bbl-csrf-token` header. This leg broke for everyone in July 2026
   (`403 CSRF error: missing_cookie`). Treat it as optional and fall back to the
   email code.
4. Cloudflare bot protection sits in front of the API. A 403 or 429 that mentions
   Cloudflare is tied to your public IP, clears on its own in hours, and gets
   longer if you retry. Hold sign-in for five minutes after seeing one. Offer a
   "paste access token" path as the escape hatch.
5. Tokens last about 90 days. `refreshtoken` only returns 401 now. Nobody avoids
   re-login. Cache the token on disk and prompt again at expiry.

Sources:
- https://github.com/greghesp/ha-bambulab/blob/main/custom_components/bambu_lab/pybambu/bambu_cloud.py
- https://github.com/Doridian/OpenBambuAPI/blob/main/cloud-http.md
- https://github.com/greghesp/ha-bambulab/issues/692
- https://github.com/greghesp/ha-bambulab/issues/2058
- https://github.com/greghesp/ha-bambulab/issues/2136
- https://wiki.bambuddy.cool/features/cloud-profiles/

### REST endpoints needed

Only one: `GET /v1/iot-service/api/user/bind`. It returns `devices[]` with
`dev_id` (serial), `name`, `online`, `print_status`, `dev_model_name`,
`dev_product_name`, and `dev_access_code` (the LAN access code). Call it on
login and on an explicit user refresh. Never on a timer.

### Firmware

Authorization Control shipped in January 2025: X1 series from 01.08.05.00, P1
series 01.08.02.00, A1 and A1 mini 01.05.00.00, H2D 01.01.00.01, P2 series from
launch. Status pushes stay exempt. Developer Mode is a LAN-only toggle and does
not apply to cloud.

Sources:
- https://blog.bambulab.com/firmware-update-introducing-new-authorization-control-system-2/
- https://wiki.bambulab.com/en/software/third-party-integration
- https://github.com/greghesp/ha-bambulab/issues/2036
- https://github.com/greghesp/ha-bambulab/issues/2043

## Rules a safe implementation must follow

1. One MQTT connection per account, subscribing to every serial on it. Never one
   per printer.
2. Reconnect with exponential backoff and jitter. Minimum 5 seconds, cap 5
   minutes, no more than about one attempt per minute sustained. After 10
   failures, stop for 15 to 30 minutes.
3. One reconnect owner. A single goroutine with a mutex. No parallel loops.
4. Never block the MQTT goroutine with HTTP. Keepalive 30 to 60 seconds.
5. Auth failure (MQTT rc 4 or 5, HTTP 401) is final. Stop, mark the token
   expired, show "Re-link account" in the UI. Never retry.
6. `pushall` at most once per connect per printer, never more than once a
   minute. Use `pushing.start` for silence recovery.
7. Login at most once per 60 seconds. After a Cloudflare 403 or 429, hold for 5
   minutes. Never auto-login without the user; the email code needs them anyway.
8. `user/bind` only on login and explicit refresh, at least 60 seconds apart.
9. If MQTT connect times out or is refused right after a good REST login, assume
   a temporary ban. Back off 30 minutes and tell the user bans last 24 hours to
   7 days. Never shorten the interval.

## Bambu's position

- Terms of service forbid reverse engineering and allow account deactivation
  for breaches. https://bambulab.com/en-us/policies/terms
- January 2025: status monitoring over MQTT for tools like Home Assistant is
  explicitly allowed. Third-party control goes through Bambu Connect. Farm
  developers should email devpartner@bambulab.com.
  https://blog.bambulab.com/updates-and-third-party-integration-with-bambu-connect/
- May 2026: Bambu issued a takedown against a slicer fork that faked official
  client identity headers, and said open-source licensing does not grant
  deceptive access to its private cloud.
  https://forum.bambulab.com/t/setting-the-record-straight-on-cloud-access-and-community/252164
- There is no public OAuth or third-party API program as of September 2026.

Net: read-only cloud telemetry with an honest `User-Agent` is tolerated.
Copying Bambu Studio or OrcaSlicer headers, or extracting signing certificates,
is the line Bambu enforces.

## Recommended design for the Bridge

All config changes are additive and `omitempty`, per CLAUDE.md. Existing
printers keep working with no migration.

Config:

- New top-level `cloud` block: `region` (`global` or `cn`), `email`,
  `access_token`, `token_issued_at`, `token_expires_at`, `mqtt_username`.
- On `Printer`: `connection` (`lan` when empty, or `cloud`). Keep `serial`.
  `lan_code` is auto-filled from `dev_access_code` and used only for the camera.
  `ip` becomes optional for cloud printers.
- Treat the token like a password. Redact it from every response, the same way
  `redactConfig` handles API keys today.

Flow:

1. Settings gets a "Link Bambu account" card: email and password, then the
   emailed code. On success, call `user/bind` once and show the discovered
   printers. The user picks which to add as `connection: "cloud"`.
2. Send `User-Agent: FoxTrack-Bridge/<version>`. Do not send `X-BBL-*` headers.
   Accept that an honest agent may be challenged by Cloudflare and offer the
   paste-a-token path.
3. Decode the JWT `exp` when present, else `issued_at` plus 89 days. Warn in
   the dashboard 7 days before expiry.

MQTT:

- New cloud connection type next to the LAN one in `mqtt/mqtt.go` (broker and
  auth around lines 715 to 727, topics at 751 and 769, pushall at 799 to 802).
- Broker `ssl://us.mqtt.bambulab.com:8883` or `cn.`, full TLS verification,
  stable client id `foxtrack-<hostname>-<8hex>`, keepalive 30 seconds.
- One client for all cloud printers, subscribing to each `device/<serial>/report`
  at QoS 0, feeding the existing report parser.
- Publish only the light command for cloud printers. Hide pause, resume, stop,
  speed, fan and GCode in the UI for `connection: "cloud"` cards.

Files: `mqtt/mqtt.go`, `config/config.go`, a new `cloud/` package for the REST
login, plus the server and UI endpoints for the link flow. The MQTT protocol
handling and the credential redaction path are hard boundaries in
`.llm/recipe.md`, so that work is done by hand, not delegated.

## Projects worth reading

- ha-bambulab (Python, the most used cloud client):
  https://github.com/greghesp/ha-bambulab
- OpenBambuAPI (protocol notes): https://github.com/Doridian/OpenBambuAPI
- polarn/waybar-modules `pkg/bambu` (Go, full cloud login and MQTT):
  https://pkg.go.dev/github.com/polarn/waybar-modules/pkg/bambu
- dhiaayachi/bambu-go (Go, cloud and LAN constructors):
  https://github.com/dhiaayachi/bambu-go
- Open Bamboo Networking (2026 signing reverse-engineering, read-only):
  https://github.com/ClusterM/open-bamboo-networking
