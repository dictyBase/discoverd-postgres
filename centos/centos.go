package centos

import (
    "bytes"
    "crypto/rand"
    "database/sql"
    "encoding/base64"
    "errors"
    "fmt"
    "io"
    "log"
    "net"
    "os"
    "os/exec"
    "os/signal"
    "path/filepath"
    "strings"
    "syscall"
    "text/template"
    "time"

    "github.com/codegangsta/cli"
    da "github.com/flynn/discoverd/agent"
    "github.com/flynn/go-discoverd"
    _ "github.com/lib/pq"
)

func RunPg(c *cli.Context) {
    var addr string
    if len(c.String("discoverd")) > 0 {
        addr = c.String("discoverd")
    } else if len(os.Getenv("DISCOVERD")) > 0 {
        addr = os.Getenv("DISCOVERD")
    } else {
        log.Fatal("discoverd service ip is not defined")
    }

    set, err := discoverd.RegisterWithSet(c.String("service"), addr, nil)
    if err != nil {
        log.Fatal(err)
    }

    var username, password string
    var follower *follower
    var leader *exec.Cmd
    var done <-chan struct{}
    for l := range set.Leaders() {
        if l.Attrs["username"] != "" && l.Attrs["password"] != "" {
            username, password = l.Attrs["username"], l.Attrs["password"]
        }
        if l.Addr == set.SelfAddr() {
            if follower == nil {
                leader, done = startLeader(c)
            } else {
                leader, done = promoteToLeader(follower, username, password, c)
            }
            goto wait
        } else {
            if follower == nil {
                follower = startFollower(l, set, c)
            } else {
                follower = switchLeader(l, set, follower, c)
            }
        }
    }
    // TODO: handle service discovery disconnection

wait:
    set.Close()
    <-done
    procExit(leader)
}

func startLeader(c *cli.Context) (*exec.Cmd, <-chan struct{}) {
    log.Println("Starting as leader...")
    if err := dirIsEmpty(c.String("data")); err == nil {
        log.Println("Running initdb...")
        runCmd(exec.Command(
            filepath.Join(c.String("pgbin"), "initdb"),
            "-D", c.String("data"),
            "--encoding=UTF-8",
            "--locale=en_US.UTF-8", // TODO: make this configurable?
        ))
    } else if err != ErrNotEmpty {
        log.Fatal(err)
    }

    cmd, err := startPostgres(c)
    if err != nil {
        log.Fatal(err)
    }

    db := waitForPostgres(time.Minute, c)
    password := createSuperuser(db)
    db.Close()
    register(map[string]string{"username": "flynn", "password": password, "up": "true"}, c)

    done := make(chan struct{})
    go func() {
        cmd.Wait()
        close(done)
    }()

    return cmd, done
}

func register(attrs map[string]string, c *cli.Context) {
    err := discoverd.RegisterWithAttributes(c.String("service"), c.String("discoverd"), attrs)
    if err != nil {
        log.Fatalln("discoverd registration error:", err)
    }
}

func procExit(cmd *exec.Cmd) {
    discoverd.UnregisterAll()
    var status int
    if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
        status = ws.ExitStatus()
    }
    os.Exit(status)
}

func createSuperuser(db *sql.DB) (password string) {
    log.Println("Creating superuser...")
    password = generatePassword()

    _, err := db.Exec("DROP USER IF EXISTS flynn")
    if err != nil {
        log.Fatalln("Error dropping user:", err)
    }
    _, err = db.Exec("CREATE USER flynn WITH SUPERUSER CREATEDB CREATEROLE REPLICATION PASSWORD '" + password + "'")
    if err != nil {
        log.Fatalln("Error creating user:", err)
    }
    log.Println("Superuser created.")

    return
}

func generatePassword() string {
    b := make([]byte, 16)
    enc := make([]byte, 24)
    _, err := io.ReadFull(rand.Reader, b)
    if err != nil {
        panic(err) // This shouldn't ever happen, right?
    }
    base64.URLEncoding.Encode(enc, b)
    return string(bytes.TrimRight(enc, "="))
}

func GeneratePgDataSource(c *cli.Context) string {
    return "user=postgres host=/var/run/postgresql sslmode=disable port=" + c.String("port")
}

