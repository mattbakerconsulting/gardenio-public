#!/bin/bash
# playwright.sh — install Playwright, Chromium, Chrome, and system deps.
#
# Requires: Node.js + npm already installed.
# Sources PLAYWRIGHT_VERSION and PLAYWRIGHT_SYSTEM_DEPS from versions.env.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/versions.env"
source "$SCRIPT_DIR/checksums.env"
source "$SCRIPT_DIR/lib.sh"

export DEBIAN_FRONTEND=noninteractive
export PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}

# npm packages
npm install -g "playwright@${PLAYWRIGHT_VERSION}" @playwright/mcp

# Install Chromium via Playwright (for both standalone and MCP)
npx playwright install --with-deps chromium
$(npm root -g)/@playwright/mcp/node_modules/.bin/playwright install chromium

# Install Google Chrome Stable (system browser for playwright-mcp --browser chrome).
# The signing key is sha-verified against checksums.env so a compromised
# dl.google.com cannot swap in an attacker-controlled key.
mkdir -p /etc/apt/keyrings
fetch_verify \
  "https://dl.google.com/linux/linux_signing_key.pub" \
  "${CHROME_SIGNING_KEY_SHA256}" \
  /tmp/google-chrome-signing-key.pub
gpg --dearmor -o /etc/apt/keyrings/google-chrome.gpg < /tmp/google-chrome-signing-key.pub
rm -f /tmp/google-chrome-signing-key.pub
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
  > /etc/apt/sources.list.d/google-chrome.list
apt-get update && apt-get install -y --no-install-recommends google-chrome-stable \
  fonts-noto-core fontconfig

chmod -R 777 "$PLAYWRIGHT_BROWSERS_PATH"

# Cleanup
apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/*
npm cache clean --force

echo "Playwright ${PLAYWRIGHT_VERSION} + Chromium + Chrome installed."
