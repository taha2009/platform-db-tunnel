package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// diagnose inspects the machine for a SQL Server and tries to connect to it,
// so that connection problems can be understood without guessing. It only reads:
// it never changes SQL Server or Windows settings. Everything works without
// administrator rights; a few registry values are unreadable without them and
// are reported as such.

const diagOutName = "platform-db-tunnel-diagnose.txt"

type report struct {
	w     io.Writer
	fails int
	warns int
}

func (r *report) line(tag, format string, a ...any) {
	fmt.Fprintf(r.w, "[%s] %s\n", tag, fmt.Sprintf(format, a...))
}
func (r *report) ok(format string, a ...any)   { r.line(" OK ", format, a...) }
func (r *report) info(format string, a ...any) { r.line("INFO", format, a...) }
func (r *report) warn(format string, a ...any) { r.warns++; r.line("WARN", format, a...) }
func (r *report) fail(format string, a ...any) { r.fails++; r.line("FAIL", format, a...) }
func (r *report) section(title string)         { fmt.Fprintf(r.w, "\n=== %s ===\n", title) }
func (r *report) hint(h string) {
	if h != "" {
		fmt.Fprintf(r.w, "       -> %s\n", h)
	}
}

func runDiagnose(args []string) error {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	dsnFlag := fs.String("dsn", "", "SQL Server DSN to test instead of the one in config.json")
	userFlag := fs.String("user", "", "try this SQL login (the password is asked for) instead of Windows authentication")
	dbFlag := fs.String("database", "master", "database to connect to while probing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	password := ""
	if *userFlag != "" {
		var err error
		if password, err = readPassword(fmt.Sprintf("SQL password for %s: ", *userFlag)); err != nil {
			return err
		}
	}

	var out io.Writer = os.Stdout
	outPath := ""
	if exe, err := os.Executable(); err == nil {
		outPath = filepath.Join(filepath.Dir(exe), diagOutName)
	}
	f, err := os.Create(outPath)
	if err != nil {
		outPath = filepath.Join(os.TempDir(), diagOutName)
		f, err = os.Create(outPath)
	}
	if err == nil {
		defer f.Close()
		out = io.MultiWriter(os.Stdout, f)
	}
	r := &report{w: out}

	fmt.Fprintln(r.w, "platform-db-tunnel diagnose (read-only; passwords are never printed)")
	diagEnvironment(r)
	cfg := diagConfig(r)
	m := openSCM()
	if m != nil {
		defer m.Disconnect()
	}
	diagServices(r, m)
	insts := diagInstances(r)
	listeners := diagNetwork(r)

	driver, dsn := "sqlserver", *dsnFlag
	if cfg != nil && dsn == "" {
		driver, dsn = cfg.Driver, cfg.DSN
	}
	if dsn != "" {
		r.section("Connection using the configured DSN")
		r.info("%s", describeDSN(dsn))
		if err := tryPing(driver, dsn); err != nil {
			r.fail("cannot connect: %v", err)
			r.hint(explain(err.Error()))
		} else {
			r.ok("connected with the configured DSN")
			if driver == "sqlserver" {
				diagDetails(r, driver, dsn)
			}
		}
	}

	diagProbe(r, insts, listeners, *userFlag, password, *dbFlag)

	r.section("Summary")
	r.info("%d problem(s), %d warning(s)", r.fails, r.warns)
	if outPath != "" && f != nil {
		r.info("this report was saved to %s - send it back to whoever is helping you", outPath)
	}
	return nil
}

func diagEnvironment(r *report) {
	r.section("This machine")
	host, _ := os.Hostname()
	r.info("time: %s", time.Now().Format("2006-01-02 15:04:05 -07:00"))
	r.info("machine: %s (%s/%s)", host, runtime.GOOS, runtime.GOARCH)
	if v := windows.RtlGetVersion(); v != nil {
		r.info("windows: %d.%d build %d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	if u, err := user.Current(); err == nil {
		r.info("running as: %s", u.Username)
	}
	if windows.GetCurrentProcessToken().IsElevated() {
		r.info("administrator: yes")
	} else {
		r.info("administrator: no (fine; registry values that need it are marked, rerun elevated for full detail)")
	}
	if exe, err := os.Executable(); err == nil {
		r.info("exe: %s", exe)
		probe := filepath.Join(filepath.Dir(exe), ".diag-write-test")
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			r.warn("this user cannot write to %s: the log file cannot be created there", filepath.Dir(exe))
			r.hint(`use a folder such as C:\platform-db-tunnel\ instead of Program Files`)
		} else {
			os.Remove(probe)
			r.ok("exe folder is writable (log file can be created)")
		}
	}
}

func diagConfig(r *report) *config {
	r.section("config.json")
	cfg, err := loadConfig()
	if err != nil {
		r.warn("%v", err)
		r.hint("put config.json next to the exe (see README), or pass --dsn to test a connection string directly")
		return nil
	}
	r.ok("loaded %s", cfg.Path)
	r.info("type: driver=%s, listen=%s, timeout=%s", cfg.Driver, cfg.Addr, cfg.QueryTimeout)
	if ln, err := net.Listen("tcp", cfg.Addr); err != nil {
		c := http.Client{Timeout: 3 * time.Second}
		if resp, herr := c.Get("http://" + cfg.Addr + "/healthz"); herr == nil {
			resp.Body.Close()
			r.info("platform-db-tunnel is already running on %s (healthz returned %s)", cfg.Addr, resp.Status)
		} else {
			r.warn("port %s is in use by something else: %v", cfg.Addr, err)
			r.hint(`change "port" in config.json`)
		}
	} else {
		ln.Close()
		r.ok("port %s is free", cfg.Addr)
	}
	return cfg
}

// openSCM opens the service manager with only the rights a normal user has.
func openSCM() *mgr.Mgr {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil
	}
	return &mgr.Mgr{Handle: h}
}

