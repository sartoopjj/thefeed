# thefeed

DNS-based feed reader for Telegram channels. Designed for environments where only DNS queries work.

## How It Works

```
┌──────────────┐     DNS TXT Query       ┌──────────────┐     MTProto     ┌──────────┐
│    Client    │ ──────────────────────▸ │    Server    │ ──────────────▸ │ Telegram │
│  (Web UI)    │ ◂────────────────────── │  (DNS auth)  │ ◂────────────── │   API    │
└──────────────┘     Encrypted TXT       └──────────────┘                 └──────────┘
```

**Server** (runs outside censored network):
- Connects to Telegram, reads messages from configured channels
- Serves feed data as encrypted DNS TXT responses
- Random padding on responses to vary size (anti-DPI)
- Session persistence — login once, run forever
- All data stored in a single directory

**Client** (runs inside censored network):
- Browser-based web UI with RTL/Farsi support (VazirMatn font)
- Configure via the web UI — no CLI flags needed
- Sends encrypted DNS TXT queries via available resolvers
- Single-label base32 encoding (stealthier) or double-label hex
- Rate limiting to respect resolver limits
- Live DNS query log in the browser
- All data (config, cache) stored next to the binary

## Anti-DPI Features

- **Variable response size**: Random padding (0-32 bytes) on each DNS response prevents fingerprinting by fixed packet size
- **Single-label queries**: Base32 encoded subdomain in one DNS label (`abc123def.t.example.com`) instead of the more detectable two-label hex pattern
- **Resolver shuffling**: Queries are distributed across resolvers randomly
- **Rate limiting**: Configurable query rate to blend with normal DNS traffic
- **Concurrency limiting**: Max 3 concurrent block fetches to avoid DNS bursts
- **Random query padding**: 4 random bytes in each query payload

## Protocol

**Block size**: 180 bytes payload (fits in 512-byte UDP DNS with padding + encryption overhead)

**Query format** (single-label, default): `[base32_encrypted].t.example.com`
**Query format** (double-label): `[hex_part1].[hex_part2].t.example.com`
- Payload: 4 random bytes + 2 channel + 2 block = 8 bytes, AES-256-GCM encrypted

**Response**: `[2-byte length][data][random padding]` → AES-256-GCM encrypted → Base64

**Encryption**: AES-256-GCM with HKDF-derived keys from shared passphrase

## Quick Install (Server)

One-line install (downloads latest release from GitHub)

```bash
bash <(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)
```

Or manually:

```bash
# On your server (Linux with systemd)
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh -o install.sh
sudo bash install.sh
```

The script will:
1. Download the latest release binary from GitHub
2. Ask for your domain, passphrase, Telegram credentials, channels
3. Login to Telegram interactively (one-time)
4. Set up a systemd service

Update: `sudo bash install.sh` (detects existing config, only updates binary)
Re-login: `sudo bash install.sh --login`
Uninstall: `sudo bash install.sh --uninstall`

## Manual Setup

### Prerequisites

- Go 1.26+
- Telegram API credentials from https://my.telegram.org
- A domain with NS records pointing to your server

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
  --listen ":5300"
```

All data files (session, channels) are stored in the `--data-dir` directory (default: `./data`).

Environment variables: `THEFEED_DOMAIN`, `THEFEED_KEY`, `TELEGRAM_API_ID`, `TELEGRAM_API_HASH`, `TELEGRAM_PHONE`, `TELEGRAM_PASSWORD`

#### Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--data-dir` | `./data` | Data directory for channels, session, config |
| `--domain` | | DNS domain (required) |
| `--key` | | Encryption passphrase (required) |
| `--channels` | `{data-dir}/channels.txt` | Path to channels file |
| `--api-id` | | Telegram API ID (required) |
| `--api-hash` | | Telegram API Hash (required) |
| `--phone` | | Telegram phone number (required) |
| `--session` | `{data-dir}/session.json` | Path to Telegram session file |
| `--login-only` | `false` | Authenticate to Telegram, save session, exit |
| `--listen` | `:5300` | DNS listen address |
| `--padding` | `32` | Max random padding bytes (0=disabled) |
| `--version` | | Show version and exit |

### Client

```bash
# Build
make build-client

# Run (opens web UI in browser)
./build/thefeed-client

# Custom data directory and port
./build/thefeed-client --data-dir ./mydata --port 9090
```

On first run, the client creates a `./thefeeddata/` directory next to where you run it. Open `http://127.0.0.1:8080` in your browser and configure your domain, passphrase, and resolvers through the Settings page.

All configuration, cache, and data files are stored in the data directory.

#### Client Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--data-dir` | `./thefeeddata` | Data directory for config, cache |
| `--port` | `8080` | Web UI port |
| `--version` | | Show version and exit |

### Web UI

The browser-based UI has:
- **Channels sidebar** (left): channel list with selection
- **Messages panel** (right): messages with native RTL/Farsi rendering (VazirMatn font)
- **Log panel** (bottom): live DNS query log
- **Settings modal**: configure domain, passphrase, resolvers, query mode, rate limit

## Development

```bash
make test        # Run tests
make build       # Build both binaries
make build-all   # Cross-compile all platforms
make vet         # Go vet
make fmt         # Format code
make clean       # Remove build artifacts
```

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

## channels.txt Format

```
# Comments start with #
@VahidOnline
```

## Security

- All queries and responses are encrypted with AES-256-GCM
- Separate HKDF-derived keys for queries and responses
- Random padding in queries prevents caching and replay
- Random padding in responses prevents DPI size fingerprinting
- No session state — each query is independent
- Pre-shared passphrase required for both client and server
- Telegram 2FA password is prompted interactively (not stored in CLI args)
- Session file stored with 0600 permissions

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
