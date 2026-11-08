# Storage usage and cost

MongoRescue keeps track of how much it stores on each [storage target](configuration.md), how that changes over time, what that costs per month at a price you enter, and how much data verification, restore tests and copies read back (egress). It answers "what is my backup storage costing me, and what is growing?" without leaving the dashboard.

**Everything here is an estimate computed from MongoRescue's own records.** It never calls a cloud billing, cost explorer or provider pricing API. It does not know the provider's rounding, storage tiers, minimum storage durations, request fees, free allowances or discounts, so it will not match an invoice to the cent; it is meant to show the order of magnitude and the trend.

## What is measured

The sum of the sizes of the archives MongoRescue holds, as of now, per storage target. Sizes are the bytes actually stored (after compression and encryption), taken from the backup and chunk records, not read from the storage.

| Kind | What it counts |
| :--- | :--- |
| `backup` | Primary backup archives (status completed, or deleted and waiting for the purge, because a soft-deleted archive stays on storage until the delete [grace period](security.md#lowering-a-protection-takes-the-grace-period-too) ends). Records that share one archive count once. Failed, cancelled, missing, pruned and purged backups hold nothing. Per target, job and database. |
| `copy` | Copies of backups on [copy targets](production.md#3-2-1-backups) in status done (the primary's size), and the done copies of PITR chunks. A copy that is pending or failed holds nothing yet. |
| `retained` | Copies that outlive their deleted primary because the job's copy retention keeps them. Listed apart because they are kept on purpose and they keep costing. |
| `pitr` | Oplog chunks of [point-in-time recovery](pitr.md) streams that are committed or superseded and not yet pruned (including chunks waiting for their purge), per stream. |

Not counted: metadata backups and HA snapshots, run logs and the data directory, and objects a [storage scan](verification.md) finds that MongoRescue does not know (orphans: look at the scan result for those). A backup record from before storage targets existed is counted on the default target.

## History

Once a day MongoRescue writes a snapshot of those sizes: one row per UTC day, target, kind, job, database and PITR stream, in the metadata database (`storage_usage_daily`, migration `0032`). The job runs when the instance starts and then every hour, and each run rewrites the rows of today, so a day keeps its last values and running it twice changes nothing. Snapshots older than 400 days are deleted by the same job. Only the active instance takes snapshots: an [HA](production.md#high-availability) standby takes none, and receives the history with the metadata snapshots like everything else.

The first snapshot is taken when the release that adds this feature starts; there is no history before that.

## Entering a price

Open **Storage** (the storage targets), edit a target and fill **Estimated cost (optional)**:

| Field | API field | Meaning |
| :--- | :--- | :--- |
| Currency | `pricing.currency` | ISO 4217 code of both prices, three letters (`USD`, `EUR`, `CHF`) |
| Storage price per GiB per month | `pricing.storage_per_gib_month` | What keeping 1 GiB for a month costs |
| Egress price per GiB | `pricing.egress_per_gib` | What reading 1 GiB back out of the target costs |

A GiB is 2^30 bytes. Prices are at least 0 and at most 1,000,000. A local target can carry a price too (the cost of the disk or the NAS per GiB). The price is configuration, not a secret: it is stored in the target's document and returned as it is. In the API, a save that omits `pricing` keeps the stored price, and `"pricing": {}` removes it:

```bash
curl -s -X PUT "$URL/api/v1/storage-targets/$ID" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Offsite","type":"s3","s3":{"bucket":"backups","access_key_id":"AKIA...","secret_access_key":"******"},
       "pricing":{"currency":"USD","storage_per_gib_month":0.023,"egress_per_gib":0.09}}'
```

**Estimated monthly cost** = stored GiB now × the storage price: what the next month costs if nothing changes. Costs are shown per target, and summed per currency (targets may price in different currencies; nothing is converted).

## Egress

Egress is the data read back out of a target. The estimate uses the records MongoRescue already has, over the range of the report (30 days in the dashboard):

- **verification**: the last verification of each backup and copy (`verified_at`) reads the whole archive from its target;
- **copy**: each completed copy read the primary archive (charged to the primary's target; the copy target receives the data);
- **restore tests**: each passed or mismatched [restore test](verification.md) read the archive it tested, from the primary or, for a disaster recovery drill, from the copy target it names.

Only the **last** verification of an archive is recorded, so an archive verified several times in the range counts once: **the figure is a lower bound**. Real restores, point-in-time replays and storage scans are not included. Many providers charge nothing for reads from a bucket to an instance in the same region and charge a lot to the internet; enter the price that applies to where MongoRescue runs. Egress cost = GiB × the egress price of the target read from.

## Dashboard

The **Overview** has a **Storage usage and cost** card: the total stored size per day as a chart (with a text description and the same data in a table), one row per target (stored, change over the range, estimated cost per month, egress and its cost; hover a figure for the split by kind or source), the total per currency, and the largest jobs and databases. Keys limited to some connections do not see the card, because the totals span every connection.

## API, MCP and metrics

`GET /api/v1/storage/usage?days=30` (`read` scope, `days` from 1 to 400) returns the report; see the [REST API reference](api.md) and the [OpenAPI description](openapi.md) for the schema.

```json
{
  "days": 30, "from": "2026-10-10", "to": "2026-11-08",
  "targets": [{
    "target_id": "stg_1a2b", "name": "Offsite", "type": "s3",
    "stored_bytes": 53687091200, "objects": 120,
    "by_kind": {"backup": 42949672960, "copy": 0, "retained": 0, "pitr": 10737418240},
    "growth_bytes": 5368709120,
    "pricing": {"currency": "USD", "storage_per_gib_month": 0.023, "egress_per_gib": 0.09},
    "monthly_cost": 1.15,
    "egress": {"bytes": 8589934592, "by_source": {"verification": 8589934592, "copy": 0, "restore_test": 0}, "cost": 0.72},
    "history": [{"date": "2026-10-10", "bytes": 47244640256}]
  }],
  "totals": [{"code": "USD", "monthly_cost": 1.15, "egress_cost": 0.72}],
  "jobs": [{"job_id": "job_nightly", "bytes": 42949672960, "objects": 30}],
  "databases": [{"database": "shop", "bytes": 42949672960, "objects": 30}],
  "basis": "Estimate from MongoRescue's own records, ..."
}
```

The report covers every connection, so a key or a user limited to some connections gets `403` (like `/metrics`). The [MCP](mcp.md) tool `get_storage_usage` (read scope, optional `days`) returns the same report.

Prometheus exports one gauge, `mongorescue_storage_stored_bytes{target}`: the bytes MongoRescue stores on a target (the target ID), refreshed hourly with the snapshot. The labels are bounded by the number of targets; the per-job and per-database detail stays in the API. A standby does not export it. Use it for dashboards and for alerts on growth, for example:

```promql
# A target grew by more than 20% in a week
mongorescue_storage_stored_bytes / mongorescue_storage_stored_bytes offset 7d > 1.2
```

For free space on local targets see `mongorescue_storage_free_bytes` in [metrics](metrics.md).

## Limits

- No billing data, tiers, request fees, minimum durations, free allowances or taxes.
- Egress is a lower bound (last verification per archive; no restores, replays or scans).
- No capacity forecast ("full in N days") and no volume preview of a retention change yet.
- Sizes are what MongoRescue recorded when it wrote the object; objects changed or removed outside MongoRescue are not noticed (a storage scan reports those).
