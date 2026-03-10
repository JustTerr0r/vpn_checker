module.exports = {
  apps: [
    {
      name: 'redis-checker',
      script: '/opt/vpn_checker/redis-checker',
      interpreter: 'none',
      args: '-recheck -workers 10 -timeout 15s -serve 127.0.0.1:8081',
      cwd: '/opt/vpn_checker',
      env: {
        REDIS_URL: 'redis://default:PASSWORD@HOST:PORT',
        PUBLIC_HOST: 'your.domain.com',
      },
      autorestart: true,
      restart_delay: 5000,
      max_restarts: 10,
      log_date_format: 'YYYY-MM-DD HH:mm:ss',
      error_file: '/var/log/vpn_checker/redis-checker-error.log',
      out_file: '/var/log/vpn_checker/redis-checker-out.log',
    },
  ],
}
