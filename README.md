# WittyRelay

A relay for [WittyMediaServer](https://github.com/wittywilla/WittyMediaServer)'s peer-to-peer connections, that you can run yourself in Docker.

Most of the time an app away from home connects **straight** to its server: the two find each other and "punch a hole" through both routers. Sometimes that can't work: both ends behind strict routers, an internet provider that shares one address between customers on each side, a school or office network that lets no UDP out. Then the connection needs something in the middle that both ends can reach. That's a relay.

WittyRelay is that relay. **It can't see what it carries.** Everything is encrypted between the app and the server with keys only they hold; the relay passes on bytes it can't read. It sees which server and which device are talking, when, and how much.

The hosted WittyRelay is planned as a **free** service. Until then, and whenever you'd rather, run your own.

## Contents

- [What you need](#what-you-need)
- [Run it with Docker Compose](#run-it-with-docker-compose)
- [Point your server at it](#point-your-server-at-it)
- [Limits, and who sees them](#limits-and-who-sees-them)
- [Settings](#settings)
- [Health and metrics](#health-and-metrics)
- [How it works](#how-it-works)
- [Security](#security)
- [Building](#building)
- [Licence](#licence)

## What you need

- A machine **both ends can reach**: a small rented server (VPS), or a computer at a place with a public IP address and a forwarded port. A relay at the same home as the server doesn't help apps reach that server.
- Docker with Compose.
- Port **4001**, UDP and TCP, open to the internet.
- A name or public IP address for it, like `relay.example.com`.

It's small: about 15 MB of memory idle, a 41 MB image, and CPU only for the bytes it passes on. Upload bandwidth is what it really uses.

## Run it with Docker Compose

1. Copy [`docker-compose.yml`](docker-compose.yml) to the machine.
2. Make an access key, and put it in `WITTYRELAY_KEYS`:

   ```bash
   docker compose run --rm wittyrelay key
   ```

3. Set `WITTYRELAY_ANNOUNCE` to how the internet reaches the machine, for example `/dns4/relay.example.com/udp/4001/quic-v1,/dns4/relay.example.com/tcp/4001` (or `/ip4/203.0.113.7/...` for an IP address).
4. Start it and read its address:

   ```bash
   docker compose up -d
   docker compose logs wittyrelay | grep "relay address"
   ```

   The address ends in `/p2p/12D3KooW…`: that last part is the relay's ID, made the first time it starts and kept in the `wittyrelay-data` volume. Servers check it on every connection, so keep the volume.

Without Compose:

```bash
docker run -d --name wittyrelay --restart unless-stopped \
  -p 4001:4001/udp -p 4001:4001/tcp -p 127.0.0.1:8080:8080 \
  -v wittyrelay-data:/data \
  -e WITTYRELAY_ANNOUNCE=/dns4/relay.example.com/udp/4001/quic-v1,/dns4/relay.example.com/tcp/4001 \
  -e WITTYRELAY_KEYS=<your key> \
  ghcr.io/wittywilla/wittyrelay:latest
```

## Point your server at it

On the server: **Settings → Remote Access → Peer-to-peer connections → Your own WittyRelay**. Paste the relay's address and the access key, choose **Check**, and read what the relay allows. Then **Use this relay**. The server registers, tells the relay which of its devices may come through, and renews that every five minutes.

Apps need nothing: they use the relay by themselves whenever a direct connection can't be made, and show that they are.

## Limits, and who sees them

Every limit is told to the server **before it uses the relay** (Settings shows them under the address, in words), and to the app when it's connected through it.

| Limit | Default | Enforced by |
|---|---|---|
| Speed per connection, each way | 8 Mbit/s | The relay |
| Speed for everyone together, each way | 50 Mbit/s | The relay |
| Picture size streams keep to | 1080p | The server: the relay can't see inside what it carries, so the server converts what it sends through the relay to that size or smaller. The speed limit holds a server that doesn't |
| Data per connection | 4 GiB | The relay; the app starts a new connection |
| Time per connection | 3 hours | The relay; the app starts a new connection |
| Servers at once | 32 | The relay |
| Connections per server | 8 | The relay |
| Servers from one IP address | 4 | The relay |
| Registrations per minute, per address | 10 | The relay |

## Settings

From `docker-compose.yml`'s `environment`, or a JSON file (see [`deploy/wittyrelay.example.json`](deploy/wittyrelay.example.json)) mounted at `/config/wittyrelay.json` or named by `WITTYRELAY_CONFIG`. The environment wins over the file.

| Variable | JSON | Default | What |
|---|---|---|---|
| `WITTYRELAY_NAME`, `WITTYRELAY_NOTE` | `name`, `note` | WittyRelay | Shown to server owners: who runs it, how to reach you |
| `WITTYRELAY_ANNOUNCE` | `announce` | what the machine finds | Public addresses, comma-separated multiaddrs |
| `WITTYRELAY_PORT` | `port` | 4001 | UDP and TCP |
| `WITTYRELAY_LISTEN` | `listen` | every interface on the port | Replaces what is listened on |
| `WITTYRELAY_ACCESS` | `access` | key | `key`: servers need a key. `open`: any server may register |
| `WITTYRELAY_KEYS` | `keys` | none | Access keys, comma-separated, 16 characters or more |
| `WITTYRELAY_KEYS_FILE` | | none | A file of access keys, one a line (or comma-separated), instead: for Docker secrets |
| `WITTYRELAY_KBPS`, `WITTYRELAY_TOTAL_KBPS` | `limits.kbps`, `limits.totalKbps` | 8000, 50000 | Speed limits in kbit/s (0: none) |
| `WITTYRELAY_MAX_HEIGHT` | `limits.maxHeight` | 1080 | Picture height (0: any) |
| `WITTYRELAY_DATA_MIB`, `WITTYRELAY_MINUTES` | `limits.dataMiB`, `limits.minutes` | 4096, 180 | Per connection (0: none) |
| `WITTYRELAY_SERVERS`, `WITTYRELAY_CIRCUITS`, `WITTYRELAY_PER_IP` | `limits.servers`, `.circuits`, `.perIP` | 32, 8, 4 | |
| `WITTYRELAY_REGISTER_PER_MINUTE`, `WITTYRELAY_DEVICES_PER_SERVER` | | 10, 512 | |
| `WITTYRELAY_MEMORY_MIB` | `limits.memoryMiB` | 192 | Memory for connections and streams: a little under the container's limit |
| `WITTYRELAY_REGISTER_TTL` | `registerTTL` | 15m | How long a registration holds unless renewed |
| `WITTYRELAY_HTTP` | `http` | :8080 | Health and metrics ("" for none) |
| `WITTYRELAY_METRICS_TOKEN` | `metricsToken` | none | Bearer token for `/metrics` |
| `WITTYRELAY_METRICS_TOKEN_FILE` | | none | The same, from a file |
| `WITTYRELAY_ALLOW_PRIVATE` | `allowPrivate` | false | Lets servers on private networks register: for a relay on a LAN, or tests |
| `WITTYRELAY_DATA_DIR` | `dataDir` | /data | Where the relay's key is kept |
| `WITTYRELAY_LOG_LEVEL` | `logLevel` | info | Logs are JSON lines on standard output |

A configuration that can't work (a `key` relay with no keys, a speed that isn't a number) stops it at start with the reason.

## Health and metrics

On port 8080, which the Compose file opens to the machine itself only:

| Path | What |
|---|---|
| `/healthz` | 200 while it runs |
| `/readyz` | 200 once it listens; Docker's health check |
| `/info` | The relay's ID, addresses and limits (nothing secret) |
| `/metrics` | Prometheus: servers, places held, connections open and total, bytes passed on, refusals by reason |

## How it works

WittyMediaServer and its apps connect with [libp2p](https://libp2p.io). A relay there is libp2p's **circuit relay v2**: a server holds a place on the relay; an app connects to the relay and asks for that server; the relay joins the two. The connection inside is encrypted from the app to the server (TLS 1.3 over QUIC, or Noise over TCP), to the server's key, which the app checks.

WittyRelay is that relay with what a relay for other people needs:

- **Registration** (`/wittyrelay/register/1.0.0`). A server proves who it is (libp2p does that on every connection), shows an access key if the relay needs one, and lists the devices that may reach it. Only a registered server holds a place, and only a listed device reaches it. The list is the server's word: renewed every five minutes, forgotten fifteen minutes after it was last heard, so a device removed on the server loses the relay too.
- **Limits said first** (`/wittymedia/relay-info/1.0.0`). Any node can ask what the relay allows, and a registration's answer carries the same.
- **Speed limits** per connection and in all, on top of libp2p's limits on time and data.

Why circuit relay v2 and not TURN: both ends already speak it, it already carries the encrypted libp2p connection unchanged, and the apps already switch from a relay to a direct connection the moment one can be made. TURN would mean a second way of connecting in every app, with its own credentials, for the same result.

## Security

- **It can't read what it carries**, and can't pretend to be a server: a connection to the wrong key fails.
- Access keys are compared in constant time and never logged. A key is a password: give each server owner their own, and take a key out of `WITTYRELAY_KEYS` to cut that owner off. Anyone who can inspect the container sees its environment: `WITTYRELAY_KEYS_FILE` with a Docker secret keeps keys out of it.
- What it may use is fixed by its configuration, not by the machine: memory (`WITTYRELAY_MEMORY_MIB`), connections and file descriptors for the servers and circuits it allows and a margin, at most one address's share at a time (`WITTYRELAY_PER_IP` servers, each with its circuits: 36 by default; an IPv6 /56 counts as one address), and a pace for new ones. A connection that hasn't said who it is yet counts against a smaller share of its own.
- Every message is size-checked before it is read (64 kB for a registration); registrations are rate-limited per address and per server; the tables that count them are bounded.
- The image is distroless, runs as user 65532 with no shell, and the Compose file adds a read-only root, no capabilities and memory and process limits.
- What the relay sees: both ends' IP addresses, when, and how much. Its logs keep the end of each server's ID and its IP address on registration.

## Building

```bash
go test ./...
go build ./cmd/wittyrelay
docker build -t wittyrelay .
docker buildx build --platform linux/amd64,linux/arm64 -t wittyrelay .
```

Go 1.27. CI runs the tests with the race detector, a minute of fuzzing, `govulncheck`, and builds and starts the image for both platforms.

**Working on a Mac?** Start with WittyMediaServer's [MAC-START-HERE.md](https://github.com/wittywilla/WittyMediaServer/blob/v0.8.1-alpha/MAC-START-HERE.md): what to install, what to clone, and every Mac task in order: this repository's is item 7.9.

## Licence

GNU Affero General Public License, version 3 or later: see [LICENSE](LICENSE) and [NOTICE](NOTICE). If you run a changed WittyRelay for others, the AGPL asks you to offer them its source.
