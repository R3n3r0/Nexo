// Package cli_test runs the end-to-end scenarios of nexoctl (docs/06 §6.4,
// docs/09 §9.6): the testscript files *.txtar of this directory drive the
// real command, registered in-process with testscript.RunMain, against a
// Nexo server started in-process for each script (internal/testutil), with
// an isolated HOME holding the alias "local" of the server (root
// credentials, Admin API on the console listener). The scripts read the
// endpoints in $NEXO_URL (S3) and $NEXO_ADMIN_URL (Admin API) and the root
// credentials in $NEXO_ROOT_USER and $NEXO_ROOT_PASSWORD.
//
// Custom commands available to the scripts:
//
//	expectcode CODE PROG ARGS...     run PROG and require exit code CODE
//	foreigns3 VAR BUCKET KEY...      start a minimal S3 endpoint that, like
//	                                 AWS S3, accepts any key (even "../x"),
//	                                 serving BUCKET with the objects KEY
//	                                 ("data of KEY"); VAR gets its URL
//	eventually DURATION [-match RE] [!] PROG ARGS...
//	                                 retry PROG until it succeeds (fails with !)
//	                                 and its stdout matches RE
//	http [-X M] [-H 'K: V']... [-T FILE] [-o FILE] [-status CODE] URL
//	                                 send a request (default: expect 2xx);
//	                                 the body goes to stdout or to FILE
//	postpolicy RECORD FILE [-status CODE]
//	                                 upload FILE with the POST policy of a JSON
//	                                 record of "share upload --post --json"
//	jsonfield FILE PATH VAR          set VAR to the member PATH (a.b) of the
//	                                 first JSON line of FILE
//	jsonlines FILE                   require JSON Lines whose objects start
//	                                 with a "status" member
//	mkfile PATH SIZE                 write SIZE (e.g. 12MiB) pseudo-random bytes
//	sleep DURATION                   wait
package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/commands"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// envClockOffset shifts the clock of nexoctl in the scripts (e.g. "48h"
// makes the objects of the server look two days old to find --older-than).
const envClockOffset = "NEXOCTL_TEST_CLOCK_OFFSET"

func TestMain(m *testing.M) {
	// RunMain is the registration the docs prescribe (docs/06 §6.4); it
	// never returns.
	os.Exit(testscript.RunMain(m, map[string]func() int{"nexoctl": nexoctlMain})) //nolint:staticcheck // SA1019: RunMain keeps the exit code of the command explicit.
}

// nexoctlMain is the nexoctl binary inside the test binary.
func nexoctlMain() int {
	deps := commands.DefaultDeps()
	if v := os.Getenv(envClockOffset); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", envClockOffset, err)
			return cli.ExitUsage
		}
		deps.Clock = offsetClock{offset: d}
	}
	return commands.Run(deps, os.Args[1:])
}

// offsetClock is the wall clock shifted by an offset.
type offsetClock struct{ offset time.Duration }

var _ clock.Clock = offsetClock{}

func (c offsetClock) Now() time.Time { return time.Now().Add(c.offset) }

// covers: TCL-E-01..15, TCL-E-18, TCL-E-19, TCL-P-01..06
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 ".",
		Setup:               setup,
		RequireExplicitExec: true,
		RequireUniqueNames:  true,
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"expectcode": cmdExpectCode,
			"eventually": cmdEventually,
			"foreigns3":  cmdForeignS3,
			"http":       cmdHTTP,
			"postpolicy": cmdPostPolicy,
			"jsonfield":  cmdJSONField,
			"jsonlines":  cmdJSONLines,
			"mkfile":     cmdMkfile,
			"sleep":      cmdSleep,
		},
	})
}

