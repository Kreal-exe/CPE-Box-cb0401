# Changelog

Release notes for each version. The **Release** workflow
(`.github/workflows/release.yml`) reads the section matching the pushed tag
and uses it as the GitHub release description — followed by the shared install
guide (`.github/release-install.md`) and a Full Changelog link. To cut a
release, add a `## vX.Y.Z` section here, commit, then tag and push:

```bash
git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z
```

Because the notes live in this file, Markdown headings work as-is — no
`--cleanup=verbatim` needed on the tag. Everything up to the next `## vX.Y.Z`
heading belongs to a version; use `##` for the subsections within it.

## v1.0.10 — speed test with a live gauge, SIM contacts, ping & traceroute, cell lock, RSSI, own number on the SIM

## New

- **Speed test** (Router tab), with a speedtest.net-style gauge that moves while it runs. It picks the nearest Ookla server the way speedtest.net does and runs 16 parallel streams from the computer the panel runs on, through the router - so it's the mobile link that's measured. Results match speedtest.net within a few percent (473 / 116 Mbit/s against 446-498 / 103 on the same line). It stops as soon as the reading settles (5-10 s per direction), which roughly halves the data a fixed-length test uses. A test started on one device shows live on every open panel. (A first version ran on the router itself; that can't work above ~300 Mbit/s because the router's own traffic isn't hardware-offloaded.)
- **Contacts** - a new tab: the SIM card's phonebook. Search, add, edit and delete (one, or **Select** several / all at once), **import a .vcf** (vCard 2.1/3.0/4.0, from a phone, Google or Apple Contacts) and **export** all as .vcf. Names are stored as UCS2, so umlauts and Cyrillic survive. Senders in **Messages** now show their contact name, and a contact has a **Message** button.
- **Ping & traceroute** (Router tab), from the router, with 8.8.8.8 / 1.1.1.1 / 9.9.9.9 shortcuts. On IPv6-only APNs with 464XLAT (WAN `192.0.0.x`) traceroute hops don't answer; the panel says so instead of leaving a wall of `*`.
- **Cell lock / tower lock** (Cellular tab). Lock LTE to up to 10 EARFCN + PCI pairs and 5G to one PCI / ARFCN / SCS / band through `AT+QNWLOCK`, with "use current cell" buttons. Off by default after a reboot (a safe way to try a lock); "Keep after a reboot" stores it in the modem.
- **HTTPS on port 443.** Chrome's "Always use secure connections" tries `https://cpe.box` first and, with nobody on 443, stops at a full-page "This site doesn't support a secure connection" warning. The panel now serves itself over HTTPS too, with its own certificate (self-signed, generated once and kept next to `.env`, so the browser's one-time certificate prompt doesn't come back after a restart). `GUI_TLS_PORT=off` disables it.
- **RSSI** next to RSRP / RSRQ / SINR on the Overview signal card.
- **Phone number on the SIM** (Cellular tab). Some operators leave the SIM's own number empty, so neither the stock UI nor the panel can show it. It can now be written to the SIM (EF_MSISDN through `AT+CRSM`; the RG520N has no `ON` phonebook). The Overview shows an "Add →" link when the number is missing.

## Fixes

- **The panel showed "v1.0.1-dev"** when `start.sh` built it from source (with Go installed): only release builds stamped the version. Source builds now take it from the git tag too (`1.0.10`, or `1.0.10-3-gabc1234` on a newer commit).

## v1.0.9 — LTE only that works, SIM PIN dialog, real live speed, SMS centre, full stock/AT audit

## Fixes

- **"LTE only" failed and jumped back to SA + NSA.** The panel wrote `nr5g_disable_mode=3`, which the RG520N answers with `ERROR`. LTE only is now `mode_pref=LTE`, the same setting the stock UI's "4G only" uses. The stock network type is kept in step, so the stock UI and the panel always show the same mode, and a "4G only" set from the stock UI now reads as **LTE only** in the panel instead of SA + NSA. The boot hook (now v6) re-applies it after a reboot.
- **SIM PIN couldn't be entered in the panel (error 1502), and the PIN prompt never showed up.** The panel sent the PIN as `sim_pin`, but the stock code reads `pincode` (and `pukcode`/`newpin` for the PUK), so it got an empty PIN. The lock was also only detected from the cellular read, which doesn't answer while the SIM is locked. Now the SIM state comes from the stock PIN check itself, and a **PIN/PUK dialog opens on any page** as soon as the SIM asks for it, with "Remember the PIN" and the tries left. A wrong PIN says how many tries remain.
- **Console, IMEI read and IMEI write sometimes got an empty reply.** They didn't wait for the modem's AT port while the panel's own cellular poll held it. They now share the port lock and retry, and a busy port is reported instead of a silent blank.
- **Carriers listed an inactive 5G carrier** (e.g. `B3 + B8 + n78 + n1` while n1 had no signal). Carriers with no RSRP are left out, matching the stock daemon.
- **LTE only: signal, PCI and SINR were blank on the AT path.** In LTE-only mode the modem reports the LTE cell on the `servingcell` line itself, which is now parsed.
- **Phone number was empty on cb0401 v2**; it's read from the SIM (`AT+CNUM`) when the stock daemon leaves it blank.
- **Boot hook writes could be skipped silently** when the AT port was busy at ifup. They now wait, retry and log a failure (hook v7).
- **Port forwarding: new rules used a protocol the router doesn't understand.** It now sends TCP / UDP / ALL like the stock page.
- **UPnP list showed no ports**, reading field names the router doesn't return.
- **DMZ off didn't take effect until the next firewall restart**, and **DMZ on quietly added a DHCP reservation** for the device. Both now do exactly what the stock page does.
- **APN authentication showed blank for stock APNs** ("None" vs "NONE") and offered a "PAP & CHAP" value the router doesn't support.
- **Wi-Fi changes saved from the panel didn't take effect until a reboot.** The stock `setWifi` the panel calls only writes the config (the stock page saves through `set_all_wifi`, which also applies it), so e.g. a new channel showed as saved while the radio kept running the old one. Saving now applies it right away with `wifi update`, restarting only the radio that changed. The "actually running" note also names what really differs, and no longer flags Auto width as a mismatch.
- **2.4 GHz channel scan flip-flopped** - after moving to the "clearest" channel, a rescan said another one was clearer. Networks near the noise floor (below -82 dBm) were weighted like real neighbors, and the router hears far more of them on its own channel than on the ones it only visits while scanning. They now barely count, and the scan only suggests switching when another channel is clearly better, not on a near-tie.
- **Wi-Fi: WPA3 networks showed as WPA2**, and saving downgraded them. WPA3 and WPA2/WPA3 are now offered.
- **DHCP range on a 255.255.0.0 LAN** is sent as full addresses, as the stock page does.
- **SMS: starting a new conversation marked a message in another thread as read.**
- **Data usage showed "idle" while traffic was flowing.** The live rate came from trafficd, which the hardware offload hides traffic from: it read ~2 KB/s during a 30+ MB/s download, in router and bridge mode alike. The rate is now measured on the modem's own network devices (`wwan0` / `rmnet_mhi0`), and refreshes every 5 s instead of every 30 s.

## What's new

- **SMS centre (SMSC) setting.** Cellular → SMS centre shows the number the modem sends texts through and lets you set your provider's one, for SIMs whose default SMSC doesn't deliver. It's saved on the modem and kept after a reboot.

## v1.0.8 — 5G band fix that actually lands, internet check, band unlock on v1

## Fixes

- **5G cell still showed "n0" on cb0401 v1 (the real fix).** v1.0.7's derivation ran too late: `band_5g` was already set from the QNWINFO leg, which reports `NR5G BAND 0`, so the ARFCN fallback never fired. A band number of 0 is now treated as unknown everywhere, so the band is derived from the NR-ARFCN (427730 → **n1**) in both the 5G cell row and the Carriers list.
- **Carriers row dropped the 5G band on v1.** In NSA that firmware lists only the LTE carriers in `AT+QCAINFO`, so the Carriers row read `B3 + B7 + B20` with no n-band. The 5G leg is now appended (`B3 + B7 + B20 + n1`).
- **5G SINR showed an impossible value (e.g. 195 dB).** That firmware reports the NR SS-SINR in 0.1 dB steps; it's now scaled to real dB (~19.5), while whole-dB firmware is left as-is.

## What's new

- **Internet reachability indicator.** A pill on the Overview and Cellular cards shows whether the router actually has working internet — it pings `8.8.8.8` and resolves a hostname from the router itself, so you can tell a real outage from a healthy "Registered" state that still has no data path. Shows latency when online, "DNS issue" when only name resolution is down, "No internet" when the data path is dead.
- **5G band unlock that survives reboots on cb0401 v1.** Bands you pick in Cellular → Bands are written straight to the modem over AT (v1's stock daemon won't take them) and now re-applied on every boot, so extra bands (e.g. n75/n76/n77 and the SA bands, where the module supports them) stick instead of resetting to the firmware default. On firmware whose daemon keeps bands itself, nothing changes.

## v1.0.7 — correct 5G band on cb0401 v1

## Fixes

- **5G band shows "n0" on cb0401 v1 (ROM 3.0.116).** That firmware reports the NR band as `0` over AT even on a live cell, while the ARFCN is correct. The 5G cell row and the Carriers list now derive the band from the NR-ARFCN (3GPP TS 38.104), so an n1 cell on ARFCN 427730 shows **n1** instead of a bogus **n0** — matching the stock UI. Works for any band (n28 in another location, etc.), not just n1.
- **SA/NSA mode now survives a reboot on cb0401 v1.** The mode (`nr5g_disable_mode`) was only re-applied from a hotplug hook bound to the `wan_2` interface, which v1 firmware doesn't use, so "Auto" reverted after a reboot. The hook now matches the modem WAN under the other names too, and a boot-time re-assert applies the saved mode even when no hook fires.
- **Home Assistant add-on updates reliably.** The add-on refreshed its copy with `git pull --ff-only`, which silently kept the old code whenever upstream history was rewritten — so a fix could ship but never reach an installed add-on. It now fetches and hard-resets to the latest upstream. Your `.env` and SSH key live outside git and are untouched.

## Also in this release

A cumulative build: a fresh install or add-on update now carries everything since v1.0.3 — the Windows SSH fix (v1.0.4), the Android/Termux fix (v1.0.5) and the Home Assistant OS add-on (v1.0.6). Prebuilt binaries for Linux (amd64 / arm64 / armv7, the armv7 one also covers Android via Termux), macOS (Intel / Apple Silicon) and Windows are attached to every release.

## v1.0.6 — Home Assistant OS add-on

## What's new

- **Home Assistant OS add-on.** cpe-box can now run as an always-on Home Assistant add-on — handy when a HA OS box is the only machine on 24/7. In Home Assistant: **Settings → Add-ons → Add-on Store → ⋮ → Repositories**, add `https://github.com/Kreal-exe/CPE-Box-cb0401`, install **CPE Box**, set the router password in the options, start it, and open `http://<home-assistant>:7777`. It runs the normal `setup.sh` flow non-interactively and keeps its state in the add-on's persistent `/data`. (On HA **Container** or **Core** you have a normal Linux host — use `./start.sh` there instead.) Lives in `homeassistant/`.

## v1.0.5 — run cleanly on Android (Termux)

## Fixes

- **`start_gui.sh` failed to launch on Termux.** It wrote its pid/log to a hardcoded `/tmp`, which Android/Termux has no writable copy of, so the redirects failed with "Permission denied" and `set -e` aborted the launch (people worked around it by starting the binary by hand). It now uses `$TMPDIR` (falling back to `/tmp`), so the normal `./start.sh` runs end-to-end on an Android box. A short **Android (Termux)** section was added to the README. Thanks to [@KittyBua](https://github.com/KittyBua) for the reports and testing.
- **Blank page opened on a non-root host.** `start_gui.sh` / `start_gui.ps1` always opened `http://cpe.box` (port 80), but on a host that can't bind port 80 (e.g. Android/Termux, non-admin) cpe-box only serves `$PORT`, so that was a dead page. The launcher now checks whether port 80 is actually up and opens `http://cpe.box:7777` when it isn't.
- **Every router call failed on Windows (`getsockname failed: Not a socket`).** cpe-box reuses one SSH connection via OpenSSH connection multiplexing (`ControlMaster`), which relies on a Unix-domain control socket that Windows OpenSSH doesn't support — so each command errored out and the whole panel showed "mobile daemon unavailable". (It only surfaced once the control-socket path moved to a directory that exists on Windows.) Multiplexing is now skipped on Windows, which opens a fresh connection per call instead.

## v1.0.4 — 32-bit ARM (ARMv7) binary for Android

## What's new

- **`cpe-box-linux-armv7`** — a 32-bit ARM build, so cpe-box runs on devices with a 32-bit userland such as Android TV boxes through Termux. It's CGO-free like the rest, and `start.sh` / `setup.sh` pick it up automatically on `armv7l` / `armv8l`.

## Fixes

- **A failed first-run step no longer aborts setup.** `setup.sh`'s band-unlock step (`cpe-box --provision`) was fatal (`|| die`). If it failed, setup stopped there — the router-side `boot.sh` marker could be missing, so `start.sh` kept re-running the full setup (re-asking for the ntfy/Telegram backend), and `.env` could be left with the placeholder password (so the panel rejected the correct root password). It's now non-fatal: SSH, the router-side hooks and the saved password are already done by that point, so a `--provision` hiccup just prints a warning and setup finishes; bands can be set from **Cellular → Bands**.

## Known issue

- **The prebuilt ARMv7 binary can crash with `SIGSYS: bad system call` under a strict Android seccomp filter** (seen on some Android TV boxes in bare Termux): Go uses the `faccessat2` syscall when launching `ssh`, and the filter kills it instead of letting Go fall back. A `GOOS=android` build would avoid it but needs the CGO/NDK toolchain (this binary is intentionally CGO-free), so for now the workaround is to run under **proot-Ubuntu/Debian** or to build from source in Termux (`pkg install golang`) — see the README's **Android (Termux)** section. The binary is unaffected under proot and on ordinary 32-bit ARM Linux.

## v1.0.3 — SINR reading fixes

## Fixes

- **SINR of exactly 0 dB was shown as `—`.** The signal meters treated any value of exactly 0 as "no reading", which is right for RSRP/RSRQ (never 0 for a live signal) but wrong for SINR, where 0 dB is a valid — if poor — reading. The Overview signal card now shows `0 dB` instead of a blank.
- **Missing LTE SINR on some cb0401 v2 firmware.** When the stock daemon's `dump_status` leaves the SINR blank (or a bare 0) for a leg that's actually connected, the panel now backfills it from `AT+QENG="servingcell"` — the authoritative source, where SINR is always present — so the meter fills in instead of staying empty. Only queried when the daemon didn't provide it, so the normal fast path is unchanged.
- **Deleting a port-forwarding rule did nothing.** The delete sent the protocol as a number, but the stock daemon (`del_vs_rules`) matches a rule by the exact protocol string it returned (`TCP`/`UDP`/`TCP + UDP`), so it found no match, reported success and left the rule in place. Delete now echoes the rule's fields back verbatim, matching the stock web UI, so the rule is actually removed.

## v1.0.2 — SSH robustness on Android, ARFCN in Cellular

## Fixes

- **SSH from cpe-box running on Android (KSWEB / AWebServer / Termux).** cpe-box spawns exactly one external process — `ssh` — and two of its options assumed a desktop layout:
  - the multiplexing control socket was hardcoded at `/tmp/cpebox_ssh_%C`. Where there is no `/tmp` (Android/KSWEB), the master never came up, so every router call opened its own connection and the router's dropbear closed the racing ones (`Error (255): Connection closed … port 22`). It now prefers `/tmp` and only falls back to `os.TempDir()` where `/tmp` isn't writable, and its name uses the router IP (`%h`) instead of a 40-char hash (`%C`) so the socket path stays under the ~104-char unix-domain-socket limit (which the long `/var/folders/.../T` `os.TempDir()` on macOS would otherwise blow past).
  - the key attempt now runs with `BatchMode=yes`, so a rejected key (or a stuck control socket) fails fast with 255 and retries with the password, instead of hanging on an interactive `root@host's password:` prompt until the deadline — which surfaced as `Timed out running …`.

  These only affect where cpe-box itself runs; everything router-side already ran over SSH on the router. `sshpass` (for the password fallback) and an OpenSSH-compatible `ssh` are still required in that environment.

## What's new

- **ARFCN / EARFCN on the Cellular page.** The serving cell's channel number now shows next to *LTE cell* (EARFCN) and *5G cell* (ARFCN). Read from `+QENG` on the AT path and from the `AT+QCAINFO` PCC line on the daemon path, so it works on both cb0401 v1 and v2.

## v1.0.1 — AT fallback for cb0401 v1

## What's new since v1.0

- **AT-command fallback for cb0401 v1 firmware.** Some cb0401 v1 units run firmware without the stock mobile-daemon API (`ubus call mobile device` / `dump_status`) the panel normally reads cellular status from. On those, the panel now falls back to talking to the RG520N modem directly over AT commands and reconstructs the same essentials — operator, network type (LTE / 5G NSA), primary + 5G bands, RSRP/RSRQ/RSSI/SNR, PCI, SIM status/number/ICCID and APN — so the Cellular page works the same on both firmware generations. Covered by a parser test against a real captured modem reply.
- **Standalone-5G (SA) on cb0401 v1.** The AT fallback now also handles a pure-SA connection: registration is read from 5GS registration (`AT+C5GREG?`), not just LTE `AT+CEREG?`, and serving-cell signal/PCI/band are parsed from the `NR5G-SA` `+QENG` line — so an SA-only v1 no longer reads as "not registered" / "no service". Verified against a real cb0401 v1 SA capture. The aggregated-band readout also ignores a bogus band 0 that some v1 firmware reports on a weak carrier, so the Carriers row shows the real band instead of `n0`.
- **App version in the panel header.** The running CPE Box version is shown next to the title (and stays on the System page), so it's easy to tell which build you're on.
- **Automatic RSA key fallback in setup.** If the router's dropbear is too old to accept the default ed25519 key, setup regenerates an RSA key and reinstalls it over the same password instead of failing with "key-based login still fails".

## Repository structure

- The host-side module directory `gui/` is renamed to **`panel/`** — it holds the whole web-panel app and its Go module (not just a frontend), mirroring the router-side `router/`. Module path `cpebox/gui` -> `cpebox/panel`; local secrets and build output now live under `panel/` (`panel/.env`, `panel/router_key`, ...). No behavior, `//go:embed` paths, launcher-script names, or the `GUI_BIND` env var changed — existing `.env` files keep working.

## v1.0 — CPE Box redesign

First release under the **CPE Box** name — the full redesign of the panel and setup flow.

> The repository was renamed from `cb0401-tune-control` to **`CPE-Box-cb0401`**. Old clone URLs still redirect; update your `origin` when convenient: `git remote set-url origin git@github.com:Kreal-exe/CPE-Box-cb0401.git`.

## What's new since v0.3.3

- **Modular web panel** — separate pages (Overview / Cellular / Wi‑Fi / Devices / Messages / Network / Router / Console) instead of one long scrolling page. Light and dark themes; the CPE Box logo takes you back to Overview.
- **LAN access with login** — panel binds to the LAN by default at `http://cpe.box` (port 80 when free, `:7777` otherwise); other devices sign in with the router's root password, localhost is trusted automatically. Sessions are bound to the current root password. SIM lock banner + PIN/PUK unlock form appear inline if the SIM asks for its PIN after a reboot.
- **AT command line to the modem** on the Console page — queries and writes straight to `/dev/ttyUSB2` (Quectel RG520N) with preset chips (`AT+QENG="servingcell"`, `AT+QNWPREFCFG=…`, `AT+CGSN`, `AT+CGMR`, `AT+CIMI`).
- **IMEI editor** on the Cellular page — reads the modem's current IMEI, writes a new one with `AT+EGMR=1,7,…` for carriers that gate SA to whitelisted device IDs. Behind a large "modifies modem, not reversible" warning.
- **160 MHz on 5 GHz** — setup unblocks the stock DFS channel ban, sets `htmode=HT160`, and turns on `preCACEn=1`. Radar-triggered channel moves swap to a pre-CAC'd backup instantly instead of the 60-second CAC. Re-applied every boot by `router/wifi_dfs_persist.sh`.
- **Real Data usage totals** — Today and This month come from the modem's own `mobile.flowstat.daily_usage` / `monthly_usage`, the only counters on this SoC that catch traffic the hardware flow-offload path would otherwise hide. Live rate pill + trafficd-ratio-based ↓/↑ split.
- **Device online/offline is real** — DHCP lease outlives association by hours, so "online" now comes from `ubus call trafficd hw` + `ip neigh show`. Header shows clickable `N online · M offline` chips that filter the list, sorted online-first.
- **Wi‑Fi form respects your saved choice** — if the driver narrows 160 MHz on ch 36 to 80 MHz on ch 40 because of DFS, the dropdowns still show what you set, with a note naming what's actually running.
- **Bands via the stock modem daemon** — bands are pushed through `ubus call mobile device`, so the modem persists them across reconnects and reboots.
- **SMS in the panel** — Messages page shows the SIM's inbox and threads; replying to a forwarded SMS on Telegram sends a real text back.
- **On-disk cache** for router polls — the panel polls the router once no matter how many people have it open.
- **CI + auto-release** — every `v*` tag builds cross-compiled binaries automatically.

## What got cleaner in the release itself

- **One binary per OS.** The router-side `sms-reader` (ARMv7) is now embedded inside every host `cpe-box` binary via `//go:embed`; `setup.sh` dumps it with `cpe-box --dump-sms-reader <path>` at install time. The release page no longer carries a separate `sms-reader-linux-armv7` file next to the actual apps.
- **`build.sh` lives at the repo root**, not `gui/build.sh` — one obvious place to build everything, the same script CI runs.
- **Smarter first-run URL.** `start.sh` opens `http://cpe.box` (no port) when cpe-box grabs port 80, falls through to `http://cpe.box:7777`, then `http://127.0.0.1:7777` — instead of always opening a loopback IP that refused the connection on the first tick.
- **Router sanity check after install.** `setup.sh` verifies `boot.sh`, `wifi_dfs_persist.sh`, the cron line and `notify.conf` actually landed under `/etc/crontabs/patches/` — the stock firmware's ramfs `/etc` used to swallow a mid-install error silently.
- **`start.sh` / `start.ps1` don't skip setup on a stale key.** Both now check that SSH works AND that `/etc/crontabs/patches/boot.sh` exists on the router.
- **`.env` auto-migration.** `start_gui.sh` / `.ps1` move an existing `GUI_BIND=…:5757` to `:7777` on first run (default port moved in v1.0).
- **Go version check** — old Go trips a clear error instead of "undefined: min".
- **`sshpass` install covers Fedora / Arch / openSUSE / Alpine** in addition to apt/brew.

## v0.3.3 — band fix via stock modem daemon (legacy)

Last release under the old *CB0401 Tune + Control* name, before the CPE Box redesign — single-page GUI and `cb0401-tune-control-*` binaries. Prefer v1.0+ unless you specifically need the old UI or asset names.

- Bands are applied through the router's own stock modem daemon (`ubus call mobile device`) instead of raw AT commands, so the modem persists them across reconnects and reboots.

## v0.3.2 — CA bands, LAN access, Wi‑Fi motion sensing

- **Aggregated CA bands, SIM/phone rows, SSH multiplexing** in the System card.
- **`GUI_BIND` for LAN access** plus live operator / network / bands in System.
- **Wi‑Fi CSI motion sensing ("Motion map")** — a dedicated Go app replacing the `sensing.sh` / RuView flow: Qualcomm CFR capture (`cfr-trigger`, capture daemon), Widar2.0-style CSI cleanup, Doppler-based presence and a particle-filter tracker, plus a floor-plan editor.
- **setup** keeps the real root password in `.env` and restores the SSH key after router reboots.

## v0.3.1 — data usage counters

- **Data usage counters** added to the System card.

## v0.3.0 — 5G mode selector

- **5G mode selector** (SA+NSA / Force SA / 5G off) replacing the old SA toggle, with the SA-vs-NSA distinction documented and option labels cleaned up.
- **Band-write reliability** — the hotplug hook no longer overrides the saved `NR5G_MODE` on every reconnect, and the unreliable band-mismatch verification (which raised false "Mismatch" errors when writing a band subset) was removed.

## v0.2.0 — CVE-2023-26319 SSH fallback; LTE band prefix fix

- **`bootstrap/open_ssh.sh` / `.ps1`: Path B — SmartController mac-field injection (CVE-2023-26319)**, the same mechanism xmir-patcher's `connect5.py` uses. Tried automatically when Telnet (port 23) is closed (firmware 3.0.100+): writes the SSH-enable + key-install script to `/tmp/e` in 2-char chunks via `scene_setting` / `scene_start_by_crontab` / `scene_delete`, then installs persistence over the now-open SSH connection. `WEB_PASSWORD` overrides the derived default for the web-UI login step.
- **LTE band chips** now use the `B` prefix (B3, B7, B20…) instead of `n`; 5G NR chips keep `n` (n1, n78…).
- Add an `appVersion` constant.
- README: rewrite "How SSH access is opened" to document both paths and the cron + firewall-hook persistence mechanism.

## v0.1.0 — initial release

First working version of the toolkit — autonomous setup plus a web GUI for the Xiaomi CB0401 / CB0401V2 5G CPE, driven entirely over SSH to the router.

- **One-command setup + web GUI** to open SSH, tune the modem and unlock bands; the stock firmware's telemetry/junk is cleaned up along the way.
- **Push notifications for new devices** — event-driven off the dnsmasq lease hook instead of polling, with a `trust` reply command to whitelist a device straight from the alert.
- **SMS in notifications** — incoming SMS forwarded to ntfy / Telegram, and replying in Telegram texts the sender back; delivery hardened (no silent loss, and one undeliverable message no longer blocks the queue).
- **Nameless-device identification** — MAC vendor (OUI) lookup plus a reverse mDNS query.
- **Instant reply handling** — a long-polling listener instead of a 2-minute cron poll.
