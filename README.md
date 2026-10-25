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

Response: `{"columns": ["id", "name"], "rows": [[42, "alice"]]}`

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

The file contains database credentials, so restrict who can read it (e.g. `icacls config.json /inheritance:r /grant:r "SYSTEM:R" "Administrators:F"`). Restart the service after editing it.

The app only listens on `127.0.0.1`, so only processes on the same machine can reach it.

## Install as a Windows Service

1. Copy `platform-db-tunnel.exe` to a permanent folder, e.g. `C:\platform-db-tunnel\` (the service points at the exe's location, so don't move it afterwards). `C:\Program Files\platform-db-tunnel\` also works.
2. Create `config.json` next to it, as described above.
3. **Lock the folder down.** A folder you create directly on `C:\` is writable by ordinary users by default. The service runs with high privileges, so anyone who could replace the exe or edit `config.json` could run code as it. From an elevated prompt, allow only SYSTEM and Administrators (`C:\Program Files` is already protected like this):

```powershell
icacls C:\platform-db-tunnel /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"
```

   If the app will run as a particular user (`--current-user` or `--at-logon` below), also grant that user read access, e.g. add `"DOMAIN\you:(OI)(CI)RX"`.
4. From an **elevated** (Administrator) prompt:

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
