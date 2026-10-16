package dns01

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Route53Region is the region of the Route 53 API (a global service).
const Route53Region = "us-east-1"

// Route53Config configures the AWS Route 53 provider.
type Route53Config struct {
	// AccessKeyID and SecretAccessKey (a secret) are the credentials of
	// an IAM identity allowed route53:ListHostedZonesByName,
	// route53:ListResourceRecordSets, route53:ChangeResourceRecordSets
	// and route53:GetChange; SessionToken (a secret) completes temporary
	// credentials. Without them the ambient credentials of the node are
	// used (environment, shared files, instance or task role, web
	// identity).
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Region is the region of the API (default us-east-1).
	Region string
	// HostedZoneID is the id of the hosted zone (default: the public zone
	// whose name encloses the record).
	HostedZoneID string
	Settings
}

type route53Provider struct {
	cfg  Route53Config
	opts options

	once   sync.Once
	client *route53.Client
	err    error
	// mu serializes the read-modify-write of the record sets.
	mu sync.Mutex
}

var _ Provider = (*route53Provider)(nil)

// NewRoute53 returns the provider of the zones hosted by AWS Route 53.
func NewRoute53(cfg Route53Config, opts ...Option) (Provider, error) {
	cfg.AccessKeyID, cfg.SecretAccessKey = strings.TrimSpace(cfg.AccessKeyID), strings.TrimSpace(cfg.SecretAccessKey)
	cfg.SessionToken, cfg.HostedZoneID = strings.TrimSpace(cfg.SessionToken), strings.TrimSpace(cfg.HostedZoneID)
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, fmt.Errorf("%w: route53: the access key id and the secret access key go together (none: the ambient credentials)", ErrInvalidConfig)
	}
	if cfg.SessionToken != "" && cfg.AccessKeyID == "" {
		return nil, fmt.Errorf("%w: route53: a session token needs the access key id and the secret access key", ErrInvalidConfig)
	}
	if cfg.Region = strings.TrimSpace(cfg.Region); cfg.Region == "" {
		cfg.Region = Route53Region
	}
	return &route53Provider{cfg: cfg, opts: buildOptions(opts)}, nil
}

// Propagation implements Provider.
func (p *route53Provider) Propagation() (time.Duration, time.Duration) { return p.cfg.propagation() }

// api returns the client of the API, loading the ambient credentials on
// first use.
func (p *route53Provider) api(ctx context.Context) (*route53.Client, error) {
	p.once.Do(func() {
		var cfg aws.Config
		if p.cfg.AccessKeyID != "" {
			cfg = aws.Config{
				Region:      p.cfg.Region,
				Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(p.cfg.AccessKeyID, p.cfg.SecretAccessKey, p.cfg.SessionToken)),
				HTTPClient:  p.opts.httpClient,
			}
		} else {
			loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(p.cfg.Region)}
			if p.opts.customHTTP {
				// Otherwise the SDK builds its own client (it honors
				// AWS_CA_BUNDLE, which a plain http.Client cannot).
				loadOpts = append(loadOpts, awsconfig.WithHTTPClient(p.opts.httpClient))
			}
			cfg, p.err = awsconfig.LoadDefaultConfig(context.WithoutCancel(ctx), loadOpts...)
			if p.err != nil {
				p.err = fmt.Errorf("route53: ambient AWS credentials: %w", p.err)
				return
			}
		}
		p.client = route53.NewFromConfig(cfg, func(o *route53.Options) {
			if p.opts.endpoint != "" {
				o.BaseEndpoint = aws.String(p.opts.endpoint)
			}
		})
	})
	return p.client, p.err
}

// zone returns the id of the hosted zone of fqdn.
func (p *route53Provider) zone(ctx context.Context, api *route53.Client, fqdn string) (string, error) {
	if p.cfg.HostedZoneID != "" {
		return p.cfg.HostedZoneID, nil
	}
	for _, name := range parentNames(fqdn) {
		out, err := api.ListHostedZonesByName(ctx, &route53.ListHostedZonesByNameInput{DNSName: aws.String(name)})
		if err != nil {
			return "", fmt.Errorf("route53: list the hosted zones: %w", err)
		}
		for _, z := range out.HostedZones {
			if z.Config != nil && z.Config.PrivateZone {
				continue
			}
			if strings.EqualFold(unFqdn(aws.ToString(z.Name)), name) {
				return strings.TrimPrefix(aws.ToString(z.Id), "/hostedzone/"), nil
			}
		}
	}
	return "", fmt.Errorf("route53: no public hosted zone encloses %s", unFqdn(fqdn))
}

