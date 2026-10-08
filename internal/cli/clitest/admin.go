package clitest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// MemAdminRoot is the access key of the root user of MemAdmin: an alias
// with it administers the store; any other access key is a user of the
// store (an administrator when one of its policies is consoleAdmin).
const MemAdminRoot = "MEMKEY"

// MemAdminEndpoint is the Admin API URL reported by the errors of MemAdmin.
const MemAdminEndpoint = "http://admin.mem.test"

// memPageSize is the page size of the list methods (small, so that the
// commands follow the cursors).
const memPageSize = 2

// MemAdmin is an in-memory Nexo Admin API shared by the mocks it
// configures (docs/12 §12.2 rule 8: the state is modeled on the generated
// mock of cli.AdminClient with RunAndReturn). IDs are sequential and dates
// come from the clock, so that outputs are deterministic. Failures are
// *cli.AdminError values built like those of the real client. It is safe
// for concurrent use.
type MemAdmin struct {
	clock clock.Clock

	mu       sync.Mutex
	users    map[string]*nexoadmin.User
	secrets  map[string]string
	groups   map[string]*nexoadmin.Group
	policies map[string]*nexoadmin.Policy
	keys     map[string]*nexoadmin.AccessKey
	config   map[string]map[string]string
	shares   []*nexoadmin.Share
	info     nexoadmin.ServerInfo
	usage    nexoadmin.DataUsage
	seq      int
	fail     map[string]error
	calls    map[string]int
}

// memConfigSchema is the configuration schema of MemAdmin.
func memConfigSchema() []nexoadmin.ConfigSubsystemSchema {
	return []nexoadmin.ConfigSubsystemSchema{
		{Name: "logger", Description: "Server logs", Keys: []nexoadmin.ConfigKey{
			{Name: "format", Type: "enum", Default: "text", Values: []string{"text", "json"}},
			{Name: "level", Type: "enum", Default: "info", Values: []string{"debug", "info", "warn", "error"}},
		}},
		{Name: "notify_webhook", Description: "Bucket notifications to a webhook", Keys: []nexoadmin.ConfigKey{
			{Name: "auth_token", Type: "string", Sensitive: true},
			{Name: "endpoint", Type: "url"},
		}},
		{Name: "region", Description: "Server location", Keys: []nexoadmin.ConfigKey{
			{Name: "comment", Type: "string"},
			{Name: "name", Type: "string", Default: "us-east-1"},
		}},
	}
}

// memSensitiveMask replaces the sensitive configuration values.
const memSensitiveMask = "********"

// NewMemAdmin returns a store with the built-in policies, dated by clk
// (nil: a fake clock at 2026-01-02 03:04:05 UTC).
func NewMemAdmin(clk clock.Clock) *MemAdmin {
	if clk == nil {
		clk = clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	}
	now := clk.Now().UTC()
	m := &MemAdmin{
		clock: clk, users: map[string]*nexoadmin.User{}, secrets: map[string]string{}, groups: map[string]*nexoadmin.Group{},
		policies: map[string]*nexoadmin.Policy{}, keys: map[string]*nexoadmin.AccessKey{}, config: map[string]map[string]string{},
		fail: map[string]error{}, calls: map[string]int{},
	}
	builtins := map[string]string{
		"consoleAdmin": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:*","s3:*"],"Resource":["arn:aws:s3:::*"]}]}`,
		"diagnostics":  `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:ServerInfo"],"Resource":["arn:aws:s3:::*"]}]}`,
		"readonly":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:Get*","s3:List*"],"Resource":["arn:aws:s3:::*"]}]}`,
		"readwrite":    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}]}`,
		"writeonly":    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject"],"Resource":["arn:aws:s3:::*"]}]}`,
	}
	for name, doc := range builtins {
		m.policies[name] = &nexoadmin.Policy{Name: name, Document: json.RawMessage(doc), BuiltIn: true, UpdatedAt: now}
	}
	m.info = nexoadmin.ServerInfo{
		Version: "dev", Commit: "abc1234", BuildDate: "2026-01-01", Mode: "snsd", Region: "us-east-1",
		DeploymentID: "d-0001", StartedAt: now.Add(-90 * time.Minute), UptimeSeconds: 5400, Buckets: 2, Objects: 3, Usage: 3 << 20,
		Pools: []nexoadmin.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 1, Parity: 0}},
		Nodes: []nexoadmin.NodeInfo{{
			Endpoint: "local", State: "online", Version: "dev", Commit: "abc1234", UptimeSeconds: 5400,
			Disks: []nexoadmin.DiskInfo{{Endpoint: "/data", Path: "/data", ID: "disk-1", State: "online", Total: 100 << 30, Free: 60 << 30, Used: 40 << 30}},
		}},
	}
	m.usage = nexoadmin.DataUsage{
		LastUpdate: now, ObjectsCount: 3, VersionsCount: 4, TotalSize: 3 << 20, BucketsCount: 2,
		Buckets: map[string]nexoadmin.BucketUsage{
			"logs":   {Size: 1 << 20, ObjectsCount: 1, VersionsCount: 1},
			"photos": {Size: 2 << 20, ObjectsCount: 2, VersionsCount: 3, DeleteMarkers: 1},
		},
	}
	return m
}

// FailOn makes every call of the AdminClient method fail with err (nil
// clears it).
func (m *MemAdmin) FailOn(method string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.fail, method)
		return
	}
	m.fail[method] = err
}

// Calls returns how many times the AdminClient method was called.
func (m *MemAdmin) Calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

// SetServerInfo replaces the server information.
func (m *MemAdmin) SetServerInfo(info nexoadmin.ServerInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.info = info
}

// AddUser creates a user with policies.
func (m *MemAdmin) AddUser(accessKey, secretKey string, policies ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now().UTC()
	m.users[accessKey] = &nexoadmin.User{AccessKey: accessKey, Status: nexoadmin.StatusEnabled, PolicyNames: policies, Groups: []string{}, CreatedAt: now, UpdatedAt: now}
	m.secrets[accessKey] = secretKey
}

