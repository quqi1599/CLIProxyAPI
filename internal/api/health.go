package api

import (
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

type healthDetailsResponse struct {
	Status            string                              `json:"status"`
	Ready             bool                                `json:"ready"`
	Build             healthBuildDetails                  `json:"build"`
	Config            healthConfigDetails                 `json:"config"`
	Process           healthProcessDetails                `json:"process"`
	Dependencies      healthDependencyDetails             `json:"dependencies"`
	Admission         handlers.AdmissionSnapshot          `json:"admission"`
	PayloadBodyLimits handlers.PayloadBodyLimitSnapshot   `json:"payload_body_limits"`
	Amplification     handlers.AmplificationGuardSnapshot `json:"amplification_guard"`
	Transforms        internalpayload.TransformMetrics    `json:"transforms"`
	LargeClones       internalpayload.LargeCloneMetrics   `json:"large_clones"`
}

type healthBuildDetails struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

type healthConfigDetails struct {
	Version   string `json:"version"`
	Available bool   `json:"available"`
}

type healthProcessDetails struct {
	RSSBytes       *uint64 `json:"rss_bytes"`
	HeapLiveBytes  uint64  `json:"heap_live_bytes"`
	HeapInUseBytes uint64  `json:"heap_in_use_bytes"`
	GCCPUFraction  float64 `json:"gc_cpu_fraction"`
	Goroutines     int     `json:"goroutines"`
	OpenFDs        *int    `json:"open_fds"`
	OpenSockets    *int    `json:"open_sockets"`
	StartedAt      string  `json:"started_at"`
	UptimeSeconds  int64   `json:"uptime_seconds"`
}

type healthDependencyDetails struct {
	ConfigAvailable bool  `json:"config_available"`
	AdmissionReady  bool  `json:"admission_ready"`
	HomeEnabled     bool  `json:"home_enabled"`
	HomeHeartbeat   *bool `json:"home_heartbeat,omitempty"`
}

var processStartedAt = time.Now()

func (s *Server) healthDetails(c *gin.Context) {
	if c.Query("gc") == "1" {
		runtime.GC()
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	configVersion := ""
	configAvailable := false
	if s != nil && s.mgmt != nil {
		var errConfig error
		configVersion, errConfig = s.mgmt.ConfigSnapshotVersion()
		configAvailable = errConfig == nil
	}

	admission := handlers.AdmissionSnapshot{}
	payloadBodyLimits := handlers.PayloadBodyLimitSnapshot{}
	amplification := handlers.AmplificationGuardSnapshot{Mode: "enforce"}
	ready := s.readyForTraffic()
	admissionReady := true
	if s != nil && s.handlers != nil {
		admission = s.handlers.AdmissionSnapshot()
		payloadBodyLimits = s.handlers.PayloadBodyLimitSnapshot()
		amplification = s.handlers.AmplificationGuardSnapshot()
		admissionReady = s.handlers.AdmissionReady()
	}
	homeEnabled := s != nil && s.cfg != nil && s.cfg.Home.Enabled
	var homeHeartbeat *bool
	if homeEnabled {
		client := home.Current()
		value := client != nil && client.HeartbeatOK()
		homeHeartbeat = &value
	}
	openFDs, openSockets := currentProcessDescriptors()

	c.JSON(http.StatusOK, healthDetailsResponse{
		Status: "ok",
		Ready:  ready,
		Build: healthBuildDetails{
			Version:   buildinfo.Version,
			Commit:    buildinfo.Commit,
			BuildDate: buildinfo.BuildDate,
		},
		Config: healthConfigDetails{Version: configVersion, Available: configAvailable},
		Process: healthProcessDetails{
			RSSBytes:       currentRSSBytes(),
			HeapLiveBytes:  runtimeUint64Metric("/gc/heap/live:bytes"),
			HeapInUseBytes: mem.HeapInuse,
			GCCPUFraction:  mem.GCCPUFraction,
			Goroutines:     runtime.NumGoroutine(),
			OpenFDs:        openFDs,
			OpenSockets:    openSockets,
			StartedAt:      processStartedAt.UTC().Format(time.RFC3339Nano),
			UptimeSeconds:  max(int64(time.Since(processStartedAt)/time.Second), 0),
		},
		Dependencies: healthDependencyDetails{
			ConfigAvailable: configAvailable,
			AdmissionReady:  admissionReady,
			HomeEnabled:     homeEnabled,
			HomeHeartbeat:   homeHeartbeat,
		},
		Admission:         admission,
		PayloadBodyLimits: payloadBodyLimits,
		Amplification:     amplification,
		Transforms:        internalpayload.CurrentTransformMetrics(),
		LargeClones:       internalpayload.CurrentLargeCloneMetrics(),
	})
}

func (s *Server) readyForTraffic() bool {
	if s == nil || !s.ready.Load() {
		return false
	}
	if s.cfg != nil && s.cfg.Home.Enabled {
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			return false
		}
	}
	return s.handlers == nil || s.handlers.AdmissionReady()
}

func runtimeUint64Metric(name string) uint64 {
	samples := []metrics.Sample{{Name: name}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return samples[0].Value.Uint64()
}

func currentRSSBytes() *uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	return parseProcStatusRSS(data)
}

func parseProcStatusRSS(data []byte) *uint64 {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "VmRSS:" {
			continue
		}
		if fields[2] != "kB" {
			return nil
		}
		kilobytes, errParse := strconv.ParseUint(fields[1], 10, 64)
		if errParse != nil || kilobytes > ^uint64(0)/1024 {
			return nil
		}
		bytes := kilobytes * 1024
		return &bytes
	}
	return nil
}

func currentOpenFDs() *int {
	openFDs, _ := currentProcessDescriptors()
	return openFDs
}

func currentProcessDescriptors() (*int, *int) {
	for _, path := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		fdCount := len(entries)
		socketCount := 0
		canIdentifySockets := path == "/proc/self/fd"
		if canIdentifySockets {
			for _, entry := range entries {
				target, errLink := os.Readlink(path + "/" + entry.Name())
				if errLink == nil && strings.HasPrefix(target, "socket:[") {
					socketCount++
				}
			}
			return &fdCount, &socketCount
		}
		return &fdCount, nil
	}
	return nil, nil
}
