# Sync: one assistant on several computers

Every computer (a *node*) keeps its own full database and works offline; a *hub* on the
office server passes the changes between them. This runbook sets the hub up on Ubuntu behind
a Cloudflare Tunnel and nginx, then connects the computers to it.

```
internet ─ HTTPS ─ Cloudflare ─ tunnel ─ cloudflared ─ nginx 127.0.0.1:8768 ─ hub 127.0.0.1:6768
                                         (office server; nothing listens publicly)
```

- Cloudflare terminates TLS; the tunnel is an outgoing connection, so the router needs no port
  forward, the office no static IP or DDNS, and this host no certbot.
- nginx passes only `/sync/*` to the hub and answers 404 to everything else. The hub's UI, API
  and MCP stay on `127.0.0.1:6767`.
- Every sync request is signed with the node's Ed25519 key (`X-Node`, `X-Timestamp`,
  `X-Signature`, covering the method, path and query, time and body); the hub accepts only keys
  authorized on it.

The examples use `assistant.petricomp.com`. Any other hostname works: change it in
`deploy/nginx-assistant.conf` (`server_name`), `deploy/cloudflared-ingress.yml` (`hostname`)
and the `--server` URL below.

The files: `deploy/assistant.service` (systemd), `deploy/nginx-assistant.conf`,
`deploy/cloudflared-ingress.yml`.

## 1. The hub on the office server

**Build.** The binary is pure Go, so build it on the Mac for the server (`GOARCH=arm64` for an
ARM server) and copy it over, together with `deploy/`:

```sh
GOOS=linux GOARCH=amd64 go build -o assistant-linux ./cmd/assistant
scp assistant-linux server:/tmp/assistant
scp -r deploy server:/tmp/assistant-deploy
```

(Or, with Go on the server: clone the repo there and `make build`.) On the server:

```sh
sudo install -m 755 /tmp/assistant /usr/local/bin/assistant
```

**User and config.** The hub runs as its own user, with everything in `/var/lib/assistant`:

```sh
sudo useradd --system --home-dir /var/lib/assistant --shell /usr/sbin/nologin assistant
sudo install -d -o assistant -g assistant -m 700 /var/lib/assistant
sudo -u assistant tee /var/lib/assistant/config.json <<'EOF'
{
  "db": "/var/lib/assistant/assistant.db",
  "workdir": "/var/lib/assistant",
  "sync_listen": "127.0.0.1:6768"
}
EOF
```

`sync_listen` makes this node the hub; it must stay a loopback address (nginx is its only
client). The hub never sets `sync_server`.

**systemd.**

```sh
sudo cp /tmp/assistant-deploy/assistant.service /etc/systemd/system/assistant.service
sudo systemctl daemon-reload
sudo systemctl enable --now assistant
journalctl -u assistant -f    # "sync hub listening on http://127.0.0.1:6768"
```

**nginx.**

```sh
sudo cp /tmp/assistant-deploy/nginx-assistant.conf /etc/nginx/sites-available/assistant
sudo ln -s /etc/nginx/sites-available/assistant /etc/nginx/sites-enabled/assistant
sudo nginx -t && sudo systemctl reload nginx
```

The file starts with a `limit_req_zone` line, which belongs in the `http {}` block. Ubuntu's
`nginx.conf` includes `sites-enabled/*` inside `http {}`, so it works where it is; if your
nginx includes the file elsewhere, move that line into `http {}`. The limit (10 requests/s,
bursts of 20) is keyed on `CF-Connecting-IP`, since every request reaches nginx from
`127.0.0.1`.

**cloudflared.** Add the rule from `deploy/cloudflared-ingress.yml` to the `ingress:` list of
the tunnel's config (e.g. `/etc/cloudflared/config.yml`), before the catch-all
`- service: http_status:404`, which stays last. Then create the DNS record and restart:

```sh
cloudflared tunnel list                                   # the tunnel's name
cloudflared tunnel route dns <tunnel> assistant.petricomp.com
cloudflared tunnel ingress validate
sudo systemctl restart cloudflared
```

(A tunnel managed from the Cloudflare dashboard instead: add a *Public hostname*
`assistant.petricomp.com` with service `http://127.0.0.1:8768` there.)

**Check.** From any computer:

