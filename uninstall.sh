#!/bin/bash
set -e

LAUNCHD_PLIST="$HOME/Library/LaunchAgents/com.larker.plist"
CONFIG_DIR="$HOME/.larker"
INSTALL_DIR="/usr/local/bin"
BINARY="$INSTALL_DIR/larker"

echo "=== Larker Uninstaller ==="
echo ""

# Summary of what will be removed
echo "The following will be removed:"
if launchctl list | grep -q com.larker 2>/dev/null; then
    echo "  - launchd service: com.larker"
fi
if [ -f "$LAUNCHD_PLIST" ]; then
    echo "  - launchd plist: $LAUNCHD_PLIST"
fi
if [ -f "$BINARY" ]; then
    echo "  - binary: $BINARY"
fi
if [ -d "$CONFIG_DIR" ]; then
    echo "  - config directory: $CONFIG_DIR"
fi
echo "  - Claude Code hooks from settings.json/settings.local.json"
if [ -f "$HOME/.trae/traecli.yaml" ]; then
    echo "  - Trae CLI hooks from traecli.yaml"
fi
echo ""

read -p "Are you sure you want to uninstall Larker? [y/N]: " CONFIRM
if [[ ! "$CONFIRM" =~ ^[Yy]$ ]]; then
    echo "Uninstall cancelled."
    exit 0
fi

echo ""
echo "Uninstalling..."

# 1. Stop and unload launchd service
if launchctl list | grep -q com.larker 2>/dev/null; then
    echo "Stopping larker service..."
    launchctl bootout gui/$(id -u) "$LAUNCHD_PLIST" 2>/dev/null || launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
fi

# 2. Remove launchd plist
if [ -f "$LAUNCHD_PLIST" ]; then
    echo "Removing launchd plist..."
    rm -f "$LAUNCHD_PLIST"
fi

# 3. Remove binary
if [ -f "$BINARY" ]; then
    echo "Removing binary..."
    if [ -w "$INSTALL_DIR" ] 2>/dev/null; then
        rm -f "$BINARY"
    else
        sudo rm -f "$BINARY"
    fi
fi

# 4. Remove config directory
if [ -d "$CONFIG_DIR" ]; then
    echo "Removing config directory..."
    rm -rf "$CONFIG_DIR"
fi

# 5. Remove Claude Code hooks
CLAUDE_SETTINGS="$HOME/.claude/settings.json"
CLAUDE_LOCAL="$HOME/.claude/settings.local.json"

for TARGET in "$CLAUDE_SETTINGS" "$CLAUDE_LOCAL"; do
    if [ -f "$TARGET" ]; then
        if command -v python3 >/dev/null 2>&1; then
            python3 << PYEOF
import json

path = "$TARGET"
with open(path, 'r') as f:
    data = json.load(f)

if 'hooks' not in data:
    exit(0)

def is_larker_cmd(hook):
    if not isinstance(hook, dict):
        return False
    if hook.get('type') != 'command':
        return False
    cmd = hook.get('command', '')
    return 'larker' in cmd

def remove_larker_hooks(entries):
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
    return result

for hook_name in list(data['hooks'].keys()):
    data['hooks'][hook_name] = remove_larker_hooks(data['hooks'][hook_name])
    if not data['hooks'][hook_name]:
        del data['hooks'][hook_name]

with open(path, 'w') as f:
    json.dump(data, f, indent=2, ensure_ascii=False)
PYEOF
            echo "Removed larker hooks from: $TARGET"
        else
            echo "Warning: python3 not found. Please manually remove larker hooks from $TARGET"
        fi
    fi
done

# 6. Remove Trae CLI hooks
TRAE_CONFIG="$HOME/.trae/traecli.yaml"
if [ -f "$TRAE_CONFIG" ]; then
    if command -v python3 >/dev/null 2>&1; then
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
    exit(0)

def is_larker_hook(hook):
    cmd = hook.get('command', '')
    return 'larker' in cmd

data['hooks'] = [h for h in data['hooks'] if not is_larker_hook(h)]

with open(path, 'w') as f:
    yaml.dump(data, f, default_flow_style=False, allow_unicode=True, sort_keys=False)
TRAE_PYEOF
        echo "Removed larker hooks from: $TRAE_CONFIG"
    else
        echo "Warning: python3 not found. Please manually remove larker hooks from $TRAE_CONFIG"
    fi
fi

echo ""
echo "=== Uninstall Complete ==="
echo ""
echo "Larker has been removed."
echo "You may need to restart Claude Code / Trae CLI for hook changes to take effect."
