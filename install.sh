#!/bin/bash
set -e

LAUNCHD_PLIST="$HOME/Library/LaunchAgents/com.larker.plist"
CONFIG_DIR="$HOME/.larker"
CONFIG_FILE="$CONFIG_DIR/config.yaml"

# Always install to /usr/local/bin on macOS
INSTALL_DIR="/usr/local/bin"

# Check if we need sudo
NEED_SUDO=false
if [ ! -w "$INSTALL_DIR" ] 2>/dev/null; then
    NEED_SUDO=true
fi

echo "=== Larker Installer ==="
echo ""

# 1. Check Go
if ! command -v go >/dev/null 2>&1; then
    echo "Error: Go is not installed. Please install Go first: https://go.dev/dl/"
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
echo "Go version: $GO_VERSION"

# 2. Check tmux
if ! command -v tmux >/dev/null 2>&1; then
    echo "Error: tmux is not installed. Please install tmux first:"
    echo "  brew install tmux"
    exit 1
fi

echo "tmux version: $(tmux -V)"

# 3. Stop and unload old service if running
echo ""
if launchctl list | grep -q com.larker 2>/dev/null; then
    echo "Stopping and unloading existing larker service..."
    # Try modern bootout first, fall back to unload
    launchctl bootout gui/$(id -u) "$LAUNCHD_PLIST" 2>/dev/null || launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
fi

# 3a. Kill any lingering larker server processes to free the port
echo "Ensuring no old larker server processes are running..."
pkill -9 -f "larker server" 2>/dev/null || true
sleep 1

# 4. Build
echo ""
echo "Building larker..."
cd "$(dirname "$0")"
BUILD_TMP=$(mktemp)
go build -o "$BUILD_TMP" ./cmd/larker
if [ "$NEED_SUDO" = true ]; then
    echo "Directory $INSTALL_DIR requires administrator privileges."
    sudo mv "$BUILD_TMP" "$INSTALL_DIR/larker"
    sudo chmod +x "$INSTALL_DIR/larker"
else
    mv "$BUILD_TMP" "$INSTALL_DIR/larker"
    chmod +x "$INSTALL_DIR/larker"
fi
echo "Binary installed to: $INSTALL_DIR/larker"

# 5. Create config dir
mkdir -p "$CONFIG_DIR"

# 6. Generate config if not exists
if [ ! -f "$CONFIG_FILE" ]; then
    echo ""
    echo "=== Lark / Feishu Configuration ==="
    echo ""
    echo "Select your platform:"
    echo "  1) Feishu (国内飞书) - https://open.feishu.cn"
    echo "  2) Lark (国际版)     - https://open.larksuite.com"
    read -p "Enter 1 or 2 [1]: " PLATFORM_CHOICE
    PLATFORM_CHOICE=${PLATFORM_CHOICE:-1}

    if [ "$PLATFORM_CHOICE" = "2" ]; then
        BASE_URL="https://open.larksuite.com"
        PLATFORM_NAME="Lark"
        PLATFORM_URL="https://open.larksuite.com/app"
    else
        BASE_URL="https://open.feishu.cn"
        PLATFORM_NAME="Feishu"
        PLATFORM_URL="https://open.feishu.cn/app"
    fi

    echo ""
    echo "--- App Credentials ---"
    echo "Create a custom app at $PLATFORM_URL"
    echo "Go to: App details -> Credentials & Basic Info"
    echo ""
    read -p "$PLATFORM_NAME App ID: " APP_ID
    read -p "$PLATFORM_NAME App Secret: " APP_SECRET

    # Write temporary config for API lookup scripts
    mkdir -p "$CONFIG_DIR"
    cat > "$CONFIG_FILE" << TMPCONFIG
server:
  port: 8765
  host: "127.0.0.1"

lark:
  base_url: "$BASE_URL"
  app_id: "$APP_ID"
  app_secret: "$APP_SECRET"
  receiver_type: "open_id"
  receiver_id: "tmp"

tmux:
  session_prefix: "larker"
  keep_sessions: false

timeout:
  confirmation: 3600

log:
  file: "$CONFIG_DIR/larker.log"
  level: "info"
  max_size_mb: 10
