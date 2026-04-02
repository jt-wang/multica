#!/usr/bin/env bash
#
# oracle-vm-setup.sh — Bootstrap a fresh Oracle Cloud ARM VM (Ubuntu 24.04)
# to run Multica server + PostgreSQL + Cloudflare Tunnel.
#
# Usage:
#   1. SSH into your Oracle VM
#   2. Copy this script and run: chmod +x oracle-vm-setup.sh && ./oracle-vm-setup.sh
#   3. Follow the interactive prompts for Cloudflare tunnel setup
#
# Installs: PostgreSQL 17 + pgvector, Go 1.26.1, Node.js 22, pnpm, cloudflared
# Creates:  systemd services for multica-server, multica-frontend, cloudflared

set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────────
GO_VERSION="1.26.1"
NODE_MAJOR=22
REPO_URL="${MULTICA_REPO_URL:-git@github.com:multica-ai/multica.git}"
INSTALL_DIR="$HOME/multica"
DOMAIN=""  # set via --domain flag or prompted

# ── Parse flags ────────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case $1 in
    --domain) DOMAIN="$2"; shift 2 ;;
    --repo)   REPO_URL="$2"; shift 2 ;;
    --dir)    INSTALL_DIR="$2"; shift 2 ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

echo "================================================"
echo "  Multica — Oracle VM Setup"
echo "================================================"
echo ""

# ── 1. System packages ────────────────────────────────────────────────────────
echo "==> Updating system packages..."
sudo apt-get update -qq
sudo apt-get upgrade -y -qq
sudo apt-get install -y -qq \
  build-essential git curl wget unzip \
  ca-certificates gnupg lsb-release \
  postgresql-common

# ── 2. PostgreSQL 17 + pgvector ────────────────────────────────────────────────
echo "==> Installing PostgreSQL 17..."
sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y
sudo apt-get install -y -qq postgresql-17 postgresql-server-dev-17

echo "==> Installing pgvector extension..."
cd /tmp
git clone --branch v0.8.0 https://github.com/pgvector/pgvector.git 2>/dev/null || true
cd pgvector
make
sudo make install
cd ~

echo "==> Configuring PostgreSQL..."
sudo -u postgres psql -c "CREATE USER multica WITH PASSWORD 'multica';" 2>/dev/null || true
sudo -u postgres psql -c "ALTER USER multica CREATEDB;" 2>/dev/null || true
sudo -u postgres psql -c "CREATE DATABASE multica OWNER multica;" 2>/dev/null || true
sudo -u postgres psql -d multica -c "CREATE EXTENSION IF NOT EXISTS vector;"

# Ensure local connections use md5 auth
PG_HBA=$(sudo -u postgres psql -t -c "SHOW hba_file;" | xargs)
if ! sudo grep -q "multica" "$PG_HBA"; then
  echo "local   multica   multica   md5" | sudo tee -a "$PG_HBA" > /dev/null
  echo "host    multica   multica   127.0.0.1/32   md5" | sudo tee -a "$PG_HBA" > /dev/null
  sudo systemctl restart postgresql
fi

echo "==> PostgreSQL ready."

# ── 3. Go ──────────────────────────────────────────────────────────────────────
if ! go version 2>/dev/null | grep -q "$GO_VERSION"; then
  echo "==> Installing Go $GO_VERSION..."
  ARCH=$(dpkg --print-architecture)  # arm64 on Oracle ARM
  wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -O /tmp/go.tar.gz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tar.gz
  rm /tmp/go.tar.gz

  # Add to PATH if not already there
  if ! grep -q '/usr/local/go/bin' ~/.bashrc; then
    echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc
  fi
  export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
fi
echo "==> Go: $(go version)"

# ── 4. Node.js + pnpm ─────────────────────────────────────────────────────────
if ! node --version 2>/dev/null | grep -q "v${NODE_MAJOR}"; then
  echo "==> Installing Node.js $NODE_MAJOR..."
  curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | sudo -E bash -
  sudo apt-get install -y -qq nodejs
fi
echo "==> Node: $(node --version)"

if ! command -v pnpm &>/dev/null; then
  echo "==> Installing pnpm..."
  sudo npm install -g pnpm
fi
echo "==> pnpm: $(pnpm --version)"

# ── 5. Cloudflared ─────────────────────────────────────────────────────────────
if ! command -v cloudflared &>/dev/null; then
  echo "==> Installing cloudflared..."
  ARCH=$(dpkg --print-architecture)
  wget -q "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${ARCH}.deb" -O /tmp/cloudflared.deb
  sudo dpkg -i /tmp/cloudflared.deb
  rm /tmp/cloudflared.deb
fi
echo "==> cloudflared: $(cloudflared --version)"

# ── 6. Clone & build Multica ──────────────────────────────────────────────────
if [ ! -d "$INSTALL_DIR" ]; then
  echo "==> Cloning Multica..."
  git clone "$REPO_URL" "$INSTALL_DIR"
fi
cd "$INSTALL_DIR"

