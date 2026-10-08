package commands_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/domain"
)

// The scenarios record the human-readable output of the commands against
// the in-memory store (TCL-U-03 tables and human-readable sizes, TCL-U-05).

// covers: C-CFG-01, TCL-U-02
func TestScenarioAliases(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("alias", "set", "prod", "https://s3.example.com", "AKPROD", "SKPROD", "--path", "on", "--region", "eu-west-1")
	s.run("alias", "list")
	s.run("alias", "list", "--json")
	s.run("alias", "set", "bad name", "http://x", "a", "b")
	s.run("alias", "set", "x", "ftp://x", "a", "b")
	s.run("alias", "set", "only-two", "http://x")
	s.run("alias", "remove", "prod")
	s.run("alias", "rm", "prod")
	s.run("alias", "ls")
	h.mem.FailOn("ListBuckets", errSignature)
	s.run("alias", "set", "checked", "http://s3.mem.test", "AK", "WRONG", "--verify")
	h.mem.FailOn("ListBuckets", nil)
	s.run("alias", "set", "checked", "http://s3.mem.test", "AK", "RIGHT", "--verify")
	s.run("alias", "list", "--quiet")
	golden(t, "scenario/aliases", s.b.String())
	var outputs []string
	for _, line := range strings.Split(s.b.String(), "\n") {
		if !strings.HasPrefix(line, "$ ") {
			outputs = append(outputs, line)
		}
	}
	assert.NotContains(t, strings.Join(outputs, "\n"), "SKPROD", "secret keys are never printed")
	assert.NotContains(t, strings.Join(outputs, "\n"), "MEMSECRET")
}

// covers: C-OBJ-01, C-OBJ-02, TCL-E-02
func TestScenarioBuckets(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("ls", "mem")
	s.run("mb", "mem/photos")
	s.run("mb", "mem/photos")
	s.run("mb", "--ignore-existing", "mem/photos")
	s.run("mb", "--with-versioning", "mem/docs", "mem/logs")
	s.run("mb", "mem/x")
	s.run("mb", "mem/photos/key")
	s.run("mb", "mem")
	s.run("ls", "mem")
	s.run("stat", "mem/docs")
	s.run("version", "info", "mem/photos")
	h.mem.PutObject("logs", "app.log", "line\n", clitest.ObjectSpec{})
	s.run("rb", "mem/logs")
	s.run("rb", "--force", "mem/logs")
	s.run("rb", "mem/photos", "mem/missing")
	s.run("ls", "mem")
	golden(t, "scenario/buckets", s.b.String())
}

// covers: C-OBJ-01, C-OBJ-03, C-OBJ-04, C-OBJ-05, TCL-E-03, TCL-E-05, TCL-E-09
func TestScenarioObjects(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("bkt")
	h.mem.MakeBucket("dst")
	h.write("page.html", "<p>hello</p>\n")
	h.write("site/index.html", "<h1>index</h1>\n")
	h.write("site/css/main.css", "body{}\n")
	h.write("site/js/app.js", "let a = 1;\n")
	h.write("notes.json", "{\"a\":1}\n")
	s := h.session()
	s.run("cp", "page.html", "mem/bkt/")
	s.run("cp", "--attr", "owner=ops;env=prod", "--tags", "team=web", "notes.json", "mem/bkt/data/notes.json")
	s.run("cp", "--recursive", "site", "mem/bkt/")
	s.run("cp", "--recursive", "site/", "mem/dst/copy/")
	s.run("cp", "site", "mem/bkt/")
	s.run("cp", "missing.txt", "mem/bkt/")
	s.run("cp", "page.html")
	s.run("cp", "--part-size", "1MiB", "page.html", "mem/bkt/")
	s.run("cp", "--parallel", "0", "page.html", "mem/bkt/")
	s.run("ls", "mem/bkt")
	s.run("ls", "mem/bkt/site")
	s.run("ls", "--recursive", "mem/bkt")
	s.run("ls", "mem/bkt/page.html")
	s.run("stat", "mem/bkt/data/notes.json")
	s.run("stat", "mem/bkt/site")
	s.run("stat", "mem/bkt/nope")
	s.run("cat", "mem/bkt/page.html", "mem/bkt/site/css/main.css")
	s.run("head", "-n", "1", "mem/bkt/data/notes.json")
	s.run("cat", "mem/bkt/site/")
	s.run("cp", "mem/bkt/page.html", "downloaded.html")
	s.run("cp", "--recursive", "mem/bkt/site/", "out/")
	s.run("cp", "mem/bkt/page.html", "mem/dst/remote.html")
	s.run("mv", "mem/dst/remote.html", "mem/dst/moved.html")
	s.run("ls", "--recursive", "mem/dst")
	s.run("mv", "downloaded.html", "mem/dst/up.html")
	s.run("rm", "mem/dst/up.html")
	s.run("rm", "mem/dst/up.html")
	s.run("rm", "--recursive", "mem/dst/copy")
	s.run("rm", "--recursive", "--force", "--dry-run", "mem/dst/copy")
	s.run("rm", "--recursive", "--force", "mem/dst/copy")
	s.run("ls", "--recursive", "mem/dst")
	assert.Equal(t, "<h1>index</h1>\n", h.read("out/index.html"))
	assert.Equal(t, "body{}\n", h.read("out/css/main.css"))
	golden(t, "scenario/objects", s.b.String())
}

