package commands

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/clock"
)

// Deps are the process-level dependencies of the commands (docs/12 §12.4:
// cmd/nexoctl builds them, the tests replace them).
type Deps struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads the environment (nil: the process environment).
	LookupEnv cli.LookupEnv
	// Aliases builds the alias service (nil: the JSON file store at the
	// configuration path).
	Aliases func() (cli.AliasService, error)
	// S3 builds the S3 clients of the aliases (nil: aws-sdk-go-v2).
	S3 cli.S3Factory
	// Admin builds the Admin API clients of the aliases (nil:
	// pkg/nexoadmin).
	Admin cli.AdminFactory
	// Clock dates the outputs and the age filters (nil: the wall clock).
	Clock clock.Clock
	// IsTerminal reports whether a writer is a terminal (nil:
	// cli.IsTerminal): it enables colors and progress bars.
	IsTerminal func(io.Writer) bool
	// HomeDir returns the home directory used to expand "~" (nil:
	// os.UserHomeDir).
	HomeDir func() (string, error)
	// Exec runs the command of find --exec (nil: os/exec).
	Exec func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
	// WatchDebounce coalesces the file events of mirror --watch (0:
	// cli.DefaultWatchDebounce).
	WatchDebounce time.Duration
}

// DefaultDeps returns the dependencies of the real process.
func DefaultDeps() Deps {
	return Deps{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.LookupEnv}
}

func (d Deps) withDefaults() Deps {
	if d.Stdin == nil {
		d.Stdin = os.Stdin
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	if d.LookupEnv == nil {
		d.LookupEnv = os.LookupEnv
	}
	if d.Clock == nil {
		d.Clock = clock.NewReal()
	}
	if d.S3 == nil {
		d.S3 = cli.NewS3Factory(cli.S3FactoryDeps{Clock: d.Clock})
	}
	if d.Admin == nil {
		d.Admin = cli.NewAdminFactory(cli.AdminFactoryDeps{Clock: d.Clock})
	}
	if d.IsTerminal == nil {
		d.IsTerminal = cli.IsTerminal
	}
	if d.HomeDir == nil {
		d.HomeDir = os.UserHomeDir
	}
	if d.Exec == nil {
		d.Exec = func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
			c := exec.CommandContext(ctx, name, args...) // #nosec G204 -- runs the program the user asked nexoctl to run (watch --exec)
			c.Stdout, c.Stderr = stdout, stderr
			return c.Run()
		}
	}
	if d.WatchDebounce <= 0 {
		d.WatchDebounce = cli.DefaultWatchDebounce
	}
	return d
}

// env is the state of one execution of the command tree: the parsed
// global flags, the printer, the aliases and the S3 clients (one per
// alias).
type env struct {
	deps Deps
	// started reports that a command passed the validation of its flags
	// and arguments (Main classifies the earlier errors as usage errors).
	started bool

	out       *cli.Printer
	debug     bool
	configDir string
	// limitUpload and limitDownload are set by the transfer commands.
	limitUpload   int64
	limitDownload int64

	mu       sync.Mutex
	aliasSvc cli.AliasService
	aliases  map[string]cli.Alias
	clients  map[string]cli.S3Client
	admins   map[string]cli.AdminClient
}

func newEnv(deps Deps) *env {
	return &env{deps: deps, clients: map[string]cli.S3Client{}, admins: map[string]cli.AdminClient{}}
}

// init reads the global flags (PersistentPreRunE of the root).
func (e *env) init(cmd *cobra.Command) error {
	flags := cmd.Flags()
	jsonOut, _ := flags.GetBool(flagJSON)
	quiet, _ := flags.GetBool(flagQuiet)
	noColor, _ := flags.GetBool(flagNoColor)
	e.debug, _ = flags.GetBool(flagDebug)
	e.configDir, _ = flags.GetString(flagConfigDir)
	_, noColorEnv := e.deps.LookupEnv("NO_COLOR")
	color := !noColor && !noColorEnv && e.deps.IsTerminal(e.deps.Stdout)
	e.out = cli.NewPrinter(e.deps.Stdout, e.deps.Stderr, cli.PrinterOptions{JSON: jsonOut, Quiet: quiet, Color: color})
	return nil
}

// printer returns the printer (a plain one before init).
func (e *env) printer() *cli.Printer {
	if e.out == nil {
		e.out = cli.NewPrinter(e.deps.Stdout, e.deps.Stderr, cli.PrinterOptions{})
	}
	return e.out
}

// progress returns a progress bar over total bytes, drawn on stderr when
// both outputs are terminals and neither --quiet nor --json is set.
func (e *env) progress(total int64) *cli.Progress {
	p := e.printer()
	enabled := !p.JSON() && !p.Quiet() && e.deps.IsTerminal(e.deps.Stdout) && e.deps.IsTerminal(e.deps.Stderr)
	return cli.NewProgress(e.deps.Stderr, total, enabled, e.deps.Clock)
}

