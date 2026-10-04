package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultDockerSocket is where the daemon listens when nothing says otherwise.
const DefaultDockerSocket = "/var/run/docker.sock"

// Docker samples one container's own CPU and memory from the Docker daemon.
//
// It exists for the case Prometheus cannot cover: a target with no monitoring
// installed at all. Everything Docker runs already exports these two numbers.
type Docker struct {
	// Source identifies the container in the report, e.g. "api".
	Source string
	// Container is the container's name or ID, as the daemon knows it.
	Container string
	// Socket is the daemon's unix socket. Empty means $DOCKER_HOST (if it names
	// a unix socket) or DefaultDockerSocket.
	Socket string
	// Interval is how often the container is sampled.
	Interval time.Duration
	// Timeout bounds one stats call. It needs room: with stream=false the daemon
	// takes two samples before answering, which takes about a second.
	Timeout time.Duration
	// HTTP is the client used for stats calls; nil means one that dials Socket.
	HTTP *http.Client
}

var _ Collector = (*Docker)(nil)

// Name implements Collector.
func (d *Docker) Name() string { return d.Source }

// Start begins sampling in the background and returns the function that stops
// it and reports what was seen.
func (d *Docker) Start(ctx context.Context) func() Report {
	return sample(ctx, d.Source, d.Interval, &dockerPoller{
		client:    d.client(),
		container: d.Container,
		timeout:   d.timeout(),
	})
}

// dockerPoller reads one container.
type dockerPoller struct {
	client    *http.Client
	container string
	timeout   time.Duration
}

// errNoDockerInterval is what a first-ever stats call reports: the daemon has
// not seen two samples, so there is no interval to measure a CPU percentage
// over, and reporting one would mean inventing a zero.
var errNoDockerInterval = fmt.Errorf("no CPU interval yet: the daemon needs two samples")

// poll reads the container's current stats.
func (dp *dockerPoller) poll(ctx context.Context) ([]reading, []pollFailure) {
	stats, err := dp.stats(ctx)
	if err != nil {
		return nil, []pollFailure{{key: dp.container, err: err}}
	}

	// Each metric is reported on its own, so one the daemon could not compute
	// does not cost the other.
	var readings []reading
	var failures []pollFailure

	if cpu, ok := stats.cpuPercent(); ok {
		readings = append(readings, reading{
			order: 0, key: "cpu", name: "cpu", unit: "percent",
			origin: dp.endpoint(), value: cpu,
		})
	} else {
		failures = append(failures, pollFailure{key: "cpu", err: errNoDockerInterval})
	}

	readings = append(readings, reading{
		order: 1, key: "memory", name: "memory", unit: "bytes",
		origin: dp.endpoint(), value: stats.memoryBytes(),
	})
	return readings, failures
}

// stats fetches one container's statistics.
//
// stream=false is what makes this a single call rather than a stream. It also
// makes the daemon wait for two samples so the response carries both the current
// and previous counters, which is what the CPU percentage is computed from.
func (dp *dockerPoller) stats(ctx context.Context) (dockerStats, error) {
	var out dockerStats

	ctx, cancel := context.WithTimeout(ctx, dp.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dp.endpoint(), nil)
	if err != nil {
		return out, err
	}
	resp, err := dp.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return out, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%s: %s", resp.Status, firstLine(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

// endpoint is the stats URL. The host is a placeholder: the client dials the
// unix socket instead, so only the path reaches the daemon.
func (dp *dockerPoller) endpoint() string {
	return "http://docker/containers/" + url.PathEscape(dp.container) + "/stats?stream=false"
}

// dockerStats is the part of the daemon's stats response this collector reads.
// The documented CPU percentage needs both the current and previous counters,
// which is why precpu_stats is here.
type dockerStats struct {
	CPU struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PerCPUUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`

	PreCPU struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`

	Memory struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			// cgroups v2 reclaimable page cache.
			InactiveFile uint64 `json:"inactive_file"`
			// cgroups v1 reclaimable page cache.
			Cache uint64 `json:"cache"`
		} `json:"stats"`
	} `json:"memory_stats"`
}

// cpuPercent implements the daemon's own documented formula:
//
//	(cpu_delta / system_cpu_delta) * online_cpus * 100
//
// It reports false when the daemon has not produced two samples yet, rather
// than inventing a zero.
func (s dockerStats) cpuPercent() (float64, bool) {
	cpuDelta := float64(s.CPU.CPUUsage.TotalUsage) - float64(s.PreCPU.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPU.SystemCPUUsage) - float64(s.PreCPU.SystemCPUUsage)
	if systemDelta <= 0 || cpuDelta < 0 {
		return 0, false
	}
	return cpuDelta / systemDelta * float64(s.cpus()) * 100, true
}

// cpus is how many CPUs the percentage is expressed against: the container may
// use more than one, so the figure can exceed 100.
func (s dockerStats) cpus() int {
	if s.CPU.OnlineCPUs > 0 {
		return int(s.CPU.OnlineCPUs)
	}
	// Older daemons omit online_cpus; the per-CPU array is the documented
	// fallback.
	if n := len(s.CPU.CPUUsage.PerCPUUsage); n > 0 {
		return n
	}
	return 1
}

// memoryBytes is the container's memory use as the docker CLI reports it: the
// kernel counts reclaimable page cache as usage, but the container does not
// really hold it.
//
// The limit is deliberately not used to produce a percentage. A container with
// no memory limit set reports the host's total as its limit, so the percentage
// would say more about the host than about the container.
func (s dockerStats) memoryBytes() float64 {
	used := s.Memory.Usage
	reclaimable := s.Memory.Stats.InactiveFile
	if reclaimable == 0 {
		reclaimable = s.Memory.Stats.Cache
	}
	if reclaimable > used {
		return float64(used)
	}
	return float64(used - reclaimable)
}

func (d *Docker) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return DefaultTimeout
}

// client returns a client that talks to the daemon over its unix socket.
func (d *Docker) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	socket := d.socket()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: d.timeout(),
	}
}

// socket is the daemon's socket path, following the same convention as the
// docker CLI so a configured environment works unchanged.
func (d *Docker) socket() string {
	if d.Socket != "" {
		return d.Socket
	}
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		if path, ok := strings.CutPrefix(host, "unix://"); ok {
			return path
		}
		// A tcp:// daemon is a different transport, and quietly treating it as a
		// path would fail later with a confusing error.
		return host
	}
	return DefaultDockerSocket
}