// covers: S-OBJ-10, C-OBJ-04, C-OBJ-09, TCL-E-05
func TestScenarioVersionsAndTags(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("mb", "--with-versioning", "mem/ver")
	h.mem.PutObject("ver", "doc.json", "{\"v\":1}", clitest.ObjectSpec{})
	h.clock.Advance(time.Minute)
	h.mem.PutObject("ver", "doc.json", "{\"v\":2}", clitest.ObjectSpec{})
	h.clock.Advance(time.Minute)
	s.run("rm", "mem/ver/doc.json")
	s.run("ls", "--versions", "mem/ver")
	s.run("ls", "--versions", "mem/ver/doc.json")
	s.run("cat", "--version-id", "v0001", "mem/ver/doc.json")
	s.run("stat", "--version-id", "v0002", "mem/ver/doc.json")
	s.run("cp", "--version-id", "v0001", "mem/ver/doc.json", "mem/ver/restored.json")
	s.run("rm", "--version-id", "v0003", "mem/ver/doc.json")
	s.run("cat", "mem/ver/doc.json")
	s.run("rm", "--versions", "mem/ver/doc.json")
	s.run("ls", "--versions", "--recursive", "mem/ver")
	s.run("version", "suspend", "mem/ver")
	s.run("version", "info", "mem/ver")
	s.run("version", "enable", "mem/ver")
	s.run("version", "bogus")
	s.run("tag", "set", "mem/ver/restored.json", "project=nexo&status=final")
	s.run("tag", "list", "mem/ver/restored.json")
	s.run("tag", "remove", "mem/ver/restored.json")
	s.run("tag", "list", "mem/ver/restored.json")
	s.run("tag", "set", "mem/ver", "team=ops")
	s.run("tag", "list", "mem/ver")
	s.run("tag", "set", "mem/ver/dir/", "a=1")
	s.run("tag", "set", "mem/ver/restored.json", "a=1&a=2")
	s.run("stat", "mem/ver")
	golden(t, "scenario/versions", s.b.String())
}

// covers: C-OBJ-04, C-OBJ-05
func TestScenarioIncompleteAndPipe(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("up")
	h.mem.AddUpload("up", "big/a.bin", "upload-1")
	h.mem.AddUpload("up", "big/b.bin", "upload-2")
	s := h.session()
	s.run("ls", "--incomplete", "mem/up")
	s.run("rm", "--incomplete", "mem/up/big/a.bin")
	s.run("rm", "--incomplete", "--recursive", "--force", "mem/up/big/")
	s.run("ls", "--incomplete", "mem/up")
	h.stdin = "streamed from stdin\n"
	s.run("pipe", "--content-type", "text/plain", "mem/up/stream.txt")
	s.run("cat", "mem/up/stream.txt")
	s.run("pipe", "mem/up/")
	golden(t, "scenario/incomplete", s.b.String())
}

// covers: C-OBJ-06, C-OBJ-08, TCL-E-06, TCL-E-09
func TestScenarioMirrorAndUtilities(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("site")
	h.write("src/index.html", "<h1>v1</h1>\n")
	h.write("src/css/a.css", "a{}\n")
	h.write("src/css/b.css", "b{}\n")
	h.write("src/img/logo.svg", "<svg/>\n")
	h.write("src/logs/old.log", "old\n")
	s := h.session()
	h.clock.Advance(time.Hour)
	s.run("mirror", "src", "mem/site")
	s.run("mirror", "src", "mem/site")
	h.write("src/index.html", "<h1>v2!</h1>\n")
	h.write("src/new.html", "<p>new</p>\n")
	require.NoError(t, removeFile(h, "src/css/b.css"))
	s.run("mirror", "--dry-run", "--remove", "src", "mem/site")
	s.run("mirror", "src", "mem/site")
	s.run("mirror", "--overwrite", "--remove", "src", "mem/site")
	s.run("mirror", "src", "mem/site")
	s.run("mirror", "mem/site", "backup")
	s.run("mirror", "mem/site", "backup")
	s.run("diff", "src", "mem/site")
	h.write("src/extra.html", "<p>extra</p>\n")
	h.write("src/new.html", "<p>new, changed</p>\n")
	s.run("diff", "src", "mem/site")
	s.run("tree", "mem/site")
	s.run("tree", "-d", "mem/site")
	s.run("du", "mem/site")
	s.run("du", "--depth", "1", "mem/site")
	s.run("find", "mem/site", "--name", "*.css")
	s.run("find", "mem/site", "--larger", "10B")
	s.run("find", "mem/site", "--name", "*.log", "--older-than", "30m")
	s.run("find", "mem/site", "--newer-than", "30m")
	s.run("find", "mem/site", "--path", "css/*", "--exec", "echo found {}")
	s.run("find", "mem/site", "--name", "[")
	s.run("mirror", "src/index.html", "mem/site")
	s.run("mirror", "--watch", "mem/site", "out")
	golden(t, "scenario/mirror", s.b.String())
	assert.Equal(t, []string{"echo found mem/site/css/a.css"}, h.execs)
	assert.Equal(t, "<h1>v2!</h1>\n", h.read("backup/index.html"))
}