func waitForPostgres(maxWait time.Duration, c *cli.Context) *sql.DB {
    log.Println("Waiting for postgres to boot...")
    start := time.Now()
    for {
        var ping string
        db, err := sql.Open("postgres", GeneratePgDataSource(c))
        if err != nil {
            goto fail
        }
        err = db.QueryRow("SELECT 'ping'").Scan(&ping)
        if ping == "ping" {
            log.Println("Postgres is up.")
            return db
        }
        db.Close()

    fail:
        if time.Now().Sub(start) >= maxWait {
            log.Fatalf("Unable to connect to postgres after %s, last error: %q", maxWait, err)
        }
        time.Sleep(time.Second)
    }
}

func waitForPromotion(c *cli.Context) {
    log.Println("Waiting for promotion...")
    db, err := sql.Open("postgres", GeneratePgDataSource(c))
    if err != nil {
        log.Fatalln("Error connecting to postgres:", err)
    }
    defer db.Close()
    for {
        var recovery bool
        err := db.QueryRow("SELECT pg_is_in_recovery()").Scan(&recovery)
        if err != nil {
            log.Fatalln("Error checking recovery status:", err)
        }
        if !recovery {
            return
        }
        time.Sleep(time.Second)
    }
}

func promoteToLeader(follower *follower, username, password string, c *cli.Context) (*exec.Cmd, <-chan struct{}) {
    log.Println("Promoting follower to leader...")
    register(map[string]string{"up": "false"}, c)
    f, err := os.Create(filepath.Join(c.String("data"), "promote.trigger"))
    if err != nil {
        panic(err)
    }
    f.Close()

    waitForPromotion(c)

    if username == "" || password == "" {
        // TODO: create superuser
    }

    register(map[string]string{"up": "true", "username": username, "password": password}, c)
    log.Println("Follower promoted to leader.")
    return follower.Cancel()
}

func runCmd(cmd *exec.Cmd) {
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    if err := cmd.Run(); err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
                os.Exit(status.ExitStatus())
            }
        }
        log.Fatal(err)
    }
}

func pullBaseBackup(s *discoverd.Service, c *cli.Context) {
    log.Println("Running pg_basebackup...")
    runCmd(exec.Command(filepath.Join(c.String("pgbin"),
        "pg_basebackup"),
        "-D", c.String("data"),
        "-d", fmt.Sprintf("host=%s port=%s user=%s password=%s", s.Host, s.Port, s.Attrs["username"], s.Attrs["password"]),
        "--xlog-method=stream",
        "--progress",
        "--verbose",
    ))
    log.Println("pg_basebackup complete.")
}

var recoveryTempl = template.Must(template.New("recovery").Parse(`
standby_mode = 'on'
primary_conninfo = 'host={{.Host}} port={{.Port}} user={{.Username}} password={{.Password}}'
trigger_file = '{{.Trigger}}'
`))

type recoveryConfig struct {
    Host     string
    Port     string
    Username string
    Password string
    Trigger  string
}

func writeRecoveryConf(leader *discoverd.Service, c *cli.Context) {
    f, err := os.Create(filepath.Join(c.String("data"), "recovery.conf"))
    if err != nil {
        log.Fatalln("Error creating recovery.conf:", err)
    }
    defer f.Close()

    err = recoveryTempl.Execute(f, &recoveryConfig{
        Host:     leader.Host,
        Port:     leader.Port,
        Username: leader.Attrs["username"],
        Password: leader.Attrs["password"],
        Trigger:  filepath.Join(c.String("data"), "promote.trigger"),
    })
    if err != nil {
        log.Fatalln("Error writing recovery.conf:", err)
    }
}

func updateToService(u *da.ServiceUpdate) *discoverd.Service {
    host, port, _ := net.SplitHostPort(u.Addr)
    return &discoverd.Service{
        Created: u.Created,
        Name:    u.Name,
        Addr:    u.Addr,
        Attrs:   u.Attrs,
        Host:    host,
        Port:    port,
    }
}

func waitForLeaderUp(leader *discoverd.Service, set discoverd.ServiceSet) *discoverd.Service {
    if leader.Attrs["up"] == "true" {
        return leader
    }
    log.Println("Waiting for leader to come up...")
    watch := set.Watch(true)
    defer set.Unwatch(watch)
    for update := range watch {
        if update.Addr == set.Leader().Addr && update.Attrs["up"] == "true" {
            return updateToService(update)
        }
    }
    return nil
}

