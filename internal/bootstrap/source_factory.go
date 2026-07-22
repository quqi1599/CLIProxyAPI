// Package bootstrap builds the configuration source used to start the server.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/homeplugins"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// SourceKind identifies the backing system selected for configuration and auth data.
type SourceKind string

const (
	SourceLocal       SourceKind = "local"
	SourceHome        SourceKind = "home"
	SourcePostgres    SourceKind = "postgres"
	SourceObjectStore SourceKind = "object-store"
	SourceGit         SourceKind = "git"
)

// SourceMeta describes the selected source without exposing credentials.
type SourceMeta struct {
	Kind       SourceKind
	ConfigPath string
	Home       bool
}

// ConfigSource loads configuration and owns its associated token store lifecycle.
type ConfigSource interface {
	Load(context.Context) (*config.Config, SourceMeta, error)
	TokenStore() coreauth.Store
	Close() error
}

// Options contains source selection inputs collected by the composition root.
type Options struct {
	WorkingDir                  string
	WritableBase                string
	ConfigPath                  string
	CloudDeploy                 bool
	HomeJWT                     string
	HomeDisableClusterDiscovery bool
	PluginRuntime               homeplugins.PluginRuntime
	LookupEnv                   func(string) (string, bool)
}

// Plan owns the selected configuration source.
type Plan struct {
	source ConfigSource
	kind   SourceKind
}

// NewPlan selects one configuration source using the established precedence:
// Home, Postgres, object store, Git, then local files.
func NewPlan(opts Options) (*Plan, error) {
	workingDir := strings.TrimSpace(opts.WorkingDir)
	if workingDir == "" {
		return nil, fmt.Errorf("bootstrap: working directory is required")
	}
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	lookup := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := lookupEnv(key); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
		return ""
	}

	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" {
		configPath = filepath.Join(workingDir, "config.yaml")
	}
	basePath := strings.TrimSpace(opts.WritableBase)
	if basePath == "" {
		basePath = workingDir
	}
	homeJWT := strings.TrimSpace(opts.HomeJWT)
	if homeJWT == "" {
		homeJWT = lookup("HOME_JWT", "home_jwt")
	}
	postgresDSN := lookup("PGSTORE_DSN", "pgstore_dsn")
	objectEndpoint := lookup("OBJECTSTORE_ENDPOINT", "objectstore_endpoint")
	gitRemoteURL := lookup("GITSTORE_GIT_URL", "gitstore_git_url")

	var source ConfigSource
	var kind SourceKind
	switch {
	case homeJWT != "":
		kind = SourceHome
		source = &homeSource{
			jwt:                     homeJWT,
			disableClusterDiscovery: opts.HomeDisableClusterDiscovery,
			configPath:              configPath,
			pluginRuntime:           opts.PluginRuntime,
		}
	case postgresDSN != "":
		kind = SourcePostgres
		source = &postgresSource{
			dsn:           postgresDSN,
			schema:        lookup("PGSTORE_SCHEMA", "pgstore_schema"),
			spoolRoot:     filepath.Join(firstNonEmpty(lookup("PGSTORE_LOCAL_PATH", "pgstore_local_path"), basePath), "pgstore"),
			workingDir:    workingDir,
			configPath:    configPath,
			cloudDeploy:   opts.CloudDeploy,
			legacyAuthDir: resolveLegacyAuthDir(configPath),
		}
	case objectEndpoint != "":
		kind = SourceObjectStore
		source = &objectSource{
			endpoint:    objectEndpoint,
			accessKey:   lookup("OBJECTSTORE_ACCESS_KEY", "objectstore_access_key"),
			secretKey:   lookup("OBJECTSTORE_SECRET_KEY", "objectstore_secret_key"),
			bucket:      lookup("OBJECTSTORE_BUCKET", "objectstore_bucket"),
			localRoot:   filepath.Join(firstNonEmpty(lookup("OBJECTSTORE_LOCAL_PATH", "objectstore_local_path"), basePath), "objectstore"),
			workingDir:  workingDir,
			cloudDeploy: opts.CloudDeploy,
		}
	case gitRemoteURL != "":
		kind = SourceGit
		source = &gitSource{
			remoteURL:   gitRemoteURL,
			username:    lookup("GITSTORE_GIT_USERNAME", "gitstore_git_username"),
			password:    lookup("GITSTORE_GIT_TOKEN", "gitstore_git_token"),
			branch:      lookup("GITSTORE_GIT_BRANCH", "gitstore_git_branch"),
			root:        filepath.Join(firstNonEmpty(lookup("GITSTORE_LOCAL_PATH", "gitstore_local_path"), basePath), "gitstore"),
			workingDir:  workingDir,
			cloudDeploy: opts.CloudDeploy,
		}
	default:
		kind = SourceLocal
		source = &localSource{configPath: configPath, cloudDeploy: opts.CloudDeploy}
	}
	return &Plan{source: source, kind: kind}, nil
}

// Kind returns the selected source kind before any network-backed load begins.
func (p *Plan) Kind() SourceKind {
	if p == nil {
		return ""
	}
	return p.kind
}

// Load delegates configuration loading to the selected source.
func (p *Plan) Load(ctx context.Context) (*config.Config, SourceMeta, error) {
	if p == nil || p.source == nil {
		return nil, SourceMeta{}, fmt.Errorf("bootstrap: source is not configured")
	}
	cfg, meta, err := p.source.Load(ctx)
	if err != nil {
		return nil, SourceMeta{}, err
	}
	if p.source.TokenStore() == nil {
		return nil, SourceMeta{}, fmt.Errorf("bootstrap: %s source did not provide a token store", p.kind)
	}
	return cfg, meta, nil
}

// TokenStore returns the store owned by the selected source after a successful load.
func (p *Plan) TokenStore() coreauth.Store {
	if p == nil || p.source == nil {
		return nil
	}
	return p.source.TokenStore()
}

// Close releases source-owned resources.
func (p *Plan) Close() error {
	if p == nil || p.source == nil {
		return nil
	}
	return p.source.Close()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