// covers: C-OBJ-07, C-OBJ-07b, TCL-P-01, TCL-P-02, TCL-P-04, TCL-P-06
func TestScenarioSharing(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("pub")
	h.mem.PutObject("pub", "docs/report.pdf", "%PDF", clitest.ObjectSpec{})
	h.mem.PutObject("pub", "docs/2026/a.txt", "a", clitest.ObjectSpec{})
	s := h.session()
	s.run("share", "download", "--raw", "mem/pub/docs/report.pdf")
	s.run("share", "download", "--raw", "--expire", "2h", "--content-disposition", "attachment", "mem/pub/docs/report.pdf")
	s.run("share", "download", "--raw", "--recursive", "--expire", "1h", "mem/pub/docs/")
	s.run("share", "download", "--raw", "mem/pub/docs/")
	s.run("share", "download", "--raw", "--expire", "8d", "mem/pub/docs/report.pdf")
	s.run("share", "upload", "--raw", "--expire", "1h", "--content-type", "application/pdf", "mem/pub/inbox/new.pdf")
	s.run("share", "upload", "--raw", "mem/pub/inbox/")
	s.run("share", "upload", "--post", "--max-size", "10MiB", "--prefix", "uploads/", "--expire", "1h", "mem/pub")
	s.run("share", "upload", "--post", "--expire", "1h", "mem/pub/exact.bin")
	s.run("share", "upload", "--max-size", "1MiB", "mem/pub/x")
	s.run("presign", "--method", "DELETE", "--expire", "10m", "mem/pub/docs/report.pdf")
	s.run("presign", "-X", "PUT", "-H", "Content-Type: image/png", "-H", "x-amz-meta-origin: cli", "mem/pub/img.png")
	s.run("presign", "--method", "PATCH", "mem/pub/x")
	s.run("presign", "-H", "bad", "mem/pub/x")
	s.run("anonymous", "get", "mem/pub")
	s.run("anonymous", "set", "download", "mem/pub/docs/")
	s.run("anonymous", "set", "upload", "mem/pub/inbox/")
	s.run("anonymous", "get", "mem/pub/docs/")
	s.run("anonymous", "get", "mem/pub/inbox/")
	s.run("anonymous", "get", "mem/pub/other/")
	s.run("anonymous", "list", "mem/pub")
	s.run("anonymous", "get-json", "mem/pub")
	s.run("anonymous", "set", "private", "mem/pub/docs/")
	s.run("anonymous", "set", "private", "mem/pub/inbox/")
	s.run("anonymous", "get-json", "mem/pub")
	s.run("anonymous", "set", "readonly", "mem/pub")
	s.run("anonymous", "set", "download", "mem/pub/a*")
	h.write("policy.json", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)
	s.run("anonymous", "set-json", "policy.json", "mem/pub")
	s.run("anonymous", "get", "mem/pub")
	s.run("anonymous", "list", "mem/pub")
	h.write("bad.json", `{"Statement":`)
	s.run("anonymous", "set-json", "bad.json", "mem/pub")
	h.write("empty.json", `{"Version":"2012-10-17","Statement":[]}`)
	s.run("anonymous", "set-json", "empty.json", "mem/pub")
	s.run("anonymous", "get", "mem/pub")
	golden(t, "scenario/sharing", s.b.String())
	assert.Empty(t, h.mem.Policy("pub"))
}

func removeFile(h *harness, rel string) error {
	return os.Remove(filepath.Join(h.dir, filepath.FromSlash(rel)))
}

// errSignature is the failure of wrong credentials.
var errSignature = fmt.Errorf("%w: the secret key is wrong", domain.ErrSignatureMismatch)
