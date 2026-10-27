# platform-db-tunnel

A small HTTP service that runs SQL against a local database: SQL Server, PostgreSQL or MySQL. It runs as a Windows Service, starts at boot, and restarts itself on failure. The app has no authentication of its own and only listens on `127.0.0.1`, so put it behind something that authenticates callers.

## API

| Method | Path       | Description                                   |
|--------|------------|-----------------------------------------------|
| GET    | `/healthz` | Returns `ok`, or 503 if the database is down  |
| POST   | `/query`   | Runs SQL and returns rows as JSON             |

```bash
curl -X POST http://127.0.0.1:8080/query \
  -d '{"sql": "select * from users where id = @p1", "args": [42]}'
```

Response: `{"columns": ["id", "name"], "types": ["INT", "NVARCHAR"], "rows": [[42, "alice"]]}`

`types` is the driver's type name for each column (upper case, empty when the driver doesn't report one). JSON can't tell dates, decimals and bytes apart from text, so clients use it to: parse `DATETIME`/`DATE`/... strings back into dates, and decode **binary columns** (`BINARY`, `VARBINARY`, `IMAGE`, `BYTEA`, `BLOB`, ...), which are sent base64-encoded. SQL Server `UNIQUEIDENTIFIER` columns are sent as the canonical `xxxxxxxx-xxxx-...` text. Versions before this field was added sent binary and GUID values as corrupted text and no `types`.

Arguments bind positionally, using the placeholder style of the chosen database:

| Database   | Placeholders      |
|------------|-------------------|
| SQL Server | `@p1`, `@p2`, ... |
| PostgreSQL | `$1`, `$2`, ...   |
| MySQL      | `?`               |

Rows are streamed as they are read, so memory stays flat however large the result is, and there is no row cap. A query errors out if it runs longer than the timeout (30 minutes by default, `queryTimeoutSeconds` in `config.json`). The request body is limited to 1 MB.

Errors:
- A bad query, or a failure before any row is produced, returns status 400 or 500 with `{"error": "..."}`.
- A failure **after** rows have started streaming can't change the status, which is already 200. The JSON is closed properly and carries an `error` key, e.g. `{"columns": [...], "rows": [[1, "a"]], "error": "connection lost"}`. Clients must check for `error` even on a 200.

## Build

Requires Go 1.26 or later (see `go.mod`). Only the machine that builds needs Go; the resulting exe runs without it.

```powershell
go build -o platform-db-tunnel.exe .
```

There is no separate dependency install step: `go build` downloads the modules listed in `go.mod` on first run (internet access required; `go mod download` fetches them without building).

The result is a single self-contained exe, with no runtime to install on the target machine.

## CI and releases

`.github/workflows/build.yml` runs only when a version tag (`v*`) is pushed; ordinary pushes and pull requests do not trigger a build. To publish a release:

```powershell
git tag v1.0.0
git push origin v1.0.0
```

The workflow vets and builds the Windows exe, then creates a GitHub Release with `platform-db-tunnel.exe` and `platform-db-tunnel.exe.sha256` attached. Verify a download with `certutil -hashfile platform-db-tunnel.exe SHA256` and compare it with the `.sha256` file.

## Configure

Create `config.json` in the same folder as `platform-db-tunnel.exe`. It is the only source of settings; environment variables are not read.

```json
{
  "type": "postgres",
  "dsn": "postgres://USER:PASSWORD@localhost:5432/DBNAME",
  "port": "8080",
  "queryTimeoutSeconds": 1800
}
```

| Key    | Purpose                                              | Default |
|--------|------------------------------------------------------|---------|
| `type` | `mssql`, `postgres` or `mysql`                       | `mssql` |
| `dsn`  | Connection string for the database (required)        |         |
| `port` | Port to listen on (always bound to `127.0.0.1`)      | `8080`  |
| `queryTimeoutSeconds` | Max duration of one query, including reading all its rows | `1800` (30 min) |

The DSN (Data Source Name) is a single string with the host, port, credentials and database name. Its format depends on the database:

- SQL Server: `sqlserver://user:pass@localhost:1433?database=mydb`
- PostgreSQL: `postgres://user:pass@localhost:5432/mydb`
- MySQL: `user:pass@tcp(localhost:3306)/mydb`

Special characters in a password must be URL-encoded in the URL-style formats. Driver options go in the DSN too, e.g. `encrypt=disable` (SQL Server) or `sslmode=disable` (PostgreSQL).

### SQL Server notes

**Windows authentication.** Leave the user and password out of the DSN and the driver signs in as the Windows account the app runs as (not necessarily whoever is signed in): you when you use `run`, the installing user for `--at-logon` or `--current-user`, and `LocalSystem` for a plain `install`. That account needs a SQL Server login with access to the database.

```json
"dsn": "sqlserver://localhost:1433?database=mydb"
```

A named instance goes in the path (`sqlserver://localhost/SQLEXPRESS?database=mydb`) and needs the *SQL Server Browser* service running. A default instance (service name `MSSQLSERVER`) uses port 1433 and does not. A named instance normally listens on a dynamic port that can change when it restarts, so prefer the instance name over a fixed port number. A database name with spaces or other special characters must be URL-encoded: `ABS 26-27` is written `database=ABS%2026-27`.

**Tables and schemas.** A connection is tied to one database (the `database=` option). Within it, qualify tables with their schema (`dbo.orders`); the default schema is `dbo`. Tables in other databases on the same server can be reached as `otherdb.dbo.orders`.

**Troubleshooting.** The startup log prints the database error in full as `WARNING: database not reachable yet: ...`. The app keeps running, so fix the config or the server and restart it.

| Error contains | Cause and fix |
|---|---|
| `actively refused it` on port 1433 | SQL Server is not listening on TCP. Check `Get-NetTCPConnection -State Listen -OwningProcess (Get-Process sqlservr).Id`. If nothing is listed, TCP/IP is disabled (the default on some installs): enable it in *SQL Server Configuration Manager > SQL Server Network Configuration > Protocols > TCP/IP*, set *IPAll* `TCP Port` to `1433` with *TCP Dynamic Ports* blank, and restart the SQL Server service. A named instance listens on another port; use `localhost/INSTANCE` instead. |
| `tls: server selected unsupported protocol version 301` | An old SQL Server that only offers TLS 1.0, which Go refuses by default. Add `&tlsmin=1.0` to the DSN. |
| `x509: certificate signed by unknown authority` | The server has a self-signed certificate. Add `&TrustServerCertificate=true`. |
| `connection was forcibly closed` right after a TLS attempt, or `tlsmin=1.0` did not help | A very old SQL Server (2005 / 2008) whose TLS Go cannot negotiate even at 1.0. Use `&encrypt=disable`, which turns TLS off entirely. This only works if the server does not force encryption, and is acceptable only because the connection stays on the same machine (with a SQL login the password then crosses localhost unencrypted). Verified on SQL Server 2005 SP3 (9.00.4035) with Windows authentication. |
| `Login failed for user` | The account running the app has no SQL Server login, or no access to that database. Grant it one, e.g. `CREATE LOGIN [DOMAIN\user] FROM WINDOWS`, then `CREATE USER` and `db_datareader` in the database. |

A DSN that worked against an old server:

```json
"dsn": "sqlserver://localhost:1433?database=mydb&tlsmin=1.0&TrustServerCertificate=true"
```

Restart the service after editing it.

The app only listens on `127.0.0.1`, so only processes on the same machine can reach it.

## Install as a Windows Service