TMPCONFIG

    echo ""
    echo "--- Message Receiver ---"
    echo "Select how you want to receive messages:"
    echo "  1) Group chat (推荐) - 把机器人拉到群里，所有人在群里确认"
    echo "  2) Direct message    - 发给个人"
    read -p "Enter 1 or 2 [1]: " RECEIVER_TYPE_CHOICE
    RECEIVER_TYPE_CHOICE=${RECEIVER_TYPE_CHOICE:-1}

    if [ "$RECEIVER_TYPE_CHOICE" = "2" ]; then
        RECEIVER_TYPE="open_id"
        echo ""
        echo "We can look up your open_id automatically."
        read -p "Enter your email or mobile (with country code, e.g. +86138xxxx): " CONTACT

        echo "Looking up open_id via API..."
        if echo "$CONTACT" | grep -q "@"; then
            LOOKUP_RESULT=$(go run ./scripts/get-openid-api --email "$CONTACT" 2>&1)
        else
            LOOKUP_RESULT=$(go run ./scripts/get-openid-api --mobile "$CONTACT" 2>&1)
        fi
        echo "$LOOKUP_RESULT"
        RECEIVER_ID=$(echo "$LOOKUP_RESULT" | grep "open_id:" | head -1 | awk '{print $2}')
        if [ -n "$RECEIVER_ID" ]; then
            echo "Found open_id: $RECEIVER_ID"
        else
            echo ""
            echo "Could not find open_id automatically. Please enter manually:"
            read -p "Receiver Open ID: " RECEIVER_ID
        fi
    else
        RECEIVER_TYPE="chat_id"
        echo ""
        echo "Fetching your group chats..."
        CHAT_OUTPUT=$(go run ./scripts/get-openid-api --list 2>&1)
        echo "$CHAT_OUTPUT"

        CHAT_IDS=()
        while IFS= read -r line; do
            chat_id=$(echo "$line" | sed -n 's/.*chat_id: \([^ ]*\).*/\1/p')
            if [ -n "$chat_id" ]; then
                CHAT_IDS+=("$chat_id")
            fi
        done <<< "$(echo "$CHAT_OUTPUT" | grep "chat_id:")"

        if [ ${#CHAT_IDS[@]} -eq 0 ]; then
            echo ""
            echo "No chats found. Make sure the bot has been added to a group."
            read -p "Group Chat ID: " RECEIVER_ID
        elif [ ${#CHAT_IDS[@]} -eq 1 ]; then
            RECEIVER_ID="${CHAT_IDS[0]}"
            echo "Selected chat: $RECEIVER_ID"
        else
            echo ""
            echo "Multiple chats found. Select one:"
            i=1
            while IFS= read -r line; do
                echo "  $i) $line"
                i=$((i+1))
            done <<< "$(echo "$CHAT_OUTPUT" | grep "chat_id:")"
            read -p "Enter number [1]: " CHOICE
            CHOICE=${CHOICE:-1}
            idx=$((CHOICE-1))
            if [ "$idx" -ge 0 ] && [ "$idx" -lt "${#CHAT_IDS[@]}" ]; then
                RECEIVER_ID="${CHAT_IDS[$idx]}"
            else
                echo "Invalid choice, using first chat"
                RECEIVER_ID="${CHAT_IDS[0]}"
            fi
        fi
    fi

    cat > "$CONFIG_FILE" << CONFIGEOF
server:
  port: 8765
  host: "127.0.0.1"

lark:
  base_url: "$BASE_URL"
  app_id: "$APP_ID"
  app_secret: "$APP_SECRET"
  receiver_type: "$RECEIVER_TYPE"
  receiver_id: "$RECEIVER_ID"

tmux:
  session_prefix: "larker"
  keep_sessions: false

timeout:
  confirmation: 3600

log:
  file: "$CONFIG_DIR/larker.log"
  level: "info"
  max_size_mb: 10
CONFIGEOF

    chmod 600 "$CONFIG_FILE"
    echo ""
    echo "Config written to: $CONFIG_FILE"
else
    echo "Config already exists: $CONFIG_FILE"
fi

# 7. Write launchd plist
echo ""
echo "Creating launchd service..."
cat > "$LAUNCHD_PLIST" << PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.larker</string>
    <key>ProgramArguments</key>
    <array>
        <string>$INSTALL_DIR/larker</string>
        <string>server</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/larker.out</string>
    <key>StandardErrorPath</key>
    <string>/tmp/larker.err</string>
</dict>
</plist>
PLISTEOF

# 8. Load and start service
# Try modern bootstrap first, fall back to load for older macOS
launchctl bootstrap gui/$(id -u) "$LAUNCHD_PLIST" 2>/dev/null || launchctl load "$LAUNCHD_PLIST" 2>/dev/null || true

# 9. Configure Claude Code settings
echo ""
echo "Configuring Claude Code hooks..."

CLAUDE_SETTINGS="$HOME/.claude/settings.json"
CLAUDE_LOCAL="$HOME/.claude/settings.local.json"

# Prefer settings.local.json if it exists or if settings.json does not exist
if [ -f "$CLAUDE_LOCAL" ] || [ ! -f "$CLAUDE_SETTINGS" ]; then
    TARGET="$CLAUDE_LOCAL"
else
    TARGET="$CLAUDE_SETTINGS"
fi

# Create or update settings
if [ ! -f "$TARGET" ]; then
    echo '{}' > "$TARGET"
fi

# Backup before modifying
BACKUP="$TARGET.larker-backup.$(date +%Y%m%d-%H%M%S)"
cp "$TARGET" "$BACKUP"
echo "Backup created: $BACKUP"

# Use Python or jq to merge JSON if available, otherwise warn
if command -v python3 >/dev/null 2>&1; then
    python3 << PYEOF
import json, sys, shutil

path = "$TARGET"
with open(path, 'r') as f:
    data = json.load(f)

if 'hooks' not in data:
    data['hooks'] = {}

def is_larker_cmd(hook):
    """Check if a hook entry is a larker command."""
    if not isinstance(hook, dict):
        return False
    if hook.get('type') != 'command':
        return False
    cmd = hook.get('command', '')
    return 'larker' in cmd

def append_hooks(existing, new_entries):
    """Append new_entries to existing list, deduplicating by larker command."""
    if existing is None:
        existing = []
    result = list(existing)
    for entry in new_entries:
        # For command hooks, deduplicate by command string
        inner_hooks = entry.get('hooks', [])
        is_duplicate = False
        for existing_entry in result:
            existing_inner = existing_entry.get('hooks', [])
            for h in inner_hooks:
                if is_larker_cmd(h):
                    for eh in existing_inner:
                        if is_larker_cmd(eh) and eh.get('command') == h.get('command'):
                            is_duplicate = True
                            break
                if is_duplicate:
                    break
            if is_duplicate:
                break
        if not is_duplicate:
            result.append(entry)
    return result

def remove_larker_hooks(entries):
    """Remove all larker command hooks from entries."""
    if not isinstance(entries, list):
        return entries
    result = []
    for entry in entries:
        inner_hooks = entry.get('hooks', [])
        filtered = [h for h in inner_hooks if not is_larker_cmd(h)]
        if filtered:
            new_entry = dict(entry)
            new_entry['hooks'] = filtered
            result.append(new_entry)
        # If all inner hooks were larker, drop the entire entry
    return result

# Remove old PreToolUse larker hooks (migrated to Notification)
if 'PreToolUse' in data['hooks']:
    data['hooks']['PreToolUse'] = remove_larker_hooks(data['hooks']['PreToolUse'])
    # Clean up empty list
    if not data['hooks']['PreToolUse']:
        del data['hooks']['PreToolUse']

# SessionStart: append
sessionstart_entries = [
    {
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=sessionstart"
            }
        ]
    }
]
data['hooks']['SessionStart'] = append_hooks(data['hooks'].get('SessionStart'), sessionstart_entries)

# UserPromptSubmit: append
promptsubmit_entries = [
    {
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=promptsubmit"
            }
        ]
    }
]
data['hooks']['UserPromptSubmit'] = append_hooks(data['hooks'].get('UserPromptSubmit'), promptsubmit_entries)