// aliasService returns the alias service.
func (e *env) aliasService() (cli.AliasService, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.aliasSvc != nil {
		return e.aliasSvc, nil
	}
	if e.deps.Aliases != nil {
		svc, err := e.deps.Aliases()
		if err != nil {
			return nil, err
		}
		e.aliasSvc = svc
		return svc, nil
	}
	path, err := e.configPath()
	if err != nil {
		return nil, err
	}
	e.aliasSvc = cli.NewAliasService(cli.AliasDeps{Store: cli.NewFileStore(path), Clock: e.deps.Clock})
	return e.aliasSvc, nil
}

// configPath returns the configuration file: --config-dir, then
// $NEXOCTL_CONFIG_DIR, then ~/.nexoctl.
func (e *env) configPath() (string, error) {
	if e.configDir != "" {
		return e.configDir + string(os.PathSeparator) + cli.ConfigFileName, nil
	}
	return cli.DefaultConfigPath(e.deps.LookupEnv)
}

// loadAliases returns the aliases by name (loaded once).
func (e *env) loadAliases(ctx context.Context) (map[string]cli.Alias, error) {
	svc, err := e.aliasService()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.aliases != nil {
		return e.aliases, nil
	}
	list, err := svc.List(ctx)
	if err != nil {
		return nil, err
	}
	e.aliases = make(map[string]cli.Alias, len(list))
	for _, a := range list {
		e.aliases[a.Name] = a
	}
	return e.aliases, nil
}

// client returns the S3 client of an alias (created once).
func (e *env) client(ctx context.Context, alias string) (cli.S3Client, error) {
	aliases, err := e.loadAliases(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.clients[alias]; c != nil {
		return c, nil
	}
	a, ok := aliases[alias]
	if !ok {
		return nil, &cli.AliasNotFoundError{Name: alias}
	}
	c, err := e.deps.S3.Client(ctx, a, cli.ClientOptions{
		Debug: debugWriter(e), LimitUpload: e.limitUpload, LimitDownload: e.limitDownload,
	})
	if err != nil {
		return nil, err
	}
	e.clients[alias] = c
	return c, nil
}

// admin returns the Admin API client of an alias (created once).
func (e *env) admin(ctx context.Context, alias string) (cli.AdminClient, error) {
	aliases, err := e.loadAliases(ctx)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.admins[alias]; c != nil {
		return c, nil
	}
	a, ok := aliases[alias]
	if !ok {
		return nil, &cli.AliasNotFoundError{Name: alias}
	}
	c, err := e.deps.Admin.Client(ctx, a, cli.ClientOptions{Debug: debugWriter(e)})
	if err != nil {
		return nil, err
	}
	e.admins[alias] = c
	return c, nil
}

// adminAlias parses an argument naming an alias ("ALIAS" or "ALIAS/") and
// returns its name and its Admin API client.
func (e *env) adminAlias(ctx context.Context, arg string) (string, cli.AdminClient, error) {
	name, rest, _ := strings.Cut(arg, "/")
	if name == "" || strings.Trim(rest, "/") != "" {
		return "", nil, cli.UsageError("%s: expected an alias name (ALIAS), not a bucket or an object", arg)
	}
	c, err := e.admin(ctx, name)
	if err != nil {
		return "", nil, err
	}
	return name, c, nil
}

// resolver adapts client to cli.ClientResolver.
func (e *env) resolver() cli.ClientResolver { return e.client }

// transfer returns the transfer engine.
func (e *env) transfer() cli.Transfer {
	return cli.Transfer{Resolve: e.resolver()}
}

// parser returns the target parser over the configured aliases.
func (e *env) parser(ctx context.Context) (cli.TargetParser, error) {
	aliases, err := e.loadAliases(ctx)
	if err != nil {
		return cli.TargetParser{}, err
	}
	home, _ := e.deps.HomeDir()
	return cli.NewTargetParser(func(name string) bool { _, ok := aliases[name]; return ok }, home), nil
}

// targets parses arguments; local paths with glob characters are expanded
// when glob is set.
func (e *env) targets(ctx context.Context, args []string, glob bool) ([]cli.Target, error) {
	p, err := e.parser(ctx)
	if err != nil {
		return nil, err
	}
	var out []cli.Target
	for _, a := range args {
		t, err := p.Parse(a)
		if err != nil {
			return nil, err
		}
		if glob && !t.IsRemote() && cli.HasGlob(t.Path) {
			matches, err := cli.ExpandGlob(t.Path)
			if err != nil {
				return nil, err
			}
			for _, m := range matches {
				out = append(out, cli.Target{Raw: m, Path: m})
			}
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// remoteTarget parses one argument that must name a remote location;
// needBucket requires a bucket.
func (e *env) remoteTarget(ctx context.Context, arg string, needBucket bool) (cli.Target, error) {
	p, err := e.parser(ctx)
	if err != nil {
		return cli.Target{}, err
	}
	t, err := p.Parse(arg)
	if err != nil {
		return cli.Target{}, err
	}
	if !t.IsRemote() {
		first, _, _ := strings.Cut(arg, "/")
		return cli.Target{}, &cli.AliasNotFoundError{Name: first, Arg: arg}
	}
	if needBucket && t.Bucket == "" {
		return cli.Target{}, cli.UsageError("%s: a bucket is required (ALIAS/BUCKET)", arg)
	}
	return t, nil
}