// setup starts a server for the script and configures the alias "local"
// in an isolated HOME.
func setup(env *testscript.Env) error {
	tb, ok := env.T().(testing.TB)
	if !ok {
		return errors.New("the testscript T is not a testing.TB")
	}
	srv := testutil.StartServer(tb)
	home := filepath.Join(env.WorkDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	env.Setenv("HOME", home)
	env.Setenv("USERPROFILE", home)
	env.Setenv("NO_COLOR", "1")
	env.Setenv("NEXO_URL", srv.S3URL)
	env.Setenv("NEXO_ADMIN_URL", srv.ConsoleURL)
	env.Setenv("NEXO_ROOT_USER", srv.RootUser)
	env.Setenv("NEXO_ROOT_PASSWORD", srv.RootPassword)
	svc := cli.NewAliasService(cli.AliasDeps{Store: cli.NewFileStore(filepath.Join(home, cli.ConfigDirName, cli.ConfigFileName))})
	return svc.Set(context.Background(), cli.Alias{Name: "local", URL: srv.S3URL, AdminURL: srv.ConsoleURL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword})
}

// exitCode returns the exit code of an Exec error (0 for nil).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func cmdExpectCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: expectcode CODE PROG ARGS...")
	}
	want, err := strconv.Atoi(args[0])
	ts.Check(err)
	got := exitCode(ts.Exec(args[1], args[2:]...))
	if got != want {
		ts.Fatalf("%s exited with code %d, want %d", args[1], got, want)
	}
}