```sh
curl -i https://assistant.petricomp.com/sync/hello   # 401: the hub answers, unsigned is refused
curl -i https://assistant.petricomp.com/             # 404: nginx serves nothing else
```

A 502 means cloudflared can't reach nginx or nginx can't reach the hub
(`systemctl status assistant`, `/var/log/nginx/error.log`); a 404 on `/sync/hello` means the
request didn't reach this server block (hostname or port in the tunnel rule). If Cloudflare's
*Bot Fight Mode* or a WAF rule answers the app with a challenge page, skip it for
`assistant.petricomp.com/sync/*`.

The hub's own UI is at `127.0.0.1:6767` on the server: `ssh -L 6767:127.0.0.1:6767 server`
and open http://127.0.0.1:6767.

**Authorizing a key** (steps 2 and 3) is done on the server, as the hub's user:

```sh
sudo -u assistant /usr/local/bin/assistant -config /var/lib/assistant/config.json sync authorize PUBKEY --name NAME
```

The hub need not be restarted for that. **Upgrading the hub:** install the new binary as
above, then `sudo systemctl restart assistant`; the database is kept.

## 2. The main computer (with your data)

This computer's database becomes everyone's. On the Mac, in the repo:

```sh
make backup
./assistant sync init --server https://assistant.petricomp.com
```

`sync init` creates the key `~/.assistant/sync_ed25519`, sets `sync_server` in
`~/.assistant/config.json` and prints the public key. On the hub:

```sh
sudo -u assistant /usr/local/bin/assistant -config /var/lib/assistant/config.json sync authorize PUBKEY --name laptop
```

Back on the Mac, send all of the database to the blank hub, then restart the app to keep
syncing:

```sh
make stop
./assistant sync seed
make start
```

`sync seed` backs the database up again before it starts, and a seed cut short (offline, say)
goes on where it stopped when run again. It refuses a hub that already has data.

## 3. Another computer

Build the app (`make build`) on it as usual. With the app stopped (`make stop`) and an empty
database (a fresh install, or one where nothing was created yet):

```sh
./assistant sync init --server https://assistant.petricomp.com
# on the hub: sync authorize PUBKEY --name desktop (as in step 1)
./assistant sync join
make start
```

`sync join` downloads the hub's snapshot and catches up with what changed since. It refuses a
database with data of its own; to start over on such a computer, `make reset` first (it backs
the database up, wipes it and starts the app; `make stop` again before `sync join`). Every node runs as before on the Mac
(`make start`, `make restart`); the hub is the only one under systemd.

Workdirs are per computer: set them again under *Settings* and in projects where this
computer's paths differ. A session belongs to the computer it runs on and is read-only on the
others.

## 4. Revoking a key

On the hub:

```sh
sudo -u assistant /usr/local/bin/assistant -config /var/lib/assistant/config.json sync keys
sudo -u assistant /usr/local/bin/assistant -config /var/lib/assistant/config.json sync revoke laptop
```

`sync keys` lists every key with its node, name and state; `sync revoke` takes the name or the
public key. The hub refuses that key's requests from then on (401), immediately. What the node
pushed before stays. To bring the computer back, `sync init` it again with a new key
(delete `~/.assistant/sync_ed25519` first) and authorize that.

## 5. When the office has no internet

Nothing stops: every node keeps working on its own database. Local changes wait in the
outbox, and the node retries the hub with backoff (up to a minute apart).
When the hub is reachable again each node pushes what waited and pulls what the others did,
with no intervention. Edits to the same row on two computers resolve to the later one; a
delete wins over an edit.

The same holds for a laptop on a plane or a hub restart.

## 6. Checklist after the deploy

- [ ] `curl -i https://assistant.petricomp.com/sync/hello` is 401, `curl -i https://assistant.petricomp.com/` is 404.
- [ ] `sync keys` on the hub lists every computer, none revoked.
- [ ] Each computer's log (`~/.assistant/assistant.log`) has
      `syncing with https://assistant.petricomp.com as ...` and no sync errors after it.
- [ ] A task created or moved on the laptop shows on the other computer within a few seconds,
      and the other way round.
- [ ] Offline: turn the internet off (or `sudo systemctl stop cloudflared` on the server), work
      on both computers (create, edit, move different tasks; edit the same task on both), turn
      it back on: within a minute both computers show the same board, the same task with the
      later edit.
- [ ] `sync revoke` on a spare key makes that node's sync fail with 401.
