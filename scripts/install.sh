#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
blue='\033[0;34m'
yellow='\033[0;33m'
plain='\033[0m'

GITHUB_REPO="sartoopjj/thefeed"
INSTALL_DIR="/opt/thefeed"
DATA_DIR="${INSTALL_DIR}/data"
SERVICE_FILE="/etc/systemd/system/thefeed-server.service"

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}Fatal error:${plain} Please run this script with root privilege" && exit 1

# Check OS and set release variable
if [[ -f /etc/os-release ]]; then
    source /etc/os-release
    release=$ID
elif [[ -f /usr/lib/os-release ]]; then
    source /usr/lib/os-release
    release=$ID
else
    echo -e "${red}Failed to check the system OS, please contact the author!${plain}" >&2
    exit 1
fi
echo -e "OS: ${green}$release${plain}"

arch() {
    case "$(uname -m)" in
        x86_64 | x64 | amd64) echo 'amd64' ;;
        armv8* | armv8 | arm64 | aarch64) echo 'arm64' ;;
        *) echo -e "${red}Unsupported CPU architecture: $(uname -m)${plain}" && exit 1 ;;
    esac
}

echo -e "Arch: ${green}$(arch)${plain}"

install_base() {
    echo -e "${green}Installing base dependencies...${plain}"
    case "${release}" in
        ubuntu | debian | armbian)
            apt-get update && apt-get install -y -q curl tar ca-certificates
        ;;
        fedora | amzn | rhel | almalinux | rocky | ol)
            dnf -y update && dnf install -y -q curl tar ca-certificates
        ;;
        centos)
            if [[ "${VERSION_ID}" =~ ^7 ]]; then
                yum -y update && yum install -y curl tar ca-certificates
            else
                dnf -y update && dnf install -y -q curl tar ca-certificates
            fi
        ;;
        arch | manjaro | parch)
            pacman -Syu --noconfirm curl tar ca-certificates
        ;;
        alpine)
            apk update && apk add curl tar ca-certificates bash
        ;;
        *)
            apt-get update && apt-get install -y -q curl tar ca-certificates
        ;;
    esac
}

