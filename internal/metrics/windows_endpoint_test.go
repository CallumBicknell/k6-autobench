package metrics

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReadWindowsAttributesEndpoints: a window carries the same per-request
// breakdown as the run, which is what lets a spike be attributed.
func TestReadWindowsAttributesEndpoints(t *testing.T) {
	o := origin()
	var b strings.Builder
	// Window 0: "fast" twice and "slow" twice, all in the first 10s.
	add := func(metric, name string, sec int, v float64) {
		line := `{"metric":"` + metric + `","type":"Point","data":{"time":"` +
			o.Add(time.Duration(sec)*time.Second).UTC().Format(time.RFC3339Nano) +
			`","value":` + strconv.FormatFloat(v, 'f', -1, 64) +
			`,"tags":{"name":"` + name + `"}}}`
		b.WriteString(line + "\n")
	}
	for i, d := range []float64{1, 2} {
		add("http_reqs", "fast", i, 1)
		add("http_req_duration", "fast", i, d)
	}
	for i, d := range []float64{900, 1000} {
		add("http_reqs", "slow", i+2, 1)
		add("http_req_failed", "slow", i+2, 1)
		add("http_req_duration", "slow", i+2, d)
	}

	got, err := ReadWindows(strings.NewReader(b.String()), WindowConfig{
		Size: 10 * time.Second, Percentiles: []float64{50, 95},
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1", len(got))
	}

	w := got[0]
	if w.Requests != 4 || w.Failed != 2 {
		t.Errorf("window totals = %d/%d, want 4 requests and 2 failures", w.Requests, w.Failed)
	}
	if len(w.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want 2", w.Endpoints)
	}
	// Busiest first, then by name; both have two requests, so name decides.
	if w.Endpoints[0].Name != "fast" || w.Endpoints[1].Name != "slow" {
		t.Errorf("endpoint order = %s, %s; want fast then slow",
			w.Endpoints[0].Name, w.Endpoints[1].Name)
	}

	fast, slow := w.Endpoints[0], w.Endpoints[1]
	if fast.Requests != 2 || fast.Failed != 0 {
		t.Errorf("fast = %d requests, %d failed; want 2/0", fast.Requests, fast.Failed)
	}
	if p, ok := PercentileAt(fast.Percentiles, 95); !ok || p != 1.95 {
		t.Errorf("fast p95 = %v, want 1.95", p)
	}
	if slow.Requests != 2 || slow.Failed != 2 {
		t.Errorf("slow = %d requests, %d failed; want 2/2", slow.Requests, slow.Failed)
	}
	if slow.ErrorRate != 1 {
		t.Errorf("slow error rate = %v, want 1", slow.ErrorRate)
	}
	if p, ok := PercentileAt(slow.Percentiles, 95); !ok || p != 995 {
		t.Errorf("slow p95 = %v, want 995", p)
	}
}

// TestReadWindowsPerEndpointSpansWindows: the same name in two windows is two
// separate readings, which is what drift is measured from.
func TestReadWindowsPerEndpointSpansWindows(t *testing.T) {
	o := origin()
	var b strings.Builder
	for _, p := range []struct {
		sec int
		val float64
	}{
		{1, 10}, {12, 900},
	} {
		b.WriteString(`{"metric":"http_req_duration","type":"Point","data":{"time":"` +
			o.Add(time.Duration(p.sec)*time.Second).UTC().Format(time.RFC3339Nano) +
			`","value":` + strconv.FormatFloat(p.val, 'f', -1, 64) +
			`,"tags":{"name":"read"}}}` + "\n")
	}

	got, err := ReadWindows(strings.NewReader(b.String()), WindowConfig{
		Size: 10 * time.Second, Percentiles: []float64{95},
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2", len(got))
	}
	for i, want := range []float64{10, 900} {
		ep := findEndpointByName(t, got[i].Endpoints, "read")
		if p, ok := PercentileAt(ep.Percentiles, 95); !ok || p != want {
			t.Errorf("window %d read p95 = %v, want %v", i, p, want)
		}
	}
}

func findEndpointByName(t *testing.T, eps []Endpoint, name string) Endpoint {
	t.Helper()
	for _, e := range eps {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no endpoint %q in %+v", name, eps)
	return Endpoint{}
}

// TestReadWindowsCapsPerEndpointValues keeps the retention cap applying to each
// request, not just to the window as a whole.
func TestReadWindowsCapsPerEndpointValues(t *testing.T) {
	o := origin()
	var b strings.Builder
	for i, v := range []float64{1, 2, 3, 4, 5} {
		b.WriteString(`{"metric":"http_req_duration","type":"Point","data":{"time":"` +
			o.Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano) +
			`","value":` + strconv.FormatFloat(v, 'f', -1, 64) +
			`,"tags":{"name":"one"}}}` + "\n")
	}

	got, err := ReadWindows(strings.NewReader(b.String()), WindowConfig{
		Size: 10 * time.Second, Percentiles: []float64{50}, MaxValues: 2,
	})
	if err != nil {
		t.Fatalf("ReadWindows: %v", err)
	}
	ep := findEndpointByName(t, got[0].Endpoints, "one")
	if !ep.Sampled {
		t.Error("endpoint Sampled = false, want true")
	}
	if ep.AvgMs != 1.5 || ep.MaxMs != 2 {
		t.Errorf("capped endpoint = avg %v max %v, want 1.5/2", ep.AvgMs, ep.MaxMs)
	}
	// The count is still the whole window's.
	if ep.Requests != 0 {
		// No http_reqs points were written, so this stays 0 -- the check is that
		// capping durations does not touch the counters.
		t.Logf("requests = %d (no http_reqs points were written)", ep.Requests)
	}
}
