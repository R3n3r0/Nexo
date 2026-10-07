package topology

import (
	"context"
	"net"
	"os"

	"github.com/google/uuid"
)

// osHostResolver is the HostResolver backed by the operating system.
type osHostResolver struct{}

var _ HostResolver = osHostResolver{}

// NewOSHostResolver returns the HostResolver backed by os.Hostname, the default
// net.Resolver and net.InterfaceAddrs.
func NewOSHostResolver() HostResolver { return osHostResolver{} }

func (osHostResolver) Hostname() (string, error) { return os.Hostname() }

func (osHostResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

func (osHostResolver) InterfaceAddrs() ([]net.Addr, error) { return net.InterfaceAddrs() }

// uuidGenerator is the IDGenerator producing random UUID v4 strings.
type uuidGenerator struct{}

var _ IDGenerator = uuidGenerator{}

// NewUUIDGenerator returns an IDGenerator producing random UUID v4 strings.
func NewUUIDGenerator() IDGenerator { return uuidGenerator{} }

func (uuidGenerator) NewID() string { return uuid.NewString() }