1. Copy `platform-db-tunnel.exe` to a permanent folder, e.g. `C:\platform-db-tunnel\` (the service points at the exe's location, so don't move it afterwards). `C:\Program Files\platform-db-tunnel\` also works.
2. Create `config.json` next to it, as described above.
3. From an **elevated** (Administrator) prompt:

```powershell
cd C:\platform-db-tunnel
.\platform-db-tunnel.exe install
```

This registers the `PlatformDBTunnel` service with automatic start and starts it. On failure it restarts after 5 s, 5 s, then 30 s.

By default the service runs as `LocalSystem`. For SQL Server Windows authentication (a DSN without user/password), install it as your own account instead; you will be prompted for that account's Windows password:

```powershell
.\platform-db-tunnel.exe install --current-user
```

The account is whoever is signed in, even from an elevated prompt. It must have a password: Windows refuses service logons for blank-password accounts ("The service did not start due to a logon failure"). If the password later changes, update it with `sc.exe config PlatformDBTunnel obj= "DOMAIN\you" password= "..."`.

### Alternative: start at sign-in (no password needed)

If the service can't run as the account you need (for example a Windows-authentication user with a blank password), install a scheduled task that starts the app whenever that user signs in:

```powershell
.\platform-db-tunnel.exe install --at-logon
```

This needs no elevation when run for your own account. It registers a task named `PlatformDBTunnel` that runs `platform-db-tunnel.exe run` as the user who ran the command, at each sign-in, and starts it straight away. It restarts up to 3 times (1 minute apart) on failure, and has no run-time limit. Remove it with `.\platform-db-tunnel.exe uninstall --at-logon`.

Compared with the service:
- It only runs while that user is signed in: nothing starts at boot, and it stops at sign-out.
- It opens a console window that stays open; closing the window stops the app.
- If a second user signs in, their copy exits with a port-in-use error, which is harmless.

Check the service:

```powershell
Get-Service PlatformDBTunnel
curl http://127.0.0.1:8080/healthz
```

### Logs

`run` prints logs to the console: the startup settings (never the DSN), whether the database was reachable, the listen address, and one line per query with its outcome, row count, duration and the first 200 characters of the SQL. Bound arguments are never logged. Example:

```
2026/10/25 14:11:45 query ok rows=3 took=12ms sql="select * from users where id = $1"
```

Both `run` and the service also write the same lines to `platform-db-tunnel.log` in the exe's folder, so the logs survive after a console window closes. The file rotates at 10 MB, keeping 5 compressed backups for up to 30 days. Follow it live with:

```powershell
Get-Content C:\platform-db-tunnel\platform-db-tunnel.log -Wait -Tail 50
```

If the account running the app can't write to that folder (for example a non-admin user in `C:\Program Files`), it logs a warning and uses the console only. The service also writes start, stop and fatal errors to the Windows Event Log (Event Viewer → Windows Logs → Application, source `PlatformDBTunnel`).

### Diagnose a SQL Server connection

`diagnose` inspects the machine and tries to connect, so a problem can be understood without guessing. It only reads: it never changes SQL Server or Windows. It works without administrator rights (registry values that need them are marked; rerun from an elevated prompt for full detail).

```powershell
.\platform-db-tunnel.exe diagnose                    # uses config.json if present, and probes the machine
.\platform-db-tunnel.exe diagnose --user sa          # also try a SQL login (the password is asked for, never printed)
.\platform-db-tunnel.exe diagnose --dsn "sqlserver://localhost:1433?database=mydb"
.\platform-db-tunnel.exe diagnose --database ACCDATA # database to connect to while probing (default: master)
```

It reports, in order:

- **This machine:** Windows version, the account it runs as, whether it is elevated, and whether the exe folder is writable.
- **config.json:** whether it loads, the DSN with the password hidden, and whether the listen port is free or already used by this app.
- **Windows services:** the SQL Server engine, SQL Browser and agent services with their state, start type and account.
- **Installed instances (registry):** version and edition, whether TCP/IP is enabled, the TCP port, Windows-only versus mixed authentication, and Force Encryption.
- **Network:** the instances SQL Browser announces, and the TCP ports `sqlservr.exe` is actually listening on. A running SQL Server with no listening port means TCP/IP is disabled.
- **Connection probes:** each place SQL Server can be reached is tried with the option sets that old or default servers need (default, `TrustServerCertificate=true`, `tlsmin=1.0`, `encrypt=disable`), then the login, the server version, whether the login is sysadmin and the databases it can open. The first combination that works is printed as a ready-to-paste `"dsn"` for `config.json`.

Every failure comes with the usual cause and fix. The whole report is also saved as `platform-db-tunnel-diagnose.txt` beside the exe (or in the temp folder when that folder is not writable), so it can be sent for review. It contains machine and instance names but no passwords.

### Enable SQL authentication and create a login

Use these when a SQL Server only allows Windows authentication and the app needs a username and password instead. Unlike `diagnose`, **these change the customer's SQL Server**. Each one prints exactly what it will do, supports `--dry-run`, and asks you to type `yes` (skip with `--yes`).

**1. `enable-sql-auth`** switches the instance to mixed mode (SQL Server and Windows authentication). Run it from an **elevated** prompt.

```powershell
.\platform-db-tunnel.exe enable-sql-auth --dry-run   # show what would change
.\platform-db-tunnel.exe enable-sql-auth             # change it, then ask whether to restart now
.\platform-db-tunnel.exe enable-sql-auth --instance SQLEXPRESS --restart --yes
```

It sets `LoginMode` to 2 in the instance's registry key (the same setting as SSMS > Server Properties > Security). The change only takes effect after the SQL Server service restarts, and **a restart disconnects every user of that SQL Server** and stops SQL Server Agent while it runs, so do it when nobody is working. Without `--restart` it asks, and if you answer no it prints the commands to restart later. Use `--instance` when several instances are installed (`default` means `MSSQLSERVER`).

**2. `create-sql-login`** creates a SQL login with **read-only** access (`db_datareader`) to one database. It connects with your own Windows account, which must be a SQL Server `sysadmin` (usually the account that installed SQL Server) and does not need to be elevated.

```powershell
.\platform-db-tunnel.exe create-sql-login --login tunnel --database ACCDATA --generate
.\platform-db-tunnel.exe create-sql-login --login tunnel --database ACCDATA --dry-run
.\platform-db-tunnel.exe create-sql-login --login tunnel --database ACCDATA --reset-password
```

- The password is prompted for twice (never taken on the command line), or generated with `--generate` and shown once. Generated passwords use letters and digits only, so they never need URL-encoding in the DSN.
- The login's password does not expire and is not subject to Windows password policy (`CHECK_EXPIRATION = OFF`, `CHECK_POLICY = OFF`), so the service does not stop working months later.
- It refuses to run if the server is still Windows-authentication-only (run `enable-sql-auth` and restart first), if your account is not `sysadmin`, or if the login already exists (use `--reset-password` to set a new password).
- It then signs in as the new login to check it works, and prints the `"dsn"` line for `config.json`, including any `tlsmin` / `TrustServerCertificate` option the server needed.
- `--login` accepts letters, digits and underscore only, and not `sa`. Logins with more access (for example `db_owner`) are deliberately not created by this tool.

### Other commands

```powershell
.\platform-db-tunnel.exe run         # run in the console (default), useful for debugging
.\platform-db-tunnel.exe diagnose    # inspect the machine and the SQL Server connection (read-only)
.\platform-db-tunnel.exe enable-sql-auth    # switch SQL Server to mixed mode (changes the server; see above)
.\platform-db-tunnel.exe create-sql-login   # create a read-only SQL login (changes the server; see above)
.\platform-db-tunnel.exe start       # start the service
.\platform-db-tunnel.exe stop        # stop the service
.\platform-db-tunnel.exe uninstall   # stop and remove the service
```

To upgrade: `stop`, replace the exe, `start`.

## Security notes

- There is no authentication. Anything that can reach the port can run SQL, so only expose it through something that authenticates callers.
- `/query` runs whatever SQL it receives. The SQL login the app uses is the real limit on what a caller can do, so give it a least-privilege account (read-only if that is all you need).

## Adding another database

Import its `database/sql` driver in `db.go` and add an entry to the `drivers` map. Nothing else needs to change.
