#!/bin/bash
# zzRouter Install Script - Linux and macOS
#
# Usage:
#   ./install.sh [--port PORT] [--bind ADDR] [--role ROLE]
#                [--cluster-port PORT] [--service] [--uninstall]
#
#   --port PORT          admin/API port (default 9090, auto-increments if taken)
#   --bind ADDR          listen address (default 127.0.0.1; use 0.0.0.0 for LAN)
#   --role ROLE          disabled | worker | coordinator (default disabled)
#   --cluster-port PORT  mTLS cluster listener (default 9091)
#   --service            install a system service (the default when root)
#   --no-service         do not install a service; run detached instead
#   --uninstall          remove zzRouter
#   --yes                do not prompt (unattended)
#   --force-config       overwrite an existing node.yaml (backed up first)
#   --no-install-policy  do not write the root-owned install policy
#
# --role picks what this node is. A worker still has to pair before it
# joins anything, but pairing refuses to run unless the mode is already
# worker, so installing as "disabled" means hand-editing YAML before the
# first useful command. --cluster-port is the mTLS listener a coordinator
# and its workers speak on; it has to match across the cluster, because
# the coordinator reaches each worker by putting its own cluster port onto
# the worker's host.
#
# Run as root (sudo), this installs a supervised system service by
# default, the same shape as ollama's own installer: a dedicated user, a
# systemd unit, enabled at boot. Run as a normal user, it installs
# per-user and starts the node detached.
#
# Detached is a weaker guarantee and is not the default for a reason: it
# survives logoff but NOT a reboot, and it is killed when the session that
# launched it ends -- an SSH session takes its child processes with it on
# disconnect. --no-service opts into that deliberately.
set -euo pipefail

BINARIES="zzrouter-node zzrouter zzrouter-launcher"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
DEFAULT_PORT=9090
DEFAULT_CLUSTER_PORT=9091

# Set in main() from the flags above.
INSTALL_DIR="${HOME}/.local/bin"
BIND="127.0.0.1"
ROLE="disabled"
CLUSTER_PORT="$DEFAULT_CLUSTER_PORT"
SERVICE_MODE=0
NO_SERVICE=0
ASSUME_YES=0
FORCE_CONFIG=0
INSTALL_POLICY=1

# Set by configure_install_policy when it could not write the policy itself.
POLICY_HINT=""

# Set by write_node_yaml to the port the node will actually bind, which
# is the existing config's when one is preserved.
RESOLVED_PORT=""

# The config root a system service reads. On Linux PathResolver maps the
# service user to /etc/zzrouter natively; setting ZZROUTER_CONFIG_DIR for
# login shells too is what stops the operator's CLI and the service from
# reading two different files.
SERVICE_CONFIG_DIR="/etc/zzrouter"
SERVICE_INSTALL_DIR="/usr/local/bin"

# Where a service install actually keeps its binaries. This script still
# drops them in SERVICE_INSTALL_DIR; the first `zzrouter-node start` as
# root moves them here, one directory per version, and leaves symlinks
# behind (pkg/update/managed_install.go). The tree stays root-owned so
# the unprivileged node cannot rewrite the binary that sudo runs -- see
# pkg/service/templates/zzrouter-update.service.
MANAGED_ROOT="/opt/zzrouter"
MANAGED_BIN_DIR="${MANAGED_ROOT}/bin"
MANAGED_VERSIONS_DIR="${MANAGED_ROOT}/versions"
PROFILE_SNIPPET="/etc/profile.d/zzrouter.sh"

# --- Colors & formatting ---

RED='\033[0;31m'
ORANGE='\033[38;2;250;179;135m'
DIM='\033[2m'
BOLD='\033[1m'
RESET='\033[0m'

check() { echo -e "  ${ORANGE}✔${RESET} $1"; }
fail()  { echo -e "  ${RED}✘${RESET} $1" >&2; }
info()  { echo -e "  $1"; }
dim()   { echo -e "  ${DIM}$1${RESET}"; }

# --- Key generation ---

generate_random_hex() {
    openssl rand -hex 24 2>/dev/null \
        || LC_ALL=C tr -dc 'a-f0-9' </dev/urandom 2>/dev/null | head -c 48 \
        || date +%s%N | sha256sum 2>/dev/null | head -c 48 \
        || echo "$(date +%s)-$(od -An -tx4 -N16 /dev/random 2>/dev/null | tr -d ' ')" | head -c 48
}

# --- Port detection ---

is_port_available() {
    local port="$1"
    if command -v lsof >/dev/null 2>&1; then
        ! lsof -i :"$port" >/dev/null 2>&1
    elif command -v ss >/dev/null 2>&1; then
        ! ss -tlnH "sport = :$port" 2>/dev/null | grep -q .
    elif command -v netstat >/dev/null 2>&1; then
        ! netstat -tln 2>/dev/null | grep -q ":$port "
    else
        # No tool available — assume port is free
        return 0
    fi
}

find_available_port() {
    local port="${1:-$DEFAULT_PORT}"
    local max_port=$((port + 100))

    while [ "$port" -le "$max_port" ]; do
        # Skip the cluster listener. Walking up from 9090 lands on 9091
        # first, and handing that out writes a config where the admin API
        # and the mTLS cluster listener both bind the same port -- one of
        # them then fails to start, on a machine that merely had something
        # else on 9090.
        if [ "$port" = "$CLUSTER_PORT" ]; then
            port=$((port + 1))
            continue
        fi
        if is_port_available "$port"; then
            echo "$port"
            return
        fi
        port=$((port + 1))
    done

    fail "No available port found in range ${1:-$DEFAULT_PORT}-${max_port}"
    exit 1
}

# --- Platform detection ---

detect_platform() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux)  OS="linux" ;;
        darwin) OS="darwin" ;;
        *)      fail "Unsupported OS: $OS"; exit 1 ;;
    esac

    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64)         ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        *)              fail "Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    PLATFORM="${OS}-${ARCH}"
}

# --- Find binaries ---

