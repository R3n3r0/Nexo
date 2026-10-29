package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Layout of the cluster archive (a zip file).
const (
	// ClusterArchiveFormat names the archive in its manifest.
	ClusterArchiveFormat = "nexo-cluster-export"
	// ClusterArchiveVersion is the version of the layout written; an
	// import refuses newer ones.
	ClusterArchiveVersion = 1
	// MaxClusterArchive bounds the size of an archive, and the total size
	// of its documents once uncompressed.
	MaxClusterArchive = 64 << 20
	// MaxClusterArchiveEntries bounds the number of entries of an archive
	// (the documents and one per bucket).
	MaxClusterArchiveEntries = 1 << 16

	archiveManifest = "manifest.json"
	archiveIAM      = "iam.bin"
	archivePolicies = "policies.json"
	archiveConfig   = "config.json"
	archiveBuckets  = "buckets/"
)

// ClusterManagerDeps are the dependencies of the cluster manager. Every
// service is required.
type ClusterManagerDeps struct {
	IAM     iam.Service
	Policy  policy.Service
	Config  ConfigManager
	Buckets bucket.Service
	Clock   clock.Clock
	Logger  *slog.Logger
}

// ClusterManagerOption configures the cluster manager.
type ClusterManagerOption func(*clusterManager)

// WithServerVersion records the version of the server in the manifests of
// the archives (default "dev").
func WithServerVersion(v string) ClusterManagerOption {
	return func(m *clusterManager) {
		if v != "" {
			m.version = v
		}
	}
}

type clusterManager struct {
	deps    ClusterManagerDeps
	version string
}

var _ ClusterManager = (*clusterManager)(nil)

