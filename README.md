# external-dns-connbyte-webhook

[ExternalDNS](https://github.com/kubernetes-sigs/external-dns) webhook provider for [ConnByte](https://connbyte.com/) DNS.

It lets ExternalDNS create, update and delete DNS records in your ConnByte zones for Kubernetes Ingresses and Services.
It uses the ConnByte DNS API (`https://api.connbyte.com`) with an API token.

Supported record types: `A`, `AAAA`, `CNAME`, `MX`, `NS`, `SRV`, `TXT`.

## Requirements

- ExternalDNS v0.14.0 or later (webhook provider).
- A ConnByte account with at least one DNS zone and an API token. See [connbyte.com](https://connbyte.com/).

## Quick start

1. Create a secret with your ConnByte API token:

   ```sh
   kubectl create secret generic connbyte-api-token --from-literal=CONNBYTE_API_TOKEN=<your-token>
   ```

2. Deploy ExternalDNS with the webhook as a sidecar. Edit `--domain-filter` first:

   ```sh
   kubectl apply -f example/external-dns.yaml
   ```

   The webhook listens on `127.0.0.1:8888` (only ExternalDNS in the same pod can reach it) and serves `/healthz` on port `8080`.

### Helm (external-dns chart)

```yaml
provider:
  name: webhook
  webhook:
    image:
      repository: ghcr.io/steffenbarthel/external-dns-connbyte-webhook
      tag: latest
    args:
      - --domain-filter=example.com
    env:
      - name: CONNBYTE_API_TOKEN
        valueFrom:
          secretKeyRef:
            name: connbyte-api-token
            key: CONNBYTE_API_TOKEN
    livenessProbe:
      httpGet:
        path: /healthz
        port: http-webhook
    readinessProbe:
      httpGet:
        path: /healthz
        port: http-webhook
```

## Configuration

Every flag can also be set with the environment variable in brackets.

| Flag | Default | Description |
| --- | --- | --- |
| (`CONNBYTE_API_TOKEN`) | – | ConnByte API token. Required, unless `--connbyte-api-token-file` is set. Only accepted as environment variable, so it does not show up in the process list. |
| `--connbyte-api-token-file` (`CONNBYTE_API_TOKEN_FILE`) | – | Read the token from a file, for example a mounted secret. |
| `--connbyte-api-url` (`CONNBYTE_API_URL`) | `https://api.connbyte.com` | API base URL. |
| `--domain-filter` (`DOMAIN_FILTER`) | all zones | Comma separated list of domains to manage. Records outside these domains are never changed. |
| `--exclude-domains` (`EXCLUDE_DOMAINS`) | – | Comma separated list of domains that are never changed. |
| `--default-ttl` (`DEFAULT_TTL`) | `300` | TTL in seconds for records without a TTL. ConnByte accepts 60–86400; other values are clamped. |
| `--dry-run` (`DRY_RUN`) | `false` | Log the changes, do not apply them. |
| `--listen-address` (`WEBHOOK_LISTEN_ADDRESS`) | `127.0.0.1:8888` | Address of the provider API for ExternalDNS. |
| `--health-address` (`HEALTH_LISTEN_ADDRESS`) | `0.0.0.0:8080` | Address of `/healthz`. |
| `--log-level` (`LOG_LEVEL`) | `info` | `debug`, `info`, `warn` or `error`. |
| `--log-format` (`LOG_FORMAT`) | `text` | `text` or `json`. |
| `--request-timeout` | `25s` | Timeout for one ExternalDNS request. |

## How it works

- Zones are read from `/ns/zone/list`. A record is always written to the most specific zone (for example `sub.example.com` before `example.com`).
- ExternalDNS updates are applied as delete + create, because the API has no update call. Targets that did not change are kept.
- All operations are idempotent: an existing record is not created again, a missing record is not deleted again.
- If an API call fails, the other changes still run; ExternalDNS gets an error (HTTP 500) and retries on the next sync.
- MX and SRV priorities are stored in the ConnByte `priority` field and returned as part of the target (`10 mail.example.com`), like ExternalDNS expects.
- Disabled records and record types ExternalDNS does not manage (for example `SOA`) are ignored.

Use `--txt-owner-id` in ExternalDNS so it only changes records it created.

## Development

```sh
go test ./...
go build .
docker build -t external-dns-connbyte-webhook .
```

The tests run against an in-memory mock of the ConnByte API (`internal/connbyte/connbytetest`); no account is needed.

Release: push a tag `vX.Y.Z`. GitHub Actions builds a multi-arch image and pushes it to `ghcr.io/steffenbarthel/external-dns-connbyte-webhook`.
The workflows are in `ci/github-workflows/` until they are moved to `.github/workflows/` (see the README there).

## License

MIT, see [LICENSE](LICENSE).

This project is not part of the ExternalDNS project. ConnByte: https://connbyte.com/