echo "==> Installing frontend dependencies..."
pnpm install

echo "==> Building Go server..."
cd server && go build -o bin/server ./cmd/server && go build -o bin/migrate ./cmd/migrate
cd "$INSTALL_DIR"

echo "==> Running migrations..."
DATABASE_URL="postgres://multica:multica@localhost:5432/multica?sslmode=disable" \
  ./server/bin/migrate up

echo "==> Building frontend..."
NEXT_PUBLIC_API_URL="https://api.${DOMAIN:-localhost}" \
NEXT_PUBLIC_WS_URL="wss://api.${DOMAIN:-localhost}/ws" \
  pnpm build

# ── 7. Create .env ────────────────────────────────────────────────────────────
if [ ! -f "$INSTALL_DIR/.env" ]; then
  JWT_SECRET=$(openssl rand -hex 32)
  cat > "$INSTALL_DIR/.env" <<ENVEOF
# Database
POSTGRES_DB=multica
POSTGRES_USER=multica
POSTGRES_PASSWORD=multica
POSTGRES_PORT=5432
DATABASE_URL=postgres://multica:multica@localhost:5432/multica?sslmode=disable

# Server
PORT=8080
JWT_SECRET=${JWT_SECRET}
MULTICA_SERVER_URL=ws://localhost:8080/ws
MULTICA_APP_URL=https://app.${DOMAIN:-localhost}

# Google OAuth — fill these in
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
GOOGLE_REDIRECT_URI=https://app.${DOMAIN:-localhost}/auth/callback

# Frontend
FRONTEND_PORT=3000
FRONTEND_ORIGIN=https://app.${DOMAIN:-localhost}
NEXT_PUBLIC_API_URL=https://api.${DOMAIN:-localhost}
NEXT_PUBLIC_WS_URL=wss://api.${DOMAIN:-localhost}/ws
ENVEOF
  echo "==> Created .env — edit it to add Google OAuth credentials."
fi

# ── 8. Firewall ───────────────────────────────────────────────────────────────
# Oracle VM uses iptables by default. Open SSH only — all traffic goes through tunnel.
echo "==> Configuring firewall (iptables)..."
sudo iptables -L INPUT -n | grep -q "dpt:22" || {
  sudo iptables -I INPUT 1 -p tcp --dport 22 -j ACCEPT
  sudo iptables -A INPUT -i lo -j ACCEPT
  sudo iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT
  # No need to open 8080/3000 — traffic comes through cloudflared locally
}

# ── 9. Systemd services ──────────────────────────────────────────────────────
echo "==> Creating systemd services..."

# Multica backend
sudo tee /etc/systemd/system/multica-server.service > /dev/null <<EOF
[Unit]
Description=Multica API Server
After=postgresql.service
Requires=postgresql.service

[Service]
Type=simple
User=$USER
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$INSTALL_DIR/.env
ExecStart=$INSTALL_DIR/server/bin/server
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# Multica frontend
sudo tee /etc/systemd/system/multica-frontend.service > /dev/null <<EOF
[Unit]
Description=Multica Frontend (Next.js)
After=multica-server.service

[Service]
Type=simple
User=$USER
WorkingDirectory=$INSTALL_DIR/apps/web
EnvironmentFile=$INSTALL_DIR/.env
ExecStart=$(which pnpm) start --port 3000
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable multica-server multica-frontend

echo ""
echo "================================================"
echo "  Installation complete!"
echo "================================================"
echo ""
echo "Next steps:"
echo ""
echo "  1. Edit credentials:"
echo "     nano $INSTALL_DIR/.env"
echo "     (add GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET)"
echo ""
echo "  2. Set up Cloudflare Tunnel:"
echo "     cloudflared tunnel login"
echo "     cloudflared tunnel create multica"
echo "     # Then create the config (see below) and run:"
echo "     sudo cloudflared service install"
echo "     sudo systemctl enable --now cloudflared"
echo ""
echo "  3. Start Multica:"
echo "     sudo systemctl start multica-server"
echo "     sudo systemctl start multica-frontend"
echo ""
echo "  4. Check status:"
echo "     sudo systemctl status multica-server"
echo "     sudo systemctl status multica-frontend"
echo "     sudo systemctl status cloudflared"
echo ""
echo "  Cloudflare Tunnel config (~/.cloudflared/config.yml):"
echo ""
echo "    tunnel: <TUNNEL_ID>"
echo "    credentials-file: $HOME/.cloudflared/<TUNNEL_ID>.json"
echo "    ingress:"
echo "      - hostname: api.${DOMAIN:-yourdomain.com}"
echo "        service: http://localhost:8080"
echo "      - hostname: app.${DOMAIN:-yourdomain.com}"
echo "        service: http://localhost:3000"
echo "      - service: http_status:404"
echo ""
echo "  Then add DNS routes:"
echo "    cloudflared tunnel route dns multica api.${DOMAIN:-yourdomain.com}"
echo "    cloudflared tunnel route dns multica app.${DOMAIN:-yourdomain.com}"
