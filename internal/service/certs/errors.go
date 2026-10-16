package certs

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinels of the package; they wrap the domain ones.
var (
	// ErrCertificateNotFound reports an unknown certificate.
	ErrCertificateNotFound = fmt.Errorf("certificate not found: %w", domain.ErrNotFound)
	// ErrChallengeNotFound reports an unknown ACME challenge.
	ErrChallengeNotFound = fmt.Errorf("acme challenge not found: %w", domain.ErrNotFound)
	// ErrInvalidCertificate reports a certificate or key refused at upload.
	ErrInvalidCertificate = fmt.Errorf("invalid certificate: %w", domain.ErrInvalidArgument)
	// ErrDNSProviderNotFound reports an unknown DNS provider.
	ErrDNSProviderNotFound = fmt.Errorf("dns provider not found: %w", domain.ErrNotFound)
	// ErrDNSProviderExists reports a DNS provider created twice.
	ErrDNSProviderExists = fmt.Errorf("dns provider already exists: %w", domain.ErrConflict)
	// ErrInvalidDNSProvider reports a DNS provider configuration refused.
	ErrInvalidDNSProvider = fmt.Errorf("invalid dns provider: %w", domain.ErrInvalidArgument)
	// ErrDNSProviderInUse reports the deletion of a DNS provider a
	// distribution uses.
	ErrDNSProviderInUse = fmt.Errorf("dns provider in use: %w", domain.ErrConflict)
)
