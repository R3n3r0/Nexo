package commands

import (
	"encoding/json"
	"time"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The JSON records of the admin and managed share commands (--json), part
// of the CLI contract like those of records.go (TCL-E-18). Lists of names
// are always arrays (never null).

// Entity types of adminActionRecord.
const (
	kindUser      = "user"
	kindGroup     = "group"
	kindPolicy    = "policy"
	kindAccessKey = "accessKey"
	kindShare     = "share"
	kindConfig    = "config"
	kindService   = "service"
)

// adminActionRecord confirms an action without a richer result: a
// removal, a status change, an attachment, a revocation, a restart...
type adminActionRecord struct {
	Alias  string `json:"alias"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Action string `json:"action"`
	// Names lists the policies attached or detached, or the members added
	// or removed.
	Names []string `json:"names,omitempty"`
	// Count is the number of revoked links of share revoke --all.
	Count *int `json:"count,omitempty"`
}

// adminInfoRecord is the server information (admin info).
type adminInfoRecord struct {
	Alias string `json:"alias"`
	nexoadmin.ServerInfo
}

// adminUsageRecord is the data usage (admin usage).
type adminUsageRecord struct {
	Alias string `json:"alias"`
	nexoadmin.DataUsage
}

// adminHealRecord is a healing job (admin heal, admin heal status).
type adminHealRecord struct {
	Alias  string `json:"alias"`
	DryRun bool   `json:"dryRun"`
	nexoadmin.HealJob
}

// adminUserRecord is an IAM user.
type adminUserRecord struct {
	Alias       string     `json:"alias"`
	AccessKey   string     `json:"accessKey"`
	Status      string     `json:"state"`
	PolicyNames []string   `json:"policyNames"`
	Groups      []string   `json:"groups"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

func userRecord(alias string, u *nexoadmin.User) adminUserRecord {
	return adminUserRecord{
		Alias: alias, AccessKey: u.AccessKey, Status: u.Status, PolicyNames: names(u.PolicyNames),
		Groups: names(u.Groups), CreatedAt: timePtr(u.CreatedAt), UpdatedAt: timePtr(u.UpdatedAt),
	}
}

// adminGroupRecord is an IAM group.
type adminGroupRecord struct {
	Alias       string     `json:"alias"`
	Name        string     `json:"name"`
	Status      string     `json:"state"`
	Members     []string   `json:"members"`
	PolicyNames []string   `json:"policyNames"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

func groupRecord(alias string, g *nexoadmin.Group) adminGroupRecord {
	return adminGroupRecord{
		Alias: alias, Name: g.Name, Status: g.Status, Members: names(g.Members), PolicyNames: names(g.PolicyNames),
		CreatedAt: timePtr(g.CreatedAt), UpdatedAt: timePtr(g.UpdatedAt),
	}
}

// adminPolicyRecord is a named IAM policy; the document is omitted by
// the listings.
type adminPolicyRecord struct {
	Alias     string          `json:"alias"`
	Name      string          `json:"name"`
	BuiltIn   bool            `json:"builtIn"`
	UpdatedAt *time.Time      `json:"updatedAt,omitempty"`
	Document  json.RawMessage `json:"document,omitempty"`
}

// adminEntitiesRecord lists the users and groups of a policy.
type adminEntitiesRecord struct {
	Alias  string   `json:"alias"`
	Policy string   `json:"policy"`
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// adminSimulationRecord is the result of admin policy simulate.
type adminSimulationRecord struct {
	Alias string `json:"alias"`
	nexoadmin.Simulation
}

// adminValidationRecord is the result of admin policy validate.
type adminValidationRecord struct {
	Alias  string            `json:"alias"`
	File   string            `json:"file"`
	Bucket string            `json:"bucket,omitempty"`
	Valid  bool              `json:"valid"`
	Errors []cli.ErrorDetail `json:"errors"`
}

// adminAccessKeyRecord is an access key (service account). SecretKey is
// set once, by admin accesskey create.
type adminAccessKeyRecord struct {
	Alias       string          `json:"alias"`
	AccessKey   string          `json:"accessKey"`
	SecretKey   string          `json:"secretKey,omitempty"`
	ParentUser  string          `json:"parentUser"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Status      string          `json:"state"`
	Expiration  *time.Time      `json:"expiration,omitempty"`
	Policy      json.RawMessage `json:"policy,omitempty"`
	CreatedAt   *time.Time      `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time      `json:"updatedAt,omitempty"`
}

func accessKeyRecord(alias string, k *nexoadmin.AccessKey) adminAccessKeyRecord {
	r := adminAccessKeyRecord{
		Alias: alias, AccessKey: k.AccessKey, ParentUser: k.ParentUser, Name: k.Name, Description: k.Description,
		Status: k.Status, CreatedAt: timePtr(k.CreatedAt), UpdatedAt: timePtr(k.UpdatedAt),
	}
	if k.Expiration != nil {
		r.Expiration = timePtr(*k.Expiration)
	}
	if len(k.Policy) > 0 && string(k.Policy) != "null" {
		r.Policy = k.Policy
	}
	return r
}

// adminConfigRecord is the configuration of a subsystem (sensitive values
// masked by the server).
type adminConfigRecord struct {
	Alias     string            `json:"alias"`
	Subsystem string            `json:"subsystem"`
	Settings  map[string]string `json:"settings"`
}

// adminConfigExportRecord is the exported configuration (admin config
// export --json).
type adminConfigExportRecord struct {
	Alias  string          `json:"alias"`
	File   string          `json:"file,omitempty"`
	Config json.RawMessage `json:"config"`
}

// managedShareRecord is a managed, revocable share link (share list, and
// share download/upload without --raw: those also carry the presigned
// members of shareRecord).
type managedShareRecord struct {
	Alias      string `json:"alias"`
	ID         string `json:"id"`
	Target     string `json:"target"`
	Owner      string `json:"owner"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	Prefix     bool   `json:"prefix"`
	Permission string `json:"permission"`
	Managed    bool   `json:"managed"`
	Method     string `json:"method,omitempty"`
	URL        string `json:"url,omitempty"`
	// Stream and Player are set on the HLS stream shares: URL is the master
	// playlist, Player the public player page playing it.
	Stream    bool              `json:"stream,omitempty"`
	Player    string            `json:"player,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	VersionID string            `json:"versionId,omitempty"`
	Expire    string            `json:"expire,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt"`
	CreatedAt *time.Time        `json:"createdAt,omitempty"`
	AccessKey string            `json:"accessKey"`
	Command   string            `json:"command,omitempty"`
}

// names returns s, or an empty slice for nil (JSON arrays, never null).
func names(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// intPtr returns a pointer to n.
func intPtr(n int) *int { return &n }