func cmdEventually(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: eventually DURATION [-match RE] [!] PROG ARGS...")
	}
	timeout, err := time.ParseDuration(args[0])
	ts.Check(err)
	args = args[1:]
	var match *regexp.Regexp
	if len(args) > 2 && args[0] == "-match" {
		match, err = regexp.Compile(args[1])
		ts.Check(err)
		args = args[2:]
	}
	wantFailure := false
	if len(args) > 0 && args[0] == "!" {
		wantFailure = true
		args = args[1:]
	}
	if len(args) == 0 {
		ts.Fatalf("eventually: missing program")
	}
	deadline := time.Now().Add(timeout)
	for {
		err := ts.Exec(args[0], args[1:]...)
		ok := (err == nil) != wantFailure
		if ok && match != nil {
			ok = match.MatchString(ts.ReadFile("stdout"))
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			ts.Fatalf("%s did not reach the expected state within %s (last error: %v)", strings.Join(args, " "), timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// httpClient is the client of the http and postpolicy commands.
func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

func cmdHTTP(ts *testscript.TestScript, neg bool, args []string) {
	method, body, out, status := http.MethodGet, "", "", 0
	headers := http.Header{}
	var url string
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 >= len(args) {
				ts.Fatalf("http: %s needs a value", args[i])
			}
			i++
			return args[i]
		}
		switch args[i] {
		case "-X":
			method = next()
		case "-H":
			k, v, ok := strings.Cut(next(), ":")
			if !ok {
				ts.Fatalf("http: invalid header %q", args[i])
			}
			headers.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		case "-T":
			body = ts.MkAbs(next())
		case "-o":
			out = ts.MkAbs(next())
		case "-status":
			code, err := strconv.Atoi(next())
			ts.Check(err)
			status = code
		default:
			url = args[i]
		}
	}
	if url == "" {
		ts.Fatalf("usage: http [-X M] [-H 'K: V']... [-T FILE] [-o FILE] [-status CODE] URL")
	}
	var reader io.Reader = http.NoBody
	var length int64
	if body != "" {
		data, err := os.ReadFile(body)
		ts.Check(err)
		reader, length = bytes.NewReader(data), int64(len(data))
	}
	req, err := http.NewRequest(method, url, reader)
	ts.Check(err)
	req.ContentLength = length
	req.Header = headers
	resp, err := httpClient().Do(req)
	ts.Check(err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	ts.Check(err)
	checkStatus(ts, neg, resp.StatusCode, status, data)
	if out != "" {
		ts.Check(os.WriteFile(out, data, 0o600))
		return
	}
	_, err = ts.Stdout().Write(data)
	ts.Check(err)
}

// checkStatus compares a status code with the expected one (any 2xx when
// want is 0); neg inverts the check.
func checkStatus(ts *testscript.TestScript, neg bool, got, want int, body []byte) {
	ok := got == want || (want == 0 && got >= 200 && got < 300)
	if ok == neg {
		ts.Fatalf("unexpected HTTP status %d (want %d, negated %v): %s", got, want, neg, body)
	}
}

func cmdPostPolicy(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 2 && len(args) != 4 {
		ts.Fatalf("usage: postpolicy RECORD FILE [-status CODE]")
	}
	status := 0
	if len(args) == 4 {
		code, err := strconv.Atoi(args[3])
		ts.Check(err)
		status = code
	}
	var rec struct {
		URL    string            `json:"url"`
		Fields map[string]string `json:"fields"`
	}
	first, _, _ := strings.Cut(ts.ReadFile(args[0]), "\n")
	ts.Check(json.Unmarshal([]byte(first), &rec))
	data, err := os.ReadFile(ts.MkAbs(args[1]))
	ts.Check(err)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, k := range cli.SortedKeys(rec.Fields) {
		ts.Check(w.WriteField(k, rec.Fields[k]))
	}
	part, err := w.CreateFormFile("file", filepath.Base(args[1]))
	ts.Check(err)
	_, err = part.Write(data)
	ts.Check(err)
	ts.Check(w.Close())
	resp, err := httpClient().Post(rec.URL, w.FormDataContentType(), &buf)
	ts.Check(err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	checkStatus(ts, neg, resp.StatusCode, status, body)
}

func cmdJSONField(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 3 {
		ts.Fatalf("usage: jsonfield FILE PATH VAR")
	}
	first, _, _ := strings.Cut(ts.ReadFile(args[0]), "\n")
	var v any
	ts.Check(json.Unmarshal([]byte(first), &v))
	for _, key := range strings.Split(args[1], ".") {
		m, ok := v.(map[string]any)
		if !ok {
			ts.Fatalf("jsonfield: %s is not an object in %s", key, first)
		}
		if v, ok = m[key]; !ok {
			ts.Fatalf("jsonfield: no member %q in %s", args[1], first)
		}
	}
	switch x := v.(type) {
	case string:
		ts.Setenv(args[2], x)
	default:
		b, err := json.Marshal(x)
		ts.Check(err)
		ts.Setenv(args[2], string(b))
	}
}

func cmdJSONLines(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: jsonlines FILE")
	}
	sc := bufio.NewScanner(strings.NewReader(ts.ReadFile(args[0])))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, `{"status":"`) {
			ts.Fatalf("jsonlines: line %d does not start with the status: %s", n+1, line)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			ts.Fatalf("jsonlines: line %d is not JSON: %v: %s", n+1, err, line)
		}
		switch rec["status"] {
		case "success", "warning", "error":
		default:
			ts.Fatalf("jsonlines: line %d has status %v", n+1, rec["status"])
		}
		n++
	}
	ts.Check(sc.Err())
	if n == 0 {
		ts.Fatalf("jsonlines: no record in %s", args[0])
	}
}

func cmdMkfile(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: mkfile PATH SIZE")
	}
	size, err := cli.ParseSize(args[1])
	ts.Check(err)
	h := fnv.New64a()
	_, _ = h.Write([]byte(args[0]))
	rng := rand.New(rand.NewPCG(h.Sum64(), uint64(size)))
	data := make([]byte, size)
	for i := 0; i+8 <= len(data); i += 8 {
		v := rng.Uint64()
		for j := range 8 {
			data[i+j] = byte(v >> (8 * j))
		}
	}
	for i := len(data) - len(data)%8; i < len(data); i++ {
		data[i] = byte(rng.Uint32())
	}
	path := ts.MkAbs(args[0])
	ts.Check(os.MkdirAll(filepath.Dir(path), 0o755))
	ts.Check(os.WriteFile(path, data, 0o600))
}

func cmdSleep(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: sleep DURATION")
	}
	d, err := time.ParseDuration(args[0])
	ts.Check(err)
	time.Sleep(d)
}
