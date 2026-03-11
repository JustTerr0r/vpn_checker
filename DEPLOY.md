# Deploy — Ubuntu 22.04 + PM2 + Caddy

## Требования

- Ubuntu 22.04 VPS
- Домен, направленный на IP сервера (A-запись)
- Открытые порты 80 и 443
- Внешний Redis (например Upstash, Redis Cloud или свой)

---

## Шаг 1 — Собери бинарь локально

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o redis-checker ./cmd/redis-checker

scp redis-checker ecosystem.config.js Caddyfile user@YOUR_SERVER_IP:/tmp/
```

---

## Шаг 2 — На сервере: установи зависимости

### Node.js + PM2

```bash
curl -fsSL https://deb.nodesource.com/setup_lts.x | sudo -E bash -
sudo apt install -y nodejs
sudo npm install -g pm2
```

### xray (нужен для проверки VPN-конфигов)

```bash
sudo apt install -y unzip wget
wget -q -O /tmp/xray.zip \
    https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip
sudo unzip /tmp/xray.zip -d /usr/local/bin/ xray
sudo chmod +x /usr/local/bin/xray
rm /tmp/xray.zip
```

### Caddy (HTTPS reverse proxy)

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy
```

---

## Шаг 3 — Разложи файлы

```bash
sudo mkdir -p /opt/vpn_checker /var/log/vpn_checker

sudo mv /tmp/redis-checker /opt/vpn_checker/redis-checker
sudo chmod +x /opt/vpn_checker/redis-checker

sudo mv /tmp/ecosystem.config.js /opt/vpn_checker/ecosystem.config.js
```

---

## Шаг 4 — Настрой ecosystem.config.js

```bash
sudo nano /opt/vpn_checker/ecosystem.config.js
```

Заполни:
- `REDIS_URL` — DSN твоего Redis, например `redis://default:PASSWORD@HOST:6379`
- `PUBLIC_HOST` — твой домен, например `vpn.example.com`
- `args` — при необходимости измени флаги (`-workers`, `-timeout`, `-recheck-interval` и т.д.)

---

## Шаг 5 — Запусти через PM2

```bash
cd /opt/vpn_checker
pm2 start ecosystem.config.js

# Сохранить процессы (автозапуск после перезагрузки)
pm2 save

# Настроить автозапуск PM2 — выполни команду которую выдаст эта команда:
pm2 startup
```

Проверь что сервис работает:

```bash
pm2 status
pm2 logs redis-checker
curl http://localhost:8081/
```

---

## Шаг 6 — Настрой Caddy (HTTPS)

```bash
sudo cp /tmp/Caddyfile /etc/caddy/Caddyfile
sudo nano /etc/caddy/Caddyfile
# Замени configs.example.com на свой домен
```

```
vpn.example.com {
    reverse_proxy localhost:8081
}
```

```bash
sudo systemctl reload caddy
```

Caddy автоматически получит и будет обновлять Let's Encrypt сертификат.

---

## Firewall

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
# Порт 8081 НЕ открывать — он только для localhost
```

---

## Готово

- Дашборд: `https://your.domain/`
- Конфиги (подписка): `https://your.domain/configs`
- SSE stream: `https://your.domain/events`

Открой дашборд → запусти **Grabber** (вставь URL с VPN-конфигами) → запусти **Raw Checker** → включи **Recheck** с нужным интервалом.

---

## Обновление бинаря

```bash
# Локально
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
    -o redis-checker ./cmd/redis-checker
scp redis-checker root@95.140.148.197:/tmp/

# На сервере
pm2 stop redis-checker
sudo mv /tmp/redis-checker /opt/vpn_checker/redis-checker
sudo chmod +x /opt/vpn_checker/redis-checker
pm2 start redis-checker
```

---

## PM2 — полезные команды

```bash
pm2 status                    # статус всех процессов
pm2 logs redis-checker        # live логи
pm2 logs redis-checker --lines 200  # последние 200 строк
pm2 restart redis-checker     # перезапустить
pm2 stop redis-checker        # остановить
pm2 delete redis-checker      # удалить из PM2
pm2 monit                     # интерактивный мониторинг CPU/RAM
```

---

## Альтернатива: systemd

Если предпочитаешь systemd — используй `redis-checker.service`:

```bash
sudo mv redis-checker.service /etc/systemd/system/
sudo nano /etc/systemd/system/redis-checker.service  # вставь REDIS_URL
sudo systemctl daemon-reload
sudo systemctl enable --now redis-checker
sudo journalctl -u redis-checker -f
```
