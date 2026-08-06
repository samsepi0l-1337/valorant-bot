# Deploy

| Target | One-shot script | Env template |
| ------ | --------------- | ------------ |
| Local | `./scripts/setup-local.sh` | `env.local.example` → `.env` |
| Raspberry Pi | `./scripts/setup-pi.sh --host pi@…` | `env.pi.example` |
| Cloud / VPS | `./scripts/setup-cloud.sh --host user@…` | `env.server.example` |
| Docker | `./scripts/setup-cloud.sh --docker` | `.env.docker` |

## Ports

`/auth` is Riot Mobile QR. The bot host needs **no inbound port** for login.

| Port | Role |
| ---- | ---- |
| Discord / Riot | outbound only |
| `AUTH_PORT` (8787) | optional `/invite` page |
| 80 | unused |

## Files

- `install.sh` / `uninstall.sh` — systemd on Linux
- `valorant-bot.service` — unit file
- `nginx.example.conf` — optional reverse proxy for `/invite`
- `env.*.example` — environment templates

See the root [README](../README.md).