var svcStates = map[svc.State]string{
	svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping",
	svc.Running: "running", svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused",
}

func diagServices(r *report, m *mgr.Mgr) {
	r.section("Windows services")
	if m == nil {
		r.warn("cannot read the service list")
		return
	}
	names, err := m.ListServices()
	if err != nil {
		r.warn("cannot list services: %v", err)
		return
	}
	sort.Strings(names)
	sqlRunning, sqlFound := false, false
	for _, n := range names {
		u := strings.ToUpper(n)
		if !(strings.HasPrefix(u, "MSSQL") || strings.HasPrefix(u, "SQLBROWSER") || strings.HasPrefix(u, "SQLSERVERAGENT") ||
			strings.HasPrefix(u, "SQLAGENT$") || u == strings.ToUpper(serviceName)) {
			continue
		}
		p, err := windows.UTF16PtrFromString(n)
		if err != nil {
			continue
		}
		h, err := windows.OpenService(m.Handle, p, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
		if err != nil {
			r.info("%s: cannot open (%v)", n, err)
			continue
		}
		s := &mgr.Service{Name: n, Handle: h}
		st, serr := s.Query()
		cfg, cerr := s.Config()
		s.Close()
		state, start, acct := "?", "?", "?"
		if serr == nil {
			state = svcStates[st.State]
		}
		if cerr == nil {
			acct = cfg.ServiceStartName
			start = map[uint32]string{mgr.StartAutomatic: "automatic", mgr.StartManual: "manual", mgr.StartDisabled: "disabled"}[cfg.StartType]
		}
		line := fmt.Sprintf("%-22s %-9s start=%-9s account=%s", n, state, start, acct)
		isEngine := u == "MSSQLSERVER" || strings.HasPrefix(u, "MSSQL$")
		switch {
		case isEngine && state == "running":
			sqlFound, sqlRunning = true, true
			r.ok("%s (database engine)", line)
		case isEngine:
			sqlFound = true
			r.fail("%s (database engine is not running)", line)
			r.hint(`start it from an administrator prompt: Start-Service '` + n + `'`)
		default:
			r.info("%s", line)
		}
	}
	if !sqlFound {
		r.warn("no SQL Server database engine service on this machine")
		r.hint("SQL Server may be on another computer (then use that computer's name in the DSN host), or it is not installed")
	} else if !sqlRunning {
		r.hint("none of the SQL Server engines is running")
	}
}

type instance struct {
	Name, ID  string
	TCP       int // -1 unknown, 0 disabled, 1 enabled
	StaticTCP string
	Dynamic   string
}

// regView is the registry view SQL Server's keys were found in. A 32-bit SQL
// Server (for example 2005 or 2008 on 64-bit Windows) registers under the
// 32-bit view, which a 64-bit process does not see by default.
var regView uint32

// openInstanceNames opens HKLM\...\Instance Names\SQL, trying the native view
// first and the 32-bit view second, and remembers which one worked.
func openInstanceNames() (registry.Key, error) {
	path := sqlRoot + `\Instance Names\SQL`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err == nil {
		regView = 0
		return k, nil
	}
	if k32, err32 := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_32KEY); err32 == nil {
		regView = registry.WOW64_32KEY
		return k32, nil
	}
	return 0, err
}

