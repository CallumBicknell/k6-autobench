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
//
// The counters are int64 rather than the uint64 the API declares. A counter is
// cumulative and cannot go backwards on a healthy daemon, but one that does --
// a restarted daemon, a proxy, a mocked daemon -- would fail the decode and take
// the unrelated memory reading down with it. Signed counters let cpuPercent's
// own guard report the impossible delta instead, and no counter reaches 2^63.
type dockerStats struct {
	CPU struct {
		CPUUsage struct {
			TotalUsage  int64   `json:"total_usage"`
			PerCPUUsage []int64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage int64  `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`

	PreCPU struct {
		CPUUsage struct {
			TotalUsage int64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage int64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`

	Memory struct {
		Usage int64 `json:"usage"`
		Limit int64 `json:"limit"`
		Stats struct {
			// cgroups v2 reclaimable page cache.
			InactiveFile int64 `json:"inactive_file"`
			// cgroups v1 reclaimable page cache.
			Cache int64 `json:"cache"`
		} `json:"stats"`
	} `json:"memory_stats"`
}

// cpuPercent implements the daemon's own documented formula:
//
//	(cpu_delta / system_cpu_delta) * online_cpus * 100
//
// It reports false when the daemon has not produced two samples yet, or when the
// counters went backwards, rather than inventing a zero.
func (s dockerStats) cpuPercent() (float64, bool) {
	cpuDelta := float64(s.CPU.CPUUsage.TotalUsage - s.PreCPU.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPU.SystemCPUUsage - s.PreCPU.SystemCPUUsage)
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

// endpoint is how the daemon is reached: a unix socket or a TCP address.
type endpoint struct {
	network string // "unix" or "tcp"
	address string
}

// endpoint resolves where the daemon is, following the same convention as the
// docker CLI so a configured environment works unchanged.
//
// The scheme matters and is not decoration. DOCKER_HOST is commonly set to
// tcp://host:2375 on a machine whose daemon does not use a socket at all, and
// dialing that string as a path fails with "dial unix tcp://...: no such file or
// directory" -- an error that names neither the cause nor the setting.
func (d *Docker) endpoint() (endpoint, error) {
	raw := strings.TrimSpace(d.Socket)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	}
	if raw == "" {
		return endpoint{network: "unix", address: DefaultDockerSocket}, nil
	}

	for _, prefix := range []string{"unix://", "npipe://"} {
		if path, ok := strings.CutPrefix(raw, prefix); ok {
			return endpoint{network: "unix", address: path}, nil
		}
	}
	for _, prefix := range []string{"tcp://", "http://"} {
		if addr, ok := strings.CutPrefix(raw, prefix); ok {
			return endpoint{network: "tcp", address: addr}, nil
		}
	}
	if strings.Contains(raw, "://") {
		return endpoint{}, fmt.Errorf(
			"unsupported DOCKER_HOST scheme in %q: autobench reaches the daemon over a "+
				"unix socket or tcp; set socket explicitly or use unix:// or tcp://", raw)
	}

	// A bare path is what a socket setting looks like, which is the common case.
	return endpoint{network: "unix", address: raw}, nil
}

// client returns a client that talks to the daemon over its endpoint.
func (d *Docker) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	ep, err := d.endpoint()
	if err != nil {
		// The error surfaces as a collector problem on the first poll, which is
		// where every other connection failure is reported. A client that fails
		// every call carries it there.
		return &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, err
			}),
			Timeout: d.timeout(),
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, ep.network, ep.address)
			},
		},
		Timeout: d.timeout(),
	}
}

// roundTripFunc adapts a function to http.RoundTripper, so a configuration error
// can be reported through the same path as a connection failure.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
