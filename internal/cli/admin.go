package cli

import (
	"context"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// AdminClient is the Admin API surface used by the nexoctl admin and share
// commands (docs/12 §12.5): a subset of nexoadmin.AdminClient, so that the
// client of pkg/nexoadmin satisfies it. The implementation of NewAdminFactory
// wraps every failure of the server in an *AdminError, which wraps the
// matching sentinel of domain (domain.ErrUserNotFound,
// domain.ErrAccessDenied, ...) so that the commands map them to stable
// exit codes (ExitCode).
type AdminClient interface {
	// Server.
	ServerInfo(ctx context.Context) (*nexoadmin.ServerInfo, error)
	DataUsage(ctx context.Context) (*nexoadmin.DataUsage, error)
	Heal(ctx context.Context, in nexoadmin.HealRequest) (*nexoadmin.HealResult, error)
	RestartService(ctx context.Context) error
	StopService(ctx context.Context) error

	// Users and groups.
	ListUsers(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.UserList, error)
	CreateUser(ctx context.Context, in nexoadmin.CreateUserRequest) (*nexoadmin.User, error)
	GetUser(ctx context.Context, accessKey string) (*nexoadmin.User, error)
	UpdateUser(ctx context.Context, accessKey string, in nexoadmin.UpdateUserRequest) (*nexoadmin.User, error)
	DeleteUser(ctx context.Context, accessKey string) error
	ListGroups(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.GroupList, error)
	CreateGroup(ctx context.Context, in nexoadmin.CreateGroupRequest) (*nexoadmin.Group, error)
	GetGroup(ctx context.Context, name string) (*nexoadmin.Group, error)
	UpdateGroup(ctx context.Context, name string, in nexoadmin.UpdateGroupRequest) (*nexoadmin.Group, error)
	DeleteGroup(ctx context.Context, name string) error

	// Access keys (service accounts).
	ListAccessKeys(ctx context.Context, opts nexoadmin.ListAccessKeysOptions) (*nexoadmin.AccessKeyList, error)
	CreateAccessKey(ctx context.Context, in nexoadmin.CreateAccessKeyRequest) (*nexoadmin.CreatedAccessKey, error)
	GetAccessKey(ctx context.Context, accessKey string) (*nexoadmin.AccessKey, error)
	UpdateAccessKey(ctx context.Context, accessKey string, in nexoadmin.UpdateAccessKeyRequest) (*nexoadmin.AccessKey, error)
	DeleteAccessKey(ctx context.Context, accessKey string) error

	// Policies: the documents travel as their JSON text, so that the
	// errors of a malformed document carry a line and a column.
	ListPolicies(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.PolicyList, error)
	CreatePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error)
	GetPolicy(ctx context.Context, name string) (*nexoadmin.Policy, error)
	SavePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error)
	DeletePolicy(ctx context.Context, name string) error
	AttachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error
	DetachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error
	PolicyEntities(ctx context.Context, name string) (*nexoadmin.PolicyEntities, error)
	SimulatePolicy(ctx context.Context, in nexoadmin.SimulateRequest) (*nexoadmin.Simulation, error)
	ValidatePolicy(ctx context.Context, document []byte, bucket string) (*nexoadmin.ValidationReport, error)

	// Configuration.
	ListConfig(ctx context.Context) ([]nexoadmin.ConfigSubsystem, error)
	ConfigSchema(ctx context.Context) ([]nexoadmin.ConfigSubsystemSchema, error)
	GetConfig(ctx context.Context, subsystem string) (*nexoadmin.ConfigSubsystem, error)
	SetConfig(ctx context.Context, subsystem string, settings map[string]string) (*nexoadmin.ConfigSubsystem, error)
	DeleteConfig(ctx context.Context, subsystem string, keys ...string) error
	ExportConfig(ctx context.Context) ([]byte, error)
	ImportConfig(ctx context.Context, data []byte) error

	// Managed share links (docs/09 §9.1, S-OBJ-08c).
	CreateShare(ctx context.Context, in nexoadmin.CreateShareRequest) (*nexoadmin.Share, error)
	ListShares(ctx context.Context, opts nexoadmin.ListSharesOptions) (*nexoadmin.ShareList, error)
	GetShare(ctx context.Context, id string) (*nexoadmin.Share, error)
	RevokeShare(ctx context.Context, id string) error
	RevokeAllShares(ctx context.Context, owner string) (int, error)
}

// AdminFactory builds the Admin API client of an alias: the commands
// receive it in their dependencies so that the tests can substitute the
// client.
type AdminFactory interface {
	Client(ctx context.Context, a Alias, opts ClientOptions) (AdminClient, error)
}