func startFollower(leader *discoverd.Service, set discoverd.ServiceSet, c *cli.Context) *follower {
    log.Println("Starting as follower...")
    leader = waitForLeaderUp(leader, set)
    if err := dirIsEmpty(c.String("data")); err == nil {
        pullBaseBackup(leader, c)
    } else if err != ErrNotEmpty {
        log.Fatal(err)
    }

    writeRecoveryConf(leader, c)
    cmd, err := startPostgres(c)
    if err != nil {
        log.Fatal(err)
    }

    waitForPostgres(time.Minute, c).Close()
    register(map[string]string{"up": "true"}, c)
    log.Println("Follower started.")

    // TODO: if data and insufficient WAL, pg_basebackup
    return newFollower(cmd)
}

func switchLeader(leader *discoverd.Service, set discoverd.ServiceSet, follower *follower, c *cli.Context) *follower {
    log.Println("Switching leaders...")
    leader = waitForLeaderUp(leader, set)
    register(map[string]string{"up": "false"}, c)
    writeRecoveryConf(leader, c)
    follower.Stop()

    cmd, err := startPostgres(c)
    if err != nil {
        log.Fatal(err)
    }
    waitForPostgres(time.Minute, c).Close()
    // TODO: check for insufficient WAL, then pg_basebackup
    register(map[string]string{"up": "true"}, c)
    log.Println("Leader switch complete.")
    return newFollower(cmd)
}

func newFollower(cmd *exec.Cmd) *follower {
    f := &follower{
        cmd:  cmd,
        stop: make(chan struct{}),
        done: make(chan struct{}),
    }
    go f.wait()
    return f
}

type follower struct {
    cmd  *exec.Cmd
    stop chan struct{}
    done chan struct{}
}

func (f *follower) wait() {
    go func() {
        f.cmd.Wait()
        close(f.done)
    }()

    select {
    case <-f.done:
        procExit(f.cmd)
    case <-f.stop:
    }
}

func (f *follower) Cancel() (*exec.Cmd, <-chan struct{}) {
    close(f.stop)
    return f.cmd, f.done
}

func (f *follower) Stop() error {
    close(f.stop)
    if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
        return err
    }
    // TODO: escalate to kill?
    <-f.done
    return nil
}

func writeConfig(c *cli.Context) {
    err := copyFile("/etc/postgresql/9.3/main/postgresql.conf", filepath.Join(c.String("data"), "postgresql.conf"))
    if err != nil {
        log.Fatalln("Error creating postgresql.conf", err)
    }

    err = copyFile("/etc/postgresql/9.3/main/pg_hba.conf", filepath.Join(c.String("data"), "pg_hba.conf"))
    if err != nil {
        log.Fatalln("Error creating pg_hba.conf", err)
    }

    //err = writeCert(os.Getenv("EXTERNAL_IP"), dataDir))
    //if err != nil {
    //log.Fatalln("Error writing ssl info", err)
    //}
}

func copyFile(src, dest string) error {
    sf, err := os.Open(src)
    if err != nil {
        return err
    }
    defer sf.Close()
    df, err := os.Create(dest)
    if err != nil {
        return err
    }
    defer df.Close()

    _, err = io.Copy(df, sf)
    return err
}

func startPostgres(c *cli.Context) (*exec.Cmd, error) {
    writeConfig(c)

    log.Println("Starting postgres...")
    cmd := exec.Command(
        filepath.Join(c.String("pgbin"), "postgres"),
        "-D", c.String("data"), // Set datadir
        "-p", c.String("port"), // Set port to $PORT
        "-h", "*", // Listen on all interfaces
        "-l", // Enable SSL
    )
    log.Println("exec", strings.Join(cmd.Args, " "))
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    if err := cmd.Start(); err != nil {
        return nil, err
    }
    go handleSignals(cmd)
    return cmd, nil
}

func handleSignals(cmd *exec.Cmd) {
    c := make(chan os.Signal, 1)
    signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)

    sig := <-c
    discoverd.UnregisterAll()
    cmd.Process.Signal(sig)
}

var ErrNotEmpty = errors.New("directory is not empty")

func dirIsEmpty(dir string) error {
    d, err := os.Open(dir)
    if err != nil {
        if errno, ok := err.(syscall.Errno); ok && errno == syscall.ENOENT {
            return nil
        }
        return err
    }
    defer d.Close()

    for {
        fs, err := d.Readdir(10)
        if err != nil {
            if err == io.EOF {
                break
            }
            return err
        }
        for _, f := range fs {
            if !strings.HasPrefix(f.Name(), ".") {
                return ErrNotEmpty
            }
        }
    }

    return nil
}
