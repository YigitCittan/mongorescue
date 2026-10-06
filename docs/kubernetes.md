# Kubernetes

MongoRescue ships a Helm chart in [`deploy/helm/mongorescue`](../deploy/helm/mongorescue). It runs the same container image as Docker (`ghcr.io/yigitcittan/mongorescue`) as a one-replica StatefulSet. Everything except the bootstrap options is configured in the dashboard or the REST API and stored in the database on the data volume, exactly as with Docker.

## Install

```bash
helm install mongorescue ./deploy/helm/mongorescue \
  --namespace mongorescue --create-namespace \
  --set secretKey.existingSecret=mongorescue-key   # recommended, see below
kubectl -n mongorescue rollout status statefulset/mongorescue
kubectl -n mongorescue logs statefulset/mongorescue | grep setup_code
kubectl -n mongorescue port-forward svc/mongorescue 8080:8080
```

Open http://localhost:8080, enter the setup code and create the admin account. The chart's `NOTES.txt` prints these commands for your release. Pin the image with `image.tag` (an exact release such as `0.22.0`) or `image.digest`. By default the image tag is the chart's `appVersion`.

## What the chart runs

| Part | Default | Values |
| :--- | :--- | :--- |
| StatefulSet | Always **one replica**: the data directory is locked by one process, so a second replica could not start. `OrderedReady` stops the old pod before the new one starts. | |
| Data volume `/data` | A 2 GiB claim (`data-<release>-0`) holding `mongorescue.db`, the run logs and, without `secretKey`, `secret.key` | `persistence.*`, `persistence.existingClaim` |
| Archive volume `/backups` | A 20 GiB claim for the default "Local disk" storage target | `backups.persistence.*` |
| `/tmp` | An `emptyDir` (256 MiB) for the private `mongodump`/`mongorestore` config files | `tmp.sizeLimit` |
| Secret key | A generated Secret `<release>-secret-key`, kept on uninstall | `secretKey.*` |
| Probes | Startup, liveness and readiness on `GET /api/v1/health` | `probes.*` |
| Resources | Requests 100m CPU and 256 MiB, limit 1 GiB | `resources` |
| Shutdown | `terminationGracePeriodSeconds: 600`, of which up to 540 s go to waiting for running backups | `terminationGracePeriodSeconds`, `config.shutdownGrace` |
| Security context | UID/GID 10001, non-root, read-only root filesystem, all capabilities dropped, no privilege escalation, seccomp `RuntimeDefault`, no service account token | `podSecurityContext`, `securityContext` |
| Optional | NetworkPolicy, ServiceMonitor, PrometheusRule, Ingress | `networkPolicy`, `serviceMonitor`, `prometheusRule`, `ingress` |

Claims are kept when the release is deleted (`persistentVolumeClaimRetentionPolicy: Retain`). `values.schema.json` checks the values, so a typo such as `loglevel` fails at `helm install` instead of being ignored.

Prefer an S3 storage target for archives (Settings → Storage, then make it the default). The `/backups` claim shares the failure domain of the cluster, so treat it as a convenience for trying MongoRescue out, not as the copy you restore from after losing the cluster. With `backups.persistence.enabled: false`, `/backups` is an `emptyDir` and its archives are lost with the pod.

## The secret key

The key encrypts every stored credential: connection strings, S3 keys, notification secrets and the age identity used for backup encryption. **If the key is lost, those credentials are lost with it.** MongoRescue refuses to start with a key that does not match the database and changes nothing, so a wrong key can be fixed by putting the right one back.

