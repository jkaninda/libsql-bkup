# libsql-bkup

Back up and restore [libSQL](https://github.com/tursodatabase/libsql) databases (`sqld` / `libsql-server`, Turso) to local, S3, GCS, Azure, SFTP or FTP storage.

`libsql-bkup` talks to the server over HTTP (the Hrana protocol), writes a logical SQL dump in the format of the `sqlite3` shell's `.dump`, and restores by replaying it. Everything is streamed: dump → gzip → GPG/age → upload, and back, with no temporary files and bounded memory. It is built on [bkupkit](https://github.com/jkaninda/bkupkit), the shared core of [pg-bkup](https://github.com/jkaninda/pg-bkup), [mysql-bkup](https://github.com/jkaninda/mysql-bkup), [mongodb-bkup](https://github.com/jkaninda/mongodb-bkup) and [volume-bkup](https://github.com/jkaninda/volume-bkup), so it has the same commands, variables and file names, and works with [Miabi](https://github.com/miabi-io/miabi) managed backups.

## Features

- **Consistent dumps:** every read happens inside one transaction on one server connection, so the dump is a snapshot even while the application writes.
- **Faithful dumps:** tables with their data, `AUTOINCREMENT` counters (`sqlite_sequence`), views, triggers, indexes, `WITHOUT ROWID` and `STRICT` tables, FTS and R*Tree virtual tables. Generated and hidden columns are left out of `INSERT`s, rowids are kept, and every value round-trips: 64-bit integers, REALs (including ±Inf), text with quotes, newlines or NUL bytes, and blobs.
- **Streaming, batched restore:** statements are parsed from the stream as they arrive (strings, quoted identifiers, comments and `CREATE TRIGGER … END` bodies are handled like SQLite's own `sqlite3_complete()`), and sent 500 per request and transaction. `--atomic` restores everything in one transaction.
- Storage: local, S3 (and compatibles), GCS, Azure Blob, SFTP, FTP.
- Encryption: GPG (passphrase or public keys) and age (recipients or passphrase); fail closed.
- Backup index with retention by age or count, `list`, `verify`, `delete`, `prune`, `reconcile`.
- Cron daemon mode with health checks, Prometheus metrics and notifications (email via SMTP or Posta, Telegram, Slack, Discord, ntfy, webhooks, healthchecks.io).

## Quick start

```sh
# Back up the database "mydb" to ./backups (mounted on /backup)
docker run --rm -v ./backups:/backup \
  -e DB_URL=http://libsql:8080 -e LIBSQL_AUTH_TOKEN="$TOKEN" \
  jkaninda/libsql-bkup backup -d mydb

# Restore it
docker run --rm -v ./backups:/backup \
  -e DB_URL=http://libsql:8080 -e LIBSQL_AUTH_TOKEN="$TOKEN" \
  jkaninda/libsql-bkup restore -d mydb -f mydb_20261027_031500.sql.gz
```

The artifact is `<name>_<YYYYMMDD_HHMMSS>.sql.gz` (`.sql.gz.gpg` with GPG, `.sql.gz.age` with age). The last line of a successful backup is

```
level=INFO msg="Backup completed" database=mydb file=mydb_20261027_031500.sql.gz size=… duration=… storage=local location=/backup
```

`backup` and `restore` also exist as bare commands in the image (`docker run … jkaninda/libsql-bkup backup …` and `--entrypoint backup` both work), and `bkup` is a short alias of `libsql-bkup`.

### Miabi and 1.x usage (unchanged)

```sh
# env: DB_HOST, DB_PORT, DB_NAME, DB_URL=http://host:port, LIBSQL_AUTH_TOKEN, AWS_*, GPG_PASSPHRASE
libsql-bkup backup -d <db>
libsql-bkup backup --storage s3 -d <db> --path <folder>
libsql-bkup restore -d <db> -f <file>
libsql-bkup restore --storage s3 -d <db> --path <folder> -f <file>
```

## Connecting

| Setting | Meaning |
|---|---|
| `DB_URL` | Server URL. `http://` and `https://` as is; `libsql://host` means `https://host` (`libsql://host:port?tls=0` for plain HTTP); `ws://`/`wss://` are mapped to `http://`/`https://` (sqld serves Hrana over HTTP on the same port). A token may also be given as `?authToken=…` (or `auth_token`, `jwt`). |
| `DB_HOST`, `DB_PORT` | Used when `DB_URL` is empty: `http://DB_HOST:DB_PORT` (default `localhost:8080`). |
| `LIBSQL_AUTH_TOKEN` | JWT sent as `Authorization: Bearer` (`DB_AUTH_TOKEN` still works; `LIBSQL_AUTH_TOKEN_FILE` reads it from a file). |
| `DB_NAME` / `-d` | Only names the artifacts (`<DB_NAME>_<timestamp>.sql.gz`) and index entries. When empty, the first label of the server host is used (`mydb-org` for `libsql://mydb-org.turso.io`), else `libsql`. |

**Namespaces.** A multi-tenant `sqld` (`--enable-namespaces`) selects the database from the subdomain of the request host, so address a namespace through `DB_URL`: `DB_URL=http://tenant1.db.example.com:8080` (the name must resolve to the server, e.g. a wildcard DNS record, an `/etc/hosts` entry or `--add-host tenant1.db.example.com:<ip>`; `*.localhost` resolves to the loopback address on most systems). Set `DB_NAME=tenant1` to name the artifacts after it.

## How the dump works

The dump is plain SQL that the `sqlite3` shell, `sqld` and `libsql-bkup` itself can replay:

```sql
PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
DROP TABLE IF EXISTS "items";
CREATE TABLE items(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, total INT GENERATED ALWAYS AS (…));
INSERT INTO "items"("id","name") VALUES(1,'it''s');
…
DELETE FROM sqlite_sequence WHERE name = 'items';
INSERT INTO sqlite_sequence(name,seq) VALUES('items',42);
DROP VIEW IF EXISTS "v"; CREATE VIEW v AS …;          -- then triggers, then indexes
COMMIT;
```

- All reads run in one `BEGIN … ROLLBACK` transaction on one Hrana stream (server connection): the first read fixes the snapshot, concurrent writes are not blocked and do not show up. (Hrana cannot request a read-only transaction; the dump only reads.)
- Tables are read page by page (keyset on the rowid or primary key; the page size adapts to keep each response near 4 MiB), so memory does not grow with the table.
- Each object is preceded by `DROP … IF EXISTS`, so a restore replaces existing tables, views, triggers and indexes of the same name (as 1.x did); other objects in the target are left alone.
- Left out: SQLite's internal tables (`sqlite_stat1`, …), the shadow tables of virtual tables (the virtual table is dumped and its index rebuilt on restore), and server-internal tables (`libsql_*`, `_litestream_*`). Contentless FTS tables (`content=''`) cannot be read back and restore empty.
- `sqld` also has a `GET /dump` endpoint. It is not used: it writes infinities as the invalid literal `inf`, has no `DROP … IF EXISTS`, and gives no control over the snapshot.

## Restoring

```sh
libsql-bkup restore -d mydb -f mydb_20261027_031500.sql.gz.gpg            # 500 statements per transaction
libsql-bkup restore -d mydb -f mydb_20261027_031500.sql.gz --atomic       # all or nothing
libsql-bkup restore -d mydb -f mydb_20261027_031500.sql.gz --batch-size 2000
```

- Accepted input: dumps made by any libsql-bkup version, `sqlite3 .dump` output and hand-written `.sql` files, plain, gzip- or zstd-compressed, GPG- or age-encrypted (detected from the content, not the extension).
- The dump's own `BEGIN`/`COMMIT` and its `PRAGMA`s are skipped: foreign keys are switched off for the restore connection and transactions are managed by the tool. A dump that ends in `ROLLBACK` (the `sqlite3` shell writes that when its dump failed) is rejected.
- **Default (batches):** each batch of `RESTORE_BATCH_SIZE` statements (default 500) is one request running `BEGIN; …; COMMIT`. A failing statement rolls back its batch and stops the restore with the statement's line number. A batch is only sent once it has been read completely, so a decryption, checksum or decompression error never commits a partial batch; but batches committed before the error stay, and the error says how many.
- **`--atomic` / `RESTORE_ATOMIC=true`:** one transaction for the whole restore; any error (bad statement, damaged or tampered artifact, lost connection) leaves the database unchanged. It is off by default because a long transaction holds the database's write lock for the whole restore, and `sqld` closes a stream (and rolls back its transaction) that stays idle too long, which very large restores over slow links can hit. Use it for small and medium databases, or when an all-or-nothing restore matters more.
- GPG only authenticates the data at the end of the file. For GPG-encrypted artifacts, use `--atomic` or `--verify-first` (`RESTORE_VERIFY_FIRST=true`: the artifact is downloaded and checked once before the restore) to be sure nothing tampered is ever committed. age authenticates every 64 KiB chunk before it is used.

## Storage

```sh
# S3 / MinIO
docker run --rm -e DB_URL=http://libsql:8080 -e LIBSQL_AUTH_TOKEN="$TOKEN" \
  -e AWS_S3_ENDPOINT=https://s3.example.com -e AWS_S3_BUCKET_NAME=backups \
  -e AWS_ACCESS_KEY=… -e AWS_SECRET_KEY=… -e AWS_FORCE_PATH_STYLE=true \
  jkaninda/libsql-bkup backup --storage s3 -d mydb --path libsql/mydb

# SFTP (host keys are verified: mount a known_hosts file)
  -e SFTP_HOST=backup.example.com -e SFTP_USER=backup -e SFTP_IDENTITY_FILE=/config/id_ed25519 \
  -e SFTP_KNOWN_HOSTS=/config/known_hosts … backup --storage sftp -d mydb --path /srv/backups

# Google Cloud Storage, Azure Blob, FTP
  -e GCS_BUCKET=… -e GCS_CREDENTIALS_FILE=/config/gcs.json … backup --storage gcs -d mydb
  -e AZURE_STORAGE_ACCOUNT_NAME=… -e AZURE_STORAGE_ACCOUNT_KEY=… -e AZURE_STORAGE_CONTAINER_NAME=… … backup --storage azure -d mydb
  -e FTP_HOST=… -e FTP_USER=… -e FTP_PASSWORD=… … backup --storage ftp -d mydb --path /backups
```

`STORAGE` works as well as `--storage`. With local storage, files go to `STORAGE_PATH` (default `/backup`) plus `--path`.

## Encryption

| Mode | Backup | Restore |
|---|---|---|
| GPG passphrase | `GPG_PASSPHRASE` | `GPG_PASSPHRASE` |
| GPG public key | `GPG_PUBLIC_KEY` (path or inline key; `/config/gnupg/public_key.{asc,gpg}` is picked up automatically) | `GPG_PRIVATE_KEY` (+ `GPG_PRIVATE_KEY_PASSPHRASE`, default `GPG_PASSPHRASE`) |
| age recipients | `AGE_RECIPIENTS` (`age1…`, `ssh-ed25519 …`) or `AGE_RECIPIENTS_FILE` | `AGE_IDENTITY` or `AGE_IDENTITY_FILE` (default `/config/age/identity.txt`) |
| age passphrase | `AGE_PASSPHRASE` | `AGE_PASSPHRASE` |

`ENCRYPTION=auto` (default) picks GPG when a GPG key or passphrase is set, otherwise age when an age recipient or passphrase is set. Encryption fails closed: a key that is configured but cannot be loaded aborts the backup instead of uploading plaintext. A restore tries every configured key, so old passphrase backups still restore after moving to keys.

## Backup index and retention

Each storage folder holds an index (`BACKUP_INDEX_NAME`, default `libsql-bkup-metadata.json`; placeholders `{tool}` `{engine}` `{name}` `{db}`) and a `<artifact>.meta.json` sidecar per backup with its size and sha256. `restore` and `verify` check the sha256.

- `BACKUP_RETENTION_DAYS=7` and/or `BACKUP_RETENTION_COUNT=10` prune after each backup, or run `libsql-bkup prune [--dry-run]`.
- Retention only deletes backups recorded in the index and always keeps the newest one. Backups made by 1.x are adopted once with `BACKUP_RETENTION_ADOPT_LEGACY=true`.
- With `BACKUP_INDEX=false`, retention falls back to file names: only `<name>_<timestamp>.sql*` files directly in `--path`.

## Scheduled backups

```sh
docker run -d --name libsql-backup -v ./backups:/backup \
  -e DB_URL=http://libsql:8080 -e LIBSQL_AUTH_TOKEN="$TOKEN" -e BACKUP_RETENTION_DAYS=7 \
  jkaninda/libsql-bkup backup -d mydb --cron-expression '@daily'
```

Cron expressions take 5 or 6 fields, `@daily`, `@every 6h`. A failed run is logged and notified and the daemon keeps going; overlapping runs are skipped. `BACKUP_RUN_ON_START=true` runs once at start, `METRICS_ADDR=:9090` serves Prometheus metrics, and the image's `HEALTHCHECK` (`libsql-bkup health`) reports the last run.

## Commands

| Command | |
|---|---|
| `backup` | Back up now, or as a daemon with `--cron-expression` |
| `restore -f FILE` | Stream a backup into the database (`--atomic`, `--batch-size`, `--verify-first`) |
| `verify -f FILE` | Download, decrypt, decompress and check the sha256, without restoring |
| `list` | History from the index (`--json`; `--storage-scan` lists the storage itself) |
| `delete -f FILE` | Delete a backup, its sidecar and its index entry |
| `prune` | Apply retention (`--dry-run`) |
| `reconcile` | Rebuild the index from the sidecars (`--from-index OLD` after renaming it) |
| `config` | Show the resolved configuration with secrets redacted (`--env-docs` prints the reference below) |
| `health` | Health check for daemon mode |
| `version` | Print the version |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Failure (dump or restore error) |
| 2 | Configuration or usage error |
| 3 | Cannot connect to or authenticate with the server |
| 4 | Storage error (upload, download, missing file) |
| 5 | Encryption or decryption error |

## Upgrading from 1.x

Every 1.x variable and flag still works; renamed variables log a deprecation warning once.

- **Streaming:** 1.x wrote the dump to `/tmp/backup`, encrypted it in memory and read the whole dump into memory to restore it (about three times its size). Nothing is buffered now.
- **Consistent snapshot:** 1.x read each table separately, so a dump taken during writes could mix states.
- **More complete dumps:** `sqlite_sequence` is now included (1.x's `LIKE 'sqlite_%'` skipped it, so `AUTOINCREMENT` counters restarted), generated columns no longer break the `INSERT`s, infinities are written as valid literals (`9e999`), rowids of tables without an `INTEGER PRIMARY KEY` are kept, and the dump has the `sqlite3 .dump` header (`PRAGMA foreign_keys=OFF; BEGIN TRANSACTION; … COMMIT;`). 1.x dumps still restore.
- **Restore:** 1.x replayed the whole dump in one transaction after splitting it with a tokenizer that broke on columns named `begin`, quoted identifiers and `CASE … END` in triggers. The restore now streams, runs 500 statements per transaction, and `--atomic` gives the 1.x all-or-nothing behaviour.
- **Variables:** `AWS_S3_PATH` → `REMOTE_PATH`, `BACKUP_TMP_DIR` → `TMP_DIR`, `DB_AUTH_TOKEN` → `LIBSQL_AUTH_TOKEN` (old names accepted). `DB_NAME` is no longer required. `LIBSQL_AUTH_TOKEN_FILE`, `AWS_SECRET_KEY_FILE`, `GPG_PASSPHRASE_FILE` read secrets from files.
- **`STORAGE` is honoured** (1.x always used the flag's default `local`). Unknown storage names are errors instead of falling back to local. New backends: GCS, Azure, SFTP, FTP.
- **GPG:** the `GPG_PUBLIC_KEY` variable now wins over the auto-detected `/config/gnupg` key; a key that cannot be loaded aborts the backup. Restores detect encryption from the content and try every configured key.
- **Retention** is new and only deletes backups recorded in the index (`BACKUP_INDEX=false` for name-based retention).
- **SFTP host keys are verified** (`SFTP_KNOWN_HOSTS`, or `SFTP_INSECURE_IGNORE_HOST_KEY=true`). **`MAIL_SKIP_TLS`** means what it says (`true` skips certificate verification); prefer `MAIL_INSECURE_SKIP_VERIFY`.
- The output: 1.x printed `Backup saved: <file>`; the last line is now `Backup completed … file=<file>` (the file name is still the last `*.sql.gz[.gpg]` in the output).
- The image runs `tini` as PID 1, has a `HEALTHCHECK` (daemon mode only), and is tagged `latest`, `vX.Y.Z`, `vX.Y` and `vX`; pin a version in production.

## Environment variables

Generated with `libsql-bkup config --env-docs`. Every secret also accepts `<NAME>_FILE` (Docker/Kubernetes secrets). Resolution order: flag > variable > deprecated name > config file > default.

<!-- env-docs:start -->
### storage

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `STORAGE` | `--storage` `-s` | `local` |  | Storage backend: local, s3, gcs, azure, sftp (ssh), ftp (local, s3, gcs, azure, sftp, ssh, ftp) |
| `STORAGE_PATH` |  | `/backup` |  | Root directory of the local storage backend |
| `REMOTE_PATH` | `--path` `-P` |  | `AWS_S3_PATH` (deprecated), `S3_PATH` (deprecated) | Folder inside the storage backend (without file name) |

### general

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `BACKUP_NAME` | `--name` `-n` |  |  | Base name of the artifact; a timestamp is appended (default: the database or volume name) |
| `BACKUP_CUSTOM_NAME` | `--custom-name` |  |  | Fixed artifact name without timestamp (overwrites the previous one) |
| `FILE_NAME` | `--file` `-f` |  |  | Backup file to restore, verify or delete |
| `BACKUP_CONFIG_FILE` | `--config` `-c` |  |  | YAML configuration file (default /config/config.yaml if present) |
| `BACKUP_TIMEOUT` |  |  |  | Abort a backup after this duration (0 = none) |
| `RESTORE_TIMEOUT` |  |  |  | Abort a restore after this duration (0 = none) |
| `TMP_DIR` |  | `/tmp/backup` | `BACKUP_TMP_DIR` (deprecated) | Scratch directory (only for secret files and disk-based modes) |
| `COMPRESSION` | `--compression` | `gzip` |  | Compression: gzip, zstd or none (gzip, zstd, none) |
| `COMPRESSION_LEVEL` |  |  |  | Compression level (0 = default) |
| `DISABLE_COMPRESSION` | `--disable-compression` |  |  | Same as COMPRESSION=none |
| `RESTORE_VERIFY_FIRST` | `--verify-first` | `auto` |  | Verify the whole artifact before restoring (auto: on for GPG-encrypted volumes) (auto, true, false) |
| `LOG_LEVEL` |  | `info` |  | Log level (debug, info, warn, error) |
| `LOG_FORMAT` |  | `text` |  | Log format (text, json) |
| `TIME_FORMAT` |  | `2006-01-02 15:04:05` |  | Time layout used in notifications |
| `RESULT_FILE` | `--result-file` |  |  | Write the run result as JSON to this path |

### scheduling

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `BACKUP_CRON_EXPRESSION` | `--cron-expression` `-e` |  |  | Run as a daemon on this cron schedule (5 or 6 fields, @daily, @every 6h) |
| `BACKUP_RUN_ON_START` |  |  |  | In daemon mode, run one backup immediately at start |
| `BACKUP_RESCUE_MODE` |  |  |  | Deprecated: runs always continue with the remaining targets after a failure |
| `METRICS_ADDR` |  |  |  | Serve Prometheus /metrics and /healthz on this address in daemon mode (e.g. :9090) |
| `HEALTH_FILE` |  | `/tmp/bkup-health.json` |  | State file read by the health command |
| `HEALTH_MAX_AGE` |  |  |  | health fails when the last success is older than this (default: 2x the schedule interval) |

### retention

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `BACKUP_RETENTION_DAYS` |  | `0` |  | Delete backups older than N days (0 = keep) |
| `BACKUP_RETENTION_COUNT` |  | `0` |  | Keep only the newest N backups (0 = no limit) |
| `BACKUP_RETENTION_ALLOW_ROOT` |  |  |  | Allow pattern-based retention at the storage root (index disabled) |
| `BACKUP_RETENTION_ADOPT_LEGACY` |  |  |  | Import pre-index backups matching this tool's naming into the index |
| `BACKUP_INDEX` |  | `true` |  | Keep the backup index file and per-backup .meta.json sidecars |
| `BACKUP_INDEX_NAME` | `--index-name` | `{tool}-metadata.json` |  | Index file name; placeholders {tool} {engine} {name} {db} |

### storage-s3

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `AWS_S3_ENDPOINT` |  |  | `S3_ENDPOINT` (deprecated), `AWS_ENDPOINT_URL_S3`, `AWS_ENDPOINT_URL` | S3 endpoint (empty = AWS) |
| `AWS_S3_BUCKET_NAME` |  |  | `BUCKET_NAME` (deprecated), `AWS_BUCKET_NAME` (deprecated) | S3 bucket |
| `AWS_REGION` |  | `us-east-1` | `AWS_DEFAULT_REGION` | S3 region |
| `AWS_ACCESS_KEY` |  |  | `ACCESS_KEY` (deprecated), `AWS_ACCESS_KEY_ID` | S3 access key (empty = default AWS credential chain) |
| `AWS_SECRET_KEY` |  |  | `SECRET_KEY` (deprecated), `AWS_SECRET_ACCESS_KEY`, `AWS_SECRET_KEY_FILE` | S3 secret key |
| `AWS_SESSION_TOKEN` |  |  | `AWS_SESSION_TOKEN_FILE` | S3 session token |
| `AWS_PROFILE` |  |  |  | AWS shared config profile |
| `AWS_DISABLE_SSL` |  |  |  | Use http for the S3 endpoint |
| `AWS_FORCE_PATH_STYLE` |  |  |  | Path-style S3 addressing (MinIO) |
| `S3_STORAGE_CLASS` |  |  |  | S3 storage class |
| `S3_SSE` |  |  |  | Server-side encryption: AES256 or aws:kms (aes256, aws:kms) |
| `S3_SSE_KMS_KEY_ID` |  |  |  | KMS key id for aws:kms |
| `S3_PART_SIZE` |  | `64MiB` |  | Multipart part size; max object size is 10000 x this |
| `S3_UPLOAD_CONCURRENCY` |  | `2` |  | Parallel part uploads (memory = part size x this) |

### storage-sftp

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `SFTP_HOST` |  |  |  | SFTP host |
| `SFTP_PORT` |  | `22` |  | SFTP port |
| `SFTP_USER` |  |  |  | SFTP user |
| `SFTP_PASSWORD` |  |  | `SFTP_PASSWORD_FILE` | SFTP password |
| `SFTP_IDENTITY_FILE` |  |  |  | SFTP private key file (the SSH agent is also used when SSH_AUTH_SOCK is set) |
| `SFTP_IDENTITY_PASSPHRASE` |  |  | `SFTP_IDENTITY_PASSPHRASE_FILE` | Passphrase of the SFTP private key |
| `SFTP_KNOWN_HOSTS` |  |  |  | known_hosts file used to verify the SFTP host key (default ~/.ssh/known_hosts) |
| `SFTP_INSECURE_IGNORE_HOST_KEY` |  |  |  | Do not verify the SFTP host key (insecure) |

### storage-ftp

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `FTP_HOST` |  |  | `FTP_HOST_NAME` (deprecated) | FTP host |
| `FTP_PORT` |  | `21` |  | FTP port |
| `FTP_USER` |  |  |  | FTP user |
| `FTP_PASSWORD` |  |  | `FTP_PASSWORD_FILE` | FTP password |
| `FTP_TLS` |  |  |  | Use explicit FTPS |

### storage-azure

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `AZURE_STORAGE_ACCOUNT_NAME` |  |  |  | Azure storage account |
| `AZURE_STORAGE_ACCOUNT_KEY` |  |  | `AZURE_STORAGE_ACCOUNT_KEY_FILE` | Azure storage account key |
| `AZURE_STORAGE_CONTAINER_NAME` |  |  |  | Azure blob container |
| `AZURE_STORAGE_ENDPOINT` |  |  |  | Azure blob endpoint (Azurite, sovereign clouds) |
| `AZURE_STORAGE_SAS_TOKEN` |  |  | `AZURE_STORAGE_SAS_TOKEN_FILE` | Azure SAS token (instead of the account key) |

### storage-gcs

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `GCS_BUCKET` |  |  |  | GCS bucket |
| `GCS_CREDENTIALS_FILE` |  |  | `GOOGLE_APPLICATION_CREDENTIALS` | GCS service account JSON file |
| `GCS_CREDENTIALS_JSON` |  |  | `GCS_CREDENTIALS_JSON_FILE` | GCS service account JSON content |
| `GCS_ENDPOINT` |  |  |  | GCS endpoint (emulators) |

### encryption

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `ENCRYPTION` | `--encryption` | `auto` |  | auto (from the keys set), none, gpg or age (auto, none, gpg, age) |
| `ENCRYPTION_ARMOR` |  |  |  | ASCII-armor encrypted output |
| `GPG_PASSPHRASE` |  |  | `GPG_PASSPHRASE_FILE` | GPG symmetric passphrase |
| `GPG_PUBLIC_KEY` |  |  |  | GPG public key(s): file path or inline key (default /config/gnupg/public_key.{asc,gpg}) |
| `GPG_PRIVATE_KEY` |  |  | `GPG_PRIVATE_KEY_FILE` | GPG private key for restore: file path or inline key (default /config/gnupg/private_key.{asc,gpg}) |
| `GPG_PRIVATE_KEY_PASSPHRASE` |  |  | `GPG_PRIVATE_KEY_PASSPHRASE_FILE` | Passphrase of the GPG private key (default GPG_PASSPHRASE) |
| `AGE_RECIPIENTS` |  |  |  | age recipients (age1..., ssh-ed25519 ...), comma or newline separated |
| `AGE_RECIPIENTS_FILE` |  |  |  | File with age recipients |
| `AGE_PASSPHRASE` |  |  | `AGE_PASSPHRASE_FILE` | age passphrase (scrypt) |
| `AGE_IDENTITY` |  |  | `AGE_IDENTITY_FILE` | age identity (AGE-SECRET-KEY-...) for restore |
| `AGE_IDENTITY_FILE` |  |  |  | age identity file for restore (default /config/age/identity.txt) |

### notifications

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `MAIL_HOST` |  |  |  | SMTP host |
| `MAIL_PORT` |  | `587` |  | SMTP port |
| `MAIL_USERNAME` |  |  |  | SMTP user |
| `MAIL_PASSWORD` |  |  | `MAIL_PASSWORD_FILE` | SMTP password |
| `MAIL_FROM` |  |  |  | Sender address |
| `MAIL_TO` |  |  |  | Recipients, comma separated |
| `MAIL_SKIP_TLS` |  |  |  | Deprecated: true skips certificate verification (use MAIL_INSECURE_SKIP_VERIFY) |
| `MAIL_INSECURE_SKIP_VERIFY` |  |  |  | Skip SMTP certificate verification |
| `MAIL_TLS` |  | `starttls` |  | SMTP TLS mode (starttls, tls, none) |
| `TG_TOKEN` |  |  | `TELEGRAM_BOT_TOKEN`, `TG_TOKEN_FILE` | Telegram bot token |
| `TG_CHAT_ID` |  |  | `TELEGRAM_CHAT_ID` | Telegram chat id |
| `POSTA_URL` |  |  |  | Posta base URL (https://posta.example.com) for email notifications through Posta |
| `POSTA_API_KEY` |  |  | `POSTA_API_KEY_FILE` | Posta API key with the send scope |
| `POSTA_WORKSPACE_ID` |  |  |  | Posta workspace id (only for account-wide API keys) |
| `POSTA_FROM` |  |  |  | Posta sender address (default MAIL_FROM) |
| `POSTA_TO` |  |  |  | Posta recipients, comma separated (default MAIL_TO) |
| `POSTA_TEMPLATE` |  |  |  | Posta template name for notifications; the event fields are passed as template data (default: built-in HTML) |
| `POSTA_TEMPLATE_ERROR` |  |  |  | Posta template name for failures (default POSTA_TEMPLATE) |
| `POSTA_LANGUAGE` |  |  |  | Posta template language |
| `WEBHOOK_URL` |  |  |  | Generic JSON webhook URL |
| `WEBHOOK_SECRET` |  |  | `WEBHOOK_SECRET_FILE` | HMAC-SHA256 secret for the X-Bkup-Signature header |
| `SLACK_WEBHOOK_URL` |  |  | `SLACK_WEBHOOK_URL_FILE` | Slack incoming webhook URL |
| `DISCORD_WEBHOOK_URL` |  |  | `DISCORD_WEBHOOK_URL_FILE` | Discord webhook URL |
| `NTFY_URL` |  |  |  | ntfy topic URL |
| `NTFY_TOKEN` |  |  | `NTFY_TOKEN_FILE` | ntfy access token |
| `NOTIFY_ON` |  | `always` |  | When to notify (always, failure, success, never) |
| `HEALTHCHECK_PING_URL` |  |  |  | healthchecks.io-style URL pinged on start (/start), success and failure (/fail) |
| `BACKUP_REFERENCE` |  |  |  | Free text included in notifications |
| `TEMPLATES_DIR` |  | `/config/templates` |  | Directory with custom notification templates |

### database

| Variable | Flag | Default | Fallbacks | Description |
|---|---|---|---|---|
| `DB_URL` |  |  | `DB_URL_FILE` | libSQL server URL: http(s)://, libsql:// or ws(s):// (default http://DB_HOST:DB_PORT). A namespace of a multi-tenant sqld is addressed by its subdomain |
| `DB_HOST` |  | `localhost` |  | libSQL server host (when DB_URL is not set) |
| `DB_PORT` |  | `8080` |  | libSQL server HTTP port (when DB_URL is not set) |
| `DB_NAME` | `--dbname` `-d` |  |  | Database name, used to name artifacts (default: first label of the server host, or "libsql") |
| `LIBSQL_AUTH_TOKEN` |  |  | `DB_AUTH_TOKEN` (deprecated), `LIBSQL_AUTH_TOKEN_FILE` | JWT for the libSQL server (Authorization: Bearer) |
| `RESTORE_BATCH_SIZE` | `--batch-size` | `500` |  | Statements per transaction and per request during restore |
| `RESTORE_ATOMIC` | `--atomic` |  |  | Restore in a single transaction (all or nothing; long transactions may time out on large databases) |

<!-- env-docs:end -->

## Development

```sh
make build              # ./bin/libsql-bkup
make test vet           # unit tests (no docker)
make test-integration   # sqld + MinIO in docker
make docker VERSION=dev # image for this host
make env-docs           # the table above
```

The integration tests start `ghcr.io/tursodatabase/libsql-server:latest` and `pgsty/minio:latest` (override with `LIBSQL_BKUP_SQLD_IMAGE` / `LIBSQL_BKUP_MINIO_IMAGE`) and skip when docker is missing. The statement splitter is fuzzed against a port of SQLite's `sqlite3_complete()` (`go test ./internal/sqlscript -fuzz FuzzSplit`).

## License

[MIT](./LICENSE) © Jonas Kaninda
