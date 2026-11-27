# revive-dns

A small Go DNS server for game revivals. Point a console or emulator at it to
resolve old game hostnames to your replacement servers.

## Get started

You’ll need Docker with Compose and Linux host networking.

```sh
git clone https://github.com/dead-game-entrepreneurs/revive-dns.git
cd revive-dns
cp .env.example .env
cp config.example.yaml config.yaml
```

1. In `.env`, set `DNS_LISTEN_IP` to the LAN IP of the machine running
   revive-dns. Set `DNS_ALLOW_CIDR` to the subnet your consoles or emulators
   connect from.
2. In `config.yaml`, replace the example names and address with your own (see below).
3. Build and start the resolver.

   ```sh
   docker compose up -d --build
   docker compose logs dns
   ```

Set your console or emulator’s DNS server to `DNS_LISTEN_IP`. The resolver
listens on UDP and TCP port 53, which must be free on that address. The
`.env` and `config.yaml` files are local to your install.

## Add a game

Add another list item under `rules:` in `config.yaml`. Use the hostnames the
game actually looks up and an IPv4 address it can reach.

```yaml
  - service: my-game
    names:
      - login.example.com
      - "*.lobby.example.com"
    answer: "192.0.2.10"
```

- `service` is the label shown in logs.
- `names` lists exact hostnames or quoted wildcard names. A wildcard also
  matches its base domain. Exact names win, then the most specific wildcard.
- `answer` returns an IPv4 address for A queries. Other record types get an
  empty reply.

To send matching queries to another resolver, use `forward` in place of
`answer`. The forwarding port defaults to 53. Unmatched queries go to
`DNS_UPSTREAM`. Leave it blank to refuse unmatched queries, and keep the allow
list scoped to your clients when using an upstream.

```yaml
  - service: my-forwarder
    names:
      - "*.auth.example.com"
    forward: "192.0.2.54"
```

Validate the file and apply the changes from the repo directory.

```sh
docker compose run --rm --build dns -check
docker compose up -d --force-recreate dns
```

From a machine on the allowed subnet, query the resolver directly. Replace the
example IP and name with your listener IP and one of your configured hostnames.

```sh
dig @192.0.2.53 login.example.com A +short
```

## Development

Run the tests with `scripts/go test ./...` using the pinned Docker toolchain,
or with `go test ./...` if you have Go installed.

## License

[GNU General Public License v3.0](LICENSE) (GPL-3.0-only).