func regString(path, name string) (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|regView)
	if err != nil {
		return "", err
	}
	defer k.Close()
	s, _, err := k.GetStringValue(name)
	return s, err
}

func regInt(path, name string) (int, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|regView)
	if err != nil {
		return 0, err
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	return int(v), err
}

func diagInstances(r *report) []instance {
	r.section("Installed SQL Server instances (registry)")
	const root = `SOFTWARE\Microsoft\Microsoft SQL Server`
	k, err := openInstanceNames()
	if err != nil {
		r.info("no instances registered on this machine (%v)", err)
		return nil
	}
	defer k.Close()
	if regView != 0 {
		r.info("(32-bit SQL Server on 64-bit Windows: read from the 32-bit registry view)")
	}
	names, _ := k.ReadValueNames(0)
	sort.Strings(names)
	var out []instance
	for _, n := range names {
		id, _, err := k.GetStringValue(n)
		if err != nil {
			continue
		}
		in := instance{Name: n, ID: id, TCP: -1}
		label := n
		if strings.EqualFold(n, "MSSQLSERVER") {
			label = "MSSQLSERVER (default instance)"
		}
		r.info("instance %s [%s]", label, id)
		base := root + `\` + id
		ver, _ := regString(base+`\Setup`, "Version")
		ed, _ := regString(base+`\Setup`, "Edition")
		if ver != "" || ed != "" {
			r.info("  version %s, %s", ver, ed)
		}

		tcp := base + `\MSSQLServer\SuperSocketNetLib\Tcp`
		if v, err := regInt(tcp, "Enabled"); err != nil {
			r.info("  TCP/IP: not readable (%v) - rerun as administrator", err)
		} else {
			in.TCP = v
			if v == 0 {
				r.fail("  TCP/IP protocol is DISABLED: this app (and any network client) cannot connect")
				r.hint("enable it in SQL Server Configuration Manager > SQL Server Network Configuration > Protocols > TCP/IP, then restart the SQL Server service")
			} else {
				r.ok("  TCP/IP protocol is enabled")
			}
		}
		in.StaticTCP, _ = regString(tcp+`\IPAll`, "TcpPort")
		in.Dynamic, _ = regString(tcp+`\IPAll`, "TcpDynamicPorts")
		switch {
		case in.StaticTCP != "":
			r.info("  static TCP port: %s", in.StaticTCP)
		case in.Dynamic != "":
			r.info("  dynamic TCP port (changes at restart unless fixed): %s", in.Dynamic)
		}

		if v, err := regInt(base+`\MSSQLServer`, "LoginMode"); err == nil {
			if v == 1 {
				r.info("  authentication: Windows authentication ONLY (SQL logins such as 'sa' will be refused)")
			} else {
				r.info("  authentication: mixed mode (Windows and SQL logins)")
			}
		}
		if v, err := regInt(base+`\MSSQLServer\SuperSocketNetLib`, "ForceEncryption"); err == nil && v == 1 {
			r.info("  Force Encryption is ON: encrypt=disable will not work, use TrustServerCertificate=true instead")
		}
		out = append(out, in)
	}
	return out
}

type listener struct {
	Addr string
	Port int
}

// browserInfo is one instance announced by the SQL Browser service.
type browserInfo map[string]string

func diagNetwork(r *report) []listener {
	r.section("Network")
	if conn, err := net.DialTimeout("udp", "127.0.0.1:1434", 2*time.Second); err == nil {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		conn.Write([]byte{0x03})
		buf := make([]byte, 65535)
		if n, err := conn.Read(buf); err == nil {
			for _, b := range parseBrowser(buf[:n]) {
				r.info("SQL Browser: instance %q on server %s, tcp port %s", b["InstanceName"], b["ServerName"], b["tcp"])
			}
		} else {
			r.info("SQL Browser did not answer on UDP 1434 (service stopped, or only a default instance exists - fine when connecting by port)")
		}
		conn.Close()
	}

	pids := sqlPIDs()
	if len(pids) == 0 {
		r.info("no sqlservr.exe process on this machine")
		return nil
	}
	r.info("sqlservr.exe is running (pid %s)", joinInts(pids))
	ns, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		r.warn("cannot run netstat: %v", err)
		return nil
	}
	all := parseNetstat(string(ns))
	var out []listener
	for _, pid := range pids {
		out = append(out, all[pid]...)
	}
	if len(out) == 0 {
		r.fail("SQL Server is running but is not listening on any TCP port")
		r.hint("TCP/IP is disabled for the instance (see the registry section above), or it only uses shared memory / named pipes. Enable TCP/IP and restart the SQL Server service.")
		return nil
	}
	var parts []string
	for _, l := range out {
		parts = append(parts, fmt.Sprintf("%s:%d", l.Addr, l.Port))
	}
	r.ok("SQL Server listens on: %s", strings.Join(parts, ", "))
	onLoopback := false
	for _, l := range out {
		if l.Addr == "0.0.0.0" || l.Addr == "::" || l.Addr == "127.0.0.1" || l.Addr == "::1" {
			onLoopback = true
		}
	}
	if !onLoopback {
		r.warn("SQL Server listens only on specific addresses, not on localhost")
		r.hint("use one of the addresses above as the host in the DSN instead of localhost")
	}
	return out
}

func sqlPIDs() []int {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq sqlservr.exe", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil
	}
	return parseTasklist(string(out))
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

// parseBrowser decodes an SQL Browser (UDP 1434) reply: 0x05, a 2-byte length,
// then ";"-separated key/value pairs with ";;" between instances.
func parseBrowser(b []byte) []browserInfo {
	if len(b) < 3 || b[0] != 0x05 {
		return nil
	}
	var out []browserInfo
	for _, rec := range strings.Split(string(b[3:]), ";;") {
		f := strings.Split(strings.Trim(rec, ";"), ";")
		m := browserInfo{}
		for i := 0; i+1 < len(f); i += 2 {
			m[f[i]] = f[i+1]
		}
		if len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// parseNetstat returns the listening TCP sockets per process id from `netstat -ano`.
func parseNetstat(s string) map[int][]listener {
	out := map[int][]listener{}
	for _, ln := range strings.Split(s, "\n") {
		f := strings.Fields(ln)
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		i := strings.LastIndex(f[1], ":")
		if i < 0 {
			continue
		}
		port, err1 := strconv.Atoi(f[1][i+1:])
		pid, err2 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil {
			continue
		}
		addr := strings.Trim(f[1][:i], "[]")
		out[pid] = append(out[pid], listener{Addr: addr, Port: port})
	}
	return out
}

// parseTasklist extracts the pids from `tasklist /FO CSV /NH` output.
func parseTasklist(s string) []int {
	var pids []int
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(strings.ToLower(ln), `"sqlservr.exe"`) {
			continue
		}
		f := strings.Split(ln, `","`)
		if len(f) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func buildDSN(host string, port int, db, user, password, opts string) string {
	u := url.URL{Scheme: "sqlserver", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	q := "database=" + url.QueryEscape(db)
	if opts != "" {
		q += "&" + opts
	}
	u.RawQuery = q
	return u.String()
}

func tryPing(driver, dsn string) error {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

type endpoint struct {
	Host string
	Port int
}

// diagProbe tries each place SQL Server could be reached, with the connection
// option combinations that old or default-configured servers need.
func diagProbe(r *report, insts []instance, ls []listener, user, password, db string) {
	r.section("Connection probes")
	seen := map[endpoint]bool{}
	var eps []endpoint
	add := func(host string, port int) {
		e := endpoint{host, port}
		if port > 0 && port < 65536 && !seen[e] {
			seen[e] = true
			eps = append(eps, e)
		}
	}
	for _, l := range ls {
		host := l.Addr
		if host == "0.0.0.0" || host == "::" || host == "::1" || host == "127.0.0.1" {
			host = "localhost"
		}
		add(host, l.Port)
	}
	for _, in := range insts {
		for _, p := range []string{in.StaticTCP, in.Dynamic} {
			for _, s := range strings.Split(p, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
					add("localhost", n)
				}
			}
		}
	}
	if len(eps) == 0 {
		add("localhost", 1433)
	}

	auth := "Windows authentication as the current user"
	if user != "" {
		auth = "SQL login " + user
	}
	r.info("authentication: %s; database: %s", auth, db)

	variants := []struct{ name, opts string }{
		{"default", ""},
		{"TrustServerCertificate", "TrustServerCertificate=true"},
		{"old TLS + TrustServerCertificate", "tlsmin=1.0&TrustServerCertificate=true"},
		{"encryption off", "encrypt=disable"},
	}
	worked := false
	for _, e := range eps {
		addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			r.fail("%s: TCP connect failed: %v", addr, err)
			r.hint(explain(err.Error()))
			continue
		}
		c.Close()
		r.ok("%s: TCP port is open", addr)
		for _, v := range variants {
			err := tryPing("sqlserver", buildDSN(e.Host, e.Port, db, user, password, v.opts))
			if err == nil {
				worked = true
				r.ok("%s: login works with options [%s]", addr, v.name)
				show := buildDSN(e.Host, e.Port, db, user, "PASSWORD", v.opts)
				if user == "" {
					show = buildDSN(e.Host, e.Port, db, "", "", v.opts)
				}
				r.info(`config.json: "dsn": %q  (use your real database instead of %s; URL-encode special characters in the password)`, show, db)
				diagDetails(r, "sqlserver", buildDSN(e.Host, e.Port, db, user, password, v.opts))
				break
			}
			r.info("%s: options [%s] -> %v", addr, v.name, err)
			if h := explain(err.Error()); h != "" {
				stop := strings.Contains(strings.ToLower(err.Error()), "login failed") || strings.Contains(strings.ToLower(err.Error()), "cannot open database")
				if stop {
					r.fail("%s: the server was reached but refused the login", addr)
					r.hint(h)
					break
				}
			}
		}
	}
	if !worked && len(eps) > 0 {
		r.fail("could not log in on any endpoint")
		r.hint("fix the first FAIL above; if the TCP port is open but every option fails, the messages show which option is needed")
	}
}

// diagDetails reports who we are on the server and what we can see.
func diagDetails(r *report, driver, dsn string) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var ver, login, cur sql.NullString
	var sysadmin, winOnly sql.NullInt64
	err = db.QueryRowContext(ctx, `select cast(@@version as nvarchar(400)), suser_sname(), db_name(),
		cast(is_srvrolemember('sysadmin') as int), cast(serverproperty('IsIntegratedSecurityOnly') as int)`).
		Scan(&ver, &login, &cur, &sysadmin, &winOnly)
	if err != nil {
		r.info("  (could not read server details: %v)", err)
		return
	}
	r.info("  server: %s", strings.SplitN(strings.TrimSpace(ver.String), "\n", 2)[0])
	r.info("  logged in as %s, database %s, sysadmin=%v", login.String, cur.String, sysadmin.Int64 == 1)
	if winOnly.Valid && winOnly.Int64 == 1 {
		r.info("  server accepts Windows authentication only")
	}
	// sys.databases needs SQL Server 2005+, and some old servers or restricted
	// logins reject it, so fall back to the SQL Server 2000 catalog view.
	rows, err := db.QueryContext(ctx, `select name, state_desc, cast(has_dbaccess(name) as int) from sys.databases order by name`)
	withState := err == nil
	if err != nil {
		first := err
		rows, err = db.QueryContext(ctx, `select name, 'ONLINE', cast(has_dbaccess(name) as int) from master.dbo.sysdatabases order by name`)
		if err != nil {
			r.info("  (could not list databases: %v)", first)
			return
		}
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var name, state string
		var access sql.NullInt64
		if err := rows.Scan(&name, &state, &access); err != nil {
			r.info("  (database list incomplete: %v)", err)
			break
		}
		mark := ""
		if withState && state != "ONLINE" {
			mark = " [" + state + "]"
		} else if access.Valid && access.Int64 == 0 {
			mark = " [no access]"
		}
		parts = append(parts, name+mark)
	}
	if err := rows.Err(); err != nil {
		r.info("  (database list incomplete: %v)", err)
	}
	r.info("  databases: %s", strings.Join(parts, ", "))
}

// explain turns a driver or OS error into the usual cause and fix.
func explain(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "actively refused"):
		return "Nothing is listening on that port. TCP/IP may be disabled for the instance, or it uses another port (see the instance and network sections)."
	case strings.Contains(m, "unsupported protocol version"):
		return "The server only offers an old TLS version. Add tlsmin=1.0 (and TrustServerCertificate=true) to the DSN."
	case strings.Contains(m, "x509") || strings.Contains(m, "certificate"):
		return "The server certificate is not trusted (usually self-signed). Add TrustServerCertificate=true to the DSN."
	case strings.Contains(m, "login failed for user"):
		return "The server reached but refused the login. Windows auth: the Windows account running the app needs a SQL Server login. SQL login: wrong password, login missing or disabled, or the server is Windows-authentication-only. The server's ERRORLOG has a State number that says which."
	case strings.Contains(m, "cannot open database") || strings.Contains(m, "4060"):
		return "The login is valid but cannot open that database. Check the database name and that the login has a user in it."
	case strings.Contains(m, "i/o timeout") || strings.Contains(m, "timed out") || strings.Contains(m, "deadline exceeded"):
		return "No answer in time: a firewall, a wrong host or port, or the server is not running."
	case strings.Contains(m, "no such host"):
		return "The host name does not resolve. Check the spelling, or use an IP address."
	case strings.Contains(m, "sspi") || strings.Contains(m, "target principal") || strings.Contains(m, "untrusted domain"):
		return "Windows authentication failed. Try 'localhost' versus the machine name, or use a SQL login."
	case strings.Contains(m, "forcibly closed") || strings.Contains(m, "connection reset") || strings.Contains(m, "unexpected eof"):
		return "The server dropped the connection. Right after a TLS attempt this means a very old SQL Server (2005/2008) whose encryption Go cannot negotiate even with tlsmin=1.0: use encrypt=disable (fine on localhost)."
	case strings.Contains(m, "encryption") || strings.Contains(m, "tls handshake"):
		return "TLS negotiation failed. Try TrustServerCertificate=true, tlsmin=1.0, or encrypt=disable (not if Force Encryption is on)."
	}
	return ""
}
