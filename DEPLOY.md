# Deploy on VDS (Ubuntu/Debian) with Caddy + HTTPS

## Requirements

- VDS with Ubuntu 22.04+
- Domain pointed to the VDS IP (A-record)
- Ports 80 and 443 open in firewall

---

## 1. Install Caddy

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install caddy
```

## 2. Deploy the app

Репо приватное — собираем бинарь локально и копируем на сервер по scp.

### 2a. Локально (на своей машине)

```bash
# Собрать бинарь под Linux amd64
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o redis-checker ./cmd/redis-checker

# Скопировать бинарь и service-файл на сервер
scp redis-checker root@YOUR_VDS_IP:/tmp/redis-checker
scp redis-checker.service root@YOUR_VDS_IP:/tmp/redis-checker.service
scp Caddyfile root@YOUR_VDS_IP:/tmp/Caddyfile
```

### 2b. На сервере

```bash
# Установить xray (нужен для проверки конфигов)
apt install -y unzip wget
wget -q -O /tmp/xray.zip \
    https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip
unzip /tmp/xray.zip -d /usr/local/bin/ xray
chmod +x /usr/local/bin/xray
rm /tmp/xray.zip

# Разложить файлы
mkdir -p /opt/vpn_checker
mv /tmp/redis-checker /opt/vpn_checker/redis-checker
chmod +x /opt/vpn_checker/redis-checker

# Установить и настроить service
mv /tmp/redis-checker.service /etc/systemd/system/redis-checker.service

# Вставить реальный REDIS_URL:
nano /etc/systemd/system/redis-checker.service

systemctl daemon-reload
systemctl enable --now redis-checker
```

## 3. Configure Caddy

```bash
sudo cp Caddyfile /etc/caddy/Caddyfile

# Replace the placeholder domain with your real domain:
sudo nano /etc/caddy/Caddyfile

sudo systemctl reload caddy
```

Caddy automatically obtains and renews a Let's Encrypt TLS certificate.

## 4. Verify

```bash
systemctl status redis-checker
systemctl status caddy
journalctl -u redis-checker -f
```

Your endpoints:
- `https://your.domain/` — live dashboard
- `https://your.domain/configs` — plain-text config list
- `https://your.domain/events` — SSE stream

---

## Firewall (ufw)

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
# Do NOT expose 8081 externally — it's localhost-only
```

## Update app binary

```bash
# Локально
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o redis-checker ./cmd/redis-checker
scp redis-checker root@YOUR_VDS_IP:/tmp/redis-checker

# На сервере
systemctl stop redis-checker
mv /tmp/redis-checker /opt/vpn_checker/redis-checker
chmod +x /opt/vpn_checker/redis-checker
systemctl start redis-checker
```

# Локально
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o redis-checker ./cmd/redis-checker
scp redis-checker root@95.140.148.197:/tmp/redis-checker

# На сервере
systemctl stop redis-checker
mv /tmp/redis-checker /opt/vpn_checker/redis-checker
chmod +x /opt/vpn_checker/redis-checker
systemctl start redis-checker