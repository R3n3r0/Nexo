package clitest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stretchr/testify/mock"

	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The updates and coordinated restarts of the store (nexoctl admin update,
// admin service --cluster): a job ends at once, on the nodes of
// SetUpdateNodes ("local" by default); the binary named by an URL ending
// with "/broken" fails its staging.

// memUpdateState is the last job of MemAdmin.
type memUpdateState struct {
	nodes []string
	last  *nexoadmin.UpdateJob
	seq   int
}

// SetUpdateNodes sets the nodes the jobs run on.
func (m *MemAdmin) SetUpdateNodes(nodes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.update.nodes = nodes
}

// expectUpdates registers the update methods on c.
func (s *memSession) expectUpdates(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().StartUpdate(x, x).RunAndReturn(s.startUpdate).Maybe()
	c.EXPECT().UpdateStatus(x).RunAndReturn(s.updateStatus).Maybe()
	c.EXPECT().RestartCluster(x, x).RunAndReturn(s.restartCluster).Maybe()
	c.EXPECT().StopCluster(x).RunAndReturn(s.stopCluster).Maybe()
}

// runJob ends a job on every node.
func (s *memSession) runJob(j nexoadmin.UpdateJob, version, failure string) *nexoadmin.UpdateJob {
	nodes := s.m.update.nodes
	if len(nodes) == 0 {
		nodes = []string{"local"}
	}
	s.m.update.seq++
	j.ID = fmt.Sprintf("%016x", s.m.update.seq)
	j.Node = nodes[0]
	j.Started = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	j.Finished = j.Started.Add(time.Minute)
	j.State, j.Version = "completed", version
	for _, n := range nodes {
		p := nexoadmin.UpdateNode{Node: n, Phase: "done", Version: version}
		if failure != "" {
			p = nexoadmin.UpdateNode{Node: n, Phase: "failed", Error: failure}
		}
		j.Nodes = append(j.Nodes, p)
	}
	if failure != "" {
		j.State, j.Error, j.Version = "failed", "staging: "+failure, ""
	}
	s.m.update.last = &j
	out := j
	return &out
}

func (s *memSession) startUpdate(_ context.Context, in nexoadmin.UpdateRequest) (*nexoadmin.UpdateJob, error) {
	if err := s.replBegin("StartUpdate", "admin:ServerUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return nil, invalid("update: the URL of the binary must be http(s)://host/path")
	}
	if len(in.SHA256) != 64 {
		return nil, invalid("update: the SHA-256 of the binary must be 64 hex digits")
	}
	if s.m.update.last != nil && s.m.update.last.State == "running" {
		return nil, conflict("update: job %s is running", s.m.update.last.ID)
	}
	failure := ""
	if strings.HasSuffix(in.URL, "/broken") {
		failure = "the binary does not run on this node"
	}
	return s.runJob(nexoadmin.UpdateJob{Kind: "update", Mode: in.Mode, URL: in.URL, SHA256: in.SHA256}, "v1.1.0", failure), nil
}

func (s *memSession) updateStatus(context.Context) (*nexoadmin.UpdateJob, error) {
	if err := s.replBegin("UpdateStatus", "admin:ServerInfo"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if s.m.update.last == nil {
		return nil, notFound("not_found", "no update or coordinated restart ran")
	}
	out := *s.m.update.last
	return &out, nil
}

func (s *memSession) restartCluster(_ context.Context, mode string) (*nexoadmin.UpdateJob, error) {
	if err := s.replBegin("RestartCluster", "admin:ServiceRestart"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return s.runJob(nexoadmin.UpdateJob{Kind: "restart", Mode: mode}, "", ""), nil
}

func (s *memSession) stopCluster(context.Context) error {
	if err := s.replBegin("StopCluster", "admin:ServiceStop"); err != nil {
		return err
	}
	s.m.mu.Unlock()
	return nil
}
