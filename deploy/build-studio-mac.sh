#!/bin/bash
# Builds HomeLLM Studio as a Mac app: ~/Applications/HomeLLM Studio.app
#
#   deploy/build-studio-mac.sh [user@home-pc]
#
# Opening the app starts the studio in the background (it opens an SSH tunnel to the home PC,
# so `ssh user@home-pc` must work without a password prompt) and shows it in a browser window.
# Opening it again just shows the window. Use the Quit button in the studio to stop it.
set -euo pipefail
cd "$(dirname "$0")/.."

HOST="${1:-cisiwen@192.168.1.120}"
APP="$HOME/Applications/HomeLLM Studio.app"

mkdir -p "$APP/Contents/MacOS"
GOOS=darwin go build -o "$APP/Contents/MacOS/homellm-studio" ./cmd/studio

cat > "$APP/Contents/MacOS/launch" <<EOF
#!/bin/bash
# The app process exits straight away; the studio keeps running on its own, so opening the
# app again reaches it (macOS wouldn't relaunch a still-running app).
mkdir -p "\$HOME/HomeLLM Studio"
nohup "\$(dirname "\$0")/homellm-studio" -host "$HOST" >> "\$HOME/HomeLLM Studio/studio.log" 2>&1 &
EOF
chmod +x "$APP/Contents/MacOS/launch"

cat > "$APP/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>HomeLLM Studio</string>
  <key>CFBundleIdentifier</key><string>local.homellm.studio</string>
  <key>CFBundleExecutable</key><string>launch</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
EOF

echo "Built $APP (home PC: $HOST)."
echo "Open it from Finder, Spotlight or: open \"$APP\""
