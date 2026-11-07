# Deployment recipes

Ready-to-copy setups for the common ways to run MongoRescue. Each recipe is a file in [`examples/`](https://github.com/YigitCittan/mongorescue/tree/main/examples) that was run for real (the Compose files with `docker compose config` and a live start, the Kubernetes manifest with `kubeconform`, the workflow with `actionlint`), plus the reasoning you need to adapt it.

| I want to | Recipe |
| :--- | :--- |
| Try it in five minutes, with a throwaway MongoDB | [`examples/with-mongodb.yml`](https://github.com/YigitCittan/mongorescue/blob/main/examples/with-mongodb.yml) and the [CLI walkthrough](getting-started-cli.md) |
| Add it to a Compose project that already runs MongoDB | [`examples/compose-existing-stack.yml`](https://github.com/YigitCittan/mongorescue/blob/main/examples/compose-existing-stack.yml) |
| Serve it over HTTPS on a VM or a home server | [Caddy with automatic HTTPS](#caddy-with-automatic-https) |
| Run it as a system service on Linux | [systemd](production.md#systemd) (the `.deb` and `.rpm` packages install the unit) |
| Run it on Windows | The [desktop app](desktop.md) (tray icon, runs while the window is closed) or the [CLI walkthrough](getting-started-cli.md#1-start-the-server) for the server binary |
| Run it in Kubernetes | The [Helm chart](kubernetes.md) |
| Trigger a backup from Kubernetes on my own schedule | [CronJob with the CLI](#kubernetes-cronjob-with-the-cli) |
| Back up before every deployment in CI | [GitHub Actions](#github-actions-back-up-before-a-deployment) (GitLab CI: [CLI reference](cli.md#ci-examples)) |

## Caddy with automatic HTTPS

MongoRescue serves plain HTTP. [`examples/compose-caddy.yml`](https://github.com/YigitCittan/mongorescue/blob/main/examples/compose-caddy.yml) puts [Caddy](https://caddyserver.com) in front of it, which obtains and renews a Let's Encrypt certificate by itself:

```bash
MONGORESCUE_DOMAIN=backup.example.com docker compose -f examples/compose-caddy.yml up -d
docker compose -f examples/compose-caddy.yml logs mongorescue | grep -i setup
```

The domain's DNS must point at the host and ports 80 and 443 must be open. Without `MONGORESCUE_DOMAIN` the site is `https://localhost` with a certificate from Caddy's own CA, which is enough to try it. The whole Caddyfile ([`examples/Caddyfile`](https://github.com/YigitCittan/mongorescue/blob/main/examples/Caddyfile)) is:

```text
{$MONGORESCUE_DOMAIN:localhost} {
	encode zstd gzip
	reverse_proxy mongorescue:8080
}
```

### Trusted proxies: the step that is easy to forget

After the first sign-in, tell MongoRescue which address its proxy has: **Settings → Security → Trusted proxies**, or with an admin API key:

```bash
curl -X PUT https://backup.example.com/api/v1/settings \
  -H "Authorization: Bearer $MONGORESCUE_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"security": {"trusted_proxies": ["172.28.0.0/24"]}}'
```

`172.28.0.0/24` is the subnet the Compose file gives the network that only Caddy and MongoRescue share. It is fixed in the file so that the value stays right when containers are recreated. What this setting does, and what breaks without it (measured on this very file):

| | Without trusted proxies | With `172.28.0.0/24` |
| :--- | :--- | :--- |
| Client address (login throttle, rate limits, audit log) | Always Caddy's address: one bad client, or one script with a wrong key, locks everybody out | The real client address, from `X-Forwarded-For` |
| Session cookie | `HttpOnly; SameSite=Strict`, **no `Secure` flag** | `HttpOnly; Secure; SameSite=Strict` |
| HSTS, MCP behind the proxy | Not applied | Applied when [enabled](configuration.md#settings) |

Three rules make it safe, and the example follows all of them:

1. **List only the proxy**, never a broad range such as `0.0.0.0/0`. MongoRescue reads forwarding headers only from peers in the list.
2. **Do not publish MongoRescue's port.** The service has no `ports:` entry, so a request cannot reach it without passing through Caddy, and nobody can send their own `X-Forwarded-For` directly.
3. **The proxy must overwrite, not trust, the client's headers.** Caddy's `reverse_proxy` does this for an edge proxy. Do not add Caddy's own `trusted_proxies` global option here: that tells Caddy to believe headers from clients, which is only right when another proxy or CDN sits in front of Caddy (then list *that* in Caddy, and Caddy's address in MongoRescue).

The details, the algorithm that picks the client address and the Nginx variant are in [TLS and the reverse proxy](production.md#tls-and-the-reverse-proxy). For Traefik, HAProxy or Nginx the MongoRescue side is the same: put the proxy's address or network in the list and leave the port unpublished.

## Kubernetes CronJob with the CLI

MongoRescue schedules its own jobs, so a CronJob is for the cases where Kubernetes should own the schedule or the result: GitOps-managed cadence per cluster, a backup in a deployment window, or alerting you already have on failed Kubernetes Jobs. [`examples/kubernetes/backup-cronjob.yaml`](https://github.com/YigitCittan/mongorescue/blob/main/examples/kubernetes/backup-cronjob.yaml) runs the MongoRescue image as a client:

```yaml
args: [backup, --job, job_REPLACE_ME, --wait, --timeout, 30m]
env:
  - name: MONGORESCUE_URL
    value: http://mongorescue.mongorescue.svc:8080
  - name: MONGORESCUE_CLI_API_KEY
    valueFrom:
      secretKeyRef: {name: mongorescue-cli-key, key: key}
```

- The Job needs only the service address and an **operator** [API key](api.md#api-key-scopes), kept in a Secret you create with `kubectl create secret generic mongorescue-cli-key --from-literal=key=...`. No MongoDB credentials, no Database Tools, no data volume.
- `--wait` turns the Job's exit code into the backup's: `0` completed, `1` failed or cancelled, `5` another backup of the database is already running, `6` the wait timed out (the backup continues on the server), `7` MongoRescue unreachable ([exit codes](cli.md#exit-codes)). A failed backup is a failed Job; no retry (`backoffLimit: 0`) because the next tick tries again.
- `concurrencyPolicy: Forbid` and `activeDeadlineSeconds` keep a stuck run from stacking up; the Job pod runs non-root with a read-only filesystem and no capabilities.
- Pin the image tag to the release your server runs. If the chart's NetworkPolicy is on, allow the CronJob's namespace in `networkPolicy.ingress` ([chart values](kubernetes.md#what-the-chart-runs)).

## GitHub Actions: back up before a deployment

[`examples/github-actions/backup-before-deploy.yml`](https://github.com/YigitCittan/mongorescue/blob/main/examples/github-actions/backup-before-deploy.yml) runs a job and gates the deployment on it, with the MongoRescue image as the CLI (nothing to install on the runner):

```yaml
- name: Run the backup job and wait for it
  env:
    MONGORESCUE_URL: ${{ vars.MONGORESCUE_URL }}
    MONGORESCUE_CLI_API_KEY: ${{ secrets.MONGORESCUE_OPERATOR_KEY }}
    MONGORESCUE_JOB: ${{ vars.MONGORESCUE_JOB }}
  run: |
    docker run --rm -e MONGORESCUE_URL -e MONGORESCUE_CLI_API_KEY \
      "$MONGORESCUE_IMAGE" backup --job "$MONGORESCUE_JOB" --wait --timeout 30m
```

`-e NAME` without a value passes the variable through, so the key never appears on a command line or in the log. The `deploy` job has `needs: backup`, so it never starts after a failed backup. The runner must reach your MongoRescue URL over HTTPS (a MongoRescue on a private network needs a self-hosted runner). The same step with the downloaded binary instead of `docker run` is in the [CLI reference](cli.md#ci-examples), together with a GitLab CI restore drill.
