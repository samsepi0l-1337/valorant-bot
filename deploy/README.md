# Deploy

| Target       | One-shot script                          | Env template                 |
| ------------ | ---------------------------------------- | ---------------------------- |
| Local        | `./scripts/setup-local.sh`               | `env.local.example` → `.env` |
| Raspberry Pi | `./scripts/setup-pi.sh --host pi@…`      | `env.pi.example`             |
| Cloud / VPS  | `./scripts/setup-cloud.sh --host user@…` | `env.server.example`         |
| Docker       | `./scripts/setup-cloud.sh --docker`      | `.env.docker`                |

## Ports

`/auth` QR login needs **no inbound port**.

Password login opens Chrome on the **bot host** and serves the hCaptcha widget
over loopback TLS. It does not need an inbound port or public tunnel. A headless
Pi without a desktop browser should use Riot Mobile QR.

| Port               | Role                                      |
| ------------------ | ----------------------------------------- |
| Discord / Riot     | outbound only                             |
| `AUTH_PORT` (8787) | `/invite` + optional local helper pages    |
| 80                 | unused                                    |

### Raspberry Pi password login

```bash
# A desktop session and Chrome/Chromium are required.
sudo ./bin/valorant-bot
```

For a public `/invite` URL only, run
`./scripts/pi-tunnel.sh` and set `AUTH_BASE_URL` to the printed HTTPS URL.

## Files

- `install.sh` / `uninstall.sh` — systemd on Linux
- `valorant-bot.service` — unit file
- `nginx.example.conf` — optional reverse proxy
- `env.*.example` — environment templates
- `../scripts/pi-tunnel.sh` — optional Cloudflare quick tunnel for `/invite`

See the root [README](../README.md).
