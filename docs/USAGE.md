# Using nyxd and nyx

## nyx CLI (recommended)

Install **`nyx`** next to **`nyxd`** (`make build-nyx`). It talks to the daemon over the Unix socket (default `/run/nyxd/nyxd.sock`, override with `-socket` or `NYXD_SOCKET`).

```bash
nyx ping
nyx version

# Pull (progress UI on stderr; add --json for a single JSON blob on stdout)
sudo nyx pull nginx:alpine

# Run — foreground streams container logs; Ctrl+C sends SIGKILL (POST /v1/containers/{id}/kill)
sudo nyx run nginx:alpine

# Detach immediately after start (fire-and-forget)
sudo nyx run -d nginx:alpine

# Named container
sudo nyx run --name web nginx:alpine

# Docker-style flags: publish, env, hostname, restart
sudo nyx run -p 8080:80 -e MYVAR=1 --hostname web --restart no nginx:alpine

# Logs (optional follow; tail count)
sudo nyx logs web
sudo nyx logs -f --tail 100 web
sudo nyx container logs web -f

# List / remove (docker-like)
sudo nyx ps
sudo nyx ps -q
sudo nyx stop web other-id
sudo nyx rm web

# Images (local refs pulled into the store)
sudo nyx image ls
sudo nyx image rm nginx:alpine

# Stop from another shell
sudo nyx stop web

# Exec (flags before id like docker; `--` optional)
sudo nyx exec <id> sh -c 'hostname'
sudo nyx exec <id> -- sh -c 'hostname'
```

## Raw HTTP with curl

See **[openapi.yaml](openapi.yaml)** for schemas. Socket example:

```bash
SOCK=/run/nyxd/nyxd.sock

curl -sS --unix-socket "$SOCK" http://localhost/v1/ping
curl -sS --unix-socket "$SOCK" http://localhost/v1/version
curl -sS --unix-socket "$SOCK" http://localhost/v1/containers
curl -sS --unix-socket "$SOCK" 'http://localhost/v1/containers?detail=1'

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"ref":"nginx:alpine","stream":false}' \
  http://localhost/v1/images/pull

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"image":"nginx:alpine","publish":["8080:80"]}' \
  http://localhost/v1/containers/run

curl -sS --unix-socket "$SOCK" -X POST http://localhost/v1/containers/<id>/stop

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"signal":"KILL"}' \
  http://localhost/v1/containers/<id>/kill

curl -sS --unix-socket "$SOCK" 'http://localhost/v1/containers/<id>/logs?tail=200&plain=1'

curl -sS --unix-socket "$SOCK" http://localhost/v1/images

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"ref":"nginx:alpine"}' \
  http://localhost/v1/images/remove

curl -sS --unix-socket "$SOCK" -X POST http://localhost/v1/containers/<id>/remove

curl -sS --unix-socket "$SOCK" -H 'Content-Type: application/json' \
  -d '{"argv":["sh","-c","uname -a"]}' \
  http://localhost/v1/containers/<id>/exec
```

## OpenAPI

- **Spec:** [openapi.yaml](openapi.yaml)  
- **Viewer:** paste the file into [Swagger Editor](https://editor.swagger.io/) or use `redocly preview-docs`.

## Related docs

- [INSTALL.md](INSTALL.md) — Linux install  
- [networking.md](networking.md) — networking flags and behavior  
- [kernel-requirements.md](kernel-requirements.md) — modules and sysctl  