# Notification: append, don't overwrite
notification_entries = [
    {
        "matcher": "permission_prompt|idle_prompt|elicitation_dialog",
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=notification"
            }
        ]
    }
]
data['hooks']['Notification'] = append_hooks(data['hooks'].get('Notification'), notification_entries)

# PostToolUse: append
posttooluse_entries = [
    {
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=post"
            }
        ]
    }
]
data['hooks']['PostToolUse'] = append_hooks(data['hooks'].get('PostToolUse'), posttooluse_entries)

# Stop: append
stop_entries = [
    {
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=stop"
            },
            {
                "type": "command",
                "command": "afplay /System/Library/Sounds/Ping.aiff"
            }
        ]
    }
]
data['hooks']['Stop'] = append_hooks(data['hooks'].get('Stop'), stop_entries)

# StopFailure: append
stopfailure_entries = [
    {
        "hooks": [
            {
                "type": "command",
                "command": "$INSTALL_DIR/larker hook --phase=stopfailure"
            }
        ]
    }
]
data['hooks']['StopFailure'] = append_hooks(data['hooks'].get('StopFailure'), stopfailure_entries)

with open(path, 'w') as f:
    json.dump(data, f, indent=2, ensure_ascii=False)
PYEOF
    echo "Hooks written to: $TARGET"