// User returns a copy of a user.
func (m *MemAdmin) User(accessKey string) (nexoadmin.User, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[accessKey]
	if !ok {
		return nexoadmin.User{}, false
	}
	return *u, true
}

// ShareIDs returns the IDs of the active share links, oldest first.
func (m *MemAdmin) ShareIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.shares))
	for _, s := range m.shares {
		out = append(out, s.ID)
	}
	return out
}

// Factory returns a mock factory whose clients act as the user of the
// alias (its access key).
func (m *MemAdmin) Factory(t TestingT) *climock.AdminFactory {
	f := climock.NewAdminFactory(t)
	var mu sync.Mutex
	clients := map[string]*climock.AdminClient{}
	f.EXPECT().Client(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, a cli.Alias, _ cli.ClientOptions) (cli.AdminClient, error) {
			mu.Lock()
			defer mu.Unlock()
			if c := clients[a.AccessKey]; c != nil {
				return c, nil
			}
			c := m.Client(t, a.AccessKey)
			clients[a.AccessKey] = c
			return c, nil
		}).Maybe()
	return f
}

// Client returns a mock AdminClient backed by the store, acting as caller.
func (m *MemAdmin) Client(t TestingT, caller string) *climock.AdminClient {
	c := climock.NewAdminClient(t)
	x := mock.Anything
	s := &memSession{m: m, caller: caller}
	c.EXPECT().ServerInfo(x).RunAndReturn(s.serverInfo).Maybe()
	c.EXPECT().DataUsage(x).RunAndReturn(s.dataUsage).Maybe()
	c.EXPECT().Heal(x, x).RunAndReturn(s.heal).Maybe()
	c.EXPECT().RestartService(x).RunAndReturn(func(ctx context.Context) error { return s.service(ctx, "RestartService") }).Maybe()
	c.EXPECT().StopService(x).RunAndReturn(func(ctx context.Context) error { return s.service(ctx, "StopService") }).Maybe()
	c.EXPECT().ListUsers(x, x).RunAndReturn(s.listUsers).Maybe()
	c.EXPECT().CreateUser(x, x).RunAndReturn(s.createUser).Maybe()
	c.EXPECT().GetUser(x, x).RunAndReturn(s.getUser).Maybe()
	c.EXPECT().UpdateUser(x, x, x).RunAndReturn(s.updateUser).Maybe()
	c.EXPECT().DeleteUser(x, x).RunAndReturn(s.deleteUser).Maybe()
	c.EXPECT().ListGroups(x, x).RunAndReturn(s.listGroups).Maybe()
	c.EXPECT().CreateGroup(x, x).RunAndReturn(s.createGroup).Maybe()
	c.EXPECT().GetGroup(x, x).RunAndReturn(s.getGroup).Maybe()
	c.EXPECT().UpdateGroup(x, x, x).RunAndReturn(s.updateGroup).Maybe()
	c.EXPECT().DeleteGroup(x, x).RunAndReturn(s.deleteGroup).Maybe()
	c.EXPECT().ListAccessKeys(x, x).RunAndReturn(s.listAccessKeys).Maybe()
	c.EXPECT().CreateAccessKey(x, x).RunAndReturn(s.createAccessKey).Maybe()
	c.EXPECT().GetAccessKey(x, x).RunAndReturn(s.getAccessKey).Maybe()
	c.EXPECT().UpdateAccessKey(x, x, x).RunAndReturn(s.updateAccessKey).Maybe()
	c.EXPECT().DeleteAccessKey(x, x).RunAndReturn(s.deleteAccessKey).Maybe()
	c.EXPECT().ListPolicies(x, x).RunAndReturn(s.listPolicies).Maybe()
	c.EXPECT().CreatePolicy(x, x, x).RunAndReturn(func(ctx context.Context, name string, doc []byte) (*nexoadmin.Policy, error) {
		return s.savePolicy(ctx, "CreatePolicy", name, doc, true)
	}).Maybe()
	c.EXPECT().SavePolicy(x, x, x).RunAndReturn(func(ctx context.Context, name string, doc []byte) (*nexoadmin.Policy, error) {
		return s.savePolicy(ctx, "SavePolicy", name, doc, false)
	}).Maybe()
	c.EXPECT().GetPolicy(x, x).RunAndReturn(s.getPolicy).Maybe()
	c.EXPECT().DeletePolicy(x, x).RunAndReturn(s.deletePolicy).Maybe()
	c.EXPECT().AttachPolicy(x, x).RunAndReturn(func(ctx context.Context, in nexoadmin.AttachRequest) error {
		return s.attach(ctx, "AttachPolicy", in, true)
	}).Maybe()
	c.EXPECT().DetachPolicy(x, x).RunAndReturn(func(ctx context.Context, in nexoadmin.AttachRequest) error {
		return s.attach(ctx, "DetachPolicy", in, false)
	}).Maybe()
	c.EXPECT().PolicyEntities(x, x).RunAndReturn(s.policyEntities).Maybe()
	c.EXPECT().SimulatePolicy(x, x).RunAndReturn(s.simulate).Maybe()
	c.EXPECT().ValidatePolicy(x, x, x).RunAndReturn(s.validatePolicy).Maybe()
	c.EXPECT().ListConfig(x).RunAndReturn(s.listConfig).Maybe()
	c.EXPECT().ConfigSchema(x).RunAndReturn(s.configSchema).Maybe()
	c.EXPECT().GetConfig(x, x).RunAndReturn(s.getConfig).Maybe()
	c.EXPECT().SetConfig(x, x, x).RunAndReturn(s.setConfig).Maybe()
	c.EXPECT().DeleteConfig(x, x).RunAndReturn(s.deleteConfig).Maybe()
	c.EXPECT().DeleteConfig(x, x, x).RunAndReturn(s.deleteConfig).Maybe()
	c.EXPECT().ExportConfig(x).RunAndReturn(s.exportConfig).Maybe()
	c.EXPECT().ImportConfig(x, x).RunAndReturn(s.importConfig).Maybe()
	c.EXPECT().CreateShare(x, x).RunAndReturn(s.createShare).Maybe()
	c.EXPECT().ListShares(x, x).RunAndReturn(s.listShares).Maybe()
	c.EXPECT().GetShare(x, x).RunAndReturn(s.getShare).Maybe()
	c.EXPECT().RevokeShare(x, x).RunAndReturn(s.revokeShare).Maybe()
	c.EXPECT().RevokeAllShares(x, x).RunAndReturn(s.revokeAllShares).Maybe()
	return c
}

