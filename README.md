# thefeed

DNS-based feed reader for Telegram channels and public X accounts. Designed for environments where only DNS queries work.

[English](README.md) | [فارسی](README-FA.md)

## Download

- **[Latest release](https://github.com/sartoopjj/thefeed/releases/latest)** — server / client binaries for every platform, plus Android APKs.
- **Server one-liner** (Linux + systemd):
  ```bash
  sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"
  ```
- **Android APK** (Android 7.0+): pick `arm64-v8a` for any phone newer than ~2017, `armeabi-v7a` for older 32-bit-only devices.
- **iOS** (iOS 14+): App Store build planned. Source under [ios/](ios/) — see [iOS development](#ios-development) below.

Public configs to test with: [@thefeedconfig](https://t.me/thefeedconfig).

## How It Works

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

**Server** (runs outside censored network):
- Connects to Telegram, reads messages from configured channels
- Fetches public X posts via RSS-compatible mirrors (no login)
- Serves feed metadata + small media as encrypted DNS TXT responses
- **Media relays** — same file, multiple delivery paths:
  - **DNS relay** (slow, censorship-resistant) splits bytes across DNS blocks
  - **GitHub relay** (fast, default off) uploads bytes to a repo so clients pull via plain HTTPS; intended for files that are too big for DNS
  - Future relays slot in alongside without breaking older clients
- Random padding on responses (anti-DPI)
- Session persistence — login once, run forever
- No-Telegram mode (`--no-telegram`) — reads public channels without credentials
- All data stored in a single directory

**Client** (runs inside censored network):
- Browser-based web UI with RTL/Farsi support (VazirMatn font)
- Sends encrypted DNS TXT queries via the resolver bank
- **Resolver Bank**: shared pool of DNS resolvers used across all profiles. Resolvers are added via scanner, import, or manual entry and scored automatically
- **Resolver scoring**: per-resolver success-rate + latency scoreboard with persistent scores; healthier resolvers are preferred. Low-scoring entries can be pruned
- **Scatter mode**: fans out the same DNS request to multiple resolvers and uses the fastest response (default: 2 concurrent)
- **Relay-aware media downloads** — picks the fast relay when the manifest advertises one, retries on transient failure, asks before falling back to the slow DNS path. Hash + size verified on every download
- Send messages to channels and private chats (requires server `--allow-manage` + Telegram login)
- Channel management (add/remove channels remotely via admin commands when `--allow-manage` is enabled)
- **Per-channel auto-update**: pin specific channels for periodic background refresh, persisted per profile
- Message compression (deflate) for efficient transfer
- Web UI password protection (`--password` on client)
- New-message indicators (channel-list NEW badge + in-chat separator), next-fetch countdown timer
- Channel type badges (Private/Public/X) with separate colors
- Media type detection (`[IMAGE]`, `[VIDEO]`, etc.) and inline rendering
- Live DNS query log in the browser

## Protocol

All communication is encrypted (AES-256) and rides on standard DNS TXT queries / responses, with variable padding and per-resolver scoring so traffic blends with normal DNS activity. Message data is deflated before encryption.

## Image and File Downloads

Messages with attached photos, files, GIFs, audio, and videos can be cached on the server and downloaded over the same encrypted DNS channel.

The server downloads each attached media file (deduped by upstream id and content hash), pushes the bytes to every enabled relay, and adds a small metadata header to the message text:

```
[IMAGE]<size>:<flags>:<dnsCh>:<dnsBlk>:<crc32>[:<filename>]
optional caption
```

`<flags>` is a comma-separated list of per-relay availability bits (`1`=available, `0`=not). Slot 0 is DNS, slot 1 is the GitHub relay; future relays append. Older clients ignore slots they don't know.

Block 0 of every DNS-cached file begins with a 16-byte protocol header — 4 bytes CRC32 of the (decompressed) content, 1 byte version, 1 byte compression, 10 bytes reserved for future fields. The client checks the CRC against the expected value before delivering any bytes. The remaining bytes are decompressed per the compression byte. Downloads are cached on the client (IndexedDB, 7 days) and on the local thefeed-client server (`<dataDir>/media-cache/`, 7 days). Concurrent downloads are limited and extra clicks are queued.

### Media relays

Each relay is independent — the same file can be served via DNS *and* GitHub *and* future relays at the same time. Clients pick whichever the message manifest advertises and prefer the fastest available; on failure they retry, then ask before falling back to a slower one. Hash + size are verified on every download.

Two relays ship today:

- **DNS relay** (slow, default on). Bytes are split into DNS blocks. Survives in censored networks. Default cap: 100 KB.
- **GitHub relay** (fast, default off). Bytes are uploaded to a repo and pulled by clients over plain HTTPS. Needs a personal access token with `contents:write`. Files land at `<repo>/<sanitised-domain>/<size>_<crc32>` so multiple deployments can share one repo. Default cap: 15 MB.

Block 0 of every DNS-cached file begins with a 16-byte protocol header — 4 bytes CRC32 of the (decompressed) content, 1 byte version, 1 byte compression, 10 bytes reserved. The remaining bytes are decompressed per the compression byte. Downloads are cached on the client (IndexedDB, 7 days) and on the local thefeed-client server (`<dataDir>/media-cache/`, 7 days). Concurrent downloads are limited and extra clicks are queued.

Server flags / env vars:

| Flag                          | Env                                  | Default     | Notes                              |
|-------------------------------|--------------------------------------|-------------|------------------------------------|
| `--dns-media-enabled`         | `THEFEED_DNS_MEDIA_ENABLED`          | `false`     | toggle DNS relay                   |
| `--dns-media-max-size`        | `THEFEED_DNS_MEDIA_MAX_SIZE_KB`      | `100` (KB)  | per-file cap                       |
| `--dns-media-cache-ttl`       | `THEFEED_DNS_MEDIA_CACHE_TTL_MIN`    | `600` (min) | TTL                                |
| `--dns-media-compression`     | `THEFEED_DNS_MEDIA_COMPRESSION`      | `gzip`      | `none`, `gzip`, or `deflate`       |
| `--github-relay-enabled`      | `THEFEED_GITHUB_RELAY_ENABLED`       | `false`     | toggle GitHub relay                |
| `--github-relay-token`        | `THEFEED_GITHUB_RELAY_TOKEN`         | —           | PAT, `contents:write`              |
| `--github-relay-repo`         | `THEFEED_GITHUB_RELAY_REPO`          | —           | `owner/repo`                       |
| `--github-relay-branch`       | `THEFEED_GITHUB_RELAY_BRANCH`        | `main`      | branch to commit relay objects to  |
| `--github-relay-max-size`     | `THEFEED_GITHUB_RELAY_MAX_SIZE_KB`   | `15360` (KB)| per-file cap                       |
| `--github-relay-ttl`          | `THEFEED_GITHUB_RELAY_TTL_MIN`       | `600` (min) | orphans pruned next refresh cycle  |

The hourly DNS report includes `totalMediaQueries` and a `mediaCache` block (entries, bytes, hits, misses, evictions).

## Donate:

To support me, you can send any amount you wish in USDT or USDC on the following networks:

- Polygon
- BNB Chain

My wallet address:
`0xe73f022f668c57cce79feccd875ac7332311013a`

Thank you for your support ❤️

## Links
- My telegram channel: [@networkti](https://t.me/networkti)
- Public TheFeed Configs: [@thefeedconfig](https://t.me/thefeedconfig)
- Setup TheFeed server guide: [@networkti](https://t.me/networkti/25)
- Setup TheFeed server with SlipGate guide: [@networkti](https://t.me/networkti/200)
- Roadmap / task board: [GitHub project](https://github.com/users/sartoopjj/projects/1/views/1)

## Quick Install (Server)

One-line install (downloads latest release from GitHub)

```bash
sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"
```

Or manually:

```bash
# On your server (Linux with systemd)
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh -o install.sh
sudo bash install.sh
```

The script will:
1. Download the latest release binary from GitHub
2. Ask for your domain, passphrase, Telegram channels, and X accounts
3. Ask whether to use Telegram login (recommended: **No** — public channels work without it)
4. If Telegram mode: ask for API credentials and login
5. Set up a systemd service

Update:
```bash
sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"
```

Install a specific version (rollback, beta, or rc):
```bash
# Roll back to a known-good tag
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --version v0.9.2

# Install the most recent pre-release (beta / rc)
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --pre

# List recent releases (stable / pre-release labels)
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --list
```

Short forms: `-v <tag>` is the same as `--version <tag>`. The legacy positional form
`sudo bash install.sh v1.0.0` still works.

Re-login: `curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --login`
Uninstall: `curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --uninstall`


> **Note:** The server needs to receive packets on external port 53. Running on `:53` directly requires root. It's better to listen on an unprivileged port (`:5300`) and port-forward 53 to it.
>
> Replace `eth0` with your actual network interface name (check with `ip a`):
> ```bash
> sudo iptables -I INPUT -p udp --dport 5300 -j ACCEPT
> sudo iptables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
> sudo ip6tables -I INPUT -p udp --dport 5300 -j ACCEPT
> sudo ip6tables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
> ```
>
> To make these rules persistent across reboots:
> ```bash
> sudo apt install iptables-persistent   # Debian/Ubuntu
> sudo netfilter-persistent save
> ```



**If something goes wrong — remove the redirect instantly:**

```bash
# Remove the iptables rule (restores original behavior)
sudo iptables -t nat -D PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo iptables -D INPUT -p udp --dport 5300 -j ACCEPT
sudo netfilter-persistent save
```

## Docker Deployment (Server)

Run the server with Docker — no Go toolchain needed.

### Quick Start (public channels, no Telegram login)

```bash
# 1. Configure environment
cp .env.example .env
nano .env   # set THEFEED_DOMAIN and THEFEED_KEY

# 2. Prepare data directory with your channels
mkdir -p data
cp configs/channels.txt data/
cp configs/x_accounts.txt data/   # optional

# 3. Build and run
docker compose up -d

# 4. Redirect external DNS traffic to the container
#    Replace eth0 with your network interface (check with: ip a)
sudo iptables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo iptables -I INPUT -p udp --dport 5300 -j ACCEPT
sudo ip6tables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo ip6tables -I INPUT -p udp --dport 5300 -j ACCEPT

# Make iptables rules persistent across reboots
sudo apt install -y iptables-persistent
sudo netfilter-persistent save

# 5. View logs
docker compose logs -f
```

> **Note:** The container listens on port 5300 (not 53) to avoid conflict with `systemd-resolved`.
> The `iptables PREROUTING` rule redirects only **external** DNS traffic (port 53) to the container,
> while local DNS resolution on the server continues to work normally.

### With Telegram (one-time interactive login)

```bash
# 1. Configure environment (uncomment Telegram vars in .env)
cp .env.example .env
nano .env

# 2. One-time login (interactive — enter auth code when prompted)
docker compose run -it --rm server \
  --login-only --data-dir /data \
  --domain YOUR_DOMAIN --key YOUR_KEY \
  --api-id YOUR_API_ID --api-hash YOUR_HASH \
  --phone YOUR_PHONE

# 3. Edit docker-compose.yml: remove --no-telegram and add Telegram flags
# 4. Start the server
docker compose up -d
# 5. Set up iptables redirect (same as Quick Start step 4)
```

### Docker Details

| Item | Value |
|------|-------|
| Base image | `alpine:3.21` (~23 MB total) |
| Build | Multi-stage (`golang:1.26-alpine` → `alpine`) |
| User | `thefeed` (UID 1000, non-root) |
| Container port | `:5300/udp` (host `:5300/udp` + iptables redirect from `:53`) |
| Data | `./data` volume (channels, session, cache) |
| Config | `.env` file (gitignored) |

```bash
# Rebuild after code changes
docker compose build

# Stop
docker compose down
```

### Port 53 & Service Safety

The container listens on port **5300** (not 53) to avoid conflicts with `systemd-resolved` or other DNS services on the host. External DNS traffic is redirected via `iptables PREROUTING` which only affects packets arriving on the external network interface — local DNS resolution is **not** affected.

**Before setup — check what uses port 53:**

```bash
# Check if port 53 is in use
ss -ulnp | grep ':53 '

# Expected: systemd-resolved on 127.0.0.53 only (safe)
# UNCONN  127.0.0.53%lo:53  users:(("systemd-resolve",...))
```

**After setup — verify nothing is broken:**

```bash
# 1. Local DNS still works (server can resolve domains)
dig +short google.com @127.0.0.53

# 2. thefeed container is running
docker ps --filter name=thefeed

# 3. thefeed is fetching channels
docker logs thefeed-server --tail 5

# 4. iptables rule is active
iptables -t nat -L PREROUTING -n | grep 5300

# 5. Other containers are healthy
docker ps --format 'table {{.Names}}\t{{.Status}}' | head -10
```

**If something goes wrong — remove the redirect instantly:**

```bash
# Remove the iptables rule (restores original behavior)
sudo iptables -t nat -D PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo iptables -D INPUT -p udp --dport 5300 -j ACCEPT
sudo netfilter-persistent save
```

## Manual Setup

### Prerequisites

- Go 1.26+
- A domain with NS records pointing to your server
- Telegram API credentials from https://my.telegram.org (only if you need private channels)

### Server

```bash
# Build
make build-server

# First run: login to Telegram and save session
./build/thefeed-server \
  --login-only \
  --data-dir ./data \
  --domain t.example.com \
  --key "your-secret-passphrase" \
  --api-id 12345 \
  --api-hash "your-api-hash" \
  --phone "+1234567890"

# Normal run (uses saved session from data directory)
./build/thefeed-server \
  --data-dir ./data \
  --domain t.example.com \
  --key "your-secret-passphrase" \
  --api-id 12345 \
  --api-hash "your-api-hash" \
  --phone "+1234567890" \
  --listen ":53"
```

All data files (session, channels, x accounts) are stored in the `--data-dir` directory (default: `./data`).

Environment variables: `THEFEED_DOMAIN`, `THEFEED_KEY`, `THEFEED_MSG_LIMIT`, `THEFEED_FETCH_INTERVAL`, `THEFEED_ALLOW_MANAGE` (set to `0` to force-disable even if the flag is baked into the service), `THEFEED_X_RSS_INSTANCES`, `TELEGRAM_API_ID`, `TELEGRAM_API_HASH`, `TELEGRAM_PHONE`, `TELEGRAM_PASSWORD`

#### Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--data-dir` | `./data` | Data directory for channels, session, config |
| `--domain` | | DNS domain (required) |
| `--key` | | Encryption passphrase (required) |
| `--channels` | `{data-dir}/channels.txt` | Path to channels file |
| `--x-accounts` | `{data-dir}/x_accounts.txt` | Path to X usernames file |
| `--x-rss-instances` | `https://nitter.net,http://nitter.net` | Comma-separated X RSS base URLs |
| `--api-id` | | Telegram API ID (required) |
| `--api-hash` | | Telegram API Hash (required) |
| `--phone` | | Telegram phone number (required) |
| `--session` | `{data-dir}/session.json` | Path to Telegram session file |
| `--login-only` | `false` | Authenticate to Telegram, save session, exit |
| `--no-telegram` | `false` | Run without Telegram login (public channels only) |
| `--listen` | `:5300` | DNS listen address |
| `--padding` | `32` | Max random padding bytes (0=disabled) |
| `--msg-limit` | `15` | Maximum messages to fetch per Telegram channel |
| `--fetch-interval` | `10` | Fetch cycle interval in minutes (min 3) |
| `--allow-manage` | `false` | Allow remote send/channel management (default: disabled) |
| `--debug` | `false` | Log every decoded DNS query |
| `--dns-media-enabled` | `false` | Serve media via DNS (slow relay) |
| `--dns-media-max-size` | `100` | Per-file cap for the DNS relay in KB (0 = no cap) |
| `--dns-media-cache-ttl` | `600` | DNS-relay TTL, in minutes |
| `--dns-media-compression` | `gzip` | DNS-relay compression: `none`, `gzip`, or `deflate` |
| `--github-relay-enabled` | `false` | Serve media via the GitHub fast relay |
| `--github-relay-token` | | PAT with `contents:write` (or `THEFEED_GITHUB_RELAY_TOKEN`) |
| `--github-relay-repo` | | `owner/repo` for the relay |
| `--github-relay-branch` | `main` | Branch to commit relay objects to |
| `--github-relay-max-size` | `15360` | Per-file cap for the GitHub relay in KB |
| `--github-relay-ttl` | `600` | GitHub-relay TTL in minutes (orphans pruned next cycle) |
| `--version` | | Show version and exit |

### Client

```bash
# Build
make build-client

# Run (opens web UI in browser)
./build/thefeed-client

# Custom data directory and port
./build/thefeed-client --data-dir ./mydata --port 9090

# With remote management enabled
./build/thefeed-client --password "your-secret"
```

On first run, the client creates a `./thefeeddata/` directory next to where you run it. Open `http://127.0.0.1:8080` in your browser and configure your domain and passphrase through the Settings page. DNS resolvers are managed in the shared Resolver Bank (accessible from the sidebar), which is used by all profiles.

All configuration, cache, and data files are stored in the data directory.

#### Client Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--data-dir` | `./thefeeddata` | Data directory for config, cache |
| `--port` | `8080` | Web UI port |
| `--password` | | Password for web UI (empty = no auth) |
| `--version` | | Show version and exit |

The **concurrent requests (scatter)** setting and all other profile options (resolvers, rate limit, query mode, timeout) are configured through the web UI profile editor, not CLI flags.

#### Android (Termux)

```bash
# Install Termux from F-Droid
pkg update && pkg install curl

# Download Android binary
curl -Lo thefeed-client https://github.com/sartoopjj/thefeed/releases/latest/download/thefeed-client-android-arm64
chmod +x thefeed-client
./thefeed-client
# Open in browser: http://127.0.0.1:8080
```

#### Android (Native APK Wrapper)

> download it from the latest release assets:
> - `thefeed-android-<version>-arm64-v8a.apk` — modern 64-bit phones (most devices since 2017)
> - `thefeed-android-<version>-armeabi-v7a.apk` — older 32-bit phones
>
> If you install the wrong one, Android may accept the install but the bundled native binary won't run. Pick `arm64-v8a` unless you know your device is 32-bit only.

The Android app automatically requests battery optimization exemption on first launch so the background service is not killed by the OS.


You can build or download a native Android app that:
- runs thefeed client binary in a foreground/background service
- opens the local web UI inside an in-app WebView

Project path:
- `android/`

Build steps:

```bash
# 1) Build Android binary from project root
make build-android-arm64

# 2) Copy binary into Android app assets (required filename)
cp build/thefeed-client-android-arm64 android/app/src/main/assets/thefeed-client

# 3) Build debug APK
cd android
gradle wrapper --gradle-version 8.10.2
./gradlew assembleDebug
```

APK output:

```bash
android/app/build/outputs/apk/debug/app-debug.apk
```

Install on device:

```bash
adb install -r android/app/build/outputs/apk/debug/app-debug.apk
```

### Web UI

The browser-based UI has:
- **Channels sidebar** (left): channel list grouped by type (Public/X/Private) with badges
- **Messages panel** (right): messages with native RTL/Farsi rendering (VazirMatn font)
- **Send panel**: send messages to channels and private chats when Telegram is connected
- **New message badges**: visual indicators for channels with new messages
- **Next-fetch timer**: countdown to next automatic refresh
- **Media detection**: `[IMAGE]`, `[VIDEO]`, `[DOCUMENT]` tag highlighting
- **Message search**: search within the current channel's messages with match highlighting and prev/next navigation
- **Export messages**: export the last N messages of a channel to clipboard
- **Log panel** (bottom): live DNS query log
- **Settings modal**: configure domain, passphrase, resolvers, query mode, rate limit, concurrent requests (scatter), timeout, debug mode
- **Working resolvers**: view the list of currently active/healthy resolvers from settings
- **Background image**: set a custom background image URL for the messages panel (stored locally)
- **DNS query timeout**: configurable per-profile DNS query timeout (default 15s) in the profile editor
- **Per-profile cache**: 1-hour browser cache so data is visible instantly on reopen
- **Resolver Scanner**: scan IP ranges and CIDRs to discover working DNS resolvers

### Resolver Scanner

The web UI includes a built-in resolver scanner (🔍 icon in sidebar) that probes IP ranges to discover DNS servers capable of reaching your thefeed server. Features:

- **Flexible targets**: enter individual IPs, CIDRs (e.g. `5.1.0.0/16`), or domain names — one per line
- **Default CIDR preset**: one-click button to load the bundled curated CIDR range list
- **Clear targets**: button to quickly clear the scanner CIDR/IP list
- **Profile-aware**: select which profile's domain and passphrase to use for probing
- **Configurable**: set concurrency (default 50), timeout (default 15s), and max IPs to scan
- **Expand /24**: when a working resolver is found, automatically scan all nearby IPs in the same /24 subnet
- **Pause / Resume / Stop**: full control over long-running scans (pause actually stops dispatching new probes)
- **Response time**: results are sorted by latency so the fastest resolvers appear first
- **Selectable results**: checkboxes to select which resolvers to apply or copy
- **Apply results**: append to or overwrite your profile's resolver list directly from the scanner
- **Copy**: per-IP copy buttons, copy selected, or copy all discovered resolver IPs
- **New Scan**: reset the UI to start a fresh scan after completion
- **Debug logging**: when debug mode is enabled, individual probe queries/responses are logged
- **Profile editor shortcut**: open the scanner directly from a profile's edit page with "Find Resolvers" button

## Development

```bash
make test        # Run tests with race detector
make build       # Build both binaries
make build-all   # Cross-compile all platforms (incl. Android)
make upx         # Compress Linux/Windows/Android binaries with UPX
make vet         # Go vet
make fmt         # Format code
make clean       # Remove build artifacts
```

## iOS development

Wraps the Go client as a gomobile-bound xcframework consumed by a SwiftUI app under `ios/`. Server runs in-process on `127.0.0.1:<random-port>`; foreground only (iOS does not allow long-lived background servers).

Prereqs on macOS: Xcode 15+, Go 1.22+, gomobile.

```
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init
```

Common targets:

```
make ios-bind            # build Mobile.xcframework (iOS device + Simulator)
make ios-bind-catalyst   # also include Mac Catalyst slice
make ios-build           # build the app for the Simulator
make ios-test            # run unit tests on the Simulator
make ios-list-sims       # list available simulator destinations
```

Override the default simulator with `IOS_SIM_NAME='iPhone 16'`.

Open `ios/Thefeed.xcodeproj` in Xcode after `make ios-bind` to run from Xcode.

## Releases (GitHub Actions)

Pushing a tag that starts with `v` triggers CI build + GitHub Release.

- Stable release tag example: `v1.4.0`
- Pre-release tag examples: `v1.4.0-rc1`, `v1.4.0-beta.2`

Rule:
- If tag contains `-`, release is marked as **pre-release** automatically.

Release assets include:
- Server/client binaries for all current target platforms
- Native Android wrapper APK (64-bit, recommended): `thefeed-android-<version>-arm64-v8a.apk`
- Native Android wrapper APK (32-bit, legacy devices): `thefeed-android-<version>-armeabi-v7a.apk`

## DNS Records Setup

You need **two DNS records** on your domain. Suppose your server IP is `203.0.113.10` and you want to use `example.com`:

### 1. A Record for the NS server

| Type | Name | Value |
|------|------|-------|
| A | `ns.example.com` | `203.0.113.10` |

This points a hostname to your server IP.

### 2. NS Record for the tunnel subdomain

| Type | Name | Value |
|------|------|-------|
| NS | `t.example.com` | `ns.example.com` |

This delegates all DNS queries for `t.example.com` (and its subdomains) to your server.


## channels.txt Format

```
# Comments start with #
@VahidOnline
```

## x_accounts.txt Format

```
# Comments start with #
Vahid
```

## X Fetch Safety

- X fetching uses RSS/XML only.
- Instance URLs are validated (`http`/`https`, host-only, no path/query/fragment).
- Response body size is capped and request timeouts are enforced.
- If a mirror returns `403`/fails, the server automatically tries the next configured instance.
- Recommended: set your own trusted mirror list with `--x-rss-instances` (or `THEFEED_X_RSS_INSTANCES`).

## Security

### Two-Part Access Control

**Encryption passphrase (`--key`):** Required on both server and client. Anyone with this passphrase can read all channel messages (including private channels). You can share it with trusted friends so they can read too.

**Remote management (`--allow-manage` on server):** When enabled, anyone with the encryption key can also send messages and manage channels. Disabled by default. Only enable on trusted servers.

**Client web password (`--password`):** Protects all web UI endpoints with HTTP Basic Auth. This is local protection only — it does NOT affect DNS-level access.

### Security Properties

- All communication is end-to-end encrypted (AES-256)
- Pre-shared passphrase required for both client and server
- Each query is independent — no session state on the wire
- Random padding in both directions prevents traffic analysis
- Write operations gated by server-side `--allow-manage` flag
- Telegram 2FA password is prompted interactively (never stored in args)
- Session file stored with restricted permissions (0600)

> **⚠️ Warning:** If you share your passphrase publicly, **anyone** can run their own
> client with your passphrase and read all your messages. There is no way to prevent this.
> The client `--password` flag only protects the web UI on your own machine — it does NOT stop
> others from using the passphrase. **Never share your passphrase publicly.**

## Service Management

```bash
# After install.sh
systemctl status thefeed-server
systemctl restart thefeed-server
journalctl -u thefeed-server -f

# Update channels
sudo vi /opt/thefeed/data/channels.txt
sudo systemctl restart thefeed-server

# Update binary
sudo bash scripts/install.sh
```

## License

MIT

---

<div align="center">

**For FREE IRAN** <img src="internal/web/static/lion-sun.svg" alt="Lion-and-Sun" height="20">

*Everyone deserves free access to information*

</div>
