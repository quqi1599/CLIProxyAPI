package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/homeplugins"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	sourceOperationTimeout = 30 * time.Second
	usageMigrationTimeout  = 5 * time.Minute
	postgresInitRetryDelay = 2 * time.Second
)

type localSource struct {
	configPath  string
	cloudDeploy bool
	tokenStore  *sdkauth.FileTokenStore
}

func (s *localSource) Load(context.Context) (*config.Config, SourceMeta, error) {
	s.tokenStore = sdkauth.NewFileTokenStore()
	cfg, err := config.LoadConfigOptional(s.configPath, s.cloudDeploy)
	if err != nil {
		return nil, SourceMeta{}, fmt.Errorf("load local config: %w", err)
	}
	return cfg, SourceMeta{Kind: SourceLocal, ConfigPath: s.configPath}, nil
}

func (s *localSource) TokenStore() coreauth.Store { return s.tokenStore }
func (s *localSource) Close() error               { return nil }

type homeSource struct {
	jwt                     string
	disableClusterDiscovery bool
	configPath              string
	pluginRuntime           homeplugins.PluginRuntime
	client                  *home.Client
	tokenStore              *sdkauth.FileTokenStore
}

func (s *homeSource) Load(ctx context.Context) (*config.Config, SourceMeta, error) {
	s.tokenStore = sdkauth.NewFileTokenStore()
	ctxConfig, cancelConfig := operationContext(ctx)
	homeCfg, errConfig := home.ConfigFromJWT(ctxConfig, s.jwt)
	cancelConfig()
	if errConfig != nil {
		return nil, SourceMeta{}, fmt.Errorf("prepare home config: %w", errConfig)
	}
	if s.disableClusterDiscovery {
		homeCfg.DisableClusterDiscovery = true
	}
	s.client = home.New(homeCfg)

	ctxFetch, cancelFetch := operationContext(ctx)
	raw, errFetch := s.client.GetConfig(ctxFetch)
	cancelFetch()
	if errFetch != nil {
		return nil, SourceMeta{}, fmt.Errorf("fetch config from home: %w", errFetch)
	}
	parsed, errParse := config.ParseConfigBytes(raw)
	if errParse != nil {
		return nil, SourceMeta{}, fmt.Errorf("parse config payload from home: %w", errParse)
	}
	if parsed == nil {
		parsed = &config.Config{}
	}
	parsed.Home = homeCfg
	parsed.Port = 8317
	parsed.UsageStatisticsEnabled = true

	ctxPlugins, cancelPlugins := operationContext(ctx)
	errPlugins := homeplugins.Sync(ctxPlugins, parsed, s.pluginRuntime)
	cancelPlugins()
	if errPlugins != nil {
		return nil, SourceMeta{}, fmt.Errorf("fetch plugins from home: %w", errPlugins)
	}
	return parsed, SourceMeta{Kind: SourceHome, ConfigPath: s.configPath, Home: true}, nil
}

func (s *homeSource) TokenStore() coreauth.Store { return s.tokenStore }

func (s *homeSource) Close() error {
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	return nil
}

type postgresSource struct {
	dsn           string
	schema        string
	spoolRoot     string
	workingDir    string
	configPath    string
	cloudDeploy   bool
	legacyAuthDir string
	store         *store.PostgresStore
}

