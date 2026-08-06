# Deploy layouts

| Target | Env template | Binary | Install |
| ------ | ------------ | ------ | ------- |
| Local | `env.local.example` → `.env` | `make build` / `make run` | — |
| Raspberry Pi | `env.pi.example` → `/etc/valorant-bot/env` | `make build-pi` | `sudo ./install.sh` |
| Server (systemd) | `env.server.example` | `make build-linux` | `sudo ./install.sh` |
| Server (Docker) | `env.server.example` → `.env.docker` | image build | `docker compose up -d` |

## Files

- `install.sh` / `uninstall.sh` — systemd install on Linux
- `valorant-bot.service` — unit file
- `nginx.example.conf` — optional reverse proxy
- `env.*.example` — environment templates

See the root [README](../README.md) for full steps.
