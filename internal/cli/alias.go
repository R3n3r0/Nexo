package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrAliasNotFound reports an unknown alias; it wraps domain.ErrUserNotFound
// (exit code 3).
var ErrAliasNotFound = fmt.Errorf("alias not found: %w", domain.ErrUserNotFound)

// AliasNotFoundError reports an unknown alias with a readable message; it
// wraps ErrAliasNotFound.
type AliasNotFoundError struct {
	Name string
	// Arg is the command argument that names the alias, if any.
	Arg string
}

// Error implements error.
func (e *AliasNotFoundError) Error() string {
	if e.Arg != "" && e.Arg != e.Name {
		return fmt.Sprintf("unknown alias %q in %q (remote locations are ALIAS/BUCKET[/PREFIX], see \"nexoctl alias list\")", e.Name, e.Arg)
	}
	return fmt.Sprintf("unknown alias %q (see \"nexoctl alias list\")", e.Name)
}

// Unwrap returns ErrAliasNotFound.
func (e *AliasNotFoundError) Unwrap() error { return ErrAliasNotFound }

// AliasDeps are the dependencies of the alias service.
type AliasDeps struct {
	Store ConfigStore
	Clock clock.Clock
}

type aliasService struct {
	deps AliasDeps
}

var _ AliasService = (*aliasService)(nil)

// NewAliasService returns an AliasService over the store.
func NewAliasService(deps AliasDeps) AliasService {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	return &aliasService{deps: deps}
}

// ValidateAlias checks an alias definition.
func ValidateAlias(a Alias) error {
	if a.Name == "" {
		return fmt.Errorf("%w: alias name is required", domain.ErrInvalidArgument)
	}
	for _, c := range a.Name {
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit && c != '-' && c != '_' {
			return fmt.Errorf("%w: alias name %q may only contain letters, digits, '-' and '_'", domain.ErrInvalidArgument, a.Name)
		}
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: alias URL %q must be an http(s) URL", domain.ErrInvalidArgument, a.URL)
	}
	if a.AccessKey == "" || a.SecretKey == "" {
		return fmt.Errorf("%w: access key and secret key are required", domain.ErrInvalidArgument)
	}
	if a.AdminURL != "" && checkHTTPURL(a.AdminURL) != nil {
		return fmt.Errorf("%w: alias admin URL %q must be an http(s) URL", domain.ErrInvalidArgument, a.AdminURL)
	}
	return nil
}

// Set implements AliasService.
func (s *aliasService) Set(ctx context.Context, a Alias) error {
	if err := ValidateAlias(a); err != nil {
		return err
	}
	cfg, err := s.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if a.API == "" {
		a.API = "s3v4"
	}
	if a.Path == "" {
		a.Path = "auto"
	}
	a.UpdatedAt = s.deps.Clock.Now().UTC().Truncate(time.Second)
	cfg.Aliases[a.Name] = a
	return s.deps.Store.Save(ctx, cfg)
}

// Get implements AliasService.
func (s *aliasService) Get(ctx context.Context, name string) (*Alias, error) {
	cfg, err := s.deps.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	a, ok := cfg.Aliases[name]
	if !ok {
		return nil, &AliasNotFoundError{Name: name}
	}
	a.Name = name
	return &a, nil
}

// List implements AliasService, sorted by name.
func (s *aliasService) List(ctx context.Context) ([]Alias, error) {
	cfg, err := s.deps.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Alias, 0, len(cfg.Aliases))
	for name, a := range cfg.Aliases {
		a.Name = name
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove implements AliasService.
func (s *aliasService) Remove(ctx context.Context, name string) error {
	cfg, err := s.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if _, ok := cfg.Aliases[name]; !ok {
		return &AliasNotFoundError{Name: name}
	}
	delete(cfg.Aliases, name)
	return s.deps.Store.Save(ctx, cfg)
}