func (s *postgresSource) Load(ctx context.Context) (*config.Config, SourceMeta, error) {
	ctxInit, cancelInit := operationContext(ctx)
	postgresStore, errInit := newPostgresStoreWithRetry(ctxInit, store.PostgresStoreConfig{
		DSN:      s.dsn,
		Schema:   s.schema,
		SpoolDir: s.spoolRoot,
	})
	cancelInit()
	if errInit != nil {
		return nil, SourceMeta{}, fmt.Errorf("initialize postgres token store: %w", errInit)
	}
	s.store = postgresStore

	ctxBootstrap, cancelBootstrap := operationContext(ctx)
	errBootstrap := s.store.BootstrapWithFileMigration(
		ctxBootstrap,
		filepath.Join(s.workingDir, "config.example.yaml"),
		s.configPath,
		s.legacyAuthDir,
	)
	cancelBootstrap()
	if errBootstrap != nil {
		_ = s.Close()
		return nil, SourceMeta{}, fmt.Errorf("bootstrap postgres-backed config: %w", errBootstrap)
	}

	ctxUsage, cancelUsage := context.WithTimeout(normalizeContext(ctx), usageMigrationTimeout)
	if errUsage := usage.MigrateSQLiteToPostgres(ctxUsage, s.dsn, s.schema, s.legacyAuthDir); errUsage != nil {
		log.Warnf("failed to migrate legacy sqlite usage database, continuing with local runtime storage: %v", errUsage)
	}
	cancelUsage()

	cfg, errLoad := config.LoadConfigOptional(s.configPath, s.cloudDeploy)
	if errLoad != nil {
		return nil, SourceMeta{}, fmt.Errorf("load postgres-backed config: %w", errLoad)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = s.store.AuthDir()
	log.Infof("postgres-backed token store enabled, workspace path: %s", s.store.WorkDir())
	return cfg, SourceMeta{Kind: SourcePostgres, ConfigPath: s.configPath}, nil
}

func (s *postgresSource) TokenStore() coreauth.Store { return s.store }

func (s *postgresSource) Close() error {
	if s.store == nil {
		return nil
	}
	errClose := s.store.Close()
	s.store = nil
	return errClose
}

type objectSource struct {
	endpoint    string
	accessKey   string
	secretKey   string
	bucket      string
	localRoot   string
	workingDir  string
	cloudDeploy bool
	store       *store.ObjectTokenStore
}

func (s *objectSource) Load(ctx context.Context) (*config.Config, SourceMeta, error) {
	endpoint, useSSL, errEndpoint := resolveObjectEndpoint(s.endpoint)
	if errEndpoint != nil {
		return nil, SourceMeta{}, errEndpoint
	}
	objectStore, errNew := store.NewObjectTokenStore(store.ObjectStoreConfig{
		Endpoint:  endpoint,
		Bucket:    s.bucket,
		AccessKey: s.accessKey,
		SecretKey: s.secretKey,
		LocalRoot: s.localRoot,
		UseSSL:    useSSL,
		PathStyle: true,
	})
	if errNew != nil {
		return nil, SourceMeta{}, fmt.Errorf("initialize object token store: %w", errNew)
	}
	s.store = objectStore

	ctxBootstrap, cancelBootstrap := operationContext(ctx)
	errBootstrap := s.store.Bootstrap(ctxBootstrap, filepath.Join(s.workingDir, "config.example.yaml"))
	cancelBootstrap()
	if errBootstrap != nil {
		return nil, SourceMeta{}, fmt.Errorf("bootstrap object-backed config: %w", errBootstrap)
	}
	configPath := s.store.ConfigPath()
	cfg, errLoad := config.LoadConfigOptional(configPath, s.cloudDeploy)
	if errLoad != nil {
		return nil, SourceMeta{}, fmt.Errorf("load object-backed config: %w", errLoad)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = s.store.AuthDir()
	log.Infof("object-backed token store enabled, bucket: %s", s.bucket)
	return cfg, SourceMeta{Kind: SourceObjectStore, ConfigPath: configPath}, nil
}

func (s *objectSource) TokenStore() coreauth.Store { return s.store }
func (s *objectSource) Close() error               { return nil }

type gitSource struct {
	remoteURL   string
	username    string
	password    string
	branch      string
	root        string
	workingDir  string
	cloudDeploy bool
	store       *store.GitTokenStore
}

func (s *gitSource) Load(ctx context.Context) (*config.Config, SourceMeta, error) {
	gitStore := store.NewGitTokenStore(s.remoteURL, s.username, s.password, s.branch)
	gitStore.SetBaseDir(filepath.Join(s.root, "auths"))
	if errRepo := gitStore.EnsureRepository(); errRepo != nil {
		return nil, SourceMeta{}, fmt.Errorf("prepare git token store: %w", errRepo)
	}
	s.store = gitStore
	configPath := s.store.ConfigPath()
	if configPath == "" {
		configPath = filepath.Join(s.root, "config", "config.yaml")
	}
	if _, errStat := os.Stat(configPath); errors.Is(errStat, fs.ErrNotExist) {
		if errBootstrap := bootstrapGitBackedConfig(ctx, filepath.Join(s.workingDir, "config.example.yaml"), configPath, s.store); errBootstrap != nil {
			return nil, SourceMeta{}, fmt.Errorf("bootstrap git-backed config: %w", errBootstrap)
		}
		log.Infof("git-backed config initialized from template: %s", configPath)
	} else if errStat != nil {
		return nil, SourceMeta{}, fmt.Errorf("inspect git-backed config: %w", errStat)
	}
	cfg, errLoad := config.LoadConfigOptional(configPath, s.cloudDeploy)
	if errLoad != nil {
		return nil, SourceMeta{}, fmt.Errorf("load git-backed config: %w", errLoad)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = s.store.AuthDir()
	log.Infof("git-backed token store enabled, repository path: %s", s.root)
	return cfg, SourceMeta{Kind: SourceGit, ConfigPath: configPath}, nil
}

func (s *gitSource) TokenStore() coreauth.Store { return s.store }
func (s *gitSource) Close() error               { return nil }

type configPersister interface {
	PersistConfig(context.Context) error
}

func bootstrapGitBackedConfig(ctx context.Context, examplePath, configPath string, persister configPersister) error {
	if errCopy := misc.CopyConfigTemplate(examplePath, configPath); errCopy != nil {
		return fmt.Errorf("copy config template: %w", errCopy)
	}
	if errPersist := persister.PersistConfig(normalizeContext(ctx)); errPersist != nil {
		return fmt.Errorf("persist config: %w", errPersist)
	}
	return nil
}

func resolveLegacyAuthDir(configPath string) string {
	defaultPath, errDefault := util.ResolveAuthDir("~/.cli-proxy-api")
	if errDefault != nil {
		defaultPath = ""
	}
	trimmedPath := strings.TrimSpace(configPath)
	if trimmedPath == "" {
		return defaultPath
	}
	if _, errStat := os.Stat(trimmedPath); errStat != nil {
		return defaultPath
	}
	cfg, errLoad := config.LoadConfigOptional(trimmedPath, false)
	if errLoad != nil || cfg == nil {
		return defaultPath
	}
	resolved, errResolve := util.ResolveAuthDir(cfg.AuthDir)
	if errResolve != nil || strings.TrimSpace(resolved) == "" {
		return defaultPath
	}
	return resolved
}

func resolveObjectEndpoint(raw string) (endpoint string, useSSL bool, err error) {
	endpoint = strings.TrimSpace(raw)
	useSSL = true
	if strings.Contains(endpoint, "://") {
		parsed, errParse := url.Parse(endpoint)
		if errParse != nil {
			return "", false, fmt.Errorf("parse object store endpoint %q: %w", raw, errParse)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			useSSL = false
		case "https":
			useSSL = true
		default:
			return "", false, fmt.Errorf("unsupported object store scheme %q (only http and https are allowed)", parsed.Scheme)
		}
		if parsed.Host == "" {
			return "", false, fmt.Errorf("object store endpoint %q is missing host information", raw)
		}
		endpoint = parsed.Host
		if parsed.Path != "" && parsed.Path != "/" {
			endpoint = strings.TrimSuffix(parsed.Host+parsed.Path, "/")
		}
	}
	return strings.TrimRight(endpoint, "/"), useSSL, nil
}

func newPostgresStoreWithRetry(ctx context.Context, cfg store.PostgresStoreConfig) (*store.PostgresStore, error) {
	ctx = normalizeContext(ctx)
	var lastErr error
	for attempt := 1; ; attempt++ {
		postgresStore, errStart := store.NewPostgresStore(ctx, cfg)
		if errStart == nil {
			return postgresStore, nil
		}
		lastErr = errStart
		if ctx.Err() != nil {
			break
		}
		log.WithError(errStart).Warnf(
			"postgres token store initialization failed; retrying in %s (attempt %d)",
			postgresInitRetryDelay,
			attempt+1,
		)
		timer := time.NewTimer(postgresInitRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("postgres token store initialization canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(normalizeContext(ctx), sourceOperationTimeout)
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