find_binaries() {
    BINARY_SRC=""
    BINARY_SUFFIX=""
    for dir in "$PROJECT_ROOT" "."; do
        # New release layout: dist/<platform>/{zzrouter,zzrouter-launcher,zzrouter-node}
        if [ -f "$dir/dist/${PLATFORM}/zzrouter-node" ]; then
            BINARY_SRC="$dir/dist/${PLATFORM}"
            return
        fi
        # Flat release layout (legacy): zzrouter-node-<platform>
        if [ -f "$dir/zzrouter-node-${PLATFORM}" ]; then
            BINARY_SRC="$dir"
            BINARY_SUFFIX="-${PLATFORM}"
            return
        fi
        # Host build from `make all`
        if [ -f "$dir/zzrouter-node" ]; then
            BINARY_SRC="$dir"
            return
        fi
    done
    fail "Binaries not found. Build first with: make build (host) or make release (cross-platform)"
    exit 1
}

# --- Stop a running node ---

# stop_running_node stops whatever is currently serving, BEFORE the
# binaries are replaced.
#
# Two reasons, and both are silent failures otherwise. Linux refuses to
# write an executable that is currently running (ETXTBSY), so copying
# first fails every upgrade over a live node. And `zzrouter-node start`
# against an already-installed, already-running service returns "Service
# already running" without restarting it -- so the install would report
# success while the old binary kept serving.
stop_running_node() {
    if [ "$(id -u)" = "0" ]; then
        if command -v systemctl >/dev/null 2>&1 &&
           systemctl list-unit-files 2>/dev/null | grep -q '^zzrouter-node\.service'; then
            if [ "$(systemctl is-active zzrouter-node 2>/dev/null)" = "active" ]; then
                check "Stopping the running zzrouter-node service"
                systemctl stop zzrouter-node >/dev/null 2>&1 || true
            fi
        fi
        if [ "$OS" = "darwin" ] && [ -f /Library/LaunchDaemons/com.zzrouter.node.plist ]; then
            launchctl unload /Library/LaunchDaemons/com.zzrouter.node.plist 2>/dev/null || true
        fi
    fi

    # A detached node is nobody's service; stop it through the CLI so the
    # shutdown is the same one `zzrouter-node stop` performs.
    #
    # Scoped to this user's own processes when not root. A per-user install
    # has no business stopping a system service or another account's node,
    # and an unscoped pkill does exactly that: it takes down the machine's
    # node and then, if anything later fails, leaves it down. Root is
    # deliberately unscoped, because a machine-wide install is replacing
    # whatever is there.
    local pgrep_scope=""
    if [ "$(id -u)" != "0" ]; then
        pgrep_scope="-u $(id -u)"
    fi

    # $pgrep_scope is intentionally unquoted: it is an empty string or a
    # "-u <uid>" flag pair that must split into two arguments.
    if pgrep $pgrep_scope -f 'zzrouter-node start' >/dev/null 2>&1; then
        if [ -x "${INSTALL_DIR}/zzrouter-node" ]; then
            "${INSTALL_DIR}/zzrouter-node" stop >/dev/null 2>&1 || true
        fi
        local w
        for w in 1 2 3 4 5 6 7 8; do
            pgrep $pgrep_scope -f 'zzrouter-node start' >/dev/null 2>&1 || break
            sleep 1
        done
        if pgrep $pgrep_scope -f 'zzrouter-node start' >/dev/null 2>&1; then
            check "Force-stopping a previous zzrouter-node"
            pkill $pgrep_scope -f 'zzrouter-node start' >/dev/null 2>&1 || true
            sleep 1
        fi
    fi
}

# --- Install ---

install_binaries() {
    mkdir -p "$INSTALL_DIR"
    INSTALLED=()

    for bin in $BINARIES; do
        src="${BINARY_SRC}/${bin}${BINARY_SUFFIX}"
        dst="${INSTALL_DIR}/${bin}"
        if [ ! -f "$src" ]; then
            continue
        fi

        # A development install has $dst symlinked AT $src -- `make
        # install` does exactly that. cp then fails with "are identical",
        # and under `set -e` the installer dies here, which is after the
        # running node has already been stopped: the machine is left with
        # no node, no new binaries, and no message saying the install
        # failed. -ef compares device+inode through symlinks.
        if [ "$src" -ef "$dst" ]; then
            check "${bin} already is this build (symlinked)"
            INSTALLED+=("$bin")
            continue
        fi

        # Remove first, never write through. After a service install
        # $dst is a symlink into /opt/zzrouter/versions/<ver>/bin, and cp
        # follows it: the copy would land inside a version directory that
        # is supposed to be immutable, overwriting the binary a running
        # node is executing and leaving that version no longer the
        # version it claims to be. It also sidesteps ETXTBSY.
        rm -f "$dst"
        if ! cp "$src" "$dst"; then
            fail "Could not install ${bin} to ${dst}"
            exit 2
        fi
        chmod +x "$dst"
        # Clear macOS quarantine for unsigned binaries
        [ "$OS" = "darwin" ] && xattr -cr "$dst" 2>/dev/null || true
        INSTALLED+=("$bin")
    done

    if [ ${#INSTALLED[@]} -eq 0 ]; then
        fail "No binaries were installed"
        exit 1
    fi
}

get_version() {
    local node_bin="${INSTALL_DIR}/zzrouter-node"
    if [ -x "$node_bin" ]; then
        "$node_bin" version 2>/dev/null | head -1 || echo "unknown"
    else
        echo "unknown"
    fi
}

# --- Bind ---

# bind_is_loopback: only this machine can reach a node bound here. It
# decides the firewall and whether inference may be answered without a key.
bind_is_loopback() {
    case "$BIND" in
        127.0.0.1|::1|localhost) return 0 ;;
        *) return 1 ;;
    esac
}

# --- Reachable address ---

