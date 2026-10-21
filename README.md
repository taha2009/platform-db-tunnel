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

Response: `{"columns": ["id", "name"], "rows": [[42, "alice"]], "truncated": false}`

Arguments bind positionally, using the placeholder style of the chosen database:

| Database   | Placeholders      |
|------------|-------------------|
| SQL Server | `@p1`, `@p2`, ... |
| PostgreSQL | `$1`, `$2`, ...   |
| MySQL      | `?`               | Limits: 30 s per query, 10,000 rows (`truncated` is set if cut off), 1 MB request body. SQL errors come back as `{"error": "..."}` with status 400.

## Build

Requires Go 1.26 or later (see `go.mod`). Only the machine that builds needs Go; the resulting exe runs without it.

```powershell
go build -o platform-db-tunnel.exe .
```

There is no separate dependency install step: `go build` downloads the modules listed in `go.mod` on first run (internet access required; `go mod download` fetches them without building).

The result is a single self-contained exe, with no runtime to install on the target machine.

## Configure

Create `config.json` in the same folder as `platform-db-tunnel.exe`. It is the only source of settings; environment variables are not read.

```json
{
  "type": "postgres",
  "dsn": "postgres://USER:PASSWORD@localhost:5432/DBNAME",
  "port": "8080"
}
```

| Key    | Purpose                                              | Default |
|--------|------------------------------------------------------|---------|
| `type` | `mssql`, `postgres` or `mysql`                       | `mssql` |
| `dsn`  | Connection string for the database (required)        |         |
| `port` | Port to listen on (always bound to `127.0.0.1`)      | `8080`  |

The DSN (Data Source Name) is a single string with the host, port, credentials and database name. Its format depends on the database:

- SQL Server: `sqlserver://user:pass@localhost:1433?database=mydb`
- PostgreSQL: `postgres://user:pass@localhost:5432/mydb`
- MySQL: `user:pass@tcp(localhost:3306)/mydb`

Special characters in a password must be URL-encoded in the URL-style formats. Driver options go in the DSN too, e.g. `encrypt=disable` (SQL Server) or `sslmode=disable` (PostgreSQL).

The file contains database credentials, so restrict who can read it (e.g. `icacls config.json /inheritance:r /grant:r "SYSTEM:R" "Administrators:F"`). Restart the service after editing it.

The app only listens on `127.0.0.1`, so only processes on the same machine can reach it.

## Install as a Windows Service

1. Copy `platform-db-tunnel.exe` to a permanent folder, e.g. `C:\Program Files\platform-db-tunnel\` (the service points at the exe's location, so don't move it afterwards).
2. Create `config.json` next to it, as described above.
3. From an **elevated** (Administrator) prompt:

```powershell
cd "C:\Program Files\platform-db-tunnel"
.\platform-db-tunnel.exe install
```

This registers the `PlatformDBTunnel` service with automatic start and starts it. On failure it restarts after 5 s, 5 s, then 30 s.

Check it:

```powershell
Get-Service PlatformDBTunnel
curl http://127.0.0.1:8080/healthz
```

Errors are written to the Windows Event Log (Event Viewer → Windows Logs → Application, source `PlatformDBTunnel`).

### Other commands

```powershell
.\platform-db-tunnel.exe run         # run in the console (default), useful for debugging
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
