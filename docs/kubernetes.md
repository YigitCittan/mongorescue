# Kubernetes

MongoRescue ships a Helm chart in [`deploy/helm/mongorescue`](../deploy/helm/mongorescue). It runs the same container image as Docker (`ghcr.io/yigitcittan/mongorescue`) as a one-replica StatefulSet. Everything except the bootstrap options is configured in the dashboard or the REST API and stored in the database on the data volume, exactly as with Docker.

## Install

The chart is published as a signed OCI artifact at `oci://ghcr.io/yigitcittan/charts/mongorescue`. Its version is its own (see [Helm chart versions](versioning.md#helm-chart-versions)); list what exists under the package's page on GitHub, or pin one:

```bash
helm install mongorescue oci://ghcr.io/yigitcittan/charts/mongorescue --version 0.2.0 \
  --namespace mongorescue --create-namespace \
  --set secretKey.existingSecret=mongorescue-key   # recommended, see below
kubectl -n mongorescue rollout status statefulset/mongorescue
kubectl -n mongorescue logs statefulset/mongorescue | grep setup_code
kubectl -n mongorescue port-forward svc/mongorescue 8080:8080
```

Verify the chart's signature before installing (needs [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3 or later; more in [Verifying releases](security.md#helm-chart-cosign)):

```bash
cosign verify ghcr.io/yigitcittan/charts/mongorescue:0.2.0   --certificate-identity-regexp '^https://github\.com/YigitCittan/mongorescue/\.github/workflows/release\.yml@refs/tags/v.*$'   --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

To work on the chart, install from a clone instead: replace the chart reference with `./deploy/helm/mongorescue`. Its `appVersion` is only a development default there, so set `image.tag` to a release you want.

Open http://localhost:8080, enter the setup code and create the admin account. The chart's `NOTES.txt` prints these commands for your release. Pin the image with `image.tag` (an exact release such as `0.22.0`) or `image.digest`. By default the image tag is the chart's `appVersion`.

## What the chart runs

| Part | Default | Values |
| :--- | :--- | :--- |
| StatefulSet | Always **one replica**: the data directory is locked by one process, so a second replica could not start. `OrderedReady` stops the old pod before the new one starts. | |
| Data volume `/data` | A 2 GiB claim (`data-<release>-0`) holding `mongorescue.db`, the run logs and, without `secretKey`, `secret.key` | `persistence.*`, `persistence.existingClaim` |
| Archive volume `/backups` | A 20 GiB claim for the default "Local disk" storage target | `backups.persistence.*` |
| `/tmp` | An `emptyDir` (256 MiB) for temporary files | `tmp.sizeLimit` |
| `/run/mongorescue-tmp` | A memory-backed `emptyDir` (16 MiB) for the short-lived files that pass connection strings and TLS material to `mongodump`/`mongorestore` (`MONGORESCUE_TMP_DIR`); credentials never reach the node's disk. See [connections.md](connections.md#temporary-credentials-on-disk) | `credentialsTmp.medium`, `credentialsTmp.sizeLimit` |
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
- **`keyWrap`** keeps the key on the data volume, but only as `secret.key.wrapped`, wrapped by a KMS; the pod unwraps it at every start with its own identity ([key wrapping](security.md#wrapping-secretkey-with-a-kms)). No Secret is generated and `existingSecret` must stay empty. See [below](#key-wrapping-with-workload-identity).

### GitOps (Argo CD, kustomize helmCharts)

The generated key is kept across upgrades with Helm's `lookup`, which reads the live Secret. `lookup` returns nothing when the chart is rendered without cluster access: `helm template`, Argo CD and kustomize `helmCharts` all render that way. Every render then produces a *new* random key and the GitOps tool overwrites the Secret (Argo CD also ignores `helm.sh/resource-policy: keep` when syncing). After the next pod restart MongoRescue refuses to start, because the key no longer opens the stored credentials. (Flux's helm-controller runs a real install and upgrade with cluster access, so it is not affected.)

For these tools, create the key once, outside the chart:

```bash
kubectl -n mongorescue create secret generic mongorescue-key \
  --from-literal=secret-key="$(openssl rand -base64 32)"
```

Then point the chart at it (or use [`keyWrap`](#key-wrapping-with-workload-identity)) and turn on the guard:

```yaml
secretKey:
  existingSecret: mongorescue-key
  requireExternal: true
```

With `secretKey.requireExternal: true` the chart fails to render, with an actionable message, unless `secretKey.existingSecret` or `keyWrap` is set, so a GitOps render can never produce a key of its own. The guard is off by default because `lookup` is also empty on a genuine first `helm install`, `--dry-run` and `helm lint`, where generating a key is correct. Keep the Secret out of version control, or commit it only encrypted (External Secrets, Sealed Secrets, SOPS), and keep a copy outside the cluster.

### Key wrapping with workload identity

Give the pod an identity that may encrypt and decrypt with the KMS key, and nothing else, then set `keyWrap`:

- **AWS KMS with IRSA**: an IAM role with `kms:Encrypt` and `kms:Decrypt` on the key, trusted by the cluster's OIDC provider for the release's service account.

  ```yaml
  keyWrap: aws-kms:arn:aws:kms:eu-central-1:123456789012:alias/mongorescue
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/mongorescue
  ```

  With EKS Pod Identity, associate the role with the service account instead; no annotation is needed.
- **Google Cloud KMS with GKE Workload Identity**: grant `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key to the Kubernetes service account (`principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/<namespace>/sa/<service account>`), or to a Google service account it impersonates (`serviceAccount.annotations: {iam.gke.io/gcp-service-account: mongorescue@<project>.iam.gserviceaccount.com}`).
- **Azure Key Vault with Azure Workload Identity**: a user-assigned managed identity with *Key Vault Crypto User* on the key and a federated credential for the service account; set `serviceAccount.annotations: {azure.workload.identity/client-id: <client id>}` and `podLabels: {azure.workload.identity/use: "true"}`.
- **Vault Transit with the Kubernetes auth method**: a Vault role bound to the service account with a policy allowing `update` on `transit/encrypt/<key>` and `transit/decrypt/<key>`; set `extraEnv` `VAULT_K8S_ROLE` (and `VAULT_CACERT` with a mounted CA bundle) and `serviceAccount.automountServiceAccountToken: true`, which the login needs.

Unwrapping needs the KMS at every start: a pod whose identity cannot reach it stays in `CrashLoopBackOff` with the reason in its log. For [high availability](#high-availability), copy `secret.key.wrapped` from the first release's data volume into the second's before it starts (for example `kubectl cp` while it is scaled to 0); both pods need the KMS permission.

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

The PrometheusRule holds the rules of [`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml) unchanged (the chart carries a copy in `files/alerts.yml`, and CI checks the two are equal); see [monitoring.md](monitoring.md#prometheus-alert-rules); every rule links to its [incident runbook](runbooks.md). Pair them with an external [heartbeat](monitoring.md#global-heartbeat): Prometheus in the same cluster fails with it.

The [Grafana dashboard](monitoring.md#grafana-dashboard) can ship as a ConfigMap for the Grafana sidecar (kube-prometheus-stack or the Grafana chart), which imports every ConfigMap that carries its label:

```yaml
grafana:
  dashboard:
    enabled: true
    labels:
      grafana_dashboard: "1"           # the sidecar's sidecar.dashboards.label
    annotations:
      grafana_folder: MongoRescue      # optional: the folder (sidecar.dashboards.folderAnnotation)
```

The ConfigMap is `<release>-mongorescue-grafana-dashboard` in the release namespace; the sidecar must watch that namespace (`sidecar.dashboards.searchNamespace`). The chart carries a copy of `deploy/grafana/mongorescue.json` in `files/grafana-dashboard.json`, and CI checks the two are equal.

### Memory limit and GOMEMLIMIT

`resources.limits.memory` bounds `mongodump` and `mongorestore` as well as MongoRescue itself, and an OOMKill shows up as a failed backup: size it with the [sizing guide](production.md#sizing). `config.goMemLimitRatio` (default `0`, off) makes the chart set the Go runtime's soft memory limit `GOMEMLIMIT` to that share of the memory limit, computed in the template (Kubernetes cannot derive a fraction of a limit through the downward API):

```yaml
resources:
  limits:
    memory: 2Gi
config:
  goMemLimitRatio: 0.7     # GOMEMLIMIT=1503238553
```

A ratio needs `resources.limits.memory` (the render fails without it) and stays between 0 and 1; a `GOMEMLIMIT` in `extraEnv` wins.

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

MongoRescue serves plain HTTP; terminate TLS at the Ingress. Then set **Settings → Security → Trusted proxies** to the Ingress controller's pod CIDR, so login throttling sees the client address and session cookies are marked `Secure` (or set *Secure cookies* to `always` if the controller does not send `X-Forwarded-Proto`), as described in [production.md](production.md#tls-and-the-reverse-proxy).

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
| `config.goMemLimitRatio` | `GOMEMLIMIT`, a share of `resources.limits.memory` ([memory limit](#memory-limit-and-gomemlimit)) | `0` (off) |
| `grafana.dashboard.*` | A ConfigMap with the [Grafana dashboard](monitoring.md#grafana-dashboard) for the Grafana sidecar | off |
| `secretKey.*` | `MONGORESCUE_SECRET_KEY` | a generated Secret |
| `keyWrap` | `MONGORESCUE_KEY_WRAP` ([key wrapping](#key-wrapping-with-workload-identity)) | off |
| `ha.*` | `MONGORESCUE_HA_*` ([high availability](#high-availability)) | off |
| `extraEnv`, `extraEnvFrom` | any variable, e.g. `AWS_*` for S3 targets without static keys | none |
| `extraVolumes`, `extraVolumeMounts` | e.g. a CA bundle for MongoDB TLS | none |

The data directory (`/data`), host and port (`0.0.0.0:8080`) are fixed by the chart. Every other setting lives in the database: see [configuration.md](configuration.md).

## High availability

The StatefulSet keeps one replica: its data directory belongs to one process. For [active-passive high availability](production.md#high-availability), install the chart **twice** (two releases, each with its own data claim), ideally on different nodes or zones, with the same `ha` values and the same secret key:

```bash
kubectl create secret generic mongorescue-key --from-literal=secret-key="$(openssl rand -base64 32)"
kubectl create secret generic mongorescue-ha-s3 --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
cat > ha-values.yaml <<'EOF'
secretKey:
  existingSecret: mongorescue-key
ha:
  enabled: true
  target: s3://mongorescue-ha/prod?region=eu-central-1
  cluster: prod
  # Ready only on the active instance (GET /api/v1/ready), see below.
  readinessActiveOnly: true
extraEnvFrom:
  - secretRef:
      name: mongorescue-ha-s3
# Keep the two releases on different nodes (required) and spread over zones (preferred).
# Both carry the pod label mongorescue.io/ha-cluster=<ha.cluster>.
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            mongorescue.io/ha-cluster: prod
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        mongorescue.io/ha-cluster: prod
EOF
# The shared Service (mongorescue-prod) is rendered by ONE release only.
helm install mongorescue-a deploy/helm/mongorescue -f ha-values.yaml --set ha.sharedService.enabled=true
helm install mongorescue-b deploy/helm/mongorescue -f ha-values.yaml
```

The chart refuses `ha.enabled` without `secretKey.existingSecret` or `keyWrap` (two generated keys would differ) or without `ha.target`. The instance holding the lease is active; the other serves no data and accepts no sign-in: only the health check, a static standby page and `/metrics` (process, build and HA series, without credentials), and `503` for everything else. Scrape both: the `MongoRescueHANoActiveInstance` alert in the PrometheusRule fires when no instance of a cluster is active. Prefer IRSA or Workload Identity over static keys for the HA bucket.

### Readiness and the shared Service

Kubernetes and load balancers route by status code. By default the probes all use `GET /api/v1/health`, which is the liveness check: the active and the standby both answer `200` (so neither is restarted), both are *Ready*, and a Service across them sends requests that reach the standby a `503` with `Retry-After`.

Set `ha.readinessActiveOnly: true` to make the **readiness** probe use `GET /api/v1/ready` ([monitoring.md](monitoring.md#readiness-check), [api.md](api.md#readiness-check)): `200` only on the active instance when it is not draining, its database is writable and the disk guard is satisfied; otherwise `503` with a `code` (`not_active`, `draining`, `store_unwritable`, `disk_full`). The startup and liveness probes stay on `/api/v1/health`, so a standby is alive but not *Ready*. When the lease moves, the old active (or a draining one) drops out of the Service's endpoints and the new active joins within the probe's `periodSeconds` x `failureThreshold` (30 seconds by default for leaving, one period for joining), with no change to the Service.

`ha.sharedService.enabled: true` renders that Service: `mongorescue-<cluster>` (`ha.sharedService.name` overrides it), selecting the pods of both releases by the `mongorescue.io/ha-cluster` label. Enable it in **one** release only (two releases would own the same object), install both releases in the same namespace, and point your Ingress, clients and the CLI at it. Without `readinessActiveOnly` it still selects both pods.

Consequences of `readinessActiveOnly`:

- The standby release never becomes *Ready*: do not `helm upgrade --wait` (or `helm install --wait`) it, and its own per-release Service, Ingress and ServiceMonitor targets have no ready endpoint until it holds the lease. Scrape both pods through the headless Service or pod monitors (`MongoRescueHANoActiveInstance` relies on both being scraped).
- A rolling update of the active release stays unready until the new pod holds the lease; the standby takes over during the shutdown wait, which is the intended failover.
- A disk-full or read-only active instance leaves the Service while it still holds the lease. No instance is then Ready: that is a real outage of the dashboard and API, which `disk_full` / `store_unwritable` make visible instead of serving errors.

Keep the two pods apart: the `affinity` and `topologySpreadConstraints` values above are rendered as given, and NOTES.txt warns when neither is set.

## Upgrades and backups of MongoRescue itself

- `helm upgrade` replaces the pod (see [Graceful shutdown](#graceful-shutdown)). A new release migrates the database on its first start, within the startup probe's budget. A release older than the database refuses to start (`ErrSchemaTooNew`), so `helm rollback` or the previous image alone does not work after a migration: before it migrates, MongoRescue copies the database to `mongorescue.db.pre-v<from>-to-v<to>` on the data volume, and rolling back means restoring that copy with the StatefulSet scaled down. The exact commands, the order for [high availability](#high-availability) and what to back up first are in [upgrading.md](upgrading.md).
- Turn on [metadata backups](production.md#metadata-backups) to a storage target outside the cluster and keep the [recovery kit](production.md#recovery-kit) offline. To restore into a new cluster, install the chart with the old key (`secretKey.existingSecret`) and follow [Restore MongoRescue from a snapshot](production.md#restore-mongorescue-from-a-snapshot), copying `mongorescue.db` into the data claim (for example with `kubectl cp` while the StatefulSet is scaled to 0).

## Testing the chart

CI lints the chart (`helm lint --strict`), packages it, fails a pull request that changes the chart without raising its version, , renders it with the default and an all-features values file (`ci/full-values.yaml`) and validates the manifests with kubeconform, including the ServiceMonitor and PrometheusRule CRDs. The *Helm (kind)* job then runs `scripts/test-helm-kind.sh`. It builds the image, installs the chart and a MongoDB pod on a kind cluster, waits for the probes, signs in through the setup API, runs a backup, checks that it completed, and checks that the backup is still listed after a rolling restart. The probe switching, the shared Service, the pod label and the refusals of `ha.readinessActiveOnly` and `ha.sharedService` without `ha.enabled` are asserted on rendered manifests by a step of the *Helm lint* job (`.github/workflows/helm.yml`). A failover through the shared Service is not part of the kind test: it needs a second release, an S3-compatible lease store in the cluster and waits of lease TTL multiples, which would make the job slow and timing dependent. The role-to-status-code behaviour is covered by the unit tests of `GET /api/v1/ready` (`internal/server/ready_test.go`), including the standby's. Run the script locally with `docker`, `kind`, `kubectl`, `helm`, `curl` and `jq` on your `PATH`.