// values returns the values of the TXT record set fqdn (unquoted) and its
// TTL.
func (p *route53Provider) values(ctx context.Context, api *route53.Client, zone, fqdn string) ([]string, int64, error) {
	out, err := api.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(zone), StartRecordName: aws.String(fqdn), StartRecordType: r53types.RRTypeTxt, MaxItems: aws.Int32(1),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("route53: read the record %s: %w", unFqdn(fqdn), err)
	}
	for _, rs := range out.ResourceRecordSets {
		if rs.Type != r53types.RRTypeTxt || !strings.EqualFold(unFqdn(aws.ToString(rs.Name)), unFqdn(fqdn)) {
			continue
		}
		var vals []string
		for _, rr := range rs.ResourceRecords {
			if v, err := strconv.Unquote(aws.ToString(rr.Value)); err == nil {
				vals = append(vals, v)
			} else {
				vals = append(vals, aws.ToString(rr.Value))
			}
		}
		return vals, aws.ToInt64(rs.TTL), nil
	}
	return nil, 0, nil
}

// change applies one change to the record set fqdn and waits for it to
// be in sync on the nameservers of Route 53.
func (p *route53Provider) change(ctx context.Context, api *route53.Client, zone, fqdn string, action r53types.ChangeAction, vals []string, ttl int64) error {
	recs := make([]r53types.ResourceRecord, 0, len(vals))
	for _, v := range vals {
		recs = append(recs, r53types.ResourceRecord{Value: aws.String(strconv.Quote(v))})
	}
	out, err := api.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zone),
		ChangeBatch: &r53types.ChangeBatch{Comment: aws.String("nexo ACME DNS-01"), Changes: []r53types.Change{{
			Action:            action,
			ResourceRecordSet: &r53types.ResourceRecordSet{Name: aws.String(fqdn), Type: r53types.RRTypeTxt, TTL: aws.Int64(ttl), ResourceRecords: recs},
		}}},
	})
	if err != nil {
		return fmt.Errorf("route53: change the record %s: %w", unFqdn(fqdn), err)
	}
	if out.ChangeInfo == nil {
		return nil
	}
	timeout, interval := p.Propagation()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for status := out.ChangeInfo.Status; status != r53types.ChangeStatusInsync; {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("route53: the change of %s is not in sync after %s", unFqdn(fqdn), timeout)
		case <-t.C:
		}
		got, err := api.GetChange(ctx, &route53.GetChangeInput{Id: out.ChangeInfo.Id})
		if err != nil {
			return fmt.Errorf("route53: change status: %w", err)
		}
		if got.ChangeInfo != nil {
			status = got.ChangeInfo.Status
		}
	}
	return nil
}

// Present implements Provider.
func (p *route53Provider) Present(ctx context.Context, fqdn, value string) error {
	api, err := p.api(ctx)
	if err != nil {
		return err
	}
	zone, err := p.zone(ctx, api, fqdn)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	vals, _, err := p.values(ctx, api, zone, fqdn)
	if err != nil {
		return err
	}
	if slices.Contains(vals, value) {
		return nil
	}
	return p.change(ctx, api, zone, fqdn, r53types.ChangeActionUpsert, append(vals, value), int64(p.cfg.ttl()))
}

// CleanUp implements Provider.
func (p *route53Provider) CleanUp(ctx context.Context, fqdn, value string) error {
	api, err := p.api(ctx)
	if err != nil {
		return err
	}
	zone, err := p.zone(ctx, api, fqdn)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	vals, ttl, err := p.values(ctx, api, zone, fqdn)
	if err != nil || !slices.Contains(vals, value) {
		return err
	}
	rest := slices.DeleteFunc(slices.Clone(vals), func(v string) bool { return v == value })
	if len(rest) == 0 {
		// A DELETE names the record set exactly as it is.
		return p.change(ctx, api, zone, fqdn, r53types.ChangeActionDelete, vals, ttl)
	}
	return p.change(ctx, api, zone, fqdn, r53types.ChangeActionUpsert, rest, ttl)
}