else
    echo "Warning: python3 not found. Please manually add the following hooks to $TARGET:"
    echo '  "SessionStart": [{"hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=sessionstart"}]}]'
    echo '  "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=promptsubmit"}]}]'
    echo '  "Notification": [{"matcher": "permission_prompt|idle_prompt|elicitation_dialog", "hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=notification"}]}]'
    echo '  "PostToolUse": [{"hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=post"}]}]'
    echo '  "Stop": [{"hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=stop"}, {"type": "command", "command": "afplay /System/Library/Sounds/Ping.aiff"}]}]'
    echo '  "StopFailure": [{"hooks": [{"type": "command", "command": "'$INSTALL_DIR/larker' hook --phase=stopfailure"}]}]'
fi

# 10. Configure Trae CLI hooks
echo ""
echo "Configuring Trae CLI hooks..."

TRAE_CONFIG="$HOME/.trae/traecli.yaml"

if [ -f "$TRAE_CONFIG" ]; then
    if command -v python3 >/dev/null 2>&1; then
        # Backup before modifying
        TRAE_BACKUP="$TRAE_CONFIG.larker-backup.$(date +%Y%m%d-%H%M%S)"
        cp "$TRAE_CONFIG" "$TRAE_BACKUP"
        echo "Backup created: $TRAE_BACKUP"

        python3 << TRAE_PYEOF
import sys
try:
    import yaml
except ImportError:
    print("Warning: PyYAML not installed. Trying pip install...")
    import subprocess
    subprocess.check_call([sys.executable, "-m", "pip", "install", "pyyaml", "-q"])
    import yaml

path = "$TRAE_CONFIG"
with open(path, 'r') as f:
    data = yaml.safe_load(f) or {}

if 'hooks' not in data:
    data['hooks'] = []

def is_larker_hook(hook):
    """Check if a hook entry is a larker command."""
    cmd = hook.get('command', '')
    return 'larker' in cmd

# Remove existing larker hooks (to avoid duplicates)
data['hooks'] = [h for h in data['hooks'] if not is_larker_hook(h)]

# Add larker hooks for Trae (uses snake_case events)
larker_hooks = [
    {
        'type': 'command',
        'command': '$INSTALL_DIR/larker hook --phase=notification',
        'matchers': [
            {'event': 'notification'}
        ]
    },
    {
        'type': 'command',
        'command': '$INSTALL_DIR/larker hook --phase=post',
        'matchers': [
            {'event': 'post_tool_use'}
        ]
    },
    {
        'type': 'command',
        'command': '$INSTALL_DIR/larker hook --phase=stop',
        'matchers': [
            {'event': 'stop'},
            {'event': 'subagent_stop'}
        ]
    },
]

data['hooks'].extend(larker_hooks)

with open(path, 'w') as f:
    yaml.dump(data, f, default_flow_style=False, allow_unicode=True, sort_keys=False)
TRAE_PYEOF
        echo "Trae hooks written to: $TRAE_CONFIG"
    else
        echo "Warning: python3 not found. Please manually add larker hooks to $TRAE_CONFIG"
    fi
else
    echo "Trae config not found at $TRAE_CONFIG, skipping Trae configuration."
    echo "If you use Trae CLI, create the config file and re-run install."
fi

echo ""

# 11. Configure kimi-cli hooks
echo ""
echo "Configuring kimi-cli hooks..."

KIMI_CONFIG="$HOME/.kimi/config.toml"

if command -v python3 >/dev/null 2>&1; then
    python3 << KIMI_PYEOF
import os
import re

kimi_config = os.path.expanduser("~/.kimi/config.toml")