// --- errors ---------------------------------------------------------------------

// apiError builds the error of the real client for a problem.
func apiError(status int, code, detail string, details ...nexoadmin.PolicyError) error {
	return cli.MapAdminError(&nexoadmin.APIError{
		Status: status, Code: code, Title: http.StatusText(status), Detail: detail, RequestID: "req-mem", Errors: details,
	}, MemAdminEndpoint)
}

func notFound(code, format string, args ...any) error {
	return apiError(http.StatusNotFound, code, fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return apiError(http.StatusBadRequest, nexoadmin.CodeInvalidArgument, fmt.Sprintf(format, args...))
}

func conflict(format string, args ...any) error {
	return apiError(http.StatusConflict, nexoadmin.CodeConflict, fmt.Sprintf(format, args...))
}

// --- sessions ---------------------------------------------------------------------

// memSession is the store seen by one caller.
type memSession struct {
	m      *MemAdmin
	caller string
}

// begin counts a call and returns the failure injected with FailOn (the
// lock is held on success: the caller releases it).
func (s *memSession) begin(method string) error {
	s.m.mu.Lock()
	s.m.calls[method]++
	if err := s.m.fail[method]; err != nil {
		s.m.mu.Unlock()
		return err
	}
	return nil
}

// isAdminLocked reports whether the caller administers the store.
func (s *memSession) isAdminLocked() bool {
	if s.caller == MemAdminRoot {
		return true
	}
	for _, p := range s.effectiveLocked(s.caller) {
		if p == "consoleAdmin" {
			return true
		}
	}
	return false
}

// requireAdminLocked fails (unlocking) unless the caller is an
// administrator.
func (s *memSession) requireAdminLocked(action string) error {
	if s.isAdminLocked() {
		return nil
	}
	s.m.mu.Unlock()
	return apiError(http.StatusForbidden, nexoadmin.CodeAccessDenied, fmt.Sprintf("%s: no policy allows %s", action, action))
}

// effectiveLocked returns the policies of a user: direct and through its
// enabled groups.
func (s *memSession) effectiveLocked(user string) []string {
	u, ok := s.m.users[user]
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for _, p := range u.PolicyNames {
		seen[p] = true
	}
	for _, g := range u.Groups {
		if grp, ok := s.m.groups[g]; ok && grp.Status != nexoadmin.StatusDisabled {
			for _, p := range grp.PolicyNames {
				seen[p] = true
			}
		}
	}
	return cli.SortedKeys(seen)
}

func (s *memSession) now() time.Time { return s.m.clock.Now().UTC().Truncate(time.Second) }

// page returns the page of items starting at cursor (an index).
func page[T any](items []T, cursor string) ([]T, string, error) {
	start := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", invalid("invalid cursor %q", cursor)
		}
		start = n
	}
	end := min(start+memPageSize, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return append([]T{}, items[start:end]...), next, nil
}

// --- server -------------------------------------------------------------------------