get_latest_version() {
    local version
    version=$(curl -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    if [[ -z "$version" ]]; then
        echo -e "${yellow}Trying with IPv4...${plain}" >&2
        version=$(curl -4 -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    fi
    echo "$version"
}

download_binary() {
    local version="$1"
    local arch_name
    arch_name=$(arch)
    local binary_name="thefeed-server-linux-${arch_name}"
    local url="https://github.com/${GITHUB_REPO}/releases/download/${version}/${binary_name}"

    echo -e "${green}Downloading thefeed-server ${version} for linux/${arch_name}...${plain}"
    mkdir -p "$INSTALL_DIR"

    curl -4fLo "${INSTALL_DIR}/thefeed-server" "$url"
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Failed to download binary from:${plain}"
        echo -e "${red}  ${url}${plain}"
        echo -e "${yellow}Please check that the version exists and your server can reach GitHub${plain}"
        exit 1
    fi

    chmod 755 "${INSTALL_DIR}/thefeed-server"
    echo -e "${green}Binary installed to ${INSTALL_DIR}/thefeed-server${plain}"
}

setup_channels() {
    echo -e "\n${green}Setting up Telegram channels...${plain}"
    echo "# Telegram channel usernames (one per line)" > "$DATA_DIR/channels.txt.tmp"
    echo "# Lines starting with # are comments" >> "$DATA_DIR/channels.txt.tmp"

    echo ""
    echo -e "${yellow}Enter Telegram channel usernames (one per line, empty line to finish):${plain}"
    while true; do
        read -rp "  Channel: " channel
        if [[ -z "$channel" ]]; then
            break
        fi
        channel="${channel#@}"
        echo "@$channel" >> "$DATA_DIR/channels.txt.tmp"
        echo -e "  ${green}Added @${channel}${plain}"
    done
    mv "$DATA_DIR/channels.txt.tmp" "$DATA_DIR/channels.txt"
}

setup_x_accounts() {
    echo -e "\n${green}Setting up X accounts...${plain}"
    echo "# X usernames (one per line, without @)" > "$DATA_DIR/x_accounts.txt.tmp"
    echo "# Lines starting with # are comments" >> "$DATA_DIR/x_accounts.txt.tmp"

    echo ""
    echo -e "${yellow}Enter X usernames (one per line, empty line to finish):${plain}"
    while true; do
        read -rp "  X: " account
        if [[ -z "$account" ]]; then
            break
        fi
        account="${account#@}"
        account="${account#x/}"
        echo "$account" >> "$DATA_DIR/x_accounts.txt.tmp"
        echo -e "  ${green}Added ${account}${plain}"
    done
    mv "$DATA_DIR/x_accounts.txt.tmp" "$DATA_DIR/x_accounts.txt"
}

# Helper: update or add a key=value in the env file
env_set() {
    local key="$1" val="$2"
    if grep -q "^${key}=" "$DATA_DIR/thefeed.env" 2>/dev/null; then
        sed -i "s|^${key}=.*|${key}=${val}|" "$DATA_DIR/thefeed.env"
    else
        echo "${key}=${val}" >> "$DATA_DIR/thefeed.env"
    fi
}

# Helper: read a key from the env file (empty string if missing)
env_get() {
    local key="$1"
    grep "^${key}=" "$DATA_DIR/thefeed.env" 2>/dev/null | head -1 | cut -d= -f2-
}

setup_config() {
    mkdir -p "$DATA_DIR"

    local is_update=false
    if [[ -f "$DATA_DIR/thefeed.env" ]]; then
        is_update=true
        set -a
        source "$DATA_DIR/thefeed.env"
        set +a
    fi

    # --- Channels ---
    if [[ -f "$DATA_DIR/channels.txt" ]]; then
        local ch_count
        ch_count=$(grep -c '^@' "$DATA_DIR/channels.txt" 2>/dev/null || echo 0)
        echo -e "${yellow}Telegram channels configured: ${ch_count}${plain}"
        read -rp "Change Telegram channels? [y/N]: " change_ch
        if [[ "$change_ch" == "y" || "$change_ch" == "Y" ]]; then
            setup_channels
        fi
    else
        setup_channels
    fi

    # --- X accounts ---
    if [[ -f "$DATA_DIR/x_accounts.txt" ]]; then
        local x_count
        x_count=$(grep -cv '^#\|^$' "$DATA_DIR/x_accounts.txt" 2>/dev/null || echo 0)
        echo -e "${yellow}X accounts configured: ${x_count}${plain}"
        read -rp "Change X accounts? [y/N]: " change_x
        if [[ "$change_x" == "y" || "$change_x" == "Y" ]]; then
            setup_x_accounts
        fi
    else
        setup_x_accounts
    fi

    # --- Server settings ---
    echo -e "\n${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Server Configuration${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo ""

    local cur_domain cur_key cur_limit cur_listen
    if $is_update; then
        cur_domain=$(env_get THEFEED_DOMAIN)
        cur_key=$(env_get THEFEED_KEY)
        cur_limit=$(env_get THEFEED_MSG_LIMIT)
        cur_listen=$(env_get THEFEED_LISTEN)
    fi

    local domain=""
    while true; do
        if [[ -n "$cur_domain" ]]; then
            read -rp "DNS domain [${cur_domain}]: " domain
            domain="${domain:-$cur_domain}"
        else
            read -rp "DNS domain (e.g., t.example.com): " domain
        fi
        if [[ -n "$domain" ]]; then break; fi
        echo -e "${red}Domain cannot be empty${plain}"
    done

    local passkey=""
    while true; do
        if [[ -n "$cur_key" ]]; then
            read -rp "Encryption passphrase [keep current]: " passkey
            passkey="${passkey:-$cur_key}"
        else
            read -rp "Encryption passphrase: " passkey
        fi
        if [[ -n "$passkey" ]]; then break; fi
        echo -e "${red}Passphrase cannot be empty${plain}"
    done

    local msg_limit=""
    read -rp "Max messages per channel [${cur_limit:-15}]: " msg_limit
    msg_limit="${msg_limit:-${cur_limit:-15}}"

    echo ""
    echo -e "${yellow}Allow remote management (send messages, add/remove channels)?${plain}"
    echo -e "  If enabled, anyone with the passphrase can manage channels."
    local allow_manage=""
    if [[ "${THEFEED_ALLOW_MANAGE:-}" == "1" ]]; then
        read -rp "Enable remote management? [Y/n]: " allow_manage
        if [[ "$allow_manage" == "n" || "$allow_manage" == "N" ]]; then
            allow_manage="0"
        else
            allow_manage="1"
        fi
    else
        read -rp "Enable remote management? [y/N]: " allow_manage
        if [[ "$allow_manage" == "y" || "$allow_manage" == "Y" ]]; then
            allow_manage="1"
        else
            allow_manage="0"
        fi
    fi

    # --- Telegram mode ---
    local no_telegram=""
    echo ""
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Telegram Mode Selection${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo ""
    echo -e "${yellow}Option 1: Without Telegram (Recommended)${plain}"
    echo -e "  - Safer: no Telegram credentials stored on server"
    echo -e "  - Reads public channels without login"
    echo -e "  - Cannot read private channels or send messages"
    echo ""
    echo -e "${yellow}Option 2: With Telegram${plain}"
    echo -e "  - Required for private channels, groups, and sending messages"
    echo -e "  - Needs Telegram API credentials and phone number"
    echo ""
    read -rp "Run without Telegram login? (recommended) [Y/n]: " no_telegram

    local api_id="" api_hash="" phone="" listen_addr=""
    if [[ "$no_telegram" != "n" && "$no_telegram" != "N" ]]; then
        api_id="0"
        api_hash="none"
        phone="none"
        read -rp "DNS listen address [${cur_listen:-0.0.0.0:53}]: " listen_addr
        listen_addr="${listen_addr:-${cur_listen:-0.0.0.0:53}}"

        cat > "$DATA_DIR/thefeed.env" <<ENVEOF
THEFEED_DOMAIN=${domain}
THEFEED_KEY=${passkey}
THEFEED_ALLOW_MANAGE=${allow_manage}
THEFEED_MSG_LIMIT=${msg_limit}
THEFEED_X_RSS_INSTANCES=http://nitter.net,https://nitter.net
TELEGRAM_API_ID=${api_id}
TELEGRAM_API_HASH=${api_hash}
TELEGRAM_PHONE=${phone}
THEFEED_LISTEN=${listen_addr}
THEFEED_NO_TELEGRAM=1
ENVEOF
        chmod 600 "$DATA_DIR/thefeed.env"
        echo -e "${green}Config saved to ${DATA_DIR}/thefeed.env${plain}"
        return 0
    fi

    # With Telegram
    local cur_api_id cur_api_hash cur_phone
    if $is_update; then
        cur_api_id=$(env_get TELEGRAM_API_ID)
        cur_api_hash=$(env_get TELEGRAM_API_HASH)
        cur_phone=$(env_get TELEGRAM_PHONE)
    fi

    while true; do
        if [[ -n "$cur_api_id" && "$cur_api_id" != "0" ]]; then
            read -rp "Telegram API ID [${cur_api_id}]: " api_id
            api_id="${api_id:-$cur_api_id}"
        else
            read -rp "Telegram API ID: " api_id
        fi
        if [[ "$api_id" =~ ^[0-9]+$ ]]; then break; fi
        echo -e "${red}API ID must be a number${plain}"
    done

    while true; do
        if [[ -n "$cur_api_hash" && "$cur_api_hash" != "none" ]]; then
            read -rp "Telegram API Hash [keep current]: " api_hash
            api_hash="${api_hash:-$cur_api_hash}"
        else
            read -rp "Telegram API Hash: " api_hash
        fi
        if [[ -n "$api_hash" ]]; then break; fi
        echo -e "${red}API Hash cannot be empty${plain}"
    done

    while true; do
        if [[ -n "$cur_phone" && "$cur_phone" != "none" ]]; then
            read -rp "Telegram phone number [${cur_phone}]: " phone
            phone="${phone:-$cur_phone}"
        else
            read -rp "Telegram phone number (e.g., +1234567890): " phone
        fi
        if [[ -n "$phone" ]]; then break; fi
        echo -e "${red}Phone number cannot be empty${plain}"
    done

    read -rp "DNS listen address [${cur_listen:-0.0.0.0:53}]: " listen_addr
    listen_addr="${listen_addr:-${cur_listen:-0.0.0.0:53}}"

    cat > "$DATA_DIR/thefeed.env" <<ENVEOF
THEFEED_DOMAIN=${domain}
THEFEED_KEY=${passkey}
THEFEED_ALLOW_MANAGE=${allow_manage}
THEFEED_MSG_LIMIT=${msg_limit}
THEFEED_X_RSS_INSTANCES=http://nitter.net,https://nitter.net
TELEGRAM_API_ID=${api_id}
TELEGRAM_API_HASH=${api_hash}
TELEGRAM_PHONE=${phone}
THEFEED_LISTEN=${listen_addr}
ENVEOF
    chmod 600 "$DATA_DIR/thefeed.env"
    echo -e "${green}Config saved to ${DATA_DIR}/thefeed.env${plain}"
    chmod 700 "$DATA_DIR"
}

telegram_login() {
    echo -e "\n${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Telegram Login (one-time)${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo -e "${yellow}This will authenticate with Telegram and save the session.${plain}"
    echo ""

    set -a
    source "$DATA_DIR/thefeed.env"
    set +a

    "$INSTALL_DIR/thefeed-server" \
        --login-only \
        --data-dir "$DATA_DIR" \
        --domain "$THEFEED_DOMAIN" \
        --key "$THEFEED_KEY" \
        --api-id "$TELEGRAM_API_ID" \
        --api-hash "$TELEGRAM_API_HASH" \
        --phone "$TELEGRAM_PHONE"

    if [[ $? -ne 0 ]]; then
        echo -e "${red}Telegram login failed${plain}"
        echo -e "${yellow}You can retry later with:${plain}"
        echo -e "  sudo bash install.sh --login"
        return 1
    fi

    chmod 600 "$DATA_DIR/session.json"
    echo -e "${green}Telegram login successful, session saved.${plain}"
}

install_service() {
    echo -e "${green}Installing systemd service...${plain}"

    set -a
    source "$DATA_DIR/thefeed.env"
    set +a

    local extra_flags=""
    if [[ "${THEFEED_NO_TELEGRAM:-}" == "1" ]]; then
        extra_flags="--no-telegram"
    fi
    if [[ "${THEFEED_ALLOW_MANAGE:-}" == "1" ]]; then
        extra_flags="${extra_flags} --allow-manage"
    fi

    cat > "$SERVICE_FILE" <<SVCEOF
[Unit]
Description=thefeed DNS-based Telegram Feed Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${DATA_DIR}/thefeed.env
ExecStart=${INSTALL_DIR}/thefeed-server \\
    --data-dir ${DATA_DIR} \\
    --domain \${THEFEED_DOMAIN} \\
    --key \${THEFEED_KEY} \\
    --x-accounts ${DATA_DIR}/x_accounts.txt \\
    --x-rss-instances \${THEFEED_X_RSS_INSTANCES} \\
    --api-id \${TELEGRAM_API_ID} \\
    --api-hash \${TELEGRAM_API_HASH} \\
    --phone \${TELEGRAM_PHONE} \\
    --listen \${THEFEED_LISTEN} ${extra_flags}

Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

# Security hardening
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
SVCEOF

    systemctl daemon-reload
    echo -e "${green}Service installed: thefeed-server${plain}"
}

start_service() {
    echo -e "${green}Enabling and starting service...${plain}"
    systemctl enable thefeed-server
    systemctl start thefeed-server
    echo ""
    echo -e "${green}Service status:${plain}"
    systemctl status thefeed-server --no-pager || true
}

show_usage() {
    echo ""
    echo -e "┌─────────────────────────────────────────────────────────┐"
    echo -e "│  ${blue}thefeed service management:${plain}                            │"
    echo -e "│                                                         │"
    echo -e "│  ${blue}systemctl status thefeed-server${plain}   - Status             │"
    echo -e "│  ${blue}systemctl restart thefeed-server${plain}  - Restart            │"
    echo -e "│  ${blue}systemctl stop thefeed-server${plain}     - Stop               │"
    echo -e "│  ${blue}journalctl -u thefeed-server -f${plain}  - Live logs           │"
    echo -e "│                                                         │"
    echo -e "│  All data in: ${blue}${INSTALL_DIR}/${plain}                             │"
    echo -e "│  ${blue}Config:${plain}   ${DATA_DIR}/thefeed.env                │"
    echo -e "│  ${blue}Channels:${plain} ${DATA_DIR}/channels.txt               │"
    echo -e "│  ${blue}X acct:${plain}  ${DATA_DIR}/x_accounts.txt              │"
    echo -e "│  ${blue}Session:${plain}  ${DATA_DIR}/session.json               │"
    echo -e "│  ${blue}Binary:${plain}   ${INSTALL_DIR}/thefeed-server                  │"
    echo -e "│                                                         │"
    echo -e "│  ${yellow}Quick commands (copy-paste):${plain}                           │"
    echo -e "│  Update:    ${blue}curl -Ls URL | sudo bash${plain}                    │"
    echo -e "│  Uninstall: ${blue}curl -Ls URL | sudo bash -s -- --uninstall${plain}  │"
    echo -e "│  Re-login:  ${blue}curl -Ls URL | sudo bash -s -- --login${plain}      │"
    echo -e "│                                                         │"
    echo -e "│  ${red}⚠ NEVER share your passphrase publicly!${plain}                │"
    echo -e "│  ${red}Anyone with it can read ALL your messages.${plain}             │"
    echo -e "│  ${red}--password only protects the web UI on your PC.${plain}        │"
    echo -e "└─────────────────────────────────────────────────────────┘"
    echo ""
    echo -e "Full update command:"
    echo -e "  ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash${plain}"
    echo ""
    echo -e "Full uninstall command:"
    echo -e "  ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --uninstall${plain}"
    echo ""
}

install_thefeed() {
    local version="$1"

    # Get version
    if [[ -z "$version" ]]; then
        version=$(get_latest_version)
        if [[ -z "$version" ]]; then
            echo -e "${red}Failed to fetch latest version from GitHub${plain}"
            echo -e "${yellow}Please check your network or specify a version: bash install.sh v1.0.0${plain}"
            exit 1
        fi
    fi
    echo -e "Version: ${green}${version}${plain}"

    # Check current version
    if [[ -f "${INSTALL_DIR}/thefeed-server" ]]; then
        local current_version
        current_version=$("${INSTALL_DIR}/thefeed-server" --version 2>&1 | awk '{print $2}' || echo "unknown")
        echo -e "Current: ${yellow}${current_version}${plain}"
        if [[ "$current_version" == "$version" ]]; then
            echo -e "${yellow}Already running ${version}. Reinstalling anyway...${plain}"
        fi
    fi

    # Stop existing service
    if systemctl is-active thefeed-server &>/dev/null; then
        echo -e "${yellow}Stopping existing service...${plain}"
        systemctl stop thefeed-server
    fi

    # Download
    download_binary "$version"

    # setup_config handles both first install and re-configuration
    setup_config
    set -a
    source "$DATA_DIR/thefeed.env"
    set +a
    if [[ "${THEFEED_NO_TELEGRAM:-}" != "1" ]]; then
        # Only prompt for Telegram login if credentials changed or no session exists
        if [[ ! -f "$DATA_DIR/session.json" ]]; then
            telegram_login
        else
            read -rp "Re-authenticate with Telegram? [y/N]: " relogin
            if [[ "$relogin" == "y" || "$relogin" == "Y" ]]; then
                telegram_login
            fi
        fi
    fi
    install_service
    start_service

    echo -e "\n${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  thefeed ${version} installed successfully!${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    show_usage
}

login_only() {
    if [[ ! -f "$DATA_DIR/thefeed.env" ]]; then
        echo -e "${red}Config not found. Run install first: bash install.sh${plain}"
        exit 1
    fi
    if [[ ! -f "${INSTALL_DIR}/thefeed-server" ]]; then
        echo -e "${red}Binary not found. Run install first: bash install.sh${plain}"
        exit 1
    fi
    telegram_login
    echo -e "${green}Restarting service...${plain}"
    systemctl restart thefeed-server || true
}

uninstall_thefeed() {
    echo -e "${yellow}Uninstalling thefeed...${plain}"

    systemctl stop thefeed-server 2>/dev/null || true
    systemctl disable thefeed-server 2>/dev/null || true
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload

    local remove_data=""
    if [[ -t 0 ]]; then
        read -rp "Remove all data (config, session, binary)? [y/N]: " remove_data
    else
        # When piped (curl | bash), stdin is not a terminal — default to keeping data
        echo -e "${yellow}Non-interactive mode: keeping data. Delete manually with: rm -rf ${INSTALL_DIR}${plain}"
    fi
    if [[ "$remove_data" == "y" || "$remove_data" == "Y" ]]; then
        rm -rf "$INSTALL_DIR"
        echo -e "${green}All data removed${plain}"
    else
        rm -f "${INSTALL_DIR}/thefeed-server"
        echo -e "${green}Binary removed (data preserved in ${DATA_DIR})${plain}"
    fi

    echo -e "${green}thefeed uninstalled successfully${plain}"
}

show_help() {
    echo -e "thefeed install script"
    echo ""
    echo -e "Usage: bash $0 [OPTION]"
    echo ""
    echo -e "Options:"
    echo -e "  ${green}(no args)${plain}       Install or update to latest version"
    echo -e "  ${green}v1.0.0${plain}          Install specific version"
    echo -e "  ${green}--login${plain}         Re-authenticate with Telegram"
    echo -e "  ${green}--uninstall${plain}     Remove thefeed"
    echo -e "  ${green}--help${plain}          Show this help"
    echo ""
    echo -e "No-Telegram mode (recommended for most users):"
    echo -e "  Reads public Telegram channels without needing Telegram credentials."
    echo -e "  Safer because no phone number or API keys are stored on the server."
    echo ""
    echo -e "Quick commands:"
    echo -e "  Install/Update: ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash${plain}"
    echo -e "  Uninstall:      ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --uninstall${plain}"
}

# Main
echo -e "${green}Running thefeed installer...${plain}"

case "${1:-}" in
    --help | -h)
        show_help
        ;;
    --login)
        login_only
        ;;
    --uninstall)
        uninstall_thefeed
        ;;
    *)
        install_base
        install_thefeed "$1"
        ;;
esac
