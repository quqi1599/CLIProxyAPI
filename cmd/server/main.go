// Package main provides the entry point for the CLI Proxy API server.
// This server acts as a proxy that provides OpenAI/Gemini/Claude compatible API interfaces
// for CLI models, allowing CLI models to be used with tools and libraries designed for standard AI APIs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/bootstrap"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/safemode"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tui"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	_ "time/tzdata"
)

var (
	Version           = "dev"
	Commit            = "none"
	BuildDate         = "unknown"
	DefaultConfigPath = ""
)

// init initializes the shared logger setup.
func init() {
	logging.SetupBaseLogger()
	buildinfo.Version = Version
	buildinfo.Commit = Commit
	buildinfo.BuildDate = BuildDate
}

func shouldStartExampleAPIKeyWarningServer(cfg *config.Config, commandMode, tuiMode, standalone, cloudConfigMissing, homeMode bool) bool {
	if cfg == nil || commandMode || homeMode || cloudConfigMissing {
		return false
	}
	if tuiMode && !standalone {
		return false
	}
	return safemode.HasExampleAPIKeys(cfg.APIKeys)
}

// main is the entry point of the application.
// It parses command-line flags, loads configuration, and starts the appropriate
// service based on the provided flags (login, codex-login, or server mode).
func main() {
	fmt.Printf("CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)
	configureRuntimeMemoryDefaults()

	// Command-line flags to control the application's behavior.
	var codexLogin bool
	var codexDeviceLogin bool
	var claudeLogin bool
	var noBrowser bool
	var oauthCallbackPort int
	var antigravityLogin bool
	var kimiLogin bool
	var xaiLogin bool
	var vertexImport string
	var vertexImportPrefix string
	var configPath string
	var password string
	var homeJWT string
	var homeDisableClusterDiscovery bool
	var tuiMode bool
	var standalone bool
	var localModel bool

	// Define command-line flags for different operation modes.
	flag.BoolVar(&codexLogin, "codex-login", false, "Login to Codex using OAuth")
	flag.BoolVar(&codexDeviceLogin, "codex-device-login", false, "Login to Codex using device code flow")
	flag.BoolVar(&claudeLogin, "claude-login", false, "Login to Claude using OAuth")
	flag.BoolVar(&noBrowser, "no-browser", false, "Don't open browser automatically for OAuth")
	flag.IntVar(&oauthCallbackPort, "oauth-callback-port", 0, "Override OAuth callback port (defaults to provider-specific port)")
	flag.BoolVar(&antigravityLogin, "antigravity-login", false, "Login to Antigravity using OAuth")
	flag.BoolVar(&kimiLogin, "kimi-login", false, "Login to Kimi using OAuth")
	flag.BoolVar(&xaiLogin, "xai-login", false, "Login to xAI using OAuth")
	flag.StringVar(&configPath, "config", DefaultConfigPath, "Configure File Path")
	flag.StringVar(&vertexImport, "vertex-import", "", "Import Vertex service account key JSON file")
	flag.StringVar(&vertexImportPrefix, "vertex-import-prefix", "", "Prefix for Vertex model namespacing (use with -vertex-import)")
	flag.StringVar(&password, "password", "", "")
	flag.StringVar(&homeJWT, "home-jwt", "", "Home control plane JWT for mTLS certificate bootstrap and connection")
	flag.BoolVar(&homeDisableClusterDiscovery, "home-disable-cluster-discovery", false, "Disable Home CLUSTER NODES discovery and keep using the configured -home-jwt address")
	flag.BoolVar(&tuiMode, "tui", false, "Start with terminal management UI")
	flag.BoolVar(&standalone, "standalone", false, "In TUI mode, start an embedded local server")
	flag.BoolVar(&localModel, "local-model", false, "Use embedded model catalog only, skip remote model fetching")

	flag.CommandLine.Usage = func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, "Usage of %s\n", os.Args[0])
		flag.CommandLine.VisitAll(func(f *flag.Flag) {
			if f.Name == "password" {
				return
			}
			s := fmt.Sprintf("  -%s", f.Name)
			name, unquoteUsage := flag.UnquoteUsage(f)
			if name != "" {
				s += " " + name
			}
			if len(s) <= 4 {
				s += "	"
			} else {
				s += "\n    "
			}
			if unquoteUsage != "" {
				s += unquoteUsage
			}
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				s += fmt.Sprintf(" (default %s)", f.DefValue)
			}
			_, _ = fmt.Fprint(out, s+"\n")
		})
	}

	pluginHost := pluginhost.New()
	if bootstrapCfg := loadPluginBootstrapConfig(pluginBootstrapConfigPath(os.Args[1:], DefaultConfigPath)); bootstrapCfg != nil {
		pluginHost.ApplyConfig(context.Background(), bootstrapCfg)
		pluginHost.RegisterCommandLineFlags(context.Background(), flag.CommandLine)
	}

	// Parse the command-line flags.
	flag.Parse()

	// Core application variables.
	wd, err := os.Getwd()
	if err != nil {
		log.Errorf("failed to get working directory: %v", err)
		return
	}

	// Load environment variables from .env if present.
	if errLoad := godotenv.Load(filepath.Join(wd, ".env")); errLoad != nil {
		if !errors.Is(errLoad, os.ErrNotExist) {
			log.WithError(errLoad).Warn("failed to load .env file")
		}
	}

	writableBase := util.WritablePath()

	// Check for cloud deploy mode only on first execution
	// Read env var name in uppercase: DEPLOY
	isCloudDeploy := os.Getenv("DEPLOY") == "cloud"
	bootstrapPlan, errPlan := bootstrap.NewPlan(bootstrap.Options{
		WorkingDir:                  wd,
		WritableBase:                writableBase,
		ConfigPath:                  configPath,
		CloudDeploy:                 isCloudDeploy,
		HomeJWT:                     homeJWT,
		HomeDisableClusterDiscovery: homeDisableClusterDiscovery,
		PluginRuntime:               pluginHost,
	})
	if errPlan != nil {
		log.Errorf("failed to plan config source: %v", errPlan)
		return
	}
	defer func() {
		if errClose := bootstrapPlan.Close(); errClose != nil {
			log.Errorf("failed to close config source: %v", errClose)
		}
	}()
	cfg, sourceMeta, errLoad := bootstrapPlan.Load(context.Background())
	if errLoad != nil {
		log.Errorf("failed to load config source: %v", errLoad)
		return
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	configFilePath := sourceMeta.ConfigPath
	configLoadedFromHome := sourceMeta.Home

	// In cloud deploy mode, check if we have a valid configuration
	var configFileExists bool
	if isCloudDeploy {
		if configLoadedFromHome && cfg != nil {
			configFileExists = cfg.Port != 0
		} else {
			if info, errStat := os.Stat(configFilePath); errStat != nil {
				// Don't mislead: API server will not start until configuration is provided.
				log.Info("Cloud deploy mode: No configuration file detected; standing by for configuration")
				configFileExists = false
			} else if info.IsDir() {
				log.Info("Cloud deploy mode: Config path is a directory; standing by for configuration")
				configFileExists = false
			} else if cfg.Port == 0 {
				// LoadConfigOptional returns empty config when file is empty or invalid.
				// Config file exists but is empty or invalid; treat as missing config
				log.Info("Cloud deploy mode: Configuration file is empty or invalid; standing by for valid configuration")
				configFileExists = false
			} else {
				log.Info("Cloud deploy mode: Configuration file detected; starting service")
				configFileExists = true
			}
		}
	}
	redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	coreauth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	coreauth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)

	if err = logging.ConfigureLogOutput(cfg); err != nil {
		log.Errorf("failed to configure log output: %v", err)
		return
	}

	log.Infof("CLIProxyAPI Version: %s, Commit: %s, BuiltAt: %s", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)

	// Set the log level based on the configuration.
	util.SetLogLevel(cfg)

	if resolvedAuthDir, errResolveAuthDir := util.ResolveAuthDir(cfg.AuthDir); errResolveAuthDir != nil {
		log.Errorf("failed to resolve auth directory: %v", errResolveAuthDir)
		return
	} else {
		cfg.AuthDir = resolvedAuthDir
	}
	managementasset.SetCurrentConfig(cfg)

	// Create login options to be used in authentication flows.
	options := &cmd.LoginOptions{
		NoBrowser:    noBrowser,
		CallbackPort: oauthCallbackPort,
	}

	commandMode := vertexImport != "" || antigravityLogin || codexLogin || codexDeviceLogin || claudeLogin || kimiLogin || xaiLogin
	cloudConfigMissing := isCloudDeploy && !configFileExists
	homeMode := configLoadedFromHome || (cfg != nil && cfg.Home.Enabled)
	if shouldStartExampleAPIKeyWarningServer(cfg, commandMode, tuiMode, standalone, cloudConfigMissing, homeMode) {
		matches := safemode.ExampleAPIKeys(cfg.APIKeys)
		log.WithField("api_keys", strings.Join(matches, ",")).Error("unsafe example API key configured; starting warning-only server")
		cmd.StartExampleAPIKeyWarningServer(cfg, configFilePath, matches)
		return
	}

	// Register the shared token store once so all components use the same persistence backend.
	sdkAuth.RegisterTokenStore(bootstrapPlan.TokenStore())

	// Register built-in access providers before constructing services.
	configaccess.Register(&cfg.SDKConfig)
	pluginHost.ApplyConfig(context.Background(), cfg)
	if pluginHost.HasTriggeredCommandLineFlags() {
		if exitCode, handled := pluginHost.ExecuteCommandLine(context.Background(), os.Args[0], os.Args[1:], configFilePath, flag.CommandLine); handled {
			if exitCode != 0 {
				os.Exit(exitCode)
			}
			return
		}
	}

	// Handle different command modes based on the provided flags.

	if vertexImport != "" {
		// Handle Vertex service account import
		cmd.DoVertexImport(cfg, vertexImport, vertexImportPrefix)
	} else if antigravityLogin {
		// Handle Antigravity login
		cmd.DoAntigravityLogin(cfg, options)
	} else if codexLogin {
		// Handle Codex login
		cmd.DoCodexLogin(cfg, options)
	} else if codexDeviceLogin {
		// Handle Codex device-code login
		cmd.DoCodexDeviceLogin(cfg, options)
	} else if claudeLogin {
		// Handle Claude login
		cmd.DoClaudeLogin(cfg, options)
	} else if kimiLogin {
		cmd.DoKimiLogin(cfg, options)
	} else if xaiLogin {
		cmd.DoXAILogin(cfg, options)
	} else {
		// In cloud deploy mode without config file, just wait for shutdown signals
		if isCloudDeploy && !configFileExists {
			// No config file available, just wait for shutdown
			cmd.WaitForCloudDeploy()
			return
		}
		localModel = effectiveLocalModel(localModel, flagExplicitlySet(flag.CommandLine, "local-model"), cfg)
		if localModel && (!tuiMode || standalone) {
			log.Info("Local model mode: using embedded model catalog, remote model updates disabled")
		}
		if tuiMode {
			if standalone {
				// Standalone mode: start an embedded local server and connect TUI client to it.
				managementasset.StartAutoUpdater(context.Background(), configFilePath)
				misc.StartAntigravityVersionUpdater(context.Background())
				if !localModel && !cfg.Home.Enabled {
					registry.StartModelsUpdater(context.Background())
				} else if cfg.Home.Enabled {
					log.Info("Home mode: remote model updates disabled")
				}
				hook := tui.NewLogHook(2000)
				hook.SetFormatter(&logging.LogFormatter{})
				log.AddHook(hook)

				origStdout := os.Stdout
				origStderr := os.Stderr
				origLogOutput := log.StandardLogger().Out
				log.SetOutput(io.Discard)

				devNull, errOpenDevNull := os.Open(os.DevNull)
				if errOpenDevNull == nil {
					os.Stdout = devNull
					os.Stderr = devNull
				}

				restoreIO := func() {
					os.Stdout = origStdout
					os.Stderr = origStderr
					log.SetOutput(origLogOutput)
					if devNull != nil {
						_ = devNull.Close()
					}
				}

				localMgmtPassword := fmt.Sprintf("tui-%d-%d", os.Getpid(), time.Now().UnixNano())
				if password == "" {
					password = localMgmtPassword
				}

				cancel, done := cmd.StartServiceBackgroundWithPluginHost(cfg, configFilePath, password, pluginHost)

				client := tui.NewClient(cfg.Port, password)
				ready := false
				backoff := 100 * time.Millisecond
				for i := 0; i < 30; i++ {
					if _, errGetConfig := client.GetConfig(); errGetConfig == nil {
						ready = true
						break
					}
					time.Sleep(backoff)
					if backoff < time.Second {
						backoff = time.Duration(float64(backoff) * 1.5)
					}
				}

				if !ready {
					restoreIO()
					cancel()
					<-done
					fmt.Fprintf(os.Stderr, "TUI error: embedded server is not ready\n")
					return
				}

				if errRun := tui.Run(cfg.Port, password, hook, origStdout); errRun != nil {
					restoreIO()
					fmt.Fprintf(os.Stderr, "TUI error: %v\n", errRun)
				} else {
					restoreIO()
				}

				cancel()
				<-done
			} else {
				// Default TUI mode: pure management client.
				// The proxy server must already be running.
				if errRun := tui.Run(cfg.Port, password, nil, os.Stdout); errRun != nil {
					fmt.Fprintf(os.Stderr, "TUI error: %v\n", errRun)
				}
			}
		} else {
			// Start the main proxy service
			managementasset.StartAutoUpdater(context.Background(), configFilePath)
			misc.StartAntigravityVersionUpdater(context.Background())
			if !localModel && !cfg.Home.Enabled {
				registry.StartModelsUpdater(context.Background())
			} else if cfg.Home.Enabled {
				log.Info("Home mode: remote model updates disabled")
			}
			cmd.StartServiceWithPluginHost(cfg, configFilePath, password, pluginHost)
		}
	}
}

func pluginBootstrapConfigPath(args []string, defaultPath string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return defaultPluginBootstrapConfigPath(defaultPath)
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
			return defaultPluginBootstrapConfigPath(defaultPath)
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return defaultPluginBootstrapConfigPath(defaultPath)
}

func defaultPluginBootstrapConfigPath(defaultPath string) string {
	if strings.TrimSpace(defaultPath) != "" {
		return defaultPath
	}
	wd, errGetwd := os.Getwd()
	if errGetwd != nil {
		return "config.yaml"
	}
	return filepath.Join(wd, "config.yaml")
}

func loadPluginBootstrapConfig(path string) *config.Config {
	raw, errReadFile := os.ReadFile(path)
	if errReadFile != nil {
		if !errors.Is(errReadFile, os.ErrNotExist) {
			log.Warnf("failed to read plugin bootstrap config: %v", errReadFile)
		}
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		cfg := &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	cfg, errParseConfig := config.ParseConfigBytes(raw)
	if errParseConfig != nil {
		log.Warnf("failed to parse plugin bootstrap config: %v", errParseConfig)
		cfg = &config.Config{}
		cfg.NormalizePluginsConfig()
		return cfg
	}
	return cfg
}
