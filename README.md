# thefeed

DNS-based **feed reader and lite messenger** for networks where only DNS gets through. Read Telegram channels and public X accounts, and exchange end-to-end-encrypted messages with other users — all over plain DNS.

[English](README.md) · [فارسی](README-FA.md) · [简体中文](README-ZH.md) · [Русский](README-RU.md)

[![Google Play](https://img.shields.io/badge/Google%20Play-Install-3DDC84?style=for-the-badge&logo=googleplay&logoColor=white)](https://play.google.com/store/apps/details?id=com.thefeed.android)
[![TestFlight](https://img.shields.io/badge/iOS-TestFlight%20beta-0D96F6?style=for-the-badge&logo=apple&logoColor=white)](https://testflight.apple.com/join/J6bfxDdZ)
[![Latest release](https://img.shields.io/badge/Windows%20%C2%B7%20macOS%20%C2%B7%20Linux-Latest%20release-24292F?style=for-the-badge&logo=github&logoColor=white)](https://github.com/sartoopjj/thefeed/releases/latest)

**Contents:** [Install the app](#install-the-app) · [Run a server](#run-a-server) · [Messenger](#messenger) · [How it works](#how-it-works) · [Security](#security) · [Build from source](#build-from-source) · [Reference](#reference) · [Links](#links--donate)

## Screenshots

<table align="center">
<tr>
<td align="center"><img src="docs/screenshots/mainfeed.jpg" width="170" alt="Main feed"><br><sub>Main feed</sub></td>
<td align="center"><img src="docs/screenshots/feed-post.jpg" width="170" alt="Reading a post"><br><sub>Reading a post</sub></td>
<td align="center"><img src="docs/screenshots/telemirror.jpg" width="170" alt="Telemirror"><br><sub>Telemirror</sub></td>
<td align="center"><img src="docs/screenshots/chat.jpg" width="170" alt="Messenger"><br><sub>Messenger</sub></td>
</tr>
<tr>
<td align="center"><img src="docs/screenshots/chat-list.jpg" width="170" alt="Chats"><br><sub>Chats</sub></td>
<td align="center"><img src="docs/screenshots/scanner.jpg" width="170" alt="Resolver scanner"><br><sub>Resolver scanner</sub></td>
<td align="center"><img src="docs/screenshots/resolver-bank.jpg" width="170" alt="Resolver bank"><br><sub>Resolver bank</sub></td>
<td align="center"><img src="docs/screenshots/settings.jpg" width="170" alt="Settings"><br><sub>Settings</sub></td>
</tr>
</table>

---

## Install the app

*For anyone who just wants to read feeds and chat — you don't need a server, just import a config into the client.*

Download the client for your platform from the latest release — pick the mirror that's reachable for you: **[GitHub](https://github.com/sartoopjj/thefeed/releases/latest)** · **[GitLab](https://gitlab.com/sartoopjj/thefeed/-/releases)**.

| Platform | Notes |
|----------|-------|
| **Android** (7.0+) | Easiest: **[Google Play](https://play.google.com/store/apps/details?id=com.thefeed.android)** — installs and updates itself. Otherwise the APK. Pick `arm64-v8a` (phones since ~2017) or `armeabi-v7a` (older 32-bit only). Installing the wrong ABI installs but won't run. |
| **iOS** (13+) | Install via **[TestFlight](https://testflight.apple.com/join/J6bfxDdZ)**. App Store build is planned; you can also build from source under [ios/](ios/) — see [Build from source](#build-from-source). |
| **Windows** (10/11) | The `.exe` is **unsigned**, so SmartScreen shows *"Windows protected your PC"* and Defender may quarantine it — a known false positive for DNS-tunneling tools, **not malware**. Click **More info → Run anyway**; restore from Defender → *Protection history* if removed; verify the SHA-256 from the release page if unsure. |
| **macOS** | Universal `.dmg` (Intel + Apple Silicon), drag-installs `Thefeed.app`. Unsigned, so on first launch either right-click → **Open**, or run `xattr -dr com.apple.quarantine /Applications/Thefeed.app`. |
| **Linux / Termux** | `thefeed-client` binary — run it and open `http://127.0.0.1:8080`. |

Then open **Settings → Configs** and import a config (or paste your domain + passphrase). DNS resolvers are managed in the **Resolver** tab — a shared Bank used by all configs, plus a Scanner to find more.

**Public configs to try:** [@thefeedconfig](https://t.me/thefeedconfig).

---

## Run a server

*For operators who host feeds for others.* The server runs **outside** the censored network, pulls from Telegram / X, and answers encrypted DNS queries. Setup is two steps: **(1) DNS records**, then **(2) install**.

### 1. DNS records

You need one **A** record and one **NS** delegation. Example: your server IP is `203.0.113.10` and your domain is `example.com`.

| # | Type | Name | Value | Purpose |
|---|------|------|-------|---------|
| 1 | A  | `ns.example.com` | `203.0.113.10`   | Point a hostname at your server |
| 2 | NS | `t.example.com`  | `ns.example.com` | Delegate the **feed** subdomain to your server |
| 3 *(optional)* | NS | `c.example.com` | `ns.example.com` | Delegate a **messenger** subdomain — only if you enable [chat](#messenger) |

Records **1–2** are required for the feed. Record **3** is only needed for the optional [messenger](#messenger), which must use a **separate** subdomain from the feed (e.g. `c.example.com`).

### 2. Install the server

With DNS in place, install with the **script** or with **Docker**.

#### Option A — install script (Linux + systemd)

The one-liner auto-detects a reachable mirror (GitHub first, then GitLab); pass `--gitlab` to force GitLab.

```bash
# GitHub mirror
sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"

# GitLab mirror (use while the GitHub account is unavailable)
sudo bash -c "$(curl -Ls https://gitlab.com/sartoopjj/thefeed/-/raw/main/scripts/install.sh)" -- --gitlab
```

The script downloads the latest binary, asks for your domain / passphrase / channels / X accounts, asks whether to use Telegram login (recommended: **No** — public channels work without it), and sets up a systemd service. Re-run it any time to **update**.

Other actions (pipe the script to `sudo bash -s -- <flag>`):

| Flag | Action |
|------|--------|
| `--version v0.9.2` (or `-v`) | Install a specific tag (rollback) |
| `--pre` | Install the newest pre-release (beta / rc) |
| `--list` | List recent releases |
| `--login` | Re-run Telegram login |
| `--config` | Print the import URI (domain, key, `sk=` server key, bootstrap resolvers) |
| `--uninstall` | Remove the service |

#### Option B — Docker

No Go toolchain needed. Base image `alpine:3.21` (~23 MB), runs as non-root `thefeed` (UID 1000).

```bash
# 1. Configure — set THEFEED_DOMAIN and THEFEED_KEY (uncomment Telegram vars if you need private channels)
cp .env.example .env && nano .env

# 2. Add your channels
mkdir -p data
cp configs/channels.txt data/
cp configs/x_accounts.txt data/   # optional

# 3. Build and run (listens on :5300/udp inside the container)
docker compose up -d
docker compose logs -f

# 4. Print the client import config (the thefeed:// URI — domain, key, server key sk=,
#    resolvers) to hand to your users, just like the script prints at the end:
docker compose run --rm server --print-config --data-dir /data --domain YOUR_DOMAIN --key YOUR_KEY
```

Then set up the [port 53 redirect](#port-53) below. For private channels, do a one-time interactive login first:

```bash
docker compose run -it --rm server --login-only --data-dir /data \
  --domain YOUR_DOMAIN --key YOUR_KEY \
  --api-id YOUR_API_ID --api-hash YOUR_HASH --phone YOUR_PHONE
# then remove --no-telegram from docker-compose.yml, add the Telegram flags, and `docker compose up -d`
```

### Port 53

The server must receive **external** DNS on UDP **53**, but binding `:53` directly conflicts with `systemd-resolved`. So it listens on an unprivileged port (`:5300`) and you redirect external `:53` to it with `iptables`. Local DNS on the host keeps working — only packets arriving on the external interface are redirected.

```bash
# Replace eth0 with your interface (check: ip a)
sudo iptables  -I INPUT -p udp --dport 5300 -j ACCEPT
sudo iptables  -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo ip6tables -I INPUT -p udp --dport 5300 -j ACCEPT
sudo ip6tables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300

# Persist across reboots (Debian/Ubuntu)
sudo apt install -y iptables-persistent && sudo netfilter-persistent save
```

**Undo instantly** if anything breaks:

```bash
sudo iptables -t nat -D PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo iptables -D INPUT -p udp --dport 5300 -j ACCEPT
sudo netfilter-persistent save
```

Quick sanity checks: `ss -ulnp | grep ':53 '` (expect only `systemd-resolved` on `127.0.0.53`), `dig +short google.com @127.0.0.53` (local DNS still works), `iptables -t nat -L PREROUTING -n | grep 5300` (redirect active).

### Managing the service

```bash
systemctl status thefeed-server
systemctl restart thefeed-server
journalctl -u thefeed-server -f

sudo vi /opt/thefeed/data/channels.txt   # edit channels, then:
sudo systemctl restart thefeed-server
```

The server also renders a terminal dashboard from its hourly reports (`<data-dir>/dns_hourly.jsonl`) — serves nothing on the network, just reads the data dir:

```bash
thefeed-server --data-dir /srv/thefeed --report                 # snapshot
thefeed-server --data-dir /srv/thefeed --report --report-refresh 5s   # live
```

It shows total / channel-fetch / metadata / media / chat query counts, per-channel and per-domain aggregates, and chat stats.

### Server flags

Key flags (also settable via env vars, e.g. `THEFEED_DOMAIN`, `THEFEED_KEY`, `THEFEED_ALLOW_MANAGE`):

| Flag | Default | Description |
|------|---------|-------------|
| `--data-dir` | `./data` | Data directory (channels, session, cache, config) |
| `--domain` | | DNS feed domain **(required)** |
| `--key` | | Encryption passphrase **(required)** |
| `--extra-domains` | | Comma-separated extra feed sub-domains (load spread + resilience) |
| `--chat-domains` | | Enable the [messenger](#messenger) on these sub-domains (separate from feed) |
| `--no-telegram` | `false` | Run without Telegram login (public channels only) |
| `--api-id` / `--api-hash` / `--phone` | | Telegram credentials (private channels) |
| `--login-only` | `false` | Authenticate to Telegram, save session, exit |
| `--listen` | `:5300` | DNS listen address |
| `--msg-limit` | `15` | Max messages fetched per channel |
| `--fetch-interval` | `10` | Fetch cycle in minutes (min 3) |
| `--allow-manage` | `false` | Allow remote send / channel management (leave off unless trusted) |
| `--padding` | `32` | Max random padding bytes (anti-DPI; 0 = off) |
| `--x-rss-instances` | `nitter.net,…` | Comma-separated X RSS base URLs — see the warning below |
| `--dns-media-enabled` | `false` | Serve media over the slow DNS relay |
| `--github-relay-enabled` | `false` | Serve media over the fast GitHub relay (needs `--github-relay-token` / `-repo`) |
| `--report` | | Render the terminal dashboard and exit |
| `--version` | | Show version and exit |

> **⚠ X (Twitter) posts are currently broken.** X content is fetched through
> Nitter, and the Nitter project has been **archived** — its public instances now
> answer with "Instance has been rate limited"
> ([zedeus/nitter#1442](https://github.com/zedeus/nitter/issues/1442)). Until a
> working source exists, the X section of the feed stops updating. Point
> `--x-rss-instances` at an instance you know still works, or leave X unused.
> **Server-side only** — Telegram channels, Mirror, the messenger and every
> client are unaffected.
>
> **Self-hosting Nitter still works.** The archived code runs fine: clone the
> repo, create session tokens from a real X account (scripts and instructions
> are in the Nitter repo/wiki), run it with Docker + Redis, and point
> `--x-rss-instances` at it (e.g. `http://127.0.0.1:8080` — only the thefeed
> server reads it, so it needs no domain or TLS). Keep the instance **private**:
> the project was archived after legal pressure aimed at public instances.

Full media-relay flags are in [How it works → Media relays](#media-relays).

---

## Messenger

An **optional**, standalone store-and-forward messenger between users of the same server — it never touches Telegram. Enable it by giving the server one or more dedicated sub-domains (with a matching [NS record](#1-dns-records), separate from the feed domains):

```bash
thefeed-server ... --chat-domains c.example.com     # or THEFEED_CHAT_DOMAINS=c.example.com
```

- **End-to-end encrypted** — only the two users can read a message; the server stores opaque blobs and verifies senders without reading anything. Contact names never leave the device.
- **Identity** — the client generates a recovery code locally; your address is 20 characters derived from it. Share the address out-of-band; the same recovery code works on any server.
- **Fail-closed** — chat activates only when the profile pins the server key (`sk=`) and the server's signed chat capability verifies. A signed bit in the feed metadata lets a keyless client tell you *"this server has a messenger — re-import the config with its key"* instead of failing silently.
- **Abuse limits** (advertised to clients): `--chat-send-per-hour` (30), `--chat-inbox-cap` (50), `--chat-per-pair-cap` (10), `--chat-max-msg-bytes` (500); undelivered messages expire after `--chat-ttl-hours` (72). `--chat-enabled=false` keeps the domains but advertises chat as disabled.

In the client, open **Chat** from the bottom nav. ✓ = stored on the server, ✓✓ = picked up by the recipient; matching safety emojis on both devices confirm the conversation is secure.

---

## How it works

```
                                  Encrypted DNS TXT
   ┌──────────────┐  feed meta + small media   ┌──────────────────┐    MTProto      ┌──────────┐
   │              │ ─────────────────────────▸ │      Server      │ ─────────────▸  │ Telegram │
   │    Client    │ ◂───────────────────────── │  (DNS auth +     │ ◂─────────────  │   API    │
   │  (Web UI)    │                            │   media relays)  │    RSS / HTTP   ┌──────────┐
   │              │  large media (fast relay)  │                  │ ─────────────▸  │  Nitter  │
   │              │ ◂───── api.github.com ◂──  │                  │ ◂─────────────  │ (X feed) │
   └──────────────┘     (uploaded by server)   └──────────────────┘                 └──────────┘
```

**Server** (outside the censored network): connects to Telegram and reads configured channels; fetches public X posts via RSS-compatible mirrors (no login); serves feed metadata and small media as **encrypted DNS TXT** responses with random padding (anti-DPI). Login once, run forever; `--no-telegram` reads public channels with no credentials. Optional **multi-domain** and **[messenger](#messenger)** modes. All state in one data directory.

**Client** (inside the censored network): a browser-based web UI (RTL/Farsi, VazirMatn font) that sends encrypted DNS queries through a shared **Resolver Bank** — resolvers are found via the Scanner, imported, or entered manually, and scored by success-rate + latency so healthy ones are preferred. **Scatter mode** fans a query out to several resolvers and takes the fastest. Media downloads are relay-aware and hash/size-verified. Includes the [messenger](#messenger), per-channel auto-update, new-message indicators, and a live DNS query log.

### Protocol

All communication is encrypted (AES-256) and rides standard DNS TXT queries/responses with variable padding and per-resolver scoring, so it blends with normal DNS. Message data is deflate-compressed before encryption; each query is independent (no session state on the wire).

### Media relays

Messages with photos, files, GIFs, audio, and video can be cached on the server and downloaded over the same channel. The server dedupes each file (by upstream id + content hash), pushes bytes to every enabled relay, and adds a small header to the message text:

```
[IMAGE]<size>:<flags>:<dnsCh>:<dnsBlk>:<crc32>[:<filename>]
optional caption
```

`<flags>` is a comma-separated list of per-relay availability bits (`1`=available, `0`=not): slot 0 = DNS, slot 1 = GitHub, future relays append; older clients ignore unknown slots. Each relay is independent — the same file can be served via several at once. Clients prefer the fastest available, retry on failure, and ask before falling back to a slower path. Block 0 of every DNS-cached file starts with a 16-byte header (CRC32 + version + compression byte + reserved) that the client verifies before delivering bytes. Downloads are cached client-side (IndexedDB, 7 days) and on the local client (`<dataDir>/media-cache/`, 7 days).

Two relays ship today:

- **DNS relay** (slow, censorship-resistant, off by default) — bytes split across DNS blocks. Default cap 100 KB.
- **GitHub relay** (fast, off by default) — bytes uploaded to a repo, pulled over plain HTTPS; needs a PAT with `contents:write`. Objects land at `<repo>/<sanitised-domain>/<size>_<crc32>`. Default cap 15 MB.

| Flag | Env | Default | Notes |
|------|-----|---------|-------|
| `--dns-media-enabled` | `THEFEED_DNS_MEDIA_ENABLED` | `false` | toggle DNS relay |
| `--dns-media-max-size` | `THEFEED_DNS_MEDIA_MAX_SIZE_KB` | `100` KB | per-file cap |
| `--dns-media-compression` | `THEFEED_DNS_MEDIA_COMPRESSION` | `gzip` | `none` / `gzip` / `deflate` |
| `--github-relay-enabled` | `THEFEED_GITHUB_RELAY_ENABLED` | `false` | toggle GitHub relay |
| `--github-relay-token` | `THEFEED_GITHUB_RELAY_TOKEN` | — | PAT, `contents:write` |
| `--github-relay-repo` | `THEFEED_GITHUB_RELAY_REPO` | — | `owner/repo` |
| `--github-relay-branch` | `THEFEED_GITHUB_RELAY_BRANCH` | `main` | branch for relay objects |
| `--github-relay-max-size` | `THEFEED_GITHUB_RELAY_MAX_SIZE_KB` | `15360` KB | per-file cap |
| `--github-relay-quota-gb` | `THEFEED_GITHUB_RELAY_QUOTA_GB` | `100` GB | repo size budget for warnings |

### ⚠ The relay repo grows forever — watch its size

Relay objects are written as git commits, and **git history keeps every blob**.
The TTL prune deletes files from the current tree, but that does **not** reclaim
space: the repo only ever gets bigger, until GitHub starts refusing pushes with
`403 Repository is above its size quota` (observed at ~100 GB). At that point
the relay can neither upload nor prune — a `git push --force` cannot help
either, because GitHub does not run garbage collection on demand.

What the server does for you:

- Polls the repo size every 30 min and logs a warning at **70%** and **90%** of
  `--github-relay-quota-gb`.
- Shows repo size, headroom, queue depth and last error under
  **Media cache & relay** in `thefeed-server --report`.
- Uploads each file to GitHub as it arrives and keeps only the returned object
  id, so a full or unreachable repo degrades to the DNS media path instead of
  growing in memory until the process is OOM-killed.

What you have to do when it fills up: **delete the repo on GitHub and create it
again with the same name.** If your token is **fine-grained** (`github_pat_…`)
and scoped to that one repository, **re-grant the repo in the token settings
afterwards**: fine-grained tokens are bound to a repository *ID*, which changes
when you recreate it, so the old grant stops working even though the name
matches. Classic `ghp_…` tokens with the `repo` scope are unaffected.
Ticking "Add a README" is fine — the server notices
its own folder is gone either way, resets its object index, and re-uploads on
demand, so no manual state cleanup is needed. Media stays available over DNS
while the relay is empty.

Use **one repo per server**. Two servers can share one safely (paths are
namespaced by an HMAC of the domain, and neither can delete the other's files),
but they contend on the same branch and share the same size budget.

---

## Security

**Two-part access control:**

- **Encryption passphrase (`--key`)** — required on both server and client. Anyone with it can read all channel messages (including private channels); share it only with people you trust.
- **Remote management (`--allow-manage`)** — when enabled, anyone with the passphrase can, from the client: **send messages through the server's Telegram account** (into channels / private chats — this is Telegram sending via the operator's logged-in account, and is *completely separate* from the end-to-end [messenger](#messenger)), and **change the feed's channel list** (add or remove the channels shown in the feed). Off by default; enable only on trusted servers.
- **Client web password (`--password`)** — HTTP Basic Auth on the web UI. Local protection only; it does **not** affect DNS-level access.

**Properties:** end-to-end AES-256 in both directions · random padding defeats size analysis · each query independent (no wire session state) · writes gated by `--allow-manage` · Telegram 2FA prompted interactively (never stored in args) · session file `0600`.

> **⚠️ Never share your passphrase publicly.** Anyone who has it can run their own client and read all your messages — there is no way to prevent that. The `--password` flag only guards the web UI on your own machine.

The optional [messenger](#messenger) is separately end-to-end encrypted per conversation and independent of the feed passphrase.

---

## Build from source

**Prerequisites:** Go 1.26+, and (for private channels) Telegram API credentials from <https://my.telegram.org>.

```bash
make build          # build server + client into ./build
make build-server   # server only
make build-client   # client only
make test           # tests with the race detector
make build-all      # cross-compile all platforms (incl. Android)
make vet            # go vet
```

**Run the server** (see [flags](#server-flags)):

```bash
./build/thefeed-server --data-dir ./data --domain t.example.com --key "passphrase" --no-telegram --listen ":5300"
# private channels: add --login-only once (with --api-id/--api-hash/--phone), then run without it
```

**Run the client:** `./build/thefeed-client` (creates `./thefeeddata/`, opens `http://127.0.0.1:8080`). Options: `--data-dir`, `--port` (8080), `--password`. Per-config options (resolvers, scatter, rate limit, timeout) are set in the web UI, not via flags.

**macOS:** `make mac-dmg` → `build/Thefeed.app` + universal `.dmg`. Data lives under `~/Library/Application Support/Thefeed`; a menu-bar item offers **Open / Quit**.

**Android APK:**

```bash
make build-android-arm64
cp build/thefeed-client-android-arm64 android/app/src/main/assets/thefeed-client
cd android && gradle wrapper --gradle-version 8.10.2 && ./gradlew assembleDebug
# → android/app/build/outputs/apk/debug/app-debug.apk
```

**iOS:** wraps the Go client as a gomobile-bound xcframework consumed by a SwiftUI app under `ios/`. The server runs in-process on `127.0.0.1:<random-port>`, foreground only. Needs Xcode 15+, Go 1.26+, gomobile (`go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init`).

```bash
make ios-bind    # build Mobile.xcframework (device + Simulator)
make ios-build   # build the app for the Simulator
```

Then open `ios/Thefeed.xcodeproj` in Xcode to run on a device.

**Releases:** pushing a tag starting with `v` triggers a CI build + release. A tag containing `-` (e.g. `v1.4.0-rc1`) is marked pre-release automatically. Assets include server/client binaries for every platform plus the Android APKs (`arm64-v8a`, `armeabi-v7a`).

---

## Reference

### Config file formats

All optional, in the data directory; `#` starts a comment and blank lines are ignored.

- **`channels.txt`** — public Telegram channels, one `@username` per line.
- **`x_accounts.txt`** — public X (Twitter) usernames, one per line.
- **`private_channels.txt`** — private-channel invite links, one per line. Requires Telegram login — the server joins each channel at startup, then fetches it like any other. Accepts `https://t.me/+…`, `t.me/joinchat/…`, `tg://join?invite=…`, or the bare invite hash.

```
# channels.txt      # x_accounts.txt    # private_channels.txt
@VahidOnline        Vahid               https://t.me/+aBcDeF123456
```

### The web UI

A Telegram-style shell with a **bottom navigation bar** across five sections:

- **Feed** — channel/X feed grouped by type (Public/X/Private), native RTL/Farsi rendering, a floating composer (send to channels/private chats when Telegram is connected), per-channel new-message badges, media-tag highlighting, in-channel search, and a live DNS query log. **Saved Messages** (encrypted local notes + bookmarks) lives here.
- **Mirror** (Telemirror) — a read-only, Telegram-web-style mirror of channels with aspect-ratio-stable image/album rendering.
- **Chat** — the end-to-end-encrypted [messenger](#messenger).
- **Resolver** — the shared **Bank**, your named resolver **lists**, and the **Scanner** in one place. The Scanner probes IPs / CIDRs (e.g. `5.1.0.0/16`) or domains to find DNS servers that can reach your server; results sort by latency, expand a working resolver's /24, and can be applied to a config directly. A one-click preset loads a curated CIDR list.
- **Settings** — **Display** (theme, font, language, wallpaper), **Connection** (query mode, rate limit, scatter, timeout, password, debug), **Storage** (disk-cache budget), **Backup** (encrypted export/import), **About**, and **Configs** (import/manage, with ready-made starter configs). Theme follows the device by default. All fields auto-save.

### X fetch safety

X posts are fetched via RSS/XML only. Instance URLs are validated (`http`/`https`, host-only), response size is capped and timeouts enforced, and on a `403`/failure the server tries the next configured instance. Set your own trusted mirrors with `--x-rss-instances`.

---

## Links & donate

- Telegram channel: [@networkti](https://t.me/networkti) · Public configs: [@thefeedconfig](https://t.me/thefeedconfig)
- Roadmap / task board: [GitHub project](https://github.com/users/sartoopjj/projects/1/views/1)

**Donate** — any amount in USDT or USDC on **Polygon** or **BNB Chain**:
`0xe73f022f668c57cce79feccd875ac7332311013a` — thank you ❤️

## License

MIT

---

<div align="center">

**For FREE IRAN** <img src="internal/web/static/lion-sun.svg" alt="Lion-and-Sun" height="20">

*Everyone deserves free access to information*

</div>
