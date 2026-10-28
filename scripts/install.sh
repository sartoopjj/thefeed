#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
blue='\033[0;34m'
yellow='\033[0;33m'
plain='\033[0m'

GITHUB_REPO="sartoopjj/thefeed"
GITLAB_REPO="sartoopjj/thefeed"
# URL-encoded GitLab project path for /api/v4/projects/:id calls.
GITLAB_REPO_ENC="sartoopjj%2Fthefeed"
# Source for release downloads: github, gitlab, or auto.
#   auto = try GitHub first, fall back to GitLab if GitHub is unreachable
#          (used while the GitHub account is suspended).
# Can be overridden with --source <github|gitlab> on the CLI.
SOURCE="${SOURCE:-auto}"
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

# Decide which mirror to talk to. Called lazily before any network call.
# "auto" picks GitHub if api.github.com responds with a non-empty tag_name
# for the latest release; otherwise falls back to GitLab.
resolve_source() {
    if [[ "$SOURCE" == "github" || "$SOURCE" == "gitlab" ]]; then
        return
    fi
    local probe
    probe=$(curl -Ls --max-time 6 "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -1)
    if [[ -n "$probe" ]]; then
        SOURCE="github"
    else
        SOURCE="gitlab"
    fi
    echo -e "Release source: ${green}${SOURCE}${plain}" >&2
}

get_latest_version() {
    resolve_source
    local version
    if [[ "$SOURCE" == "gitlab" ]]; then
        version=$(curl -Ls "https://gitlab.com/api/v4/projects/${GITLAB_REPO_ENC}/releases?per_page=1" \
            | sed -E 's/\},\{/}\n{/g' | head -1 \
            | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')
        if [[ -z "$version" ]]; then
            version=$(curl -4 -Ls "https://gitlab.com/api/v4/projects/${GITLAB_REPO_ENC}/releases?per_page=1" \
                | sed -E 's/\},\{/}\n{/g' | head -1 \
                | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')
        fi
    else
        version=$(curl -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ -z "$version" ]]; then
            echo -e "${yellow}Trying with IPv4...${plain}" >&2
            version=$(curl -4 -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        fi
    fi
    echo "$version"
}

_fetch_releases() {
    resolve_source
    local body
    if [[ "$SOURCE" == "gitlab" ]]; then
        body=$(curl -Ls "https://gitlab.com/api/v4/projects/${GITLAB_REPO_ENC}/releases?per_page=20")
        if [[ -z "$body" ]]; then
            body=$(curl -4 -Ls "https://gitlab.com/api/v4/projects/${GITLAB_REPO_ENC}/releases?per_page=20")
        fi
    else
        body=$(curl -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=20")
        if [[ -z "$body" ]]; then
            body=$(curl -4 -Ls "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=20")
        fi
    fi
    echo "$body"
}

# Normalise GitHub JSON (pretty or minified) to one release object per line.
_split_releases() {
    _fetch_releases | tr -d '\n' | sed 's/{/\n{/g'
}

# Decide whether a release JSON object (one line) is a pre-release.
# GitHub exposes a boolean field; GitLab does not, so on GitLab we fall back
# to the tag-name convention (anything with a hyphen, e.g. v1.0.0-rc1).
_is_pre_line() {
    local line="$1" tag
    if echo "$line" | grep -qE '"prerelease":[[:space:]]*true'; then
        return 0
    fi
    if [[ "$SOURCE" == "gitlab" ]]; then
        tag=$(echo "$line" | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')
        [[ "$tag" == *-* ]] && return 0
    fi
    return 1
}

get_latest_prerelease() {
    local line
    while IFS= read -r line; do
        case "$line" in *'"tag_name"'*) ;; *) continue ;; esac
        if _is_pre_line "$line"; then
            echo "$line" | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/'
            return
        fi
    done < <(_split_releases)
}

list_versions() {
    echo -e "${green}Recent thefeed releases (most recent first):${plain}"
    local line tag label
    while IFS= read -r line; do
        case "$line" in
            *'"tag_name"'*) ;;
            *) continue ;;
        esac
        tag=$(echo "$line" | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')
        if _is_pre_line "$line"; then
            label="[pre-release]"
        else
            label="[stable]"
        fi
        printf "  %-15s %s\n" "$tag" "$label"
    done < <(_split_releases)
    echo ""
    echo -e "Install one with: ${blue}sudo bash install.sh --version <tag>${plain}"
    echo -e "Or:               ${blue}sudo bash install.sh <tag>${plain} (positional)"
}

download_binary() {
    resolve_source
    local version="$1"
    local arch_name
    arch_name=$(arch)
    local binary_name="thefeed-server-linux-${arch_name}"
    local url
    if [[ "$SOURCE" == "gitlab" ]]; then
        # GitLab Release direct-asset URL — same path the CI registers via
        # release-cli's --assets-link.
        url="https://gitlab.com/${GITLAB_REPO}/-/releases/${version}/downloads/${binary_name}"
    else
        url="https://github.com/${GITHUB_REPO}/releases/download/${version}/${binary_name}"
    fi

    echo -e "${green}Downloading thefeed-server ${version} for linux/${arch_name} from ${SOURCE}...${plain}"
    mkdir -p "$INSTALL_DIR"

    curl -4fLo "${INSTALL_DIR}/thefeed-server" "$url"
    if [[ $? -ne 0 ]]; then
        echo -e "${red}Failed to download binary from:${plain}"
        echo -e "${red}  ${url}${plain}"
        echo -e "${yellow}Please check that the version exists and your server can reach the ${SOURCE} mirror${plain}"
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

setup_private_channels() {
    echo -e "\n${green}Setting up private Telegram channel invites...${plain}"
    echo -e "${yellow}Private channels require the server to be logged into Telegram.${plain}"
    echo -e "${yellow}Paste each invite link in any of these shapes:${plain}"
    echo "  https://t.me/+aBcDeF123456    https://t.me/joinchat/aBcDeF123456"
    echo "  tg://join?invite=aBcDeF…       +aBcDeF123456    aBcDeF123456"
    {
        echo "# Telegram private-channel invite links (one per line)"
        echo "# Server must be logged into Telegram for these to work."
        echo "# Lines starting with # are comments"
    } > "$DATA_DIR/private_channels.txt.tmp"

    echo ""
    echo -e "${yellow}Enter invite links (one per line, empty line to finish):${plain}"
    local added=0
    while true; do
        read -rp "  Invite: " link
        if [[ -z "$link" ]]; then
            break
        fi
        # Early reject for obvious public-username typos. Strict
        # validation happens server-side in ParseInviteHash.
        if [[ "$link" =~ ^https?://t\.me/[^+] ]] && [[ ! "$link" =~ /joinchat/ ]]; then
            echo -e "  ${red}Skipped: ${link} looks like a public username, not an invite link.${plain}"
            echo -e "  ${red}Put public channels in channels.txt instead.${plain}"
            continue
        fi
        echo "$link" >> "$DATA_DIR/private_channels.txt.tmp"
        echo -e "  ${green}Added ${link}${plain}"
        added=$((added + 1))
    done
    mv "$DATA_DIR/private_channels.txt.tmp" "$DATA_DIR/private_channels.txt"
    if [[ $added -eq 0 ]]; then
        echo -e "  ${yellow}(none added)${plain}"
    fi
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

    local is_update=false cur_allow_manage=""
    if [[ -f "$DATA_DIR/thefeed.env" ]]; then
        is_update=true
        cur_allow_manage=$(env_get THEFEED_ALLOW_MANAGE)
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

    # --- Private Telegram channels ---
    # Skipped in --no-telegram mode (joining needs a logged-in session).
    if [[ "${THEFEED_NO_TELEGRAM:-}" != "1" ]]; then
        if [[ -f "$DATA_DIR/private_channels.txt" ]]; then
            local pc_count
            pc_count=$(grep -cv '^#\|^$' "$DATA_DIR/private_channels.txt" 2>/dev/null || echo 0)
            echo -e "${yellow}Private channel invites configured: ${pc_count}${plain}"
            read -rp "Change private channel invites? [y/N]: " change_pc
            if [[ "$change_pc" == "y" || "$change_pc" == "Y" ]]; then
                setup_private_channels
            fi
        else
            read -rp "Add private channel invite links? [y/N]: " add_pc
            if [[ "$add_pc" == "y" || "$add_pc" == "Y" ]]; then
                setup_private_channels
            fi
        fi
    fi

    # --- Server settings ---
    echo -e "\n${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Server Configuration${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo ""

    local cur_domain cur_key cur_limit cur_listen cur_fetch_interval cur_extra_domains
    if $is_update; then
        cur_domain=$(env_get THEFEED_DOMAIN)
        cur_key=$(env_get THEFEED_KEY)
        cur_limit=$(env_get THEFEED_MSG_LIMIT)
        cur_listen=$(env_get THEFEED_LISTEN)
        cur_fetch_interval=$(env_get THEFEED_FETCH_INTERVAL)
        cur_extra_domains=$(env_get THEFEED_EXTRA_DOMAINS)
    fi

    local domain=""
    while true; do
        if [[ -n "$cur_domain" ]]; then
            read -rp "Main DNS domain [${cur_domain}]: " domain
            domain="${domain:-$cur_domain}"
        else
            read -rp "Main DNS domain (e.g., t.example.com): " domain
        fi
        if [[ -n "$domain" ]]; then break; fi
        echo -e "${red}Domain cannot be empty${plain}"
    done

    # Optional extra domains/sub-domains: clients spread feed queries across the
    # main domain + these (they can be entirely separate domains, e.g.
    # nws2.other.com). The main domain stays canonical for relay paths.
    # Stored comma-separated in THEFEED_EXTRA_DOMAINS.
    local extra_domains="$cur_extra_domains"
    local do_collect_ed=false
    echo ""
    if [[ -n "$cur_extra_domains" ]]; then
        echo -e "${yellow}Extra domains: ${cur_extra_domains}${plain}"
        read -rp "Change extra domains? [y/N]: " change_ed
        [[ "$change_ed" == "y" || "$change_ed" == "Y" ]] && do_collect_ed=true
    else
        read -rp "Add extra domains? Clients spread queries across them. [y/N]: " add_ed
        [[ "$add_ed" == "y" || "$add_ed" == "Y" ]] && do_collect_ed=true
    fi
    if $do_collect_ed; then
        echo -e "${yellow}Enter each extra domain (one per line, empty line to finish):${plain}"
        extra_domains=""
        while true; do
            read -rp "  Domain: " ed
            ed="${ed//[[:space:]]/}"
            [[ -z "$ed" ]] && break
            if [[ -n "$extra_domains" ]]; then extra_domains="${extra_domains},${ed}"; else extra_domains="$ed"; fi
            echo -e "  ${green}Added ${ed}${plain}"
        done
    fi

    # Chat messenger: a standalone, end-to-end-encrypted messenger served on
    # its own dedicated sub-domain(s) — never on the feed domains. Stored
    # comma-separated in THEFEED_CHAT_DOMAINS; empty = messenger disabled.
    local cur_chat_domains=""
    $is_update && cur_chat_domains=$(env_get THEFEED_CHAT_DOMAINS)
    local chat_domains="$cur_chat_domains"
    local do_collect_cd=false
    echo ""
    if [[ -n "$cur_chat_domains" ]]; then
        echo -e "${yellow}Messenger domains: ${cur_chat_domains}${plain}"
        read -rp "Change messenger domains? [y/N]: " change_cd
        [[ "$change_cd" == "y" || "$change_cd" == "Y" ]] && do_collect_cd=true
    else
        read -rp "Enable the messenger? It needs dedicated sub-domain(s). [y/N]: " add_cd
        [[ "$add_cd" == "y" || "$add_cd" == "Y" ]] && do_collect_cd=true
    fi
    if $do_collect_cd; then
        echo -e "${yellow}Enter each messenger sub-domain (must differ from the feed domains; empty line to finish):${plain}"
        chat_domains=""
        while true; do
            read -rp "  Domain: " cdom
            cdom="${cdom//[[:space:]]/}"
            [[ -z "$cdom" ]] && break
            if [[ "$cdom" == "$domain" ]]; then echo -e "  ${red}Must differ from the main feed domain${plain}"; continue; fi
            if [[ -n "$chat_domains" ]]; then chat_domains="${chat_domains},${cdom}"; else chat_domains="$cdom"; fi
            echo -e "  ${green}Added ${cdom}${plain}"
        done
    fi

    # Telegram proxy: carries real MTProto to Telegram's own data centres over
    # DNS, on its own dedicated sub-domain(s) — never on the feed or messenger
    # domains. Stored in THEFEED_MTPROTO_DOMAINS; empty = proxy off.
    local cur_mtp_domains=""
    $is_update && cur_mtp_domains=$(env_get THEFEED_MTPROTO_DOMAINS)
    local mtproto_domains="$cur_mtp_domains"
    local do_collect_md=false
    echo ""
    if [[ -n "$cur_mtp_domains" ]]; then
        echo -e "${yellow}Telegram proxy domains: ${cur_mtp_domains}${plain}"
        read -rp "Change Telegram proxy domains? [y/N]: " change_md
        [[ "$change_md" == "y" || "$change_md" == "Y" ]] && do_collect_md=true
    else
        echo -e "${yellow}The Telegram proxy lets clients reach Telegram itself through this"
        echo -e "server when only domestic traffic works. It needs dedicated sub-domain(s)"
        echo -e "and makes outbound TCP connections to Telegram's data centres.${plain}"
        read -rp "Enable the Telegram proxy? [y/N]: " add_md
        [[ "$add_md" == "y" || "$add_md" == "Y" ]] && do_collect_md=true
    fi
    if $do_collect_md; then
        echo -e "${yellow}Enter each Telegram proxy sub-domain (must differ from the feed and messenger domains; empty line to finish):${plain}"
        mtproto_domains=""
        while true; do
            read -rp "  Domain: " mdom
            mdom="${mdom//[[:space:]]/}"
            [[ -z "$mdom" ]] && break
            if [[ "$mdom" == "$domain" ]]; then echo -e "  ${red}Must differ from the main feed domain${plain}"; continue; fi
            if [[ ",${chat_domains}," == *",${mdom},"* ]]; then echo -e "  ${red}Already a messenger domain${plain}"; continue; fi
            if [[ -n "$mtproto_domains" ]]; then mtproto_domains="${mtproto_domains},${mdom}"; else mtproto_domains="$mdom"; fi
            echo -e "  ${green}Added ${mdom}${plain}"
        done
    fi

    # Only asked when the proxy is on: a cap matters because each active tunnel
    # holds a socket to Telegram plus its buffers.
    local cur_mtp_max=""
    $is_update && cur_mtp_max=$(env_get THEFEED_MTPROTO_MAX_TUNNELS)
    local mtproto_max_tunnels="${cur_mtp_max:-0}"
    if [[ -n "$mtproto_domains" ]]; then
        read -rp "Max concurrent Telegram proxy users (0 = uncapped) [${cur_mtp_max:-0}]: " mtproto_max_tunnels
        mtproto_max_tunnels="${mtproto_max_tunnels:-${cur_mtp_max:-0}}"
        [[ "$mtproto_max_tunnels" =~ ^[0-9]+$ ]] || mtproto_max_tunnels=0
    fi

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

    local fetch_interval=""
    while true; do
        read -rp "Fetch cycle interval, minutes (min 3) [${cur_fetch_interval:-10}]: " fetch_interval
        fetch_interval="${fetch_interval:-${cur_fetch_interval:-10}}"
        if [[ "$fetch_interval" =~ ^[0-9]+$ ]] && [[ "$fetch_interval" -ge 3 ]]; then break; fi
        echo -e "${red}Must be an integer ≥ 3${plain}"
    done

    echo ""
    echo -e "${yellow}Allow remote management (send messages, add/remove channels)?${plain}"
    echo -e "  If enabled, anyone with the passphrase can manage channels."
    local allow_manage=""
    if [[ "$cur_allow_manage" == "1" ]]; then
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

    # --- Media relays ---
    # Each relay is independent: the same file can be served by DNS, GitHub,
    # and any future relay simultaneously. Enabling a relay just gives
    # clients another way to fetch the bytes.
    echo ""
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Media relays${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    local cur_dns_enabled cur_dns_size cur_dns_ttl cur_dns_comp
    local cur_gh_enabled cur_gh_token cur_gh_repo cur_gh_branch cur_gh_size cur_gh_ttl
    if $is_update; then
        cur_dns_enabled=$(env_get THEFEED_DNS_MEDIA_ENABLED)
        cur_dns_size=$(env_get THEFEED_DNS_MEDIA_MAX_SIZE_KB)
        cur_dns_ttl=$(env_get THEFEED_DNS_MEDIA_CACHE_TTL_MIN)
        cur_dns_comp=$(env_get THEFEED_DNS_MEDIA_COMPRESSION)
        cur_gh_enabled=$(env_get THEFEED_GITHUB_RELAY_ENABLED)
        cur_gh_token=$(env_get THEFEED_GITHUB_RELAY_TOKEN)
        cur_gh_repo=$(env_get THEFEED_GITHUB_RELAY_REPO)
        cur_gh_branch=$(env_get THEFEED_GITHUB_RELAY_BRANCH)
        cur_gh_size=$(env_get THEFEED_GITHUB_RELAY_MAX_SIZE_KB)
        cur_gh_ttl=$(env_get THEFEED_GITHUB_RELAY_TTL_MIN)
    fi

    # DNS relay (slow path, off by default).
    echo ""
    echo -e "${yellow}DNS relay${plain} — files served block-by-block over DNS. Slower, works"
    echo -e "  in censored networks. Default 100 KB cap."
    local dns_default="N" dns_prompt="[y/N]"
    if [[ "$cur_dns_enabled" == "1" ]]; then dns_default="Y" dns_prompt="[Y/n]"; fi
    local dns_enabled_in=""
    read -rp "Enable DNS relay? $dns_prompt: " dns_enabled_in
    if [[ -z "$dns_enabled_in" ]]; then dns_enabled_in="$dns_default"; fi
    local dns_enabled="0"
    if [[ "$dns_enabled_in" == "y" || "$dns_enabled_in" == "Y" ]]; then dns_enabled="1"; fi

    local dns_max_size="${cur_dns_size:-100}"
    local dns_ttl="${cur_dns_ttl:-600}"
    local dns_comp="${cur_dns_comp:-gzip}"
    if [[ "$dns_enabled" == "1" ]]; then
        read -rp "DNS relay max file size in KB [${dns_max_size}]: " in
        dns_max_size="${in:-$dns_max_size}"
        read -rp "DNS relay TTL in minutes [${dns_ttl}]: " in
        dns_ttl="${in:-$dns_ttl}"
        read -rp "DNS relay compression (none|gzip|deflate) [${dns_comp}]: " in
        dns_comp="${in:-$dns_comp}"
    fi

    # GitHub relay (fast path, default off — needs a token).
    echo ""
    echo -e "${yellow}GitHub relay${plain} — files uploaded to a repo and pulled by clients over"
    echo -e "  plain HTTPS. Faster + bigger files; needs a personal access token."
    local gh_default="N" gh_prompt="[y/N]"
    if [[ "$cur_gh_enabled" == "1" ]]; then gh_default="Y"; gh_prompt="[Y/n]"; fi
    local gh_enabled_in=""
    read -rp "Enable GitHub relay? $gh_prompt: " gh_enabled_in
    if [[ -z "$gh_enabled_in" ]]; then gh_enabled_in="$gh_default"; fi
    local gh_enabled="0"
    if [[ "$gh_enabled_in" == "y" || "$gh_enabled_in" == "Y" ]]; then gh_enabled="1"; fi

    local gh_token="" gh_repo="" gh_branch="${cur_gh_branch:-main}"
    local gh_max_size="${cur_gh_size:-15360}"
    local gh_ttl="${cur_gh_ttl:-600}"
    if [[ "$gh_enabled" == "1" ]]; then
        if [[ -n "$cur_gh_token" ]]; then
            read -rp "GitHub token (PAT, contents:write) [keep current]: " gh_token
            gh_token="${gh_token:-$cur_gh_token}"
        else
            read -rp "GitHub token (PAT, contents:write): " gh_token
        fi
        while true; do
            if [[ -n "$cur_gh_repo" ]]; then
                read -rp "GitHub repo (owner/repo) [${cur_gh_repo}]: " gh_repo
                gh_repo="${gh_repo:-$cur_gh_repo}"
            else
                read -rp "GitHub repo (owner/repo): " gh_repo
            fi
            if [[ "$gh_repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then break; fi
            echo -e "${red}Invalid repo. Format: owner/repo${plain}"
        done
        read -rp "GitHub relay branch [${gh_branch}]: " in
        gh_branch="${in:-$gh_branch}"
        read -rp "GitHub relay max file size in KB [${gh_max_size}]: " in
        gh_max_size="${in:-$gh_max_size}"
        read -rp "GitHub relay TTL in minutes [${gh_ttl}]: " in
        gh_ttl="${in:-$gh_ttl}"
    fi

    # --- Telegram mode ---
    local cur_no_tg=""
    if $is_update; then
        cur_no_tg=$(env_get THEFEED_NO_TELEGRAM)
    fi
    echo ""
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Telegram Mode Selection${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    echo ""
    echo -e "  ${yellow}1)${plain} Without Telegram ${green}(recommended)${plain}"
    echo -e "     - No Telegram credentials stored on server"
    echo -e "     - Reads public channels only"
    echo ""
    echo -e "  ${yellow}2)${plain} With Telegram"
    echo -e "     - Needs API ID, API hash, and phone number"
    echo -e "     - Required for private channels and sending messages"
    echo ""
    local mode_default="1"
    [[ "$cur_no_tg" != "1" && "$is_update" == "true" && -n "$(env_get TELEGRAM_API_ID)" && "$(env_get TELEGRAM_API_ID)" != "0" ]] && mode_default="2"
    local mode_in=""
    while true; do
        read -rp "Choose mode [1/2] (default: ${mode_default}): " mode_in
        mode_in="${mode_in:-$mode_default}"
        [[ "$mode_in" == "1" || "$mode_in" == "2" ]] && break
        echo -e "${red}Enter 1 or 2${plain}"
    done

    local api_id="" api_hash="" phone="" listen_addr=""
    if [[ "$mode_in" == "1" ]]; then
        api_id="0"
        api_hash="none"
        phone="none"
        read -rp "DNS listen address [${cur_listen:-0.0.0.0:53}]: " listen_addr
        listen_addr="${listen_addr:-${cur_listen:-0.0.0.0:53}}"

        cat > "$DATA_DIR/thefeed.env" <<ENVEOF
THEFEED_DOMAIN=${domain}
THEFEED_EXTRA_DOMAINS=${extra_domains}
THEFEED_CHAT_DOMAINS=${chat_domains}
THEFEED_MTPROTO_DOMAINS=${mtproto_domains}
THEFEED_MTPROTO_MAX_TUNNELS=${mtproto_max_tunnels}
THEFEED_KEY=${passkey}
THEFEED_ALLOW_MANAGE=${allow_manage}
THEFEED_MSG_LIMIT=${msg_limit}
THEFEED_X_RSS_INSTANCES=https://nitter.net,http://nitter.net
TELEGRAM_API_ID=${api_id}
TELEGRAM_API_HASH=${api_hash}
TELEGRAM_PHONE=${phone}
THEFEED_LISTEN=${listen_addr}
THEFEED_FETCH_INTERVAL=${fetch_interval}
THEFEED_NO_TELEGRAM=1
THEFEED_DNS_MEDIA_ENABLED=${dns_enabled}
THEFEED_DNS_MEDIA_MAX_SIZE_KB=${dns_max_size}
THEFEED_DNS_MEDIA_CACHE_TTL_MIN=${dns_ttl}
THEFEED_DNS_MEDIA_COMPRESSION=${dns_comp}
THEFEED_GITHUB_RELAY_ENABLED=${gh_enabled}
THEFEED_GITHUB_RELAY_TOKEN=${gh_token}
THEFEED_GITHUB_RELAY_REPO=${gh_repo}
THEFEED_GITHUB_RELAY_BRANCH=${gh_branch}
THEFEED_GITHUB_RELAY_MAX_SIZE_KB=${gh_max_size}
THEFEED_GITHUB_RELAY_TTL_MIN=${gh_ttl}
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
        [[ "$cur_api_id" == "0" ]] && cur_api_id=""
        [[ "$cur_api_hash" == "none" ]] && cur_api_hash=""
        [[ "$cur_phone" == "none" ]] && cur_phone=""
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
THEFEED_EXTRA_DOMAINS=${extra_domains}
THEFEED_CHAT_DOMAINS=${chat_domains}
THEFEED_MTPROTO_DOMAINS=${mtproto_domains}
THEFEED_MTPROTO_MAX_TUNNELS=${mtproto_max_tunnels}
THEFEED_KEY=${passkey}
THEFEED_ALLOW_MANAGE=${allow_manage}
THEFEED_MSG_LIMIT=${msg_limit}
THEFEED_X_RSS_INSTANCES=https://nitter.net,http://nitter.net
TELEGRAM_API_ID=${api_id}
TELEGRAM_API_HASH=${api_hash}
TELEGRAM_PHONE=${phone}
THEFEED_LISTEN=${listen_addr}
THEFEED_FETCH_INTERVAL=${fetch_interval}
THEFEED_DNS_MEDIA_ENABLED=${dns_enabled}
THEFEED_DNS_MEDIA_MAX_SIZE_KB=${dns_max_size}
THEFEED_DNS_MEDIA_CACHE_TTL_MIN=${dns_ttl}
THEFEED_DNS_MEDIA_COMPRESSION=${dns_comp}
THEFEED_GITHUB_RELAY_ENABLED=${gh_enabled}
THEFEED_GITHUB_RELAY_TOKEN=${gh_token}
THEFEED_GITHUB_RELAY_REPO=${gh_repo}
THEFEED_GITHUB_RELAY_BRANCH=${gh_branch}
THEFEED_GITHUB_RELAY_MAX_SIZE_KB=${gh_max_size}
THEFEED_GITHUB_RELAY_TTL_MIN=${gh_ttl}
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

    local was_active=0
    if systemctl is-active --quiet thefeed-server 2>/dev/null; then
        was_active=1
        systemctl stop thefeed-server
    fi

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
    local rc=$?

    if [[ $rc -ne 0 ]]; then
        echo -e "${red}Telegram login failed${plain}"
        echo -e "${yellow}You can retry later with:${plain}"
        echo -e "  sudo bash install.sh --login"
        [[ $was_active -eq 1 ]] && systemctl start thefeed-server
        return 1
    fi

    chmod 600 "$DATA_DIR/session.json"
    echo -e "${green}Telegram login successful, session saved.${plain}"
    [[ $was_active -eq 1 ]] && systemctl start thefeed-server
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
    # All --dns-media-* and --github-relay-* settings come from THEFEED_*
    # env vars, so the binary picks them up via EnvironmentFile alone.

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

# Resource limits — public-facing multi-user servers can have tens
# of thousands of concurrent sockets (web clients + DNS upstream +
# Telegram MTProto + outgoing media fetches). The default systemd
# RLIMIT_NOFILE (usually 1024) trips well before that, surfacing as
# "too many open files" errors and dropped connections. Raising the
# limits here means operators don't have to run ulimit by hand.
LimitNOFILE=524288
LimitNPROC=infinity
TasksMax=infinity

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
    echo -e "│  ${blue}Private:${plain}  ${DATA_DIR}/private_channels.txt       │"
    echo -e "│  ${blue}X acct:${plain}  ${DATA_DIR}/x_accounts.txt              │"
    echo -e "│  ${blue}Session:${plain}  ${DATA_DIR}/session.json               │"
    echo -e "│  ${blue}Binary:${plain}   ${INSTALL_DIR}/thefeed-server                  │"
    echo -e "│                                                         │"
    echo -e "│  ${yellow}Quick commands (copy-paste):${plain}                           │"
    echo -e "│  Update:    ${blue}curl -Ls URL | sudo bash${plain}                    │"
    echo -e "│  Uninstall: ${blue}curl -Ls URL | sudo bash -s -- --uninstall${plain}  │"
    echo -e "│  Re-login:  ${blue}curl -Ls URL | sudo bash -s -- --login${plain}      │"
    echo -e "│  Config:    ${blue}curl -Ls URL | sudo bash -s -- --config${plain}     │"
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
    local channel="${2:-stable}"  # "stable" or "pre"

    # When invoked via `curl | bash`, stdin is the pipe, not a terminal —
    # so every read in setup_config returns empty and the user is never
    # prompted. Re-open stdin from /dev/tty so prompts work as expected.
    if [[ ! -t 0 ]] && [[ -e /dev/tty ]]; then
        exec </dev/tty
    fi

    if [[ -z "$version" ]]; then
        if [[ "$channel" == "pre" ]]; then
            version=$(get_latest_prerelease)
            if [[ -z "$version" ]]; then
                echo -e "${red}No pre-release found on GitHub${plain}"
                echo -e "${yellow}Run: bash install.sh --list  to see available versions${plain}"
                exit 1
            fi
            echo -e "${yellow}Channel:${plain} ${blue}pre-release${plain}"
        else
            version=$(get_latest_version)
            if [[ -z "$version" ]]; then
                echo -e "${red}Failed to fetch latest version from GitHub${plain}"
                echo -e "${yellow}Please check your network or specify a version: bash install.sh --version v1.0.0${plain}"
                exit 1
            fi
        fi
    fi
    if [[ "$version" =~ ^[0-9] ]]; then
        version="v${version}"
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

    setup_config
    while IFS= read -r v; do
        unset "$v"
    done < <(env | awk -F= '/^THEFEED_|^TELEGRAM_/{print $1}')
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

    # Print the import URI so the operator can share/import the feed right away.
    show_config
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

show_config() {
    if [[ ! -f "$DATA_DIR/thefeed.env" ]]; then
        echo -e "${red}Config not found. Run install first: bash install.sh${plain}"
        exit 1
    fi
    if [[ ! -f "${INSTALL_DIR}/thefeed-server" ]]; then
        echo -e "${red}Binary not found. Run install first: bash install.sh${plain}"
        exit 1
    fi
    set -a
    source "$DATA_DIR/thefeed.env"
    set +a

    echo -e "\n${green}═══════════════════════════════════════${plain}"
    echo -e "${green}  Current server config (import URI)${plain}"
    echo -e "${green}═══════════════════════════════════════${plain}"
    # --print-config loads/generates the signing key under --data-dir and
    # prints thefeed://domain/key?sk=<pubkey>&r=<bootstrap resolvers>.
    "$INSTALL_DIR/thefeed-server" --print-config \
        --data-dir "$DATA_DIR" \
        --domain "$THEFEED_DOMAIN" \
        --key "$THEFEED_KEY"
    echo ""
    echo -e "${yellow}Share this URI so others can import your feed.${plain}"
    echo -e "${red}⚠ It contains your passphrase — anyone with it can read this feed.${plain}"
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
    echo -e "  ${green}(no args)${plain}              Install or update to latest stable version"
    echo -e "  ${green}--version <tag>${plain}        Install a specific version (rollback, beta, rc)"
    echo -e "  ${green}-v <tag>${plain}               Short form of --version"
    echo -e "  ${green}<tag>${plain}                  Positional form, e.g.  bash install.sh v1.0.0"
    echo -e "  ${green}--pre${plain}                  Install the latest pre-release (beta/rc)"
    echo -e "  ${green}--list${plain}                 List recent releases with stable/pre labels"
    echo -e "  ${green}--source <name>${plain}        Pick release mirror: github | gitlab | auto (default: auto)"
    echo -e "  ${green}--github${plain} / ${green}--gitlab${plain}     Shortcut for --source github / --source gitlab"
    echo -e "  ${green}--login${plain}                Re-authenticate with Telegram"
    echo -e "  ${green}--config${plain}               Print the current server config import URI (domain, key, sk=, resolvers)"
    echo -e "  ${green}--uninstall${plain}            Remove thefeed"
    echo -e "  ${green}--help${plain}                 Show this help"
    echo ""
    echo -e "Examples:"
    echo -e "  Roll back:       ${blue}sudo bash install.sh --version v0.9.2${plain}"
    echo -e "  Install beta:    ${blue}sudo bash install.sh --pre${plain}"
    echo -e "  Specific tag:    ${blue}sudo bash install.sh --version v1.2.0-rc1${plain}"
    echo -e "  See available:   ${blue}sudo bash install.sh --list${plain}"
    echo ""
    echo -e "No-Telegram mode (recommended for most users):"
    echo -e "  Reads public Telegram channels without needing Telegram credentials."
    echo -e "  Safer because no phone number or API keys are stored on the server."
    echo ""
    echo -e "Quick commands (GitHub mirror):"
    echo -e "  Install/Update:  ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash${plain}"
    echo -e "  Install beta:    ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --pre${plain}"
    echo -e "  Roll back:       ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --version v0.9.2${plain}"
    echo -e "  Uninstall:       ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --uninstall${plain}"
    echo -e "  Show config:     ${blue}curl -Ls https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts/install.sh | sudo bash -s -- --config${plain}"
    echo ""
    echo -e "Quick commands (GitLab mirror — use while the GitHub account is unavailable):"
    echo -e "  Install/Update:  ${blue}curl -Ls https://gitlab.com/${GITLAB_REPO}/-/raw/main/scripts/install.sh | sudo bash -s -- --gitlab${plain}"
    echo -e "  Install beta:    ${blue}curl -Ls https://gitlab.com/${GITLAB_REPO}/-/raw/main/scripts/install.sh | sudo bash -s -- --gitlab --pre${plain}"
}

# Main
echo -e "${green}Running thefeed installer...${plain}"

# Flags: --version <tag> / -v <tag> / positional <tag>, --pre, --list,
# --login, --uninstall, --help. No args = latest stable.
REQUEST_VERSION=""
REQUEST_CHANNEL="stable"
ACTION="install"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --help|-h)
            ACTION="help"; shift ;;
        --login)
            ACTION="login"; shift ;;
        --config|--show-config)
            ACTION="config"; shift ;;
        --uninstall)
            ACTION="uninstall"; shift ;;
        --list)
            ACTION="list"; shift ;;
        --pre|--prerelease|--beta)
            REQUEST_CHANNEL="pre"; shift ;;
        --source)
            shift
            if [[ -z "${1:-}" ]]; then
                echo -e "${red}--source requires one of: github, gitlab, auto${plain}"
                exit 1
            fi
            case "$1" in github|gitlab|auto) SOURCE="$1" ;; *) echo -e "${red}invalid --source: $1${plain}"; exit 1 ;; esac
            shift ;;
        --source=*)
            case "${1#*=}" in github|gitlab|auto) SOURCE="${1#*=}" ;; *) echo -e "${red}invalid --source: ${1#*=}${plain}"; exit 1 ;; esac
            shift ;;
        --github)
            SOURCE="github"; shift ;;
        --gitlab)
            SOURCE="gitlab"; shift ;;
        --version|-v)
            shift
            if [[ -z "${1:-}" ]]; then
                echo -e "${red}--version requires a tag argument (e.g. --version v1.0.0)${plain}"
                exit 1
            fi
            REQUEST_VERSION="$1"; shift ;;
        --version=*)
            REQUEST_VERSION="${1#*=}"; shift ;;
        --)
            shift; break ;;
        -*)
            echo -e "${red}Unknown flag: $1${plain}"
            echo -e "Run ${blue}bash $0 --help${plain} for usage"
            exit 1 ;;
        *)
            # Positional tag, e.g. bash install.sh v1.0.0
            if [[ -z "$REQUEST_VERSION" ]]; then
                REQUEST_VERSION="$1"
            fi
            shift ;;
    esac
done

case "$ACTION" in
    help)
        show_help; exit 0 ;;
    login)
        login_only; exit 0 ;;
    config)
        show_config; exit 0 ;;
    uninstall)
        uninstall_thefeed; exit 0 ;;
    list)
        list_versions; exit 0 ;;
    install)
        install_base
        install_thefeed "$REQUEST_VERSION" "$REQUEST_CHANNEL"
        exit 0 ;;
esac