// NewClusterManager returns the cluster manager. A missing service is a
// wiring error (domain.ErrInvalidArgument).
func NewClusterManager(deps ClusterManagerDeps, opts ...ClusterManagerOption) (ClusterManager, error) {
	if deps.IAM == nil || deps.Policy == nil || deps.Config == nil || deps.Buckets == nil {
		return nil, fmt.Errorf("%w: cluster manager requires the IAM, policy, config and bucket services", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &clusterManager{deps: deps, version: "dev"}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// clusterManifest is manifest.json: what the archive holds. The names are
// in clear (the IAM document itself is encrypted), so that an import knows
// what the replace mode keeps.
type clusterManifest struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	Created       time.Time `json:"created"`
	ServerVersion string    `json:"serverVersion"`
	Users         []string  `json:"users"`
	Groups        []string  `json:"groups"`
	Policies      []string  `json:"policies"`
	Config        []string  `json:"config"`
	Buckets       []string  `json:"buckets"`
}

// archivedPolicy is one named policy of policies.json.
type archivedPolicy struct {
	Name   string          `json:"name"`
	Policy json.RawMessage `json:"policy"`
}

// archivedBucket is buckets/<name>.json: the settings of one bucket. The
// documents (policy, lifecycle, CORS, notification) are kept as the text
// the bucket stores; the media configuration as its JSON document.
type archivedBucket struct {
	Name         string                   `json:"name"`
	Versioning   string                   `json:"versioning,omitempty"`
	ObjectLock   *bucket.ObjectLockConfig `json:"objectLock,omitempty"`
	Policy       string                   `json:"policy,omitempty"`
	Lifecycle    string                   `json:"lifecycle,omitempty"`
	Encryption   *bucket.EncryptionConfig `json:"encryption,omitempty"`
	Quota        *bucket.Quota            `json:"quota,omitempty"`
	Tags         map[string]string        `json:"tags,omitempty"`
	CORS         string                   `json:"cors,omitempty"`
	Notification string                   `json:"notification,omitempty"`
	Media        *domain.MediaConfig      `json:"media,omitempty"`
	// MediaProtection is the download protection of the videos.
	MediaProtection *domain.MediaProtection `json:"mediaProtection,omitempty"`
}

// Export implements ClusterManager.
func (m *clusterManager) Export(ctx context.Context) ([]byte, error) {
	man := clusterManifest{Format: ClusterArchiveFormat, Version: ClusterArchiveVersion, Created: m.deps.Clock.Now().UTC(), ServerVersion: m.version}
	users, err := m.deps.IAM.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: export users: %w", err)
	}
	for _, u := range users {
		man.Users = append(man.Users, u.AccessKey)
	}
	groups, err := m.deps.IAM.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: export groups: %w", err)
	}
	for _, g := range groups {
		man.Groups = append(man.Groups, g.Name)
	}
	iamDoc, err := m.deps.IAM.Export(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: export IAM: %w", err)
	}
	named, err := m.deps.Policy.ListPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: export policies: %w", err)
	}
	var policies []archivedPolicy
	for _, p := range named {
		if p.BuiltIn {
			continue
		}
		doc, err := json.Marshal(p.Policy)
		if err != nil {
			return nil, fmt.Errorf("admin: export policy %s: %w", p.Name, err)
		}
		policies = append(policies, archivedPolicy{Name: p.Name, Policy: doc})
		man.Policies = append(man.Policies, p.Name)
	}
	cfgDoc, err := m.deps.Config.Export(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: export configuration: %w", err)
	}
	var cfgAll map[string]config.KV
	if err := json.Unmarshal(cfgDoc, &cfgAll); err != nil {
		return nil, fmt.Errorf("admin: export configuration: %w", err)
	}
	man.Config = sortedKeys(cfgAll)
	infos, err := m.deps.Buckets.List(ctx, bucket.ListInput{})
	if err != nil {
		return nil, fmt.Errorf("admin: export buckets: %w", err)
	}
	var buckets []archivedBucket
	for _, info := range infos {
		b, err := m.exportBucket(ctx, info.Name)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
		man.Buckets = append(man.Buckets, info.Name)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, v any) error {
		var data []byte
		switch d := v.(type) {
		case []byte:
			data = d
		default:
			var err error
			if data, err = json.MarshalIndent(v, "", "  "); err != nil {
				return err
			}
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: man.Created})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	entries := []struct {
		name string
		v    any
	}{{archiveManifest, man}, {archiveIAM, iamDoc}, {archivePolicies, nonNil(policies)}, {archiveConfig, cfgDoc}}
	for _, b := range buckets {
		entries = append(entries, struct {
			name string
			v    any
		}{archiveBuckets + b.Name + ".json", b})
	}
	for _, e := range entries {
		if err := write(e.name, e.v); err != nil {
			return nil, fmt.Errorf("admin: write the archive: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("admin: write the archive: %w", err)
	}
	m.deps.Logger.InfoContext(ctx, "admin: cluster state exported", "users", len(man.Users), "groups", len(man.Groups),
		"policies", len(man.Policies), "config", len(man.Config), "buckets", len(man.Buckets), "bytes", buf.Len())
	return buf.Bytes(), nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// absentSetting reports the error of a bucket setting that is not set.
func absentSetting(err error) bool { return errors.Is(err, domain.ErrPolicyNotFound) }

// exportBucket reads the settings of one bucket.
func (m *clusterManager) exportBucket(ctx context.Context, name string) (archivedBucket, error) {
	b := archivedBucket{Name: name}
	fail := func(what string, err error) (archivedBucket, error) {
		return archivedBucket{}, fmt.Errorf("admin: export the %s of bucket %s: %w", what, name, err)
	}
	v, err := m.deps.Buckets.GetVersioning(ctx, name)
	if err != nil {
		return fail("versioning", err)
	}
	b.Versioning = v.String()
	if lock, err := m.deps.Buckets.GetObjectLock(ctx, name); err == nil {
		b.ObjectLock = &lock
	} else if !absentSetting(err) {
		return fail("object lock", err)
	}
	if doc, err := m.deps.Buckets.GetPolicy(ctx, name); err == nil {
		b.Policy = string(doc)
	} else if !absentSetting(err) {
		return fail("policy", err)
	}
	if doc, err := m.deps.Buckets.GetLifecycle(ctx, name); err == nil {
		b.Lifecycle = string(doc)
	} else if !absentSetting(err) {
		return fail("lifecycle", err)
	}
	if enc, err := m.deps.Buckets.GetEncryption(ctx, name); err == nil {
		b.Encryption = &enc
	} else if !absentSetting(err) {
		return fail("encryption", err)
	}
	q, err := m.deps.Buckets.GetQuota(ctx, name)
	if err != nil {
		return fail("quota", err)
	}
	if q.Size > 0 {
		b.Quota = &q
	}
	if b.Tags, err = m.deps.Buckets.GetTags(ctx, name); err != nil && !absentSetting(err) {
		return fail("tags", err)
	}
	if doc, err := m.deps.Buckets.GetCORS(ctx, name); err == nil {
		b.CORS = string(doc)
	} else if !absentSetting(err) {
		return fail("CORS", err)
	}
	if doc, err := m.deps.Buckets.GetNotification(ctx, name); err == nil {
		b.Notification = string(doc)
	} else if !absentSetting(err) {
		return fail("notification", err)
	}
	if media, err := m.deps.Buckets.GetMedia(ctx, name); err == nil {
		b.Media = &media
	} else if !absentSetting(err) {
		return fail("media configuration", err)
	}
	if prot, err := m.deps.Buckets.GetMediaProtection(ctx, name); err != nil {
		return fail("media protection", err)
	} else if prot.Enabled {
		b.MediaProtection = &prot
	}
	return b, nil
}

// clusterArchive is a decoded archive.
type clusterArchive struct {
	manifest clusterManifest
	iam      []byte
	policies []archivedPolicy
	config   map[string]config.KV
	rawCfg   []byte
	buckets  []archivedBucket
}

// archiveEntry reports whether name is an entry of the archive layout:
// the documents and the buckets/<name>.json records.
func archiveEntry(name string) bool {
	switch name {
	case archiveManifest, archiveIAM, archivePolicies, archiveConfig:
		return true
	}
	rest, ok := strings.CutPrefix(name, archiveBuckets)
	return ok && strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") && len(rest) > len(".json")
}

// readArchive decodes and checks an archive: every document is read (with
// the size bound) before anything is imported.
func readArchive(data []byte) (*clusterArchive, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: cluster archive: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
	}
	if len(data) > MaxClusterArchive {
		return nil, bad("larger than %d bytes", MaxClusterArchive)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, bad("not a zip file: %v", err)
	}
	if len(zr.File) > MaxClusterArchiveEntries {
		return nil, bad("more than %d entries", MaxClusterArchiveEntries)
	}
	files := map[string][]byte{}
	// budget is what the documents may still take once uncompressed: the
	// sum of the entries is bounded, not each one (a zip bomb of many
	// small deflated entries).
	budget := int64(MaxClusterArchive)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(f.Name)
		if name != f.Name || !archiveEntry(name) {
			return nil, bad("unexpected entry %q", f.Name)
		}
		if _, dup := files[name]; dup {
			return nil, bad("entry %s appears twice", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, bad("entry %s: %v", f.Name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, budget+1))
		_ = rc.Close()
		if err != nil {
			return nil, bad("entry %s: %v", f.Name, err)
		}
		if int64(len(content)) > budget {
			return nil, bad("documents larger than %d bytes once uncompressed", MaxClusterArchive)
		}
		budget -= int64(len(content))
		files[name] = content
	}
	a := &clusterArchive{}
	raw, ok := files[archiveManifest]
	if !ok {
		return nil, bad("no %s", archiveManifest)
	}
	if err := json.Unmarshal(raw, &a.manifest); err != nil {
		return nil, bad("%s: %v", archiveManifest, err)
	}
	if a.manifest.Format != ClusterArchiveFormat {
		return nil, bad("format %q is not %q", a.manifest.Format, ClusterArchiveFormat)
	}
	if a.manifest.Version < 1 || a.manifest.Version > ClusterArchiveVersion {
		return nil, bad("version %d is not supported (this server reads 1 to %d)", a.manifest.Version, ClusterArchiveVersion)
	}
	if a.iam, ok = files[archiveIAM]; !ok {
		return nil, bad("no %s", archiveIAM)
	}
	if err := json.Unmarshal(files[archivePolicies], &a.policies); err != nil {
		return nil, bad("%s: %v", archivePolicies, err)
	}
	a.rawCfg = files[archiveConfig]
	if err := json.Unmarshal(a.rawCfg, &a.config); err != nil {
		return nil, bad("%s: %v", archiveConfig, err)
	}
	for name, content := range files {
		if !strings.HasPrefix(name, archiveBuckets) {
			continue
		}
		var b archivedBucket
		if err := json.Unmarshal(content, &b); err != nil {
			return nil, bad("%s: %v", name, err)
		}
		if b.Name == "" || archiveBuckets+b.Name+".json" != name {
			return nil, bad("%s names bucket %q", name, b.Name)
		}
		if _, err := domain.ParseVersioning(b.Versioning); err != nil {
			return nil, bad("%s: %v", name, err)
		}
		a.buckets = append(a.buckets, b)
	}
	sort.Slice(a.buckets, func(i, j int) bool { return a.buckets[i].Name < a.buckets[j].Name })
	return a, nil
}

// Import implements ClusterManager. Everything is validated first (the
// archive, every policy, the configuration, the bucket names); a dry run
// stops there and reports what would change. The import then applies the
// configuration, the named policies, the IAM state (which needs the
// policies it attaches) and the bucket settings, creating the buckets
// that are missing.
func (m *clusterManager) Import(ctx context.Context, data []byte, opts ImportOptions) (ImportSummary, error) {
	switch opts.Mode {
	case "":
		opts.Mode = ImportMerge
	case ImportMerge, ImportReplace:
	default:
		return ImportSummary{}, fmt.Errorf("%w: import mode %q (merge or replace)", domain.ErrInvalidArgument, opts.Mode)
	}
	a, err := readArchive(data)
	if err != nil {
		return ImportSummary{}, err
	}
	sum := ImportSummary{
		Mode: opts.Mode, DryRun: opts.DryRun, Users: len(a.manifest.Users), Groups: len(a.manifest.Groups),
		Policies: len(a.policies), ConfigSubsystems: len(a.config), Buckets: len(a.buckets),
	}
	// Validation.
	parsed := make(map[string]*domain.Policy, len(a.policies))
	for _, p := range a.policies {
		doc, err := m.deps.Policy.Parse(ctx, p.Policy)
		if err != nil {
			return sum, fmt.Errorf("admin: import policy %s: %w", p.Name, err)
		}
		parsed[p.Name] = doc
	}
	if err := validateConfig(a.config); err != nil {
		return sum, fmt.Errorf("admin: import configuration: %w", err)
	}
	existing := map[string]bool{}
	infos, err := m.deps.Buckets.List(ctx, bucket.ListInput{})
	if err != nil {
		return sum, fmt.Errorf("admin: import buckets: %w", err)
	}
	for _, info := range infos {
		existing[info.Name] = true
	}
	for _, b := range a.buckets {
		if err := domain.ValidateBucketName(b.Name); err != nil {
			return sum, fmt.Errorf("admin: import bucket %q: %w", b.Name, err)
		}
		if !existing[b.Name] {
			sum.BucketsCreated = append(sum.BucketsCreated, b.Name)
		}
	}
	removed, err := m.removals(ctx, a, opts.Mode)
	if err != nil {
		return sum, err
	}
	sum.Removed = removed.names()
	if opts.DryRun {
		return sum, nil
	}

	// Configuration.
	if opts.Mode == ImportReplace {
		if err := m.deps.Config.Import(ctx, a.rawCfg); err != nil {
			return sum, fmt.Errorf("admin: import configuration: %w", err)
		}
	} else if len(a.config) > 0 {
		if err := m.deps.Config.SetAll(ctx, a.config); err != nil {
			return sum, fmt.Errorf("admin: import configuration: %w", err)
		}
	}
	// Named policies, then the IAM state that attaches them.
	for _, p := range a.policies {
		if err := m.deps.Policy.SetPolicy(ctx, p.Name, parsed[p.Name]); err != nil {
			return sum, fmt.Errorf("admin: import policy %s: %w", p.Name, err)
		}
	}
	if err := m.deps.IAM.Import(ctx, a.iam); err != nil {
		return sum, fmt.Errorf("admin: import IAM (the target must share the root credentials of the source): %w", err)
	}
	for _, u := range removed.users {
		if err := m.deps.IAM.DeleteUser(ctx, u); err != nil && !errors.Is(err, domain.ErrUserNotFound) {
			return sum, fmt.Errorf("admin: remove user %s: %w", u, err)
		}
	}
	for _, g := range removed.groups {
		if err := m.deps.IAM.DeleteGroup(ctx, g); err != nil && !errors.Is(err, domain.ErrGroupNotFound) {
			return sum, fmt.Errorf("admin: remove group %s: %w", g, err)
		}
	}
	for _, p := range removed.policies {
		if err := m.deps.Policy.DeletePolicy(ctx, p); err != nil && !errors.Is(err, domain.ErrPolicyNotFound) {
			return sum, fmt.Errorf("admin: remove policy %s: %w", p, err)
		}
	}
	// Buckets.
	for _, b := range a.buckets {
		if !existing[b.Name] {
			lock := b.ObjectLock != nil && b.ObjectLock.Enabled
			if _, err := m.deps.Buckets.Create(ctx, bucket.CreateInput{Name: b.Name, ObjectLock: lock}); err != nil && !errors.Is(err, domain.ErrBucketAlreadyExists) {
				return sum, fmt.Errorf("admin: create bucket %s: %w", b.Name, err)
			}
		}
		if err := m.importBucket(ctx, b, opts.Mode); err != nil {
			return sum, err
		}
	}
	m.deps.Logger.InfoContext(ctx, "admin: cluster state imported", "mode", string(opts.Mode), "users", sum.Users, "groups", sum.Groups,
		"policies", sum.Policies, "config", sum.ConfigSubsystems, "buckets", sum.Buckets, "created", len(sum.BucketsCreated), "removed", len(sum.Removed))
	return sum, nil
}

// validateConfig checks a configuration document against the schema.
func validateConfig(all map[string]config.KV) error {
	for name, kv := range all {
		s, err := subsystem(name)
		if err != nil {
			return err
		}
		for k, v := range kv {
			ks, ok := s.key(k)
			if !ok {
				return fmt.Errorf("%w: unknown key %q of subsystem %s", domain.ErrInvalidArgument, k, name)
			}
			if err := checkValue(ks, v); err != nil {
				return fmt.Errorf("%w: %s.%s: %s", domain.ErrInvalidArgument, name, k, err)
			}
		}
	}
	return nil
}

// importRemovals are what a replace import removes: the users, groups and
// named policies of this deployment the archive does not hold.
type importRemovals struct {
	users, groups, policies []string
}

func (r importRemovals) names() []string {
	var out []string
	for _, u := range r.users {
		out = append(out, "user:"+u)
	}
	for _, g := range r.groups {
		out = append(out, "group:"+g)
	}
	for _, p := range r.policies {
		out = append(out, "policy:"+p)
	}
	return out
}

func (m *clusterManager) removals(ctx context.Context, a *clusterArchive, mode ImportMode) (importRemovals, error) {
	var r importRemovals
	if mode != ImportReplace {
		return r, nil
	}
	users, err := m.deps.IAM.ListUsers(ctx)
	if err != nil {
		return r, fmt.Errorf("admin: import: list users: %w", err)
	}
	for _, u := range users {
		if !slices.Contains(a.manifest.Users, u.AccessKey) {
			r.users = append(r.users, u.AccessKey)
		}
	}
	groups, err := m.deps.IAM.ListGroups(ctx)
	if err != nil {
		return r, fmt.Errorf("admin: import: list groups: %w", err)
	}
	for _, g := range groups {
		if !slices.Contains(a.manifest.Groups, g.Name) {
			r.groups = append(r.groups, g.Name)
		}
	}
	named, err := m.deps.Policy.ListPolicies(ctx)
	if err != nil {
		return r, fmt.Errorf("admin: import: list policies: %w", err)
	}
	kept := map[string]bool{}
	for _, p := range a.policies {
		kept[p.Name] = true
	}
	for _, p := range named {
		if !p.BuiltIn && !kept[p.Name] {
			r.policies = append(r.policies, p.Name)
		}
	}
	return r, nil
}

// importBucket applies the archived settings of one bucket: versioning
// first (object lock needs it), then the others. In replace mode the
// settings the archive does not hold are removed.
func (m *clusterManager) importBucket(ctx context.Context, b archivedBucket, mode ImportMode) error {
	name := b.Name
	fail := func(what string, err error) error {
		return fmt.Errorf("admin: import the %s of bucket %s: %w", what, name, err)
	}
	replace := mode == ImportReplace
	bk := m.deps.Buckets
	v, err := domain.ParseVersioning(b.Versioning)
	if err != nil {
		return fail("versioning", err)
	}
	if v.Versioned() {
		if err := bk.SetVersioning(ctx, name, v); err != nil {
			return fail("versioning", err)
		}
	}
	if b.ObjectLock != nil && b.ObjectLock.Enabled {
		if err := bk.SetObjectLock(ctx, name, *b.ObjectLock); err != nil {
			return fail("object lock", err)
		}
	}
	if b.Policy != "" {
		if err := bk.SetPolicy(ctx, name, json.RawMessage(b.Policy)); err != nil {
			return fail("policy", err)
		}
	} else if replace {
		if err := bk.DeletePolicy(ctx, name); err != nil && !absentSetting(err) {
			return fail("policy", err)
		}
	}
	if b.Lifecycle != "" {
		if err := bk.SetLifecycle(ctx, name, []byte(b.Lifecycle)); err != nil {
			return fail("lifecycle", err)
		}
	} else if replace {
		if err := bk.DeleteLifecycle(ctx, name); err != nil && !absentSetting(err) {
			return fail("lifecycle", err)
		}
	}
	if b.Encryption != nil && b.Encryption.Algorithm != "" {
		if err := bk.SetEncryption(ctx, name, *b.Encryption); err != nil {
			return fail("encryption", err)
		}
	} else if replace {
		if err := bk.DeleteEncryption(ctx, name); err != nil && !absentSetting(err) {
			return fail("encryption", err)
		}
	}
	if b.Quota != nil || replace {
		q := bucket.Quota{}
		if b.Quota != nil {
			q = *b.Quota
		}
		if _, err := bk.SetQuota(ctx, name, q); err != nil {
			return fail("quota", err)
		}
	}
	if len(b.Tags) > 0 {
		if err := bk.SetTags(ctx, name, b.Tags); err != nil {
			return fail("tags", err)
		}
	} else if replace {
		if err := bk.DeleteTags(ctx, name); err != nil && !absentSetting(err) {
			return fail("tags", err)
		}
	}
	if b.CORS != "" {
		if err := bk.SetCORS(ctx, name, []byte(b.CORS)); err != nil {
			return fail("CORS", err)
		}
	} else if replace {
		if err := bk.DeleteCORS(ctx, name); err != nil && !absentSetting(err) {
			return fail("CORS", err)
		}
	}
	if b.Notification != "" {
		if err := bk.SetNotification(ctx, name, []byte(b.Notification)); err != nil {
			return fail("notification", err)
		}
	}
	if b.Media != nil && len(b.Media.Rules) > 0 {
		if err := bk.SetMedia(ctx, name, *b.Media); err != nil {
			return fail("media configuration", err)
		}
	} else if replace {
		if err := bk.DeleteMedia(ctx, name); err != nil && !absentSetting(err) {
			return fail("media configuration", err)
		}
	}
	if b.MediaProtection != nil {
		if err := bk.SetMediaProtection(ctx, name, *b.MediaProtection); err != nil {
			return fail("media protection", err)
		}
	} else if replace {
		if err := bk.SetMediaProtection(ctx, name, domain.MediaProtection{}); err != nil {
			return fail("media protection", err)
		}
	}
	return nil
}

// SiteBucket implements ClusterManager.
func (m *clusterManager) SiteBucket(ctx context.Context, name string) ([]byte, error) {
	b, err := m.exportBucket(ctx, name)
	if err != nil {
		return nil, err
	}
	b.Notification = ""
	return json.Marshal(b)
}

// ApplySiteBucket implements ClusterManager.
func (m *clusterManager) ApplySiteBucket(ctx context.Context, doc []byte) error {
	var b archivedBucket
	if err := json.Unmarshal(doc, &b); err != nil {
		return fmt.Errorf("%w: bucket settings: %v", domain.ErrInvalidArgument, err)
	}
	if err := domain.ValidateBucketName(b.Name); err != nil {
		return err
	}
	_, err := m.deps.Buckets.Create(ctx, bucket.CreateInput{Name: b.Name, ObjectLock: b.ObjectLock != nil && b.ObjectLock.Enabled})
	if err != nil && !errors.Is(err, domain.ErrBucketAlreadyExists) {
		return fmt.Errorf("admin: create bucket %s: %w", b.Name, err)
	}
	return m.importBucket(ctx, b, ImportReplace)
}
