package main

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// enable-sql-auth and create-sql-login change the customer's SQL Server, unlike
// diagnose. Both print exactly what they will do, support --dry-run, and ask for
// a typed "yes" unless --yes is given.

const sqlRoot = `SOFTWARE\Microsoft\Microsoft SQL Server`

var (
	identRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	stdin   = bufio.NewReader(os.Stdin)
)

func quoteIdent(s string) string   { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
func quoteLiteral(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

// genPassword returns a random password of letters and digits only, so it never
// needs URL-encoding in a DSN. It always contains upper, lower and a digit.
func genPassword(n int) (string, error) {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	for {
		b := make([]byte, n)
		for i := range b {
			x, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
			if err != nil {
				return "", err
			}
			b[i] = chars[x.Int64()]
		}
		s := string(b)
		if strings.ContainsAny(s, "ABCDEFGHJKLMNPQRSTUVWXYZ") && strings.ContainsAny(s, "abcdefghijkmnopqrstuvwxyz") && strings.ContainsAny(s, "23456789") {
			return s, nil
		}
	}
}

func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s Type yes to continue: ", prompt)
	line, _ := stdin.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

func requireAdmin() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("this changes SQL Server settings: run it from an elevated (Administrator) prompt")
	}
	return nil
}

type regInstance struct{ Name, ID string }

