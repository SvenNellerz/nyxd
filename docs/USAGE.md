# Using nyxd and nyx

## nyx CLI (recommended)

Install **`nyx`** next to **`nyxd`** (`make build-nyx`). It talks to the daemon over the Unix socket (default `/run/nyxd/nyxd.sock`, override with `-socket` or `NYXD_SOCKET`).

```bash
nyx ping
nyx version

# Pull (progress UI on stderr; add --json for a single JSON blob on stdout)
sudo nyx pull nginx:alpine

# Run — waits; Ctrl+C stops the container (POST /v1/containers/{id}/stop)
sudo nyx run nginx:alpine

# Detach immediately after start (fire-and-forget)
sudo nyx run -d nginx:alpine

# Named container
sudo nyx run --name web nginx:alpine

# Stop from another shell
sudo nyx stop web

# Exec
sudo nyx exec <id> -- sh -c 'hostname'
```

## Raw HTTP with curl

See **[openapi.yaml](openapi.yaml)** for schemas. Socket example:

```bash
SOCK=/run/nyxd/nyxd.sock

curl -sS --unix-socket "$SOCK" http://localhost/v1/ping
curl -sS --unix-socket "$SOCK" http://localhost/v1/version
curl -sS --unix-socket "$SOCK" http://localhost/v1/containers

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"ref":"nginx:alpine","stream":false}' \
  http://localhost/v1/images/pull

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"image":"nginx:alpine"}' \
  http://localhost/v1/containers/run

curl -sS --unix-socket "$SOCK" -X POST http://localhost/v1/containers/<id>/stop

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"argv":["sh","-c","uname -a"]}' \
  http://localhost/v1/containers/<id>/exec
```

## OpenAPI

- **Spec:** [openapi.yaml](openapi.yaml)  
- **Viewer:** paste the file into [Swagger Editor](https://editor.swagger.io/) or use `redocly preview-docs`.

## Related docs

- [INSTALL.md](INSTALL.md) — Raspberry Pi lab install  
- [networking.md](networking.md) — networking flags and behavior  
- [kernel-requirements.md](kernel-requirements.md) — modules and sysctl  