hooks_toml = """\n[[hooks]]\nevent = "PostToolUse"\ncommand = "$INSTALL_DIR/larker hook --phase=post"\n\n[[hooks]]\nevent = "Stop"\ncommand = "$INSTALL_DIR/larker hook --phase=stop"\n\n[[hooks]]\nevent = "StopFailure"\ncommand = "$INSTALL_DIR/larker hook --phase=stopfailure"\n\n[[hooks]]\nevent = "Notification"\nmatcher = "permission_prompt|idle_prompt|elicitation_dialog"\ncommand = "$INSTALL_DIR/larker hook --phase=notification"\n\n[[hooks]]\nevent = "SessionStart"\ncommand = "$INSTALL_DIR/larker hook --phase=sessionstart"\n\n[[hooks]]\nevent = "UserPromptSubmit"\ncommand = "$INSTALL_DIR/larker hook --phase=promptsubmit"\n"""

def has_larker_hooks(content):
    for line in content.split('\n'):
        if 'command' in line and 'larker' in line:
            return True
    return False

if os.path.exists(kimi_config):
    # Backup before modifying
    backup = kimi_config + ".larker-backup." + os.popen('date +%Y%m%d-%H%M%S').read().strip()
    import shutil
    shutil.copy(kimi_config, backup)
    print("Backup created:", backup)

    with open(kimi_config, 'r') as f:
        content = f.read()

    # Remove empty hooks array that conflicts with [[hooks]] table-array syntax
    content = re.sub(r'^hooks\s*=\s*\[\]\s*(?:#.*)?$', '', content, flags=re.MULTILINE)
    # Clean up any extra blank lines left behind
    content = re.sub(r'\n{3,}', '\n\n', content)

    if has_larker_hooks(content):
        print("kimi-cli config already contains larker hooks, skipping")
    else:
        with open(kimi_config, 'w') as f:
            f.write(content.rstrip() + hooks_toml)
        print("kimi-cli hooks written to:", kimi_config)
else:
    os.makedirs(os.path.dirname(kimi_config), exist_ok=True)
    with open(kimi_config, 'w') as f:
        f.write(hooks_toml.strip() + '\n')
    os.chmod(kimi_config, 0o600)
    print("kimi-cli config created:", kimi_config)
KIMI_PYEOF
else
    echo "Warning: python3 not found. Please manually add the following hooks to ~/.kimi/config.toml:"
    echo '  [[hooks]]'
    echo '  event = "PostToolUse"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=post"'
    echo '  [[hooks]]'
    echo '  event = "Stop"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=stop"'
    echo '  [[hooks]]'
    echo '  event = "StopFailure"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=stopfailure"'
    echo '  [[hooks]]'
    echo '  event = "Notification"'
    echo '  matcher = "permission_prompt|idle_prompt|elicitation_dialog"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=notification"'
    echo '  [[hooks]]'
    echo '  event = "SessionStart"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=sessionstart"'
    echo '  [[hooks]]'
    echo '  event = "UserPromptSubmit"'
    echo '  command = "'$INSTALL_DIR/larker' hook --phase=promptsubmit"'
fi
echo "=== Installation Complete ==="
echo ""
echo "Larker service status:"
launchctl list | grep com.larker || echo "  (service may need a moment to start)"
echo ""
echo "Config: $CONFIG_FILE"
echo "Logs:   $CONFIG_DIR/larker.log"
echo ""
# Detect platform from existing config if available
if [ -f "$CONFIG_FILE" ] && grep -q "larksuite" "$CONFIG_FILE" 2>/dev/null; then
    PLATFORM_NAME="Lark"
else
    PLATFORM_NAME="Feishu"
fi

LOCAL_IP=$(ifconfig | grep 'inet ' | grep -v 127.0.0.1 | awk '{print $2}' | head -1)
[ -z "$LOCAL_IP" ] && LOCAL_IP="YOUR_IP"

echo "Next steps:"
echo "1. Ensure your $PLATFORM_NAME app has the following permissions:"
echo "   - im:message:send_as_bot"
echo "   - im:message.group_msg  (if sending to group)"
echo "   - im:message:p2p_msg    (if sending to user)"
echo "   - im:chat:readonly      (to read group info)"
echo ""
echo "2. Configure event subscription in $PLATFORM_NAME app:"
echo "   Go to: App -> Event Subscriptions -> Connection Method"
echo "   Select: Long Connection (WebSocket / 长连接)"
echo "   Subscribe events:"
echo "     - im.message.receive_v1    (receive text replies)"
echo "     - card.action.trigger      (receive button clicks)"
echo "   (no public URL or ngrok needed)"
echo ""
echo "3. Restart Claude Code / Trae CLI to load new hooks"