# reachable_address returns an address another machine can actually use to
# reach this one. $BIND may be 0.0.0.0, which is a listen wildcard, not a
# destination: printing it sends operators to configure a coordinator with
# an address that can never connect.
reachable_address() {
    if [ "$BIND" != "0.0.0.0" ] && [ "$BIND" != "::" ]; then
        echo "$BIND"
        return
    fi
    local addr=""
    if command -v ip >/dev/null 2>&1; then
        addr="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") {print $(i+1); exit}}')"
    fi
    # macOS: ask which interface carries the default route rather than
    # guessing en0. A box with both Wi-Fi and Ethernet up answers on both,
    # and the wrong one is a routable-looking address that peers on the
    # other subnet cannot use.
    if [ -z "$addr" ] && command -v route >/dev/null 2>&1; then
        local iface
        iface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}')"
        if [ -n "$iface" ] && command -v ipconfig >/dev/null 2>&1; then
            addr="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
        fi
    fi
    if [ -z "$addr" ] && command -v hostname >/dev/null 2>&1; then
        addr="$(hostname -I 2>/dev/null | awk '{print $1}')"
    fi
    echo "${addr:-$(hostname)}"
}

# --- Config ---

# write_node_yaml writes the node config the server will actually load.
#
# This is the step whose absence caused every worker onboarding to need a
# hand edit: with no cluster block the node falls back to the default
# mode, `cluster pair` refuses to run, and nothing on the path names the
# file to fix.
write_node_yaml() {
    local config_dir="$1" port="$2"
    local node_yaml="${config_dir}/node.yaml"

    mkdir -p "$config_dir"

    # An existing config is left alone. It is not ours to rewrite: a node
    # that has been running owns settings this template does not carry --
    # advertise_ips, the coordinator URL and CA fingerprint it paired
    # with, observability, update policy -- and regenerating the file
    # would silently drop every one of them. An installer that eats the
    # config on upgrade is worse than one that refuses to.
    #
    # --force-config overwrites (after a backup) for the case where the
    # operator really is re-provisioning the node.
    if [ -f "$node_yaml" ] && [ "$FORCE_CONFIG" != "1" ]; then
        local existing_role
        existing_role="$(sed -n '/^cluster:/,/^[^[:space:]]/s/^[[:space:]]*mode:[[:space:]]*\([a-z]*\).*/\1/p' "$node_yaml" | head -1)"
        if [ -n "$existing_role" ]; then
            # Follow the file, so the checks below verify what is really
            # running rather than what the command line asked for.
            ROLE="$existing_role"
        fi
        # The port belongs to the file too. Without this the health and
        # identity checks below poll the port the installer picked while
        # the node binds the one its config names, so a perfectly good
        # install reports itself as failed.
        local existing_port
        existing_port="$(sed -n '/^node:/,/^[^[:space:]]/s/^[[:space:]]*port:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$node_yaml" | head -1)"
        if [ -n "$existing_port" ]; then
            RESOLVED_PORT="$existing_port"
        fi

        check "Keeping existing ${node_yaml} (cluster.mode=${ROLE:-unset}, port=${RESOLVED_PORT:-$port})"
        dim "Pass --force-config to regenerate it from the installer template."
        return 0
    fi

    if [ -f "$node_yaml" ]; then
        cp "$node_yaml" "${node_yaml}.bak.$(date +%Y%m%d-%H%M%S)"
        check "Backed up existing node.yaml"
    fi

    # Keyless inference only where nobody else can reach it.
    local require_compat_auth=true
    if bind_is_loopback; then
        require_compat_auth=false
    fi

    cat > "$node_yaml" <<YAML
# Minimal zzrouter node.yaml written by install.sh
# Defaults to loopback for single-machine use. To expose this node on the
# LAN for multi-node setups, re-run install.sh with --bind 0.0.0.0
# together with --role worker (or --role coordinator).
node:
  name: ""
  bind: "${BIND}"
  port: ${port}

cluster:
  mode: ${ROLE}
  bind_port: ${CLUSTER_PORT}

# auth keys are env-var-name references (resolved at request time).
# The cluster key is provisioned by the coordinator during the join
# handshake.
auth:
  admin_key: "ZZROUTER_ADMIN_API_KEY"
  user_key: "ZZROUTER_API_KEY"
  # true: /v1/* and /api/* refuse a request without a key. Written false
  # only for a loopback bind; give LAN clients virtual keys instead.
  require_compat_auth: ${require_compat_auth}
YAML
    # 0600 (0640 for a service, whose account must read it). The node
    # REFUSES a world-readable node.yaml -- see requireSafePerms -- so a
    # default-umask 0644 file fails the load with a permissions error
    # rather than starting.
    if [ "$SERVICE_MODE" = "1" ]; then
        chmod 640 "$node_yaml"
    else
        chmod 600 "$node_yaml"
    fi
    RESOLVED_PORT="$port"
    check "Wrote ${node_yaml} (bind ${BIND}:${port}, cluster.mode=${ROLE}, cluster.bind_port=${CLUSTER_PORT})"
}

# The install policy is the root-owned file that lets this node apply
# operator overrides of provider install recipes. The node never writes it
# (an agent holding only the API must not grant itself that authority), so
# the human running this installer as root is the one moment it can be
# created. Root installs are Linux-only (macOS refuses them above); the
# macOS path is for the printed instructions, under /private/etc because
# /etc is a symlink there and the node refuses symlinked ancestors.
install_policy_path() {
    if [ "$OS" = "darwin" ]; then
        echo "/private/etc/zzrouter-install-policy.yaml"
    else
        echo "/etc/zzrouter-install-policy.yaml"
    fi
}