- **Recommended:** create the Secret yourself, from your secret manager or with `kubectl create secret generic mongorescue-key --from-literal=secret-key="$(openssl rand -base64 32)"`, and set `secretKey.existingSecret=mongorescue-key` (and `secretKey.existingSecretKey` if the key inside the Secret is not `secret-key`).
- **Default:** without `existingSecret`, the chart generates a random key into the Secret `<release>-secret-key`. Helm keeps the Secret on `helm uninstall` (`helm.sh/resource-policy: keep`) and reuses its key on upgrades. Copy it out of the cluster right after the install (`NOTES.txt` prints the command) and download the [recovery kit](production.md#recovery-kit) once the setup is done. Reinstalling into a namespace whose Secret was deleted generates a *new* key that does not open the old database.
- **`secretKey.generate: false`** (without `existingSecret`) leaves the key in `/data/secret.key` on the data volume, as with Docker.

## Probes

All three probes call `GET /api/v1/health`, which needs no authentication:

- **Startup:** every 5 s, up to 120 failures (10 minutes). The first start of a new release runs its database migrations before the HTTP server listens; raise `probes.startup.failureThreshold` for a very large metadata database.
- **Liveness:** the check answers `503` when the scheduler is stale (its last tick is older than 90 seconds: a hung or deadlocked process), so Kubernetes restarts it. With the defaults a stale scheduler restarts the pod after about 2 minutes (6 failures, every 20 s).
- **Readiness:** the same check, every 10 s.

While a shutdown waits for running backups the check stays `200` and adds `"draining": true` (see below), so liveness does not kill the pod during the wait.

## Graceful shutdown

A pod deletion, a rolling update (`helm upgrade`, `kubectl rollout restart`) or a node drain sends SIGTERM. MongoRescue then:

1. **Refuses new runs.** Backups, job runs and restores started through the dashboard, the API or MCP get `503` ("MongoRescue is shutting down"), and the scheduler skips its triggers. The HTTP server keeps serving, so the dashboard still shows the progress.
2. **Waits for the running backups and restores**, for at most `MONGORESCUE_SHUTDOWN_GRACE`. The chart sets it to `terminationGracePeriodSeconds` minus 60 seconds (540 s by default), or to `config.shutdownGrace`. The remaining databases of a multi-database run that is going do not start. A second SIGTERM ends the wait.
3. **Cancels what is still running.** Those backups and restores are cancelled through the run registry and recorded as **cancelled** (by `system`, reason "interrupted: MongoRescue shut down before the run finished"). The partial archive is deleted: on S3 the multipart upload is aborted, so no object is ever created. A cancelled backup is never recorded as completed, and failure counts and alerts ignore it. If the process is killed before it can record the outcome (SIGKILL after the grace period), the next start marks the leftover runs as failed ("interrupted").
4. Stops the HTTP server and exits.

The 60 seconds of margin cover the HTTP drain (up to 15 s), the cancellation of the runs (up to 30 s, with SIGTERM and then SIGKILL to `mongodump`/`mongorestore`) and the final writes. Size `terminationGracePeriodSeconds` for your longest backup: with large databases, raise it (for example to `3600`) so rolling updates wait for the backup instead of cancelling it. Kubernetes waits for the old pod before it starts the new one (one replica, `OrderedReady`), so a long grace period delays updates by the same amount when a backup is running.

The same behaviour applies outside Kubernetes: set `MONGORESCUE_SHUTDOWN_GRACE` (or `-shutdown-grace`) and give the process manager a longer stop timeout, for example `stop_grace_period` in Docker Compose or `TimeoutStopSec=` in systemd. Without it (the default `0s`), a shutdown cancels running backups at once, recording them as cancelled.

## Monitoring

`/metrics` needs an API key unless **Settings → Security → Public metrics** is on ([metrics.md](metrics.md)). For the Prometheus Operator:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack     # whatever your Prometheus selects
  authorization:
    secretName: mongorescue-metrics    # a Secret with an API key of read scope
    secretKey: api-key
prometheusRule:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

The PrometheusRule holds the rules of [`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml) unchanged (the chart carries a copy in `files/alerts.yml`, and CI checks the two are equal); see [monitoring.md](monitoring.md#prometheus-alert-rules). Pair them with an external [heartbeat](monitoring.md#global-heartbeat): Prometheus in the same cluster fails with it.

## Network policy

`networkPolicy.enabled: true` adds a NetworkPolicy for the pod. Ingress to the HTTP port comes from `networkPolicy.ingressFrom` (peers; empty allows everything): add your ingress controller's namespace and Prometheus. Egress always allows DNS, and either everything (the default: MongoDB servers, S3, webhooks and SSO can be anywhere) or exactly the egress rules in `networkPolicy.egressTo`:

```yaml
networkPolicy:
  enabled: true
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
  egressTo:
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: mongodb
        - namespaceSelector: {}
      ports:
        - port: 27017
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - port: 443   # S3, webhooks, SSO
```

## Ingress and TLS

MongoRescue serves plain HTTP; terminate TLS at the Ingress. Then turn on **Settings → Security → Trust proxy headers**, so login throttling sees the client address and session cookies are marked `Secure` (or set *Secure cookies* to `always` if the controller does not send `X-Forwarded-Proto`), as described in [production.md](production.md#tls-and-the-reverse-proxy).

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: mongorescue.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: mongorescue-tls
      hosts:
        - mongorescue.example.com
```

## Configuration knobs

| Value | Sets | Default |
| :--- | :--- | :--- |
| `config.dashboard` | `MONGORESCUE_DASHBOARD` | `true` |
| `config.logLevel` | `-log-level` | `info` |
| `config.shutdownGrace` | `MONGORESCUE_SHUTDOWN_GRACE` | `terminationGracePeriodSeconds` − 60 s |
| `config.toolsDir` | `MONGORESCUE_TOOLS_DIR` | the tools in the image |
| `secretKey.*` | `MONGORESCUE_SECRET_KEY` | a generated Secret |
| `extraEnv`, `extraEnvFrom` | any variable, e.g. `AWS_*` for S3 targets without static keys | none |
| `extraVolumes`, `extraVolumeMounts` | e.g. a CA bundle for MongoDB TLS | none |

The data directory (`/data`), host and port (`0.0.0.0:8080`) are fixed by the chart. Every other setting lives in the database: see [configuration.md](configuration.md).

## Upgrades and backups of MongoRescue itself

- `helm upgrade` replaces the pod (see [Graceful shutdown](#graceful-shutdown)). A new release migrates the database on its first start, within the startup probe's budget. A release older than the database refuses to start; roll back with the matching image.
- Turn on [metadata backups](production.md#metadata-backups) to a storage target outside the cluster and keep the [recovery kit](production.md#recovery-kit) offline. To restore into a new cluster, install the chart with the old key (`secretKey.existingSecret`) and follow [Restore MongoRescue from a snapshot](production.md#restore-mongorescue-from-a-snapshot), copying `mongorescue.db` into the data claim (for example with `kubectl cp` while the StatefulSet is scaled to 0).

## Testing the chart

CI lints the chart (`helm lint --strict`), renders it with the default and an all-features values file (`ci/full-values.yaml`) and validates the manifests with kubeconform, including the ServiceMonitor and PrometheusRule CRDs. The *Helm (kind)* job then runs `scripts/test-helm-kind.sh`. It builds the image, installs the chart and a MongoDB pod on a kind cluster, waits for the probes, signs in through the setup API, runs a backup, checks that it completed, and checks that the backup is still listed after a rolling restart. Run the script locally with `docker`, `kind`, `kubectl`, `helm`, `curl` and `jq` on your `PATH`.
