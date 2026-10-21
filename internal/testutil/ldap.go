package testutil

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/jimlambrt/gldap"
	"github.com/stretchr/testify/require"
)

// FakeLDAP is an LDAP directory for the tests (plain LDAP on 127.0.0.1):
// a lookup account, users under ou=people,dc=test with a uid and a
// password, groups of names under ou=groups,dc=test.
type FakeLDAP struct {
	Addr string

	mu     sync.Mutex
	users  map[string]string   // uid -> password
	groups map[string][]string // cn -> member uids
	// Binds counts the binds of the users (not of the lookup account).
	Binds int
}

// Constants of FakeLDAP.
const (
	LDAPBindDN     = "cn=admin,dc=test"
	LDAPBindPass   = "admin-secret"
	LDAPUserBase   = "ou=people,dc=test"
	LDAPGroupBase  = "ou=groups,dc=test"
	ldapUserFilter = "(uid="
)

// LDAPUserDN is the DN of a user of FakeLDAP.
func LDAPUserDN(uid string) string { return "uid=" + uid + "," + LDAPUserBase }

// NewFakeLDAP starts a directory, stopped when the test ends.
func NewFakeLDAP(t testing.TB) *FakeLDAP {
	t.Helper()
	f := &FakeLDAP{users: map[string]string{}, groups: map[string][]string{}}
	srv, err := gldap.NewServer(gldap.WithLogger(hclog.New(&hclog.LoggerOptions{Output: io.Discard})))
	require.NoError(t, err)
	mux, err := gldap.NewMux()
	require.NoError(t, err)
	require.NoError(t, mux.Bind(f.bind))
	require.NoError(t, mux.Search(f.search))
	require.NoError(t, srv.Router(mux))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.Addr = l.Addr().String()
	require.NoError(t, l.Close())
	go func() { _ = srv.Run(f.Addr) }()
	require.Eventually(t, srv.Ready, 5*time.Second, 10*time.Millisecond)
	t.Cleanup(func() { _ = srv.Stop() })
	return f
}

// AddUser adds a user and its groups.
func (f *FakeLDAP) AddUser(uid, password string, groups ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[uid] = password
	for _, g := range groups {
		f.groups[g] = append(f.groups[g], uid)
	}
}

func (f *FakeLDAP) bind(w *gldap.ResponseWriter, r *gldap.Request) {
	resp := r.NewBindResponse(gldap.WithResponseCode(gldap.ResultInvalidCredentials))
	defer func() { _ = w.Write(resp) }()
	m, err := r.GetSimpleBindMessage()
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.UserName == LDAPBindDN && string(m.Password) == LDAPBindPass {
		resp.SetResultCode(gldap.ResultSuccess)
		return
	}
	uid, ok := strings.CutPrefix(m.UserName, "uid=")
	uid, ok2 := strings.CutSuffix(uid, ","+LDAPUserBase)
	if ok && ok2 {
		f.Binds++
		if pw, found := f.users[uid]; found && pw != "" && pw == string(m.Password) {
			resp.SetResultCode(gldap.ResultSuccess)
		}
	}
}

func (f *FakeLDAP) search(w *gldap.ResponseWriter, r *gldap.Request) {
	resp := r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultSuccess))
	defer func() { _ = w.Write(resp) }()
	m, err := r.GetSearchMessage()
	if err != nil {
		resp.SetResultCode(gldap.ResultOperationsError)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.EqualFold(m.BaseDN, LDAPUserBase):
		i := strings.Index(m.Filter, ldapUserFilter)
		if i < 0 {
			return
		}
		uid := m.Filter[i+len(ldapUserFilter):]
		uid = uid[:strings.IndexByte(uid, ')')]
		if _, ok := f.users[uid]; ok {
			_ = w.Write(r.NewSearchResponseEntry(LDAPUserDN(uid), gldap.WithAttributes(map[string][]string{"uid": {uid}})))
		}
	case strings.EqualFold(m.BaseDN, LDAPGroupBase):
		for cn, members := range f.groups {
			for _, uid := range members {
				if strings.Contains(m.Filter, "(member="+LDAPUserDN(uid)+")") {
					_ = w.Write(r.NewSearchResponseEntry("cn="+cn+","+LDAPGroupBase, gldap.WithAttributes(map[string][]string{"cn": {cn}})))
					break
				}
			}
		}
	}
}