# set_install_policy_file points providers.install_policy_file at path,
# leaving an operator's existing absolute setting alone. Rewritten through
# cat so the file keeps its owner and mode.
set_install_policy_file() {
    local node_yaml="$1" path="$2"
    if grep -qE "^[[:space:]]+install_policy_file:[[:space:]]*[\"']?/" "$node_yaml"; then
        check "Keeping providers.install_policy_file already set in ${node_yaml}"
        return 0
    fi
    # An empty value, or providers written inline or quoted, would need a
    # real YAML editor to change safely.
    local unexpected=0
    grep -qE '^[[:space:]]+install_policy_file:' "$node_yaml" && unexpected=1
    if ! grep -qE '^providers:[[:space:]]*$' "$node_yaml" && grep -qE "^[\"']?providers" "$node_yaml"; then
        unexpected=1
    fi
    if [ "$unexpected" = "1" ]; then
        fail "Set providers.install_policy_file: ${path} in ${node_yaml} by hand (unexpected existing form)"
        return 0
    fi
    local updated
    updated="$(mktemp)"
    if grep -qE '^providers:[[:space:]]*$' "$node_yaml"; then
        # Indent like the block's first key; comments may sit at any depth.
        awk -v p="$path" '
            pending && /^[ \t]*[^ \t#\r]/ { match($0, /^[ \t]*/); ind = substr($0, 1, RLENGTH); if (ind == "") ind = "  "; print ind "install_policy_file: " p; pending = 0 }
            { print }
            /^providers:[[:space:]]*$/ { pending = 1 }
            END { if (pending) print "  install_policy_file: " p }
        ' "$node_yaml" > "$updated"
    else
        { cat "$node_yaml"; printf '\nproviders:\n  install_policy_file: %s\n' "$path"; } > "$updated"
    fi
    cat "$updated" > "$node_yaml"
    rm -f "$updated"
    check "Set providers.install_policy_file: ${path}"
}

# configure_install_policy writes a policy granting exactly this release's
# shipped recipes, never replacing one a human already wrote. Without root
# it cannot create a root-owned file, so it leaves instructions instead.
# Every failure here is reported and skipped: the install itself stands.
configure_install_policy() {
    local config_dir="$1"
    if [ "$INSTALL_POLICY" != "1" ]; then
        dim "Skipping the install policy (--no-install-policy)"
        return 0
    fi
    local path
    path="$(install_policy_path)"
    if [ "$(id -u)" != "0" ]; then
        POLICY_HINT="$path"
        return 0
    fi
    if [ -L "$path" ]; then
        fail "${path} is a symlink, which the node refuses; recipe overrides stay disabled"
        return 0
    fi
    if [ -e "$path" ]; then
        check "Keeping existing install policy ${path}"
    else
        local generated
        generated="$(mktemp)"
        if ! "${INSTALL_DIR}/zzrouter-node" install-policy template > "$generated"; then
            rm -f "$generated"
            dim "No install policy written; recipe overrides stay disabled"
            return 0
        fi
        if ! install -o root -g 0 -m 644 "$generated" "$path"; then
            rm -f "$generated"
            fail "Could not write ${path}; recipe overrides stay disabled"
            return 0
        fi
        rm -f "$generated"
        check "Wrote install policy ${path} (shipped recipes only)"
    fi
    set_install_policy_file "${config_dir}/node.yaml" "$path"
}

# The printed commands write straight into the root-owned file: a fixed
# name in a shared /tmp could be swapped by another user before sudo ran.
print_install_policy_hint() {
    [ -n "$POLICY_HINT" ] || return 0
    echo -e "  ${BOLD}Optional: allow recipe overrides through the API.${RESET}"
    dim "A root-owned install policy is required; this installer was not run as root."
    info "    zzrouter-node install-policy template | sudo tee ${POLICY_HINT} >/dev/null"
    info "    sudo chown root ${POLICY_HINT} && sudo chmod 644 ${POLICY_HINT}"
    dim "Then set providers.install_policy_file: ${POLICY_HINT} in node.yaml and restart the node."
    echo ""
}

# write_env_file seeds the admin/user keys. Cluster traffic uses mTLS on
# the cluster-port listener (pkg/cluster/node), so these are the only
# shared secrets needed. An existing file is never overwritten: it may
# hold keys already distributed to clients.
write_env_file() {
    local config_dir="$1"
    local env_file="${config_dir}/.env"

    if [ ! -f "$env_file" ]; then
        local admin_key api_key env_template
        admin_key="zzr_$(generate_random_hex)"
        api_key="zzr_$(generate_random_hex)"
        env_template="${SCRIPT_DIR}/../pkg/config/templates/files/env-default"
        if [ -f "$env_template" ]; then
            sed \
                -e "s|^# ZZROUTER_ADMIN_API_KEY=$|ZZROUTER_ADMIN_API_KEY=${admin_key}|" \
                -e "s|^# ZZROUTER_API_KEY=$|# ZZROUTER_API_KEY=${api_key}|" \
                "$env_template" > "$env_file"
        else
            echo "ZZROUTER_ADMIN_API_KEY=${admin_key}" > "$env_file"
            echo "# ZZROUTER_API_KEY=${api_key}" >> "$env_file"
        fi
        chmod 600 "$env_file"
        check "Generated admin/user keys in ${env_file}"
    elif ! grep -q "^ZZROUTER_ADMIN_API_KEY=" "$env_file" 2>/dev/null; then
        local admin_key
        admin_key="zzr_$(generate_random_hex)"
        echo "" >> "$env_file"
        echo "ZZROUTER_ADMIN_API_KEY=${admin_key}" >> "$env_file"
        check "Added admin key to existing ${env_file}"
    else
        check "Keeping existing ${env_file}"
    fi
}

# --- Health + identity ---

# have_http_client reports whether an HTTP client exists at all.
#
# Checked separately from the request because a minimal image ships
# neither curl nor wget (ubuntu:24.04 does not), and without this the
# probe below fails identically to a dead server -- reporting a perfectly
# healthy node as a failed install.
have_http_client() {
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1
}

http_get() {
    local url="$1"
    if command -v curl >/dev/null 2>&1; then
        curl -fsS --max-time 3 "$url" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- --timeout=3 "$url" 2>/dev/null
    else
        return 1
    fi
}

# tcp_open tests a listener without an HTTP client, using bash's own
# /dev/tcp. Weaker than /health -- it proves something bound the port, not
# that it serves -- so it is the fallback, not the default.
tcp_open() {
    local port="$1"
    (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null && exec 3<&- 2>/dev/null
}

wait_for_health() {
    local port="$1" i
    if ! have_http_client; then
        for i in $(seq 1 20); do
            sleep 1
            if tcp_open "$port"; then
                check "Port ${port} is listening after ${i}s"
                dim "Neither curl nor wget is installed, so /health was not queried."
                dim "Install one of them for a real health and identity check."
                return 0
            fi
        done
        return 1
    fi
    for i in $(seq 1 20); do
        sleep 1
        if http_get "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
            check "Health check passed after ${i}s"
            return 0
        fi
    done
    return 1
}

# report_startup_failure tails wherever the node could have logged.
# There is more than one location: a detached node logs under the user's
# data dir, a service under the service account's. Looking in only one is
# how a startup failure ends up with no log at all to read.
report_startup_failure() {
    fail "zzrouter-node did not become healthy within 20s."
    local d
    for d in "$HOME/.config/zzrouter/logs" "$HOME/.local/share/zzrouter/logs" \
             "$HOME/Library/Application Support/zzrouter/logs" \
             "/var/log/zzrouter" "/var/lib/zzrouter/logs" "${SERVICE_CONFIG_DIR}/logs"; do
        if [ -d "$d" ]; then
            info "  Logs under: $d"
            local f
            for f in "$d"/*; do
                [ -f "$f" ] || continue
                echo "--- tail $f ---"
                tail -n 20 "$f"
            done
        fi
    done
}

# verify_identity refuses to report success unless the node that answered
# is the one this script just configured.
#
# A 200 on /health only proves something is listening. The two diverge for
# real: a node already running from another config keeps serving the port
# and answers perfectly while ignoring every setting written above.
# config_path is loopback-only, and this probe is on 127.0.0.1.
verify_identity() {
    local port="$1" expect_config="$2"
    local body loaded mode

    if ! have_http_client; then
        dim "Skipping identity check: no curl or wget to query /health."
        return 0
    fi

    body="$(http_get "http://127.0.0.1:${port}/health?detailed=true" || true)"
    if [ -z "$body" ]; then
        fail "Could not read node_identity from /health?detailed=true"
        dim "Cannot confirm which node.yaml the running server loaded."
        return 0
    fi

    loaded="$(printf '%s' "$body" | sed -n 's/.*"config_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    mode="$(printf '%s' "$body" | sed -n 's/.*"cluster_mode"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"

    if [ -n "$loaded" ] && [ "$loaded" != "$expect_config" ]; then
        fail "The server on port ${port} is NOT running the config this script wrote."
        info "  wrote  : $expect_config"
        info "  loaded : $loaded"
        info "  mode   : ${mode:-unknown} (expected ${ROLE})"
        info "  Another zzrouter is reading a different config. Stop it and re-run."
        exit 2
    fi
    if [ -n "$mode" ] && [ "$mode" != "$ROLE" ]; then
        fail "The running node reports cluster mode '${mode}' but was installed as '${ROLE}'."
        info "  config : ${loaded:-unknown}"
        exit 2
    fi
    check "Running node loaded ${loaded:-$expect_config} (cluster mode: ${mode:-$ROLE})"
}

# chown_service_config hands the files this installer wrote to the account
# the service runs as.
#
# The installer writes node.yaml and .env as root. SetupDirectories chowns
# the config tree, but only on FIRST run -- runPrivilegedStart gates it on
# IsFirstRun -- so on every upgrade the files stay root-owned and the
# service, which runs as its own user, silently cannot read them. Observed
# on a worker installed in April: /etc/zzrouter/.env is root:root 0600,
# unreadable by the service, and nothing in the log says so.
#
# Runs while the service is stopped and before it is started, so the
# process comes up reading files it owns. On a genuinely first install the
# user does not exist yet and SetupDirectories does this job instead.
chown_service_config() {
    local svc_user="zzrouter"

    id "$svc_user" >/dev/null 2>&1 || return 0
    [ -d "$SERVICE_CONFIG_DIR" ] || return 0

    if chown -R "${svc_user}:${svc_user}" "$SERVICE_CONFIG_DIR" 2>/dev/null; then
        check "Handed ${SERVICE_CONFIG_DIR} to the ${svc_user} account"
    fi
}

# grant_operator_access puts the human who ran sudo into the service
# group, so they can still read the config the service owns.
#
# Two different problems, and both have to be solved: ZZROUTER_CONFIG_DIR
# says WHICH node.yaml to read, group membership says they are ALLOWED to
# read it. Without the group, every `zzrouter-node status` from the
# operator's own shell fails on permissions against a file the installer
# just told them about. (ollama's installer does the same thing:
# `usermod -a -G ollama $(whoami)`.)
#
# Group changes only take effect in new logins, which is worth saying out
# loud rather than letting the next command fail confusingly.
grant_operator_access() {
    local svc_group="zzrouter"
    local operator="${SUDO_USER:-}"

    [ -z "$operator" ] && return 0
    [ "$operator" = "root" ] && return 0
    command -v usermod >/dev/null 2>&1 || return 0
    getent group "$svc_group" >/dev/null 2>&1 || return 0

    if id -nG "$operator" 2>/dev/null | tr ' ' '\n' | grep -qx "$svc_group"; then
        return 0
    fi
    if usermod -a -G "$svc_group" "$operator" 2>/dev/null; then
        check "Added ${operator} to the ${svc_group} group"
        dim "Log out and back in for that to take effect in your shell."
    fi
}

# --- Firewall ---

# open_firewall_port is best-effort and says so. Linux has no single
# firewall, and a silent no-op here is how a paired worker ends up
# unreachable with nothing explaining why.
open_firewall_port() {
    local port="$1"
    if bind_is_loopback; then
        return 0
    fi
    if [ "$(id -u)" != "0" ]; then
        dim "Not root: cannot open TCP ${port}. Open it yourself if this node is remote."
        return 0
    fi
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
        ufw allow "${port}/tcp" >/dev/null 2>&1 && check "ufw: allowed TCP ${port}"
    elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null 2>&1 &&
            firewall-cmd --reload >/dev/null 2>&1 &&
            check "firewalld: allowed TCP ${port}"
    else
        dim "No active ufw/firewalld detected; ensure TCP ${port} is reachable."
    fi
}

# --- Uninstall ---

uninstall() {
    echo ""
    echo "Removing zzRouter..."
    echo ""

    # Stop the service first: signalling the process instead just has
    # systemd or launchd restart it seconds later, and a live node
    # recreates the directories removed below.
    if [ "$(id -u)" = "0" ]; then
        if command -v systemctl >/dev/null 2>&1 &&
           systemctl list-unit-files 2>/dev/null | grep -q '^zzrouter-node\.service'; then
            systemctl stop zzrouter-node >/dev/null 2>&1 || true
            systemctl disable zzrouter-node >/dev/null 2>&1 || true
            rm -f /etc/systemd/system/zzrouter-node.service
            systemctl daemon-reload >/dev/null 2>&1 || true
            check "Removed systemd service zzrouter-node"
        fi

        # The privileged updater. The path unit especially: left behind
        # and enabled, it keeps watching for a request file and would
        # start a service whose binary this uninstall is about to delete.
        local unit
        for unit in zzrouter-update.path zzrouter-update.timer zzrouter-update.service; do
            if systemctl list-unit-files 2>/dev/null | grep -q "^${unit}"; then
                systemctl stop "$unit" >/dev/null 2>&1 || true
                systemctl disable "$unit" >/dev/null 2>&1 || true
                rm -f "/etc/systemd/system/${unit}"
                check "Removed ${unit}"
            fi
        done
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    # launchd, both plausible locations. The Go installer writes a per-user
    # LaunchAgent (pkg/service/launchd.go, $HOME/Library/LaunchAgents); the
    # system-wide LaunchDaemon path is checked too so an uninstall is not
    # silently partial if one is ever installed there. Removing only the
    # daemon path -- which nothing currently creates -- left the real agent
    # loaded and restarting the node after every "uninstall".
    if [ "$OS" = "darwin" ]; then
        local agent_home="${SUDO_USER:+/Users/$SUDO_USER}"
        local plist
        for plist in "${HOME}/Library/LaunchAgents/com.zzrouter.node.plist" \
                     ${agent_home:+"${agent_home}/Library/LaunchAgents/com.zzrouter.node.plist"} \
                     "/Library/LaunchDaemons/com.zzrouter.node.plist"; do
            if [ -f "$plist" ]; then
                launchctl unload "$plist" 2>/dev/null || true
                rm -f "$plist"
                check "Removed launchd job $plist"
            fi
        done
    fi

    pkill -f 'zzrouter-node start' >/dev/null 2>&1 || true

    local dir
    for dir in "$INSTALL_DIR" "$SERVICE_INSTALL_DIR" "$MANAGED_BIN_DIR"; do
        for bin in $BINARIES; do
            # -e is false for a dangling symlink, and after a service
            # install every entry in $SERVICE_INSTALL_DIR is a symlink
            # into the managed tree. Test the link itself, or an
            # uninstall that removed the tree first leaves them behind.
            if [ -e "${dir}/${bin}" ] || [ -L "${dir}/${bin}" ]; then
                rm -f "${dir}/${bin}" && check "Removed ${dir}/${bin}"
            fi
        done
    done

    # The per-version directories are binaries this installer put there,
    # not user data, so they go whatever is decided below about config
    # and models -- which share /opt/zzrouter with them.
    if [ "$(id -u)" = "0" ] && [ -d "$MANAGED_VERSIONS_DIR" ]; then
        rm -rf "$MANAGED_VERSIONS_DIR" "$MANAGED_BIN_DIR" && check "Removed ${MANAGED_VERSIONS_DIR}"
    fi

    if [ "$(id -u)" = "0" ] && [ -f "$PROFILE_SNIPPET" ]; then
        rm -f "$PROFILE_SNIPPET"
        check "Removed $PROFILE_SNIPPET"
    fi

    echo ""
    # NOTE: the answer is lowercased with tr, not ${var,,} -- the latter
    # is bash 4+ and macOS still ships bash 3.2, where it is a syntax
    # error that aborts the uninstall partway through.
    #
    # `read || confirm=""` matters under `set -e`: on EOF (a pipe, or any
    # non-interactive run) read exits non-zero and would otherwise abort
    # the script right here, after the binaries are gone and before
    # anything is said about config -- a half-uninstall reported as
    # nothing at all.
    # Name what goes. "config and data" reads as a few kilobytes of YAML;
    # /opt/zzrouter holds the provider installs -- vLLM virtualenvs,
    # llama.cpp builds -- and /var/lib holds downloaded models. That can be
    # tens of gigabytes and hours to rebuild, which is not something to
    # discover after answering y.
    local confirm
    if [ "$ASSUME_YES" = "1" ]; then
        confirm="y"
    else
        info "  This would also remove:"
        local d
        for d in "$SERVICE_CONFIG_DIR" /var/lib/zzrouter /var/log/zzrouter /opt/zzrouter \
                 "$HOME/.config/zzrouter" "$HOME/.local/share/zzrouter" \
                 "$HOME/Library/Application Support/zzrouter"; do
            if [ -d "$d" ]; then
                dim "    $d ($(du -sh "$d" 2>/dev/null | cut -f1))"
            fi
        done
        dim "    (provider installs and downloaded models live under these)"
        printf "  Remove config and data? This cannot be undone. [y/N] "
        read -r confirm || confirm=""
        echo ""
    fi
    confirm="$(printf '%s' "$confirm" | tr '[:upper:]' '[:lower:]')"
    if [ "$confirm" = "y" ] || [ "$confirm" = "yes" ]; then
        # Every root, because which ones exist depends on how it was
        # installed: a user install writes the home config dir, a service
        # install writes /etc and /var/lib.
        local roots=""
        if [ "$OS" = "darwin" ]; then
            roots="$HOME/Library/Application Support/zzrouter"
        else
            roots="$HOME/.config/zzrouter $HOME/.local/share/zzrouter"
        fi
        for dir in $roots; do
            [ -d "$dir" ] && rm -rf "$dir" && check "Removed $dir"
        done
        if [ "$(id -u)" = "0" ]; then
            for dir in "$SERVICE_CONFIG_DIR" /var/lib/zzrouter /var/log/zzrouter /opt/zzrouter; do
                [ -d "$dir" ] && rm -rf "$dir" && check "Removed $dir"
            done
        else
            dim "Not root: system config under ${SERVICE_CONFIG_DIR} left in place."
        fi
    else
        dim "Config and data preserved"
    fi

    # The service account is deliberately left behind, and saying so is
    # the point: an uninstall that silently leaves a system user reads as
    # complete when it is not, and deleting it here could orphan the uid
    # on any file outside the directories removed above. Verified on a
    # Proxmox LXC: everything else goes, this remains.
    if [ "$(id -u)" = "0" ] && id zzrouter >/dev/null 2>&1; then
        echo ""
        dim "The 'zzrouter' service account was left in place."
        dim "Remove it with: userdel -r zzrouter  (and groupdel zzrouter)"
    fi

    echo ""
    check "zzRouter uninstalled"
    echo ""
}

# --- Main ---

usage() {
    cat <<'USAGE'
  Usage: ./install.sh [options]

    --port PORT          admin/API port (default 9090)
    --bind ADDR          listen address (default 127.0.0.1; 0.0.0.0 for LAN)
    --role ROLE          disabled | worker | coordinator (default disabled)
    --cluster-port PORT  mTLS cluster listener (default 9091)
    --service            install a system service (default when run as root)
    --no-service         do not install a service; run detached instead
    --uninstall          remove zzRouter
    --yes, -y            do not prompt (for unattended uninstall)
    --force-config       overwrite an existing node.yaml (backs it up first)
    --no-install-policy  do not write the root-owned install policy (root only)
    --help               this text
USAGE
}

main() {
    local requested_port=""
    local DO_UNINSTALL=0

    while [ $# -gt 0 ]; do
        case "$1" in
            --uninstall)    DO_UNINSTALL=1; shift ;;
            --yes|-y)       ASSUME_YES=1; shift ;;
            --force-config) FORCE_CONFIG=1; shift ;;
            --no-install-policy) INSTALL_POLICY=0; shift ;;
            --help|-h)
                usage
                exit 0
                ;;
            --port)         requested_port="${2:?--port needs a value}"; shift 2 ;;
            --port=*)       requested_port="${1#--port=}"; shift ;;
            --bind)         BIND="${2:?--bind needs a value}"; shift 2 ;;
            --bind=*)       BIND="${1#--bind=}"; shift ;;
            --role)         ROLE="${2:?--role needs a value}"; shift 2 ;;
            --role=*)       ROLE="${1#--role=}"; shift ;;
            --cluster-port) CLUSTER_PORT="${2:?--cluster-port needs a value}"; shift 2 ;;
            --cluster-port=*) CLUSTER_PORT="${1#--cluster-port=}"; shift ;;
            --service)      SERVICE_MODE=1; NO_SERVICE=0; shift ;;
            --no-service)   NO_SERVICE=1; SERVICE_MODE=0; shift ;;
            *)
                fail "Unknown option: $1"
                usage
                exit 1
                ;;
        esac
    done

    # Acted on after the whole command line is read, so --yes applies no
    # matter which side of --uninstall it appears on.
    # Before the guards below: they need to know the platform.
    detect_platform

    if [ "$DO_UNINSTALL" = "1" ]; then
        uninstall
        exit 0
    fi

    case "$ROLE" in
        disabled|worker|coordinator) ;;
        *) fail "--role must be disabled, worker or coordinator (got '$ROLE')"; exit 1 ;;
    esac

    # macOS has no working service install yet, and pretending otherwise
    # produces a broken node rather than an error. The launchd plist is
    # written to $HOME/Library/LaunchAgents via os.UserHomeDir(), which
    # under sudo is /var/root, and it pins ZZROUTER_CONFIG_DIR to root's
    # Application Support -- not the directory this script writes. The
    # node also refuses to run as root on every platform, so the agent
    # could not start even if the paths lined up. Say so instead.
    if [ "$OS" = "darwin" ] && { [ "$SERVICE_MODE" = "1" ] || [ "$(id -u)" = "0" ]; }; then
        fail "A system service install is not supported on macOS yet."
        info "  The launchd job would be installed into root's home and would"
        info "  read a different config than the one written here."
        info "  Install as your own user instead:"
        info "    ./install.sh --bind ${BIND} --role ${ROLE}"
        info "  The node then runs detached; it survives logoff but not a reboot."
        exit 1
    fi

    if [ "$SERVICE_MODE" = "1" ] && [ "$(id -u)" != "0" ]; then
        fail "--service installs a system service and needs root. Re-run with sudo."
        exit 1
    fi

    # Root means a machine-wide install, and a machine-wide install should
    # leave something the machine keeps running. Default to the service
    # rather than making the durable choice the opt-in one.
    if [ "$(id -u)" = "0" ] && [ "$NO_SERVICE" != "1" ]; then
        SERVICE_MODE=1
    fi

    # Root with --no-service is a dead end: the node refuses to run as root
    # (it would run every provider as root too), so this would install,
    # report "starting", and then die with a wall of service-user advice.
    if [ "$SERVICE_MODE" != "1" ] && [ "$(id -u)" = "0" ]; then
        fail "--no-service as root has nowhere to go."
        info "  zzrouter-node refuses to run as root, so it would install and"
        info "  then fail to start. Pick one:"
        info "    sudo ./install.sh [options]    system service (default when root)"
        info "    ./install.sh [options]         per-user install, run as yourself"
        exit 1
    fi

    # A service runs as its own account, so an install into $HOME would be
    # unreadable to it. Machine-wide locations for a machine-wide service.
    local config_dir
    if [ "$SERVICE_MODE" = "1" ]; then
        INSTALL_DIR="$SERVICE_INSTALL_DIR"
        config_dir="$SERVICE_CONFIG_DIR"
    elif [ "$OS" = "darwin" ]; then
        config_dir="$HOME/Library/Application Support/zzrouter"
    else
        config_dir="$HOME/.config/zzrouter"
    fi

    echo ""
    echo "Setting up zzRouter..."
    echo ""

    find_binaries
    stop_running_node
    install_binaries

    local port
    if [ -n "$requested_port" ]; then
        if is_port_available "$requested_port"; then
            port="$requested_port"
        else
            fail "Port $requested_port is already in use"
            exit 1
        fi
    else
        port="$(find_available_port "$DEFAULT_PORT")"
    fi

    mkdir -p "$config_dir"

    # Point login shells at the service's config root so the operator's
    # CLI and the service resolve ONE node.yaml. Without this the service
    # reads /etc/zzrouter while `zzrouter-node status` reads the invoking
    # user's copy, and the two disagree with nothing saying why.
    if [ "$SERVICE_MODE" = "1" ]; then
        printf 'export ZZROUTER_CONFIG_DIR=%s\n' "$SERVICE_CONFIG_DIR" > "$PROFILE_SNIPPET"
        chmod 644 "$PROFILE_SNIPPET"
        export ZZROUTER_CONFIG_DIR="$SERVICE_CONFIG_DIR"
        check "ZZROUTER_CONFIG_DIR=${SERVICE_CONFIG_DIR} (via ${PROFILE_SNIPPET})"
    fi

    write_node_yaml "$config_dir" "$port"
    port="${RESOLVED_PORT:-$port}"
    configure_install_policy "$config_dir"

    # Client config port, when a client config already exists.
    "${INSTALL_DIR}/zzrouter" version >/dev/null 2>&1 || true
    local client_yaml="${config_dir}/cli.yaml"
    if [ -f "$client_yaml" ]; then
        sed -i.bak "s/port: [0-9]*/port: ${port}/" "$client_yaml"
        rm -f "${client_yaml}.bak"
    fi

    write_env_file "$config_dir"

    open_firewall_port "$port"
    if [ "$ROLE" != "disabled" ]; then
        open_firewall_port "$CLUSTER_PORT"
    fi

    VERSION="$(get_version)"

    echo ""
    echo -e "  ${ORANGE}✔${RESET} ${BOLD}zzRouter installed${RESET}"
    echo ""
    info "  Version:   ${ORANGE}${VERSION}${RESET}"
    info "  Location:  ${ORANGE}${INSTALL_DIR}${RESET}"
    info "  Platform:  ${ORANGE}${PLATFORM}${RESET}"
    info "  Config:    ${ORANGE}${config_dir}/node.yaml${RESET}"
    info "  Listening: ${ORANGE}${BIND}:${port}${RESET}"
    if [ "$port" != "$DEFAULT_PORT" ] && [ -z "$requested_port" ]; then
        dim "Port ${DEFAULT_PORT} was in use, using ${port} instead"
    fi
    echo ""

    if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
        echo -e "  ${RED}!${RESET} ${INSTALL_DIR} is not in your PATH"
        dim "Add it with: export PATH=\"$INSTALL_DIR:\$PATH\""
        echo ""
    fi

    # Two ways to bring the node up, and they are not interchangeable.
    # A service is registered with systemd/launchd, so it outlives this
    # shell and comes back after a reboot. A detached node is a child of
    # this shell and dies with the session that launched it.
    if [ "$SERVICE_MODE" = "1" ]; then
        chown_service_config
        info "  Installing and starting the system service..."
        if ! "${INSTALL_DIR}/zzrouter-node" start; then
            fail "Service install/start failed"
            report_startup_failure
            exit 2
        fi
        # After, not before: the service user and group are created by
        # that start, so there is nothing to join until it has run.
        grant_operator_access
    else
        info "  Starting zzrouter-node on port ${port}..."
        if ! "${INSTALL_DIR}/zzrouter-node" start --no-setup --detach -P "$port"; then
            fail "Failed to start server"
            report_startup_failure
            exit 2
        fi
    fi

    if ! wait_for_health "$port"; then
        report_startup_failure
        exit 2
    fi
    verify_identity "$port" "${config_dir}/node.yaml"

    echo ""
    if [ "$SERVICE_MODE" != "1" ]; then
        # Said plainly because the failure is otherwise a mystery: the
        # install reports healthy, the session ends, and the node is gone.
        echo -e "  ${RED}!${RESET} Started detached: this node does NOT survive a reboot, and it"
        dim "is killed when the session that launched it ends (an SSH session"
        dim "takes its child processes with it). For a permanent node:"
        dim "  sudo ./install.sh --bind ${BIND} --role ${ROLE}"
        echo ""
    fi

    print_next_steps "$port"
    print_install_policy_hint
}

# print_next_steps names the exact commands for this node's role, so the
# operator does not have to work out which half of the pairing flow runs
# where.
print_next_steps() {
    local port="$1"
    case "$ROLE" in
        worker)
            echo -e "  ${BOLD}Next: pair this worker with your coordinator.${RESET}"
            info "    Here:         zzrouter-node cluster pair --coordinator-url https://COORDINATOR_HOST:${CLUSTER_PORT}"
            info "    On the coord: zzrouter-node cluster accept CODE"
            echo ""
            dim "The coordinator reaches this node at $(reachable_address):${CLUSTER_PORT},"
            dim "so that port must stay open and its cluster.bind_port must match."
            ;;
        coordinator)
            echo -e "  ${BOLD}This node is a coordinator. To add a worker:${RESET}"
            info "    On the worker: zzrouter-node cluster pair --coordinator-url https://$(reachable_address):${CLUSTER_PORT}"
            info "    Here:          zzrouter-node cluster accept CODE"
            ;;
        *)
            echo -e "  ${BOLD}Next: run ${RESET}zzrouter quickstart${BOLD} to get started${RESET}"
            echo ""
            dim "This node is not clustered (cluster.mode: disabled)."
            dim "To make it a worker, re-run with --role worker, then pair."
            ;;
    esac
    echo ""
}

# Only run when executed, not when sourced, so the functions above can be
# exercised directly by tests.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    main "$@"
fi
