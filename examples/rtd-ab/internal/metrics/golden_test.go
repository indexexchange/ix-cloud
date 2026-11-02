// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestInheritedMetricNamesMatchGolden compares the metric families the registry
// exposes against testdata/inherited_names.golden, captured from the
// rtd-simple image (every # TYPE line). Dashboards
// import these names, so a rename blanks panels. The cgroup-backed families are
// only emitted when the host exposes cgroup files, so they are checked
// conditionally here and exactly by the harness inside the container.
func TestInheritedMetricNamesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/inherited_names.golden")
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(l)
		golden[f[0]] = f[1]
	}

	reg := New("test")
	reg.ObserveRPC("RTBExtensionPoint", "GetMutations", "0", "", 0.0004)
	reg.SetModel(true, "m")
	var buf bytes.Buffer
	reg.write(&buf)

	got := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) (\S+)$`).FindAllStringSubmatch(buf.String(), -1) {
		got[m[1]] = m[2]
	}
	cgroupBacked := map[string]bool{"rtd_simple_cpu_seconds_total": true, "rtd_simple_cpu_limit_cores": true,
		"rtd_simple_memory_bytes": true, "rtd_simple_memory_limit_bytes": true, "rtd_simple_memory_percent": true}
	var names []string
	for n := range golden {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		typ, ok := got[n]
		switch {
		case ok && typ != golden[n]:
			t.Errorf("%s type %s, golden %s", n, typ, golden[n])
		case !ok && !cgroupBacked[n]:
			t.Errorf("inherited metric %s missing", n)
		}
	}
	for n := range got {
		if _, ok := golden[n]; !ok {
			t.Errorf("unexpected family %s not in the golden", n)
		}
	}
}

// TestRPCServerDurationContract keeps rpc_server_duration a seconds histogram
// with the required buckets and no extra labels (no arm label).
func TestRPCServerDurationContract(t *testing.T) {
	reg := New("test")
	reg.ObserveRPC("RTBExtensionPoint", "GetMutations", "0", "", 0.0004)
	var buf bytes.Buffer
	reg.write(&buf)
	body := buf.String()
	for _, le := range []string{"0.001", "0.002", "0.003", "0.004", "0.005", "0.007", "0.010", "+Inf"} {
		if !strings.Contains(body, `le="`+le+`"`) {
			t.Errorf("bucket %s missing", le)
		}
	}
	re := regexp.MustCompile(`rpc_server_duration_bucket\{([^}]*)\}`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		for _, kv := range strings.Split(m[1], ",") {
			k := kv[:strings.Index(kv, "=")]
			switch k {
			case "rpc_system", "rpc_grpc_status_code", "rpc_method", "rpc_service", "error_type", "le":
			default:
				t.Errorf("unexpected label %q on rpc_server_duration", k)
			}
		}
	}
	if !strings.Contains(body, "# TYPE rpc_server_duration histogram") {
		t.Error("rpc_server_duration must be a histogram")
	}
}
