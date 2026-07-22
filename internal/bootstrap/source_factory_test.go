package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewPlanSourcePrecedence(t *testing.T) {
	tests := []struct {
		name    string
		homeJWT string
		env     map[string]string
		want    SourceKind
	}{
		{name: "local default", want: SourceLocal},
		{
			name: "git",
			env:  map[string]string{"GITSTORE_GIT_URL": "https://example.com/config.git"},
			want: SourceGit,
		},
		{
			name: "object store before git",
			env: map[string]string{
				"OBJECTSTORE_ENDPOINT": "s3.example.com",
				"GITSTORE_GIT_URL":     "https://example.com/config.git",
			},
			want: SourceObjectStore,
		},
		{
			name: "postgres before object store and git",
			env: map[string]string{
				"PGSTORE_DSN":          "postgres://example/db",
				"OBJECTSTORE_ENDPOINT": "s3.example.com",
				"GITSTORE_GIT_URL":     "https://example.com/config.git",
			},
			want: SourcePostgres,
		},
		{
			name:    "home flag before all stores",
			homeJWT: "header.payload.signature",
			env: map[string]string{
				"PGSTORE_DSN":          "postgres://example/db",
				"OBJECTSTORE_ENDPOINT": "s3.example.com",
				"GITSTORE_GIT_URL":     "https://example.com/config.git",
			},
			want: SourceHome,
		},
		{
			name: "home lowercase environment alias",
			env:  map[string]string{"home_jwt": "header.payload.signature"},
			want: SourceHome,
		},
		{
			name: "postgres lowercase environment alias",
			env:  map[string]string{"pgstore_dsn": "postgres://example/db"},
			want: SourcePostgres,
		},
		{
			name: "object store lowercase environment alias",
			env:  map[string]string{"objectstore_endpoint": "s3.example.com"},
			want: SourceObjectStore,
		},
		{
			name: "git lowercase environment alias",
			env:  map[string]string{"gitstore_git_url": "https://example.com/config.git"},
			want: SourceGit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, errPlan := NewPlan(Options{
				WorkingDir: t.TempDir(),
				HomeJWT:    tt.homeJWT,
				LookupEnv:  mapLookup(tt.env),
			})
			if errPlan != nil {
				t.Fatalf("NewPlan() error = %v", errPlan)
			}
			if got := plan.Kind(); got != tt.want {
				t.Fatalf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewPlanResolvesSourceOptions(t *testing.T) {
	workingDir := t.TempDir()
	writableBase := filepath.Join(workingDir, "writable")
	plan, errPlan := NewPlan(Options{
		WorkingDir:   workingDir,
		WritableBase: writableBase,
		LookupEnv: mapLookup(map[string]string{
			"OBJECTSTORE_ENDPOINT":   "https://s3.example.com/root",
			"OBJECTSTORE_ACCESS_KEY": "access",
			"OBJECTSTORE_SECRET_KEY": "secret",
			"OBJECTSTORE_BUCKET":     "bucket",
		}),
	})
	if errPlan != nil {
		t.Fatalf("NewPlan() error = %v", errPlan)
	}
	source, ok := plan.source.(*objectSource)
	if !ok {
		t.Fatalf("source = %T, want *objectSource", plan.source)
	}
	if source.localRoot != filepath.Join(writableBase, "objectstore") {
		t.Fatalf("localRoot = %q, want writable objectstore path", source.localRoot)
	}
	if source.endpoint != "https://s3.example.com/root" {
		t.Fatalf("endpoint = %q", source.endpoint)
	}
	if source.accessKey != "access" || source.secretKey != "secret" || source.bucket != "bucket" {
		t.Fatal("object source credentials or bucket were not captured")
	}
}

func TestLocalSourceLoad(t *testing.T) {
	workingDir := t.TempDir()
	configPath := filepath.Join(workingDir, "custom.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 9123\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	plan, errPlan := NewPlan(Options{
		WorkingDir: workingDir,
		ConfigPath: configPath,
		LookupEnv:  mapLookup(nil),
	})
	if errPlan != nil {
		t.Fatalf("NewPlan() error = %v", errPlan)
	}
	cfg, meta, errLoad := plan.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if cfg == nil || cfg.Port != 9123 {
		t.Fatalf("loaded config = %+v, want port 9123", cfg)
	}
	if meta.Kind != SourceLocal || meta.ConfigPath != configPath || meta.Home {
		t.Fatalf("source meta = %+v", meta)
	}
	if plan.TokenStore() == nil {
		t.Fatal("TokenStore() = nil")
	}
}

func TestPlanRequiresTokenStoreAndClosesSource(t *testing.T) {
	source := &testSource{cfg: &config.Config{Port: 8317}}
	plan := &Plan{source: source, kind: SourceLocal}
	if _, _, errLoad := plan.Load(context.Background()); errLoad == nil {
		t.Fatal("Load() error = nil, want missing token store error")
	}

	source.tokenStore = sdkauth.NewFileTokenStore()
	if _, _, errLoad := plan.Load(context.Background()); errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if errClose := plan.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	if source.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", source.closeCalls)
	}
}

func TestNewPlanRejectsMissingWorkingDirectory(t *testing.T) {
	if _, errPlan := NewPlan(Options{LookupEnv: mapLookup(nil)}); errPlan == nil {
		t.Fatal("NewPlan() error = nil, want working directory error")
	}
}

type testSource struct {
	cfg        *config.Config
	tokenStore coreauth.Store
	closeCalls int
}

func (s *testSource) Load(context.Context) (*config.Config, SourceMeta, error) {
	return s.cfg, SourceMeta{Kind: SourceLocal}, nil
}

func (s *testSource) TokenStore() coreauth.Store { return s.tokenStore }

func (s *testSource) Close() error {
	s.closeCalls++
	return nil
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

var _ ConfigSource = (*testSource)(nil)