func (s *memSession) serverInfo(context.Context) (*nexoadmin.ServerInfo, error) {
	if err := s.begin("ServerInfo"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ServerInfo"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	info := s.m.info
	return &info, nil
}

func (s *memSession) dataUsage(context.Context) (*nexoadmin.DataUsage, error) {
	if err := s.begin("DataUsage"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:DataUsage"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	du := s.m.usage
	return &du, nil
}

func (s *memSession) heal(_ context.Context, in nexoadmin.HealRequest) (*nexoadmin.HealResult, error) {
	if err := s.begin("Heal"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:Heal"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	s.m.seq++
	res := &nexoadmin.HealResult{ID: fmt.Sprintf("heal-%04d", s.m.seq), StartedAt: s.now(), Finished: true, Scanned: 3}
	if !in.DryRun {
		res.Healed = 1
	}
	return res, nil
}

func (s *memSession) service(_ context.Context, method string) error {
	if err := s.begin(method); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:Service"); err != nil {
		return err
	}
	s.m.mu.Unlock()
	return nil
}

// --- users and groups ------------------------------------------------------------------

func copyUser(u *nexoadmin.User) *nexoadmin.User {
	c := *u
	c.PolicyNames = append([]string{}, u.PolicyNames...)
	c.Groups = append([]string{}, u.Groups...)
	return &c
}

func (s *memSession) listUsers(_ context.Context, opts nexoadmin.ListOptions) (*nexoadmin.UserList, error) {
	if err := s.begin("ListUsers"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ListUsers"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	var all []nexoadmin.User
	for _, k := range cli.SortedKeys(s.m.users) {
		all = append(all, *copyUser(s.m.users[k]))
	}
	items, next, err := page(all, opts.Cursor)
	if err != nil {
		return nil, err
	}
	return &nexoadmin.UserList{Items: items, NextCursor: next}, nil
}

func (s *memSession) createUser(_ context.Context, in nexoadmin.CreateUserRequest) (*nexoadmin.User, error) {
	if err := s.begin("CreateUser"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:CreateUser"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if len(in.AccessKey) < 3 || len(in.SecretKey) < 8 {
		return nil, invalid("the access key needs 3 characters and the secret key 8")
	}
	if _, ok := s.m.users[in.AccessKey]; ok || in.AccessKey == MemAdminRoot {
		return nil, apiError(http.StatusConflict, "user_already_exists", fmt.Sprintf("user already exists: %q", in.AccessKey))
	}
	for _, p := range in.PolicyNames {
		if _, ok := s.m.policies[p]; !ok {
			return nil, notFound("policy_not_found", "policy not found: %q", p)
		}
	}
	status := in.Status
	if status == "" {
		status = nexoadmin.StatusEnabled
	}
	now := s.now()
	u := &nexoadmin.User{AccessKey: in.AccessKey, Status: status, PolicyNames: append([]string{}, in.PolicyNames...), Groups: []string{}, CreatedAt: now, UpdatedAt: now}
	s.m.users[in.AccessKey] = u
	s.m.secrets[in.AccessKey] = in.SecretKey
	for _, g := range in.Groups {
		grp, ok := s.m.groups[g]
		if !ok {
			return nil, notFound("group_not_found", "group not found: %q", g)
		}
		grp.Members = append(grp.Members, u.AccessKey)
		u.Groups = append(u.Groups, g)
	}
	return copyUser(u), nil
}

func (s *memSession) getUser(_ context.Context, accessKey string) (*nexoadmin.User, error) {
	if err := s.begin("GetUser"); err != nil {
		return nil, err
	}
	if accessKey != s.caller {
		if err := s.requireAdminLocked("admin:GetUser"); err != nil {
			return nil, err
		}
	}
	defer s.m.mu.Unlock()
	u, ok := s.m.users[accessKey]
	if !ok {
		return nil, notFound("user_not_found", "user not found: %q", accessKey)
	}
	return copyUser(u), nil
}

func (s *memSession) updateUser(_ context.Context, accessKey string, in nexoadmin.UpdateUserRequest) (*nexoadmin.User, error) {
	if err := s.begin("UpdateUser"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:EnableUser"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	u, ok := s.m.users[accessKey]
	if !ok {
		return nil, notFound("user_not_found", "user not found: %q", accessKey)
	}
	if in.Status != nil {
		if *in.Status != nexoadmin.StatusEnabled && *in.Status != nexoadmin.StatusDisabled {
			return nil, invalid("status must be enabled or disabled")
		}
		u.Status = *in.Status
	}
	if in.SecretKey != nil {
		s.m.secrets[accessKey] = *in.SecretKey
	}
	if in.PolicyNames != nil {
		u.PolicyNames = append([]string{}, (*in.PolicyNames)...)
	}
	u.UpdatedAt = s.now()
	return copyUser(u), nil
}

func (s *memSession) deleteUser(_ context.Context, accessKey string) error {
	if err := s.begin("DeleteUser"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:DeleteUser"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	u, ok := s.m.users[accessKey]
	if !ok {
		return notFound("user_not_found", "user not found: %q", accessKey)
	}
	for _, g := range u.Groups {
		if grp, ok := s.m.groups[g]; ok {
			grp.Members = remove(grp.Members, accessKey)
		}
	}
	for k, key := range s.m.keys {
		if key.ParentUser == accessKey {
			delete(s.m.keys, k)
		}
	}
	delete(s.m.users, accessKey)
	delete(s.m.secrets, accessKey)
	return nil
}

func remove(list []string, item string) []string {
	out := []string{}
	for _, v := range list {
		if v != item {
			out = append(out, v)
		}
	}
	return out
}

func copyGroup(g *nexoadmin.Group) *nexoadmin.Group {
	c := *g
	c.Members = append([]string{}, g.Members...)
	c.PolicyNames = append([]string{}, g.PolicyNames...)
	return &c
}

func (s *memSession) listGroups(_ context.Context, opts nexoadmin.ListOptions) (*nexoadmin.GroupList, error) {
	if err := s.begin("ListGroups"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ListGroups"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	var all []nexoadmin.Group
	for _, k := range cli.SortedKeys(s.m.groups) {
		all = append(all, *copyGroup(s.m.groups[k]))
	}
	items, next, err := page(all, opts.Cursor)
	if err != nil {
		return nil, err
	}
	return &nexoadmin.GroupList{Items: items, NextCursor: next}, nil
}

func (s *memSession) createGroup(_ context.Context, in nexoadmin.CreateGroupRequest) (*nexoadmin.Group, error) {
	if err := s.begin("CreateGroup"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:AddUserToGroup"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.m.groups[in.Name]; ok {
		return nil, conflict("group already exists: %q", in.Name)
	}
	for _, p := range in.PolicyNames {
		if _, ok := s.m.policies[p]; !ok {
			return nil, notFound("policy_not_found", "policy not found: %q", p)
		}
	}
	for _, u := range in.Members {
		if _, ok := s.m.users[u]; !ok {
			return nil, notFound("user_not_found", "user not found: %q", u)
		}
	}
	now := s.now()
	g := &nexoadmin.Group{Name: in.Name, Status: nexoadmin.StatusEnabled, Members: []string{}, PolicyNames: append([]string{}, in.PolicyNames...), CreatedAt: now, UpdatedAt: now}
	s.m.groups[in.Name] = g
	s.addMembersLocked(g, in.Members)
	return copyGroup(g), nil
}

func (s *memSession) addMembersLocked(g *nexoadmin.Group, members []string) {
	for _, u := range members {
		if contains(g.Members, u) {
			continue
		}
		g.Members = append(g.Members, u)
		s.m.users[u].Groups = append(s.m.users[u].Groups, g.Name)
	}
	sort.Strings(g.Members)
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}

func (s *memSession) getGroup(_ context.Context, name string) (*nexoadmin.Group, error) {
	if err := s.begin("GetGroup"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:GetGroup"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	g, ok := s.m.groups[name]
	if !ok {
		return nil, notFound("group_not_found", "group not found: %q", name)
	}
	return copyGroup(g), nil
}

func (s *memSession) updateGroup(_ context.Context, name string, in nexoadmin.UpdateGroupRequest) (*nexoadmin.Group, error) {
	if err := s.begin("UpdateGroup"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:AddUserToGroup"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	g, ok := s.m.groups[name]
	if !ok {
		return nil, notFound("group_not_found", "group not found: %q", name)
	}
	for _, u := range in.AddMembers {
		if _, ok := s.m.users[u]; !ok {
			return nil, notFound("user_not_found", "user not found: %q", u)
		}
	}
	s.addMembersLocked(g, in.AddMembers)
	for _, u := range in.RemoveMembers {
		if !contains(g.Members, u) {
			return nil, invalid("user %q is not a member of group %q", u, name)
		}
		g.Members = remove(g.Members, u)
		s.m.users[u].Groups = remove(s.m.users[u].Groups, name)
	}
	if in.Status != nil {
		g.Status = *in.Status
	}
	if in.PolicyNames != nil {
		g.PolicyNames = append([]string{}, (*in.PolicyNames)...)
	}
	g.UpdatedAt = s.now()
	return copyGroup(g), nil
}

func (s *memSession) deleteGroup(_ context.Context, name string) error {
	if err := s.begin("DeleteGroup"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:RemoveUserFromGroup"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	g, ok := s.m.groups[name]
	if !ok {
		return notFound("group_not_found", "group not found: %q", name)
	}
	for _, u := range g.Members {
		s.m.users[u].Groups = remove(s.m.users[u].Groups, name)
	}
	delete(s.m.groups, name)
	return nil
}

// --- access keys -----------------------------------------------------------------------

func copyKey(k *nexoadmin.AccessKey) *nexoadmin.AccessKey {
	c := *k
	if k.Expiration != nil {
		exp := *k.Expiration
		c.Expiration = &exp
	}
	return &c
}

func (s *memSession) listAccessKeys(_ context.Context, opts nexoadmin.ListAccessKeysOptions) (*nexoadmin.AccessKeyList, error) {
	if err := s.begin("ListAccessKeys"); err != nil {
		return nil, err
	}
	user := opts.User
	if user != "" && user != s.caller {
		if err := s.requireAdminLocked("admin:ListServiceAccounts"); err != nil {
			return nil, err
		}
	}
	defer s.m.mu.Unlock()
	if user == "" && s.caller != MemAdminRoot {
		user = s.caller
	}
	var all []nexoadmin.AccessKey
	for _, k := range cli.SortedKeys(s.m.keys) {
		key := s.m.keys[k]
		if user == "" || key.ParentUser == user {
			all = append(all, *copyKey(key))
		}
	}
	items, next, err := page(all, opts.Cursor)
	if err != nil {
		return nil, err
	}
	return &nexoadmin.AccessKeyList{Items: items, NextCursor: next}, nil
}

func (s *memSession) createAccessKey(_ context.Context, in nexoadmin.CreateAccessKeyRequest) (*nexoadmin.CreatedAccessKey, error) {
	if err := s.begin("CreateAccessKey"); err != nil {
		return nil, err
	}
	parent := in.ParentUser
	if parent == "" {
		parent = s.caller
	}
	if parent != s.caller {
		if err := s.requireAdminLocked("admin:CreateServiceAccount"); err != nil {
			return nil, err
		}
	}
	defer s.m.mu.Unlock()
	if _, ok := s.m.users[parent]; !ok && parent != MemAdminRoot {
		return nil, notFound("user_not_found", "user not found: %q", parent)
	}
	if len(in.Policy) > 0 && !json.Valid(in.Policy) {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeMalformedPolicy, "the inline policy is not JSON")
	}
	s.m.seq++
	ak, sk := in.AccessKey, in.SecretKey
	if ak == "" {
		ak = fmt.Sprintf("AKMEM%04d", s.m.seq)
	}
	if sk == "" {
		sk = fmt.Sprintf("SKMEM%04dsecret", s.m.seq)
	}
	if _, ok := s.m.keys[ak]; ok {
		return nil, conflict("access key already exists: %q", ak)
	}
	now := s.now()
	key := &nexoadmin.AccessKey{
		AccessKey: ak, ParentUser: parent, Name: in.Name, Description: in.Description, Status: nexoadmin.StatusEnabled,
		Policy: in.Policy, CreatedAt: now, UpdatedAt: now,
	}
	if in.Expiration != nil {
		exp := in.Expiration.UTC()
		key.Expiration = &exp
	}
	s.m.keys[ak] = key
	return &nexoadmin.CreatedAccessKey{AccessKey: *copyKey(key), SecretKey: sk}, nil
}

// keyLocked returns a key the caller may see (unlocking on failure).
func (s *memSession) keyLocked(accessKey string) (*nexoadmin.AccessKey, error) {
	key, ok := s.m.keys[accessKey]
	if !ok {
		s.m.mu.Unlock()
		return nil, notFound("user_not_found", "access key not found: %q", accessKey)
	}
	if key.ParentUser != s.caller {
		if err := s.requireAdminLocked("admin:UpdateServiceAccount"); err != nil {
			return nil, err
		}
	}
	return key, nil
}

func (s *memSession) getAccessKey(_ context.Context, accessKey string) (*nexoadmin.AccessKey, error) {
	if err := s.begin("GetAccessKey"); err != nil {
		return nil, err
	}
	key, err := s.keyLocked(accessKey)
	if err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return copyKey(key), nil
}

func (s *memSession) updateAccessKey(_ context.Context, accessKey string, in nexoadmin.UpdateAccessKeyRequest) (*nexoadmin.AccessKey, error) {
	if err := s.begin("UpdateAccessKey"); err != nil {
		return nil, err
	}
	key, err := s.keyLocked(accessKey)
	if err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if in.Name != nil {
		key.Name = *in.Name
	}
	if in.Description != nil {
		key.Description = *in.Description
	}
	if in.Status != nil {
		key.Status = *in.Status
	}
	if in.Expiration != nil {
		exp := in.Expiration.UTC()
		key.Expiration = &exp
	}
	if in.ClearExpiration {
		key.Expiration = nil
	}
	if len(in.Policy) > 0 {
		key.Policy = in.Policy
	}
	if in.ClearPolicy {
		key.Policy = nil
	}
	key.UpdatedAt = s.now()
	return copyKey(key), nil
}

func (s *memSession) deleteAccessKey(_ context.Context, accessKey string) error {
	if err := s.begin("DeleteAccessKey"); err != nil {
		return err
	}
	if _, err := s.keyLocked(accessKey); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	delete(s.m.keys, accessKey)
	return nil
}

// --- policies ------------------------------------------------------------------------------

func copyPolicy(p *nexoadmin.Policy) *nexoadmin.Policy {
	c := *p
	c.Document = append(json.RawMessage{}, p.Document...)
	return &c
}

func (s *memSession) listPolicies(_ context.Context, opts nexoadmin.ListOptions) (*nexoadmin.PolicyList, error) {
	if err := s.begin("ListPolicies"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ListUserPolicies"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	var all []nexoadmin.Policy
	for _, k := range cli.SortedKeys(s.m.policies) {
		all = append(all, *copyPolicy(s.m.policies[k]))
	}
	items, next, err := page(all, opts.Cursor)
	if err != nil {
		return nil, err
	}
	return &nexoadmin.PolicyList{Items: items, NextCursor: next}, nil
}

// memStatement is the part of a statement MemAdmin understands.
type memStatement struct {
	Sid    string          `json:"Sid"`
	Effect string          `json:"Effect"`
	Action json.RawMessage `json:"Action"`
}

// parsePolicy checks a document: JSON syntax (with line and column), then
// the Effect and the Action of every statement.
func parsePolicy(doc []byte) ([]memStatement, []nexoadmin.PolicyError) {
	var parsed struct {
		Statement []memStatement `json:"Statement"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		pe := nexoadmin.PolicyError{Message: "invalid JSON: " + err.Error()}
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			pe.Line, pe.Column = lineColumn(doc, syn.Offset)
		}
		return nil, []nexoadmin.PolicyError{pe}
	}
	var problems []nexoadmin.PolicyError
	if len(parsed.Statement) == 0 {
		problems = append(problems, nexoadmin.PolicyError{Element: "Statement", Message: "at least one statement is required"})
	}
	for i, st := range parsed.Statement {
		if st.Effect != "Allow" && st.Effect != "Deny" {
			problems = append(problems, nexoadmin.PolicyError{Statement: i + 1, Sid: st.Sid, Element: "Effect", Message: fmt.Sprintf("invalid effect %q", st.Effect)})
		}
		if len(actions(st)) == 0 {
			problems = append(problems, nexoadmin.PolicyError{Statement: i + 1, Sid: st.Sid, Element: "Action", Message: "missing action"})
		}
	}
	return parsed.Statement, problems
}

// lineColumn returns the 1-based line and column of the byte that stopped
// the JSON decoder.
func lineColumn(doc []byte, offset int64) (int, int) {
	before := doc[:max(0, min(int(offset)-1, len(doc)))]
	line := bytes.Count(before, []byte("\n")) + 1
	return line, len(before) - bytes.LastIndexByte(before, '\n')
}

// actions returns the actions of a statement (a string or a list).
func actions(st memStatement) []string {
	var one string
	if json.Unmarshal(st.Action, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(st.Action, &many)
	return many
}

func (s *memSession) savePolicy(_ context.Context, method, name string, doc []byte, create bool) (*nexoadmin.Policy, error) {
	if err := s.begin(method); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:CreatePolicy"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, problems := parsePolicy(doc); len(problems) > 0 {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeMalformedPolicy, describe(problems[0]), problems...)
	}
	if p, ok := s.m.policies[name]; ok && (create || p.BuiltIn) {
		return nil, conflict("policy %q already exists", name)
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, doc)
	p := &nexoadmin.Policy{Name: name, Document: compact.Bytes(), UpdatedAt: s.now()}
	s.m.policies[name] = p
	return copyPolicy(p), nil
}

// describe renders a policy error like the server.
func describe(pe nexoadmin.PolicyError) string {
	msg := pe.Message
	if pe.Element != "" {
		msg = pe.Element + ": " + msg
	}
	if pe.Line > 0 {
		msg = fmt.Sprintf("line %d, column %d: %s", pe.Line, pe.Column, msg)
	}
	if pe.Statement > 0 {
		msg = fmt.Sprintf("statement %d, %s", pe.Statement, msg)
	}
	return msg
}

func (s *memSession) getPolicy(_ context.Context, name string) (*nexoadmin.Policy, error) {
	if err := s.begin("GetPolicy"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:GetPolicy"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	p, ok := s.m.policies[name]
	if !ok {
		return nil, notFound("policy_not_found", "policy not found: %q", name)
	}
	return copyPolicy(p), nil
}

func (s *memSession) deletePolicy(_ context.Context, name string) error {
	if err := s.begin("DeletePolicy"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:DeletePolicy"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	p, ok := s.m.policies[name]
	switch {
	case !ok:
		return notFound("policy_not_found", "policy not found: %q", name)
	case p.BuiltIn:
		return invalid("built-in policy %q cannot be removed", name)
	}
	delete(s.m.policies, name)
	return nil
}

func (s *memSession) attach(_ context.Context, method string, in nexoadmin.AttachRequest, attach bool) error {
	if err := s.begin(method); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:AttachUserOrGroupPolicy"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	if (in.User == "") == (in.Group == "") || len(in.PolicyNames) == 0 {
		return invalid("exactly one of user and group, and at least one policy, are required")
	}
	for _, p := range in.PolicyNames {
		if _, ok := s.m.policies[p]; !ok {
			return notFound("policy_not_found", "policy not found: %q", p)
		}
	}
	var list *[]string
	if in.User != "" {
		u, ok := s.m.users[in.User]
		if !ok {
			return notFound("user_not_found", "user not found: %q", in.User)
		}
		list = &u.PolicyNames
	} else {
		g, ok := s.m.groups[in.Group]
		if !ok {
			return notFound("group_not_found", "group not found: %q", in.Group)
		}
		list = &g.PolicyNames
	}
	for _, p := range in.PolicyNames {
		switch {
		case attach && !contains(*list, p):
			*list = append(*list, p)
		case !attach && !contains(*list, p):
			return invalid("policy %q is not attached", p)
		case !attach:
			*list = remove(*list, p)
		}
	}
	return nil
}

func (s *memSession) policyEntities(_ context.Context, name string) (*nexoadmin.PolicyEntities, error) {
	if err := s.begin("PolicyEntities"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ListUserPolicies"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.m.policies[name]; !ok {
		return nil, notFound("policy_not_found", "policy not found: %q", name)
	}
	out := &nexoadmin.PolicyEntities{Policy: name, Users: []string{}, Groups: []string{}}
	for _, k := range cli.SortedKeys(s.m.users) {
		if contains(s.m.users[k].PolicyNames, name) {
			out.Users = append(out.Users, k)
		}
	}
	for _, k := range cli.SortedKeys(s.m.groups) {
		if contains(s.m.groups[k].PolicyNames, name) {
			out.Groups = append(out.Groups, k)
		}
	}
	return out, nil
}

// matchAction matches an action against a pattern with a trailing "*".
func matchAction(pattern, action string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(action, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == action
}

func (s *memSession) simulate(_ context.Context, in nexoadmin.SimulateRequest) (*nexoadmin.Simulation, error) {
	if err := s.begin("SimulatePolicy"); err != nil {
		return nil, err
	}
	user := in.User
	if user == "" {
		user = s.caller
	}
	if user != s.caller {
		if err := s.requireAdminLocked("admin:GetUser"); err != nil {
			return nil, err
		}
	}
	defer s.m.mu.Unlock()
	if in.Action == "" {
		return nil, invalid("action is required")
	}
	sim := &nexoadmin.Simulation{User: user, Action: in.Action, Bucket: in.Bucket, Object: in.Object, Decision: "deny", Evaluated: []nexoadmin.EvaluatedStatement{}}
	if user == MemAdminRoot {
		sim.Allowed, sim.Decision, sim.Reason, sim.Source = true, "allow", "root credentials", "root"
		return sim, nil
	}
	u, ok := s.m.users[user]
	if !ok {
		return nil, notFound("user_not_found", "user not found: %q", user)
	}
	if u.Status == nexoadmin.StatusDisabled {
		return nil, apiError(http.StatusForbidden, nexoadmin.CodeAccessDenied, fmt.Sprintf("user %q is disabled", user))
	}
	var allow *nexoadmin.EvaluatedStatement
	for _, pname := range s.effectiveLocked(user) {
		p, ok := s.m.policies[pname]
		if !ok {
			continue
		}
		statements, _ := parsePolicy(p.Document)
		for i, st := range statements {
			ev := nexoadmin.EvaluatedStatement{Source: "iam", PolicyName: pname, Index: i, Sid: st.Sid, Effect: st.Effect}
			for _, a := range actions(st) {
				if matchAction(a, in.Action) {
					ev.Matched = true
				}
			}
			if !ev.Matched {
				ev.Mismatch = "action"
			}
			sim.Evaluated = append(sim.Evaluated, ev)
			if ev.Matched && st.Effect == "Deny" {
				sim.Decision, sim.Reason, sim.Source, sim.PolicyName = "deny", fmt.Sprintf("denied by identity policy %q, statement %d", pname, i+1), "iam", pname
				sim.Statement = &ev
				return sim, nil
			}
			if ev.Matched && allow == nil {
				allow = &ev
			}
		}
	}
	if allow == nil {
		sim.Reason = "no policy allows " + in.Action
		return sim, nil
	}
	sim.Allowed, sim.Decision, sim.Source, sim.PolicyName, sim.Statement = true, "allow", "iam", allow.PolicyName, allow
	sim.Reason = fmt.Sprintf("allowed by identity policy %q, statement %d", allow.PolicyName, allow.Index+1)
	return sim, nil
}

func (s *memSession) validatePolicy(_ context.Context, doc []byte, _ string) (*nexoadmin.ValidationReport, error) {
	if err := s.begin("ValidatePolicy"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	_, problems := parsePolicy(doc)
	return &nexoadmin.ValidationReport{Valid: len(problems) == 0, Errors: append([]nexoadmin.PolicyError{}, problems...)}, nil
}

// --- configuration ----------------------------------------------------------------------------

func subsystemSchema(name string) (nexoadmin.ConfigSubsystemSchema, bool) {
	for _, s := range memConfigSchema() {
		if s.Name == name {
			return s, true
		}
	}
	return nexoadmin.ConfigSubsystemSchema{}, false
}

func keySchema(s nexoadmin.ConfigSubsystemSchema, key string) (nexoadmin.ConfigKey, bool) {
	for _, k := range s.Keys {
		if k.Name == key {
			return k, true
		}
	}
	return nexoadmin.ConfigKey{}, false
}

// subsystemLocked returns the masked settings of a subsystem.
func (s *memSession) subsystemLocked(name string) (*nexoadmin.ConfigSubsystem, error) {
	schema, ok := subsystemSchema(name)
	if !ok {
		return nil, invalid("unknown configuration subsystem %q", name)
	}
	out := &nexoadmin.ConfigSubsystem{Name: name, Settings: map[string]string{}}
	for k, v := range s.m.config[name] {
		if ks, _ := keySchema(schema, k); ks.Sensitive && v != "" {
			v = memSensitiveMask
		}
		out.Settings[k] = v
	}
	return out, nil
}

func (s *memSession) listConfig(context.Context) ([]nexoadmin.ConfigSubsystem, error) {
	if err := s.begin("ListConfig"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	var out []nexoadmin.ConfigSubsystem
	for _, schema := range memConfigSchema() {
		sub, _ := s.subsystemLocked(schema.Name)
		out = append(out, *sub)
	}
	return out, nil
}

func (s *memSession) configSchema(context.Context) ([]nexoadmin.ConfigSubsystemSchema, error) {
	if err := s.begin("ConfigSchema"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return memConfigSchema(), nil
}

func (s *memSession) getConfig(_ context.Context, name string) (*nexoadmin.ConfigSubsystem, error) {
	if err := s.begin("GetConfig"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return s.subsystemLocked(name)
}

// setLocked validates and stores settings (the mask keeps a secret).
func (s *memSession) setLocked(name string, settings map[string]string) error {
	schema, ok := subsystemSchema(name)
	if !ok {
		return invalid("unknown configuration subsystem %q", name)
	}
	for k, v := range settings {
		ks, ok := keySchema(schema, k)
		if !ok {
			return invalid("unknown key %q of subsystem %s", k, name)
		}
		if len(ks.Values) > 0 && v != "" && !contains(ks.Values, v) {
			return invalid("%s.%s: %q is not one of %s", name, k, v, strings.Join(ks.Values, ", "))
		}
	}
	if s.m.config[name] == nil {
		s.m.config[name] = map[string]string{}
	}
	for k, v := range settings {
		if ks, _ := keySchema(schema, k); ks.Sensitive && v == memSensitiveMask {
			continue
		}
		s.m.config[name][k] = v
	}
	return nil
}

func (s *memSession) setConfig(_ context.Context, name string, settings map[string]string) (*nexoadmin.ConfigSubsystem, error) {
	if err := s.begin("SetConfig"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if err := s.setLocked(name, settings); err != nil {
		return nil, err
	}
	return s.subsystemLocked(name)
}

func (s *memSession) deleteConfig(_ context.Context, name string, keys ...string) error {
	if err := s.begin("DeleteConfig"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	schema, ok := subsystemSchema(name)
	if !ok {
		return invalid("unknown configuration subsystem %q", name)
	}
	if len(keys) == 0 {
		delete(s.m.config, name)
		return nil
	}
	for _, k := range keys {
		if _, ok := keySchema(schema, k); !ok {
			return invalid("unknown key %q of subsystem %s", k, name)
		}
		delete(s.m.config[name], k)
	}
	return nil
}

func (s *memSession) exportConfig(context.Context) ([]byte, error) {
	if err := s.begin("ExportConfig"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	data, err := json.MarshalIndent(s.m.config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (s *memSession) importConfig(_ context.Context, data []byte) error {
	if err := s.begin("ImportConfig"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	var all map[string]map[string]string
	if err := json.Unmarshal(data, &all); err != nil {
		return invalid("configuration document: %v", err)
	}
	for _, name := range cli.SortedKeys(all) {
		if err := s.setLocked(name, all[name]); err != nil {
			return err
		}
	}
	return nil
}

// --- shares ----------------------------------------------------------------------------------

func (s *memSession) createShare(_ context.Context, in nexoadmin.CreateShareRequest) (*nexoadmin.Share, error) {
	if err := s.begin("CreateShare"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if in.Bucket == "" || (in.Key == "" && !in.Prefix) {
		return nil, invalid("bucket and key are required")
	}
	perm := in.Permission
	if perm == "" {
		perm = nexoadmin.PermissionDownload
	}
	expires := in.ExpiresIn
	if expires == 0 {
		expires = int64(cli.MaxPresignExpiry / time.Second)
	}
	if expires < 1 || expires > int64(cli.MaxPresignExpiry/time.Second) {
		return nil, invalid("expiry must be between 1 second and 168h0m0s")
	}
	s.m.seq++
	id := fmt.Sprintf("sh%04d", s.m.seq)
	now := s.now()
	q := url.Values{"X-Amz-Credential": {"share-" + id}, "X-Amz-Expires": {strconv.FormatInt(expires, 10)}, "X-Amz-Signature": {"mem"}}
	if in.VersionID != "" {
		q.Set("versionId", in.VersionID)
	}
	if in.ContentDisposition != "" {
		q.Set("response-content-disposition", in.ContentDisposition)
	}
	sh := &nexoadmin.Share{
		ID: id, Owner: s.caller, Bucket: in.Bucket, Key: in.Key, Prefix: in.Prefix, Permission: perm, VersionID: in.VersionID,
		ContentType: in.ContentType, ContentDisposition: in.ContentDisposition, ExpiresAt: now.Add(time.Duration(expires) * time.Second),
		CreatedAt: now, URL: "http://s3.mem.test/" + in.Bucket + "/" + in.Key + "?" + q.Encode(), AccessKey: "share-" + id,
	}
	s.m.shares = append(s.m.shares, sh)
	c := *sh
	return &c, nil
}

func (s *memSession) listShares(_ context.Context, opts nexoadmin.ListSharesOptions) (*nexoadmin.ShareList, error) {
	if err := s.begin("ListShares"); err != nil {
		return nil, err
	}
	owner := opts.Owner
	if owner != "" && owner != s.caller {
		if err := s.requireAdminLocked("admin:ManageShares"); err != nil {
			return nil, err
		}
	}
	defer s.m.mu.Unlock()
	if owner == "" && !s.isAdminLocked() {
		owner = s.caller
	}
	var all []nexoadmin.Share
	for i := len(s.m.shares) - 1; i >= 0; i-- {
		sh := s.m.shares[i]
		if (owner == "" || sh.Owner == owner) && (opts.Bucket == "" || sh.Bucket == opts.Bucket) {
			all = append(all, *sh)
		}
	}
	items, next, err := page(all, opts.Cursor)
	if err != nil {
		return nil, err
	}
	return &nexoadmin.ShareList{Items: items, NextCursor: next}, nil
}

// shareLocked returns the index of a share the caller may manage
// (unlocking on failure).
func (s *memSession) shareLocked(id string) (int, error) {
	for i, sh := range s.m.shares {
		if sh.ID != id {
			continue
		}
		if sh.Owner != s.caller {
			if err := s.requireAdminLocked("admin:ManageShares"); err != nil {
				return 0, err
			}
		}
		return i, nil
	}
	s.m.mu.Unlock()
	return 0, notFound("share_not_found", "share not found: %q", id)
}

func (s *memSession) getShare(_ context.Context, id string) (*nexoadmin.Share, error) {
	if err := s.begin("GetShare"); err != nil {
		return nil, err
	}
	i, err := s.shareLocked(id)
	if err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	c := *s.m.shares[i]
	return &c, nil
}

func (s *memSession) revokeShare(_ context.Context, id string) error {
	if err := s.begin("RevokeShare"); err != nil {
		return err
	}
	i, err := s.shareLocked(id)
	if err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	s.m.shares = append(s.m.shares[:i], s.m.shares[i+1:]...)
	return nil
}

func (s *memSession) revokeAllShares(_ context.Context, owner string) (int, error) {
	if err := s.begin("RevokeAllShares"); err != nil {
		return 0, err
	}
	if owner == "" {
		owner = s.caller
	}
	if owner != s.caller {
		if err := s.requireAdminLocked("admin:ManageShares"); err != nil {
			return 0, err
		}
	}
	defer s.m.mu.Unlock()
	kept := s.m.shares[:0]
	n := 0
	for _, sh := range s.m.shares {
		if sh.Owner == owner {
			n++
			continue
		}
		kept = append(kept, sh)
	}
	s.m.shares = kept
	return n, nil
}