func registeredInstances() ([]regInstance, error) {
	k, err := openInstanceNames()
	if err != nil {
		return nil, fmt.Errorf("no SQL Server instances are registered on this machine (%v)", err)
	}
	defer k.Close()
	names, _ := k.ReadValueNames(0)
	var out []regInstance
	for _, n := range names {
		if id, _, err := k.GetStringValue(n); err == nil {
			out = append(out, regInstance{n, id})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no SQL Server instances are registered on this machine")
	}
	return out, nil
}

func pickInstance(all []regInstance, want string) (regInstance, error) {
	if want == "" {
		if len(all) == 1 {
			return all[0], nil
		}
		var names []string
		for _, i := range all {
			names = append(names, i.Name)
		}
		return regInstance{}, fmt.Errorf("several instances are installed (%s): pick one with --instance", strings.Join(names, ", "))
	}
	if strings.EqualFold(want, "default") {
		want = "MSSQLSERVER"
	}
	for _, i := range all {
		if strings.EqualFold(i.Name, want) {
			return i, nil
		}
	}
	return regInstance{}, fmt.Errorf("instance %q not found", want)
}

func serviceNames(instance string) (engine, agent string) {
	if strings.EqualFold(instance, "MSSQLSERVER") {
		return "MSSQLSERVER", "SQLSERVERAGENT"
	}
	return "MSSQL$" + instance, "SQLAgent$" + instance
}

func serviceState(name string) (svc.State, error) {
	m := openSCM()
	if m == nil {
		return 0, errors.New("cannot open the service manager")
	}
	defer m.Disconnect()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, err := windows.OpenService(m.Handle, p, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, err
	}
	s := &mgr.Service{Name: name, Handle: h}
	defer s.Close()
	st, err := s.Query()
	return st.State, err
}

// restartSQL restarts the database engine. Stopping it also stops dependent
// services (SQL Server Agent), which is started again if it was running.
func restartSQL(engine, agent string) error {
	agentWasRunning := false
	if st, err := serviceState(agent); err == nil && st == svc.Running {
		agentWasRunning = true
	}
	if st, err := serviceState(engine); err == nil && st != svc.Stopped {
		fmt.Printf("stopping %s (and dependent services) ...\n", engine)
		if out, err := exec.Command("net", "stop", engine, "/y").CombinedOutput(); err != nil {
			return fmt.Errorf("net stop %s: %v: %s", engine, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("starting %s ...\n", engine)
	if out, err := exec.Command("net", "start", engine).CombinedOutput(); err != nil {
		return fmt.Errorf("net start %s: %v: %s", engine, err, strings.TrimSpace(string(out)))
	}
	if agentWasRunning {
		fmt.Printf("starting %s ...\n", agent)
		if out, err := exec.Command("net", "start", agent).CombinedOutput(); err != nil {
			fmt.Printf("warning: could not start %s again: %v: %s\n", agent, err, strings.TrimSpace(string(out)))
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := serviceState(engine); err == nil && st == svc.Running {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s did not report running within 60 seconds", engine)
}

func runEnableSQLAuth(args []string) error {
	fs := flag.NewFlagSet("enable-sql-auth", flag.ContinueOnError)
	instFlag := fs.String("instance", "", `instance name ("default" or MSSQLSERVER for the default instance)`)
	restart := fs.Bool("restart", false, "restart the SQL Server service afterwards without asking")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	dry := fs.Bool("dry-run", false, "show what would change and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	all, err := registeredInstances()
	if err != nil {
		return err
	}
	inst, err := pickInstance(all, *instFlag)
	if err != nil {
		return err
	}
	path := sqlRoot + `\` + inst.ID + `\MSSQLServer`
	mode, err := regInt(path, "LoginMode")
	if err != nil {
		return fmt.Errorf("cannot read the authentication mode of %s (%v); run from an elevated prompt", inst.Name, err)
	}
	engine, agent := serviceNames(inst.Name)
	if mode == 2 {
		fmt.Printf("instance %s already allows SQL Server and Windows authentication (mixed mode). Nothing to change.\n", inst.Name)
		return nil
	}
	fmt.Printf("instance:  %s [%s]\n", inst.Name, inst.ID)
	fmt.Printf("now:       Windows authentication only (LoginMode=%d)\n", mode)
	fmt.Printf("change:    set HKLM\\%s\\LoginMode = 2 (SQL Server and Windows authentication)\n", path)
	fmt.Printf("then:      the service %s must be restarted for it to take effect.\n", engine)
	fmt.Printf("           A restart DISCONNECTS EVERY USER of this SQL Server and stops %s while it runs.\n", agent)
	if *dry {
		fmt.Println("dry run: nothing was changed.")
		return nil
	}
	if err := requireAdmin(); err != nil {
		return err
	}
	if !*yes && !confirm("Change the authentication mode?") {
		fmt.Println("cancelled; nothing was changed.")
		return nil
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.SET_VALUE|regView)
	if err != nil {
		return fmt.Errorf("open registry key for writing: %w", err)
	}
	err = k.SetDWordValue("LoginMode", 2)
	k.Close()
	if err != nil {
		return fmt.Errorf("set LoginMode: %w", err)
	}
	fmt.Println("mixed mode is set in the registry. It takes effect when the SQL Server service restarts.")

	if !*restart {
		if *yes || !confirm(fmt.Sprintf("Restart %s now? Everyone connected to SQL Server is disconnected.", engine)) {
			fmt.Printf("not restarted. Restart it when nobody is using it, e.g. from an elevated prompt:\n  net stop %s /y && net start %s\n", engine, engine)
			return nil
		}
	}
	if err := restartSQL(engine, agent); err != nil {
		return err
	}
	fmt.Println("SQL Server restarted. Next: create a login with  create-sql-login --login <name> --database <db> --generate")
	return nil
}

// loginStatements builds the T-SQL that creates (or resets) a read-only login.
// pw is a ready-to-embed literal, so callers can pass a masked one for display.
func loginStatements(login, db, pw string, loginExists, userExists, reset bool) []string {
	var s []string
	switch {
	case !loginExists:
		s = append(s, fmt.Sprintf("CREATE LOGIN %s WITH PASSWORD = %s, DEFAULT_DATABASE = %s, CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF",
			quoteIdent(login), pw, quoteIdent(db)))
	case reset:
		s = append(s, fmt.Sprintf("ALTER LOGIN %s WITH PASSWORD = %s, CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF", quoteIdent(login), pw))
	}
	if !userExists {
		s = append(s, fmt.Sprintf("CREATE USER %s FOR LOGIN %s", quoteIdent(login), quoteIdent(login)))
	}
	s = append(s, fmt.Sprintf("ALTER ROLE db_datareader ADD MEMBER %s", quoteIdent(login)))
	return s
}

var sqlVariants = []string{"", "TrustServerCertificate=true", "tlsmin=1.0&TrustServerCertificate=true", "encrypt=disable"}

func runCreateSQLLogin(args []string) error {
	fs := flag.NewFlagSet("create-sql-login", flag.ContinueOnError)
	login := fs.String("login", "", "name of the SQL login to create (letters, digits, underscore)")
	dbName := fs.String("database", "", "database the login may read")
	gen := fs.Bool("generate", false, "generate a random password and show it once")
	reset := fs.Bool("reset-password", false, "if the login already exists, set its password again")
	port := fs.Int("port", 0, "TCP port of SQL Server (default: found automatically)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	dry := fs.Bool("dry-run", false, "show what would be done and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !identRe.MatchString(*login) || strings.EqualFold(*login, "sa") {
		return errors.New("--login is required: letters, digits and underscore only (1-64), and not sa")
	}
	if strings.TrimSpace(*dbName) == "" {
		return errors.New("--database is required")
	}

	password := ""
	if *gen {
		var err error
		if password, err = genPassword(20); err != nil {
			return err
		}
	} else if !*dry {
		p1, err := readPassword(fmt.Sprintf("New password for %s: ", *login))
		if err != nil {
			return err
		}
		p2, err := readPassword("Repeat password: ")
		if err != nil {
			return err
		}
		if p1 != p2 {
			return errors.New("the two passwords differ")
		}
		if len(p1) < 8 {
			return errors.New("use a password of at least 8 characters")
		}
		password = p1
	}

	// Find SQL Server and connect with Windows authentication as the current user.
	var eps []endpoint
	if *port > 0 {
		eps = []endpoint{{"localhost", *port}}
	} else {
		for _, l := range func() []listener {
			var out []listener
			if ns, err := exec.Command("netstat", "-ano").Output(); err == nil {
				all := parseNetstat(string(ns))
				for _, pid := range sqlPIDs() {
					out = append(out, all[pid]...)
				}
			}
			return out
		}() {
			host := l.Addr
			if host == "0.0.0.0" || host == "::" || host == "::1" || host == "127.0.0.1" {
				host = "localhost"
			}
			e := endpoint{host, l.Port}
			dup := false
			for _, x := range eps {
				dup = dup || x == e
			}
			if !dup {
				eps = append(eps, e)
			}
		}
		if len(eps) == 0 {
			eps = []endpoint{{"localhost", 1433}}
		}
	}
	var adminDSN, opts string
	var ep endpoint
	var lastErr error
	for _, e := range eps {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), 3*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		c.Close()
		for _, o := range sqlVariants {
			d := buildDSN(e.Host, e.Port, *dbName, "", "", o)
			if err := tryPing("sqlserver", d); err != nil {
				lastErr = err
				continue
			}
			adminDSN, opts, ep = d, o, e
			break
		}
		if adminDSN != "" {
			break
		}
	}
	if adminDSN == "" {
		hint := ""
		if lastErr != nil {
			hint = explain(lastErr.Error())
		}
		return fmt.Errorf("cannot connect to SQL Server with your Windows account: %v\n  %s\n  run  diagnose  to see why", lastErr, hint)
	}
	fmt.Printf("connected to %s:%d as the current Windows account\n", ep.Host, ep.Port)

	db, err := sql.Open("sqlserver", adminDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	var winOnly, sysadmin int
	var who string
	if err := db.QueryRow(`select cast(serverproperty('IsIntegratedSecurityOnly') as int), cast(is_srvrolemember('sysadmin') as int), suser_sname()`).Scan(&winOnly, &sysadmin, &who); err != nil {
		return fmt.Errorf("cannot read server settings: %w", err)
	}
	if winOnly == 1 {
		return errors.New("SQL Server accepts Windows authentication only. Run  enable-sql-auth  and restart the service first")
	}
	if sysadmin != 1 {
		return fmt.Errorf("%s is not a SQL Server sysadmin, so it cannot create logins. Run this as an account that is (the one used to install SQL Server), or ask the customer", who)
	}
	var loginExists, userExists int
	if err := db.QueryRow(`select count(*) from sys.server_principals where name = @p1`, *login).Scan(&loginExists); err != nil {
		return err
	}
	if err := db.QueryRow(`select count(*) from sys.database_principals where name = @p1`, *login).Scan(&userExists); err != nil {
		return err
	}
	if loginExists > 0 && !*reset {
		return fmt.Errorf("login %s already exists. Use --reset-password to set a new password for it", *login)
	}

	masked := loginStatements(*login, *dbName, "N'********'", loginExists > 0, userExists > 0, *reset)
	fmt.Printf("\nas %s, in database %s, this will run:\n", who, *dbName)
	for _, s := range masked {
		fmt.Println("  " + s + ";")
	}
	fmt.Println("The login gets read-only access (db_datareader); its password does not expire.")
	if *dry {
		fmt.Println("dry run: nothing was changed.")
		return nil
	}
	if !*yes && !confirm("Create it?") {
		fmt.Println("cancelled; nothing was changed.")
		return nil
	}

	for _, s := range loginStatements(*login, *dbName, quoteLiteral(password), loginExists > 0, userExists > 0, *reset) {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("statement failed: %w\n  (statements before it may already have been applied)", err)
		}
	}
	fmt.Printf("login %s is ready.\n", *login)

	if err := tryPing("sqlserver", buildDSN(ep.Host, ep.Port, *dbName, *login, password, opts)); err != nil {
		fmt.Printf("WARNING: the login was created but signing in as it failed: %v\n", err)
		if h := explain(err.Error()); h != "" {
			fmt.Println("  " + h)
		}
		return nil
	}
	fmt.Println("verified: signing in as the new login works.")
	shown := "PASSWORD"
	if *gen {
		shown = password
	}
	fmt.Printf("\nconfig.json:\n  \"dsn\": %q\n", buildDSN(ep.Host, ep.Port, *dbName, *login, shown, opts))
	if *gen {
		fmt.Printf("\npassword: %s\n(shown once; it is not saved anywhere)\n", password)
	} else {
		fmt.Println("(replace PASSWORD with the password you typed)")
	}
	return nil
}
