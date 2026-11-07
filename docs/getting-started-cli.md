# Your first backup and restore from the command line

This page takes you from nothing to a verified backup and a restore into a safe clone in about five minutes, using the `mongorescue` binary or the Docker image and no dashboard clicks beyond the one-time setup. Pick the tab for your system; every step has a Linux, a Windows (PowerShell) and a Docker variant.

The commands on this page were run against MongoDB 7 and the output below is what they print (IDs, times and sizes differ on your machine). Prefer the dashboard? Start with the [quick start](index.md#quick-start-with-docker) instead; this page is the scripting path.

!!! info "Two programs, one binary"
    `mongorescue` is both the **server** (schedules, streams and stores backups) and a **command line client** of that server. The client commands (`backup`, `restore`, `list`, `verify`, `status`) never touch MongoDB or the data directory; they call the server's [REST API](api.md) with an API key. So you need a server first, then a key for the client.

## What you need

- A MongoDB 5.0 to 8.0 server or replica set you can reach (standalone is fine). To try everything with a throwaway database, use the Docker tab: it starts one.
- Docker, or the `mongorescue` binary from [Releases](https://github.com/YigitCittan/mongorescue/releases) (a `.zip` on Windows, a `.tar.gz`, `.deb` or `.rpm` on Linux) together with the [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/) (`mongodump` and `mongorestore`, 100.12 or newer) on your `PATH`. The Docker image already contains the tools.

## 1. Start the server

=== "Linux"

    ```bash
    mkdir -p ~/mongorescue && cd ~/mongorescue
    mongorescue -dashboard -data-dir ./data
    ```

    The server stays in the foreground (use a second terminal for the next steps, or [run it under systemd](production.md#systemd)). It serves plain HTTP on `127.0.0.1:8080`, so only this machine can reach it. Without `-dashboard` it serves only the API and the CLI, but you need the dashboard once, for setup in step 2 (or use the [headless variant](#headless-setup-with-the-rest-api)).

=== "Windows (PowerShell)"

    ```powershell
    # Unzip the release, then, in the folder with mongorescue.exe and the Database Tools
    # (mongodump.exe and mongorestore.exe next to it, in a "tools" subfolder, or on PATH):
    New-Item -ItemType Directory -Force "$env:LOCALAPPDATA\mongorescue" | Out-Null
    .\mongorescue.exe -dashboard -data-dir "$env:LOCALAPPDATA\mongorescue"
    ```

    The server stays in the foreground; open a second PowerShell window for the next steps. It serves plain HTTP on `127.0.0.1:8080`. The default data directory is `.\data` relative to the current folder, so always pass `-data-dir` (or set `MONGORESCUE_DATA_DIR`) to keep it in one place. [Where the tools are looked up](configuration.md#mongodb-database-tools). If you would rather click than type, the [desktop app](desktop.md) bundles the server, the tools and a tray icon.

=== "Docker"

    ```bash
    git clone https://github.com/YigitCittan/mongorescue.git && cd mongorescue
    docker compose -f docker-compose.yml -f examples/with-mongodb.yml up -d
    ```

    This starts MongoRescue on port 8080 and a throwaway MongoDB called `mongo` (user `root`, password `demo-password`, for evaluation only). Without the clone, use the `docker run` command of the [quick start](index.md#quick-start-with-docker) and your own MongoDB.

    To have something to back up:

    ```bash
    docker compose exec mongo mongosh --quiet -u root -p demo-password --authenticationDatabase admin \
      --eval 'db.getSiblingDB("shop").orders.insertMany([{sku:"a1",qty:2},{sku:"b2",qty:5},{sku:"c3",qty:1}]).insertedIds.length'
    ```

The first start prints a one-time **setup code**; copy it (it changes on every restart and stops working once the first user exists):

```text
level=WARN msg="Setup required: open http://localhost:8080/ and enter setup code WYSW-J44Z-6FXJ-3LNR-AAFB-WRNS" ...
```

In Docker, read it with `docker compose logs mongorescue | grep -i setup`.

## 2. Create the administrator and a connection

Open `http://localhost:8080`, enter the setup code, choose a username and a password (12 to 72 bytes), then **Connections → Add** and paste your MongoDB URI (for the Docker tab: `mongodb://root:demo-password@mongo:27017/?authSource=admin`). **Test** it and save. MongoRescue stores the URI encrypted and shows it with the password masked from now on.

Your own database, with a user that is allowed to back it up: the [privileges](connections.md#privileges) page lists the built-in roles to grant.

### Headless setup with the REST API

Everything above, and the API key of the next step, can be done without a browser. This is the same flow the dashboard uses: a session from `POST /api/v1/setup`, then the session's CSRF token on every change.

=== "Linux"

    ```bash
    URL=http://localhost:8080
    CODE=WYSW-J44Z-6FXJ-3LNR-AAFB-WRNS    # the setup code from the log

    curl -s -c jar -X POST "$URL/api/v1/setup" -H 'Content-Type: application/json' \
      -d "{\"setup_code\":\"$CODE\",\"username\":\"admin\",\"password\":\"choose-a-long-password\"}" > setup.json
    CSRF=$(sed 's/.*"csrf_token":"\([^"]*\)".*/\1/' setup.json)

    curl -s -b jar -X POST "$URL/api/v1/connections" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
      -d '{"name":"demo","uri":"mongodb://root:demo-password@mongo:27017/?authSource=admin"}'
    ```

=== "Windows (PowerShell)"

    ```powershell
    $url = "http://127.0.0.1:8080"
    $code = "WYSW-J44Z-6FXJ-3LNR-AAFB-WRNS"    # the setup code from the log

    $body = @{ setup_code = $code; username = "admin"; password = "choose-a-long-password" } | ConvertTo-Json
    $r = Invoke-RestMethod "$url/api/v1/setup" -Method Post -ContentType "application/json" -Body $body -SessionVariable session
    $h = @{ "X-CSRF-Token" = $r.data.csrf_token }

    $conn = @{ name = "demo"; uri = "mongodb://mr-mongo:27017" } | ConvertTo-Json
    $c = Invoke-RestMethod "$url/api/v1/connections" -Method Post -WebSession $session -Headers $h -ContentType "application/json" -Body $conn
    $c.data.id
    ```

=== "Docker"

    The Linux commands work as they are from the host (port 8080), with the URI shown in step 1. The `mongo` host name in the URI is resolved by MongoRescue inside the Compose network, not by `curl`.

Later, sign in with `POST /api/v1/auth/login` the same way (`-c jar` stores the session cookie, `-SessionVariable` does it in PowerShell).

## 3. Create an API key for the CLI

Settings → **API keys** → create a key named `cli` with scope **operator** (enough for backups, verifications and restores into a safe clone; [scopes](api.md#api-key-scopes)). The key (`mr_<prefix>_<secret>`) is shown **once**. Store it where only you can read it:

=== "Linux"

    ```bash
    mkdir -p ~/.config/mongorescue
    umask 077
    printf '%s' 'mr_xxxxxxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' > ~/.config/mongorescue/key

    export MONGORESCUE_URL=http://127.0.0.1:8080
    export MONGORESCUE_CLI_API_KEY_FILE=~/.config/mongorescue/key
    ```

=== "Windows (PowerShell)"

    ```powershell
    New-Item -ItemType Directory -Force "$env:APPDATA\mongorescue" | Out-Null
    Set-Content -NoNewline -Path "$env:APPDATA\mongorescue\key" -Value "mr_xxxxxxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

    $env:MONGORESCUE_URL = "http://127.0.0.1:8080"
    $env:MONGORESCUE_CLI_API_KEY_FILE = "$env:APPDATA\mongorescue\key"
    ```

    Files under `%APPDATA%` are readable by your account only (and administrators). The CLI does not check permissions on Windows. Put the two variables in your PowerShell profile or set them for your user with `setx` to keep them across sessions.

=== "Docker"

    The image's entrypoint is the `mongorescue` binary, so the client runs inside the container, where the default URL `http://127.0.0.1:8080` is the server itself. Pass the key from your shell instead of writing it down:

    ```bash
    export MONGORESCUE_CLI_API_KEY='mr_xxxxxxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'
    alias mr='docker compose exec -e MONGORESCUE_CLI_API_KEY mongorescue mongorescue'
    ```

    From here on, type `mr` where the other tabs type `mongorescue`. (`docker run --rm -e MONGORESCUE_URL -e MONGORESCUE_CLI_API_KEY ghcr.io/yigitcittan/mongorescue:latest status` reaches a server elsewhere.) In the following steps, Windows users type `.\mongorescue.exe` if it is not on `PATH`.

Check that the key works:

```text
$ mongorescue status
Server:        http://127.0.0.1:8080
Version:       0.28.0 (healthy)
Signed in as:  admin (api_key)
Scope:         operator
Backups:       0 (0 completed, 0 failed, 0 failed in the last 24 h, 0 running), 0 B stored
Restores:      0 (0 running)
Jobs:          0 enabled
Active runs:   0
```

Exit code `3` means the server refused the key, `7` that it is unreachable, `2` that no key was found ([all exit codes](cli.md#exit-codes)).

## 4. Take a backup

```text
$ mongorescue list connections
ID                     NAME  URI                                                  LAST TEST
conn_05c6e81fc99b4a47  demo  mongodb://root:******@mongo:27017/?authSource=admin  never

$ mongorescue backup --connection conn_05c6e81fc99b4a47 --database shop --wait
Started backup bkp_shop_20261107_213521_aa092e97 of shop; waiting for it to finish.
backup bkp_shop_20261107_213521_aa092e97: in_progress (dumping)
Backup bkp_shop_20261107_213521_aa092e97 of shop completed: 380 B in <1s.
```

`--wait` blocks until the backup ends and makes the exit code the result (`0` completed, `1` failed). Without it the command prints the new backup and returns at once. The progress lines go to stderr, the final record to stdout; add `--quiet` to print only the ID, which is what you want in a script:

=== "Linux"

    ```bash
    BACKUP=$(mongorescue backup --connection conn_05c6e81fc99b4a47 --database shop --wait --quiet)
    echo "$BACKUP"
    ```

=== "Windows (PowerShell)"

    ```powershell
    $backup = mongorescue backup --connection conn_05c6e81fc99b4a47 --database shop --wait --quiet
    $backup
    ```

=== "Docker"

    ```bash
    BACKUP=$(mr backup --connection conn_05c6e81fc99b4a47 --database shop --wait --quiet)
    echo "$BACKUP"
    ```

The archive is checksummed, written to the default **Local disk** [storage target](configuration.md#storage-targets) and listed:

```text
$ mongorescue list backups
ID                                 DATABASE  STATUS     TRIGGER  STARTED               DURATION  SIZE
bkp_shop_20261107_213521_aa092e97  shop      completed  manual   2026-11-07T21:35:21Z  <1s       380 B
```

Later, schedule it (Jobs → New job) so nobody has to remember, or run a saved job from the CLI with `mongorescue backup --job JOB_ID --wait`.

## 5. Verify the archive

```text
$ mongorescue verify bkp_shop_20261107_213521_aa092e97 --wait
Verifying backup bkp_shop_20261107_213521_aa092e97; waiting for the result.
Backup bkp_shop_20261107_213521_aa092e97 verified at 2026-11-07T21:35:30Z: the archive matches its checksum.
```

The server re-reads the archive from storage and compares it with the checksum recorded at backup time. [More about verification](verification.md).

## 6. Restore into a safe clone

```text
$ mongorescue restore bkp_shop_20261107_213521_aa092e97 --wait --verify-restore
Started restore rst_shop_20261107_213533_2b289752 of backup bkp_shop_20261107_213521_aa092e97 into shop_rescue_20261107_213533_2da5; waiting for it to finish.
restore rst_shop_20261107_213533_2b289752: in_progress (queued)
Restore rst_shop_20261107_213533_2b289752 of backup bkp_shop_20261107_213521_aa092e97 completed: shop -> shop_rescue_20261107_213533_2da5 (safe clone) in <1s.
Verification: passed.
```

The data went into a **new database**, `shop_rescue_<timestamp>_<id>`; `shop` was not touched. That is the [safe clone](glossary.md#safe-clone), the default of every restore. `--verify-restore` compares the clone with what the backup recorded. Look at it with `mongosh` (Docker: `docker compose exec mongo mongosh --quiet -u root -p demo-password --authenticationDatabase admin --eval '...'`):

```text
$ mongosh --quiet --eval 'db.getSiblingDB("shop_rescue_20261107_213533_2da5").orders.countDocuments()'
3
```

Add `--dry-run` to check a restore without writing anything, or `mongorescue list restores` to see the history. Once the clone looks right, copy what you need back (or rename the databases) yourself; MongoRescue never does that without being told.

!!! warning "Overwriting the original is an explicit decision"
    `--in-place` restores into the original database and needs `--confirm` as well; without it the command exits `2` before sending anything:

    ```text
    $ mongorescue restore bkp_shop_20261107_213521_aa092e97 --in-place
    mongorescue: usage: --in-place overwrites data in an existing database; add --confirm to restore in place (there is no prompt)
    ```

    In-place restores need an **admin** key. See [restores](cli.md#restore) and [why safe clone](faq.md#why-does-a-restore-go-into-a-safe-clone-instead-of-the-original-database).

## Clean up

=== "Linux"

    Stop the server with Ctrl-C. Delete the folder `~/mongorescue` (its `data` directory holds the database and `secret.key`) and `~/.config/mongorescue/key` when you are done experimenting.

=== "Windows (PowerShell)"

    Stop the server with Ctrl-C. Delete `$env:LOCALAPPDATA\mongorescue` and `$env:APPDATA\mongorescue\key` when you are done experimenting.

=== "Docker"

    ```bash
    docker compose -f docker-compose.yml -f examples/with-mongodb.yml down -v
    ```

    `-v` also deletes the volumes: the demo data, the backups and `secret.key`.

## Where to go next

- **Run it for real:** [production checklist](production.md#production-checklist), behind [a TLS proxy](production.md#tls-and-the-reverse-proxy) ([Caddy compose example](deployment-recipes.md#caddy-with-automatic-https)).
- **Schedule and retain:** jobs and retention in the dashboard, [retention safety](verification.md#retention).
- **Send backups off the machine:** an S3 target (Settings → Storage), ideally with [Object Lock](security.md#immutable-backups-s3-object-lock).
- **Encrypt them:** [age encryption](encryption.md).
- **Automate:** [CLI reference](cli.md), [CI and Kubernetes recipes](deployment-recipes.md).
- **Know what to do on a bad day:** [incident runbooks](runbooks.md) and the [FAQ](faq.md).
