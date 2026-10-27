package resourceadmission

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParsePlacement(t *testing.T) {
	cases := []struct {
		name   string
		data   string
		want   placement
		reason Reason
	}{
		{"unified", "0::/system.slice/job.scope\n", placement{}, ""},
		{
			"hybrid memory and cpu in v1", "12:memory:/x\n4:cpu,cpuacct:/x\n0::/y\n",
			placement{memoryOutside: true, cpuOutside: true},
			"",
		},
		{"hybrid cpuset only", "3:cpuset:/x\n1:name=systemd:/x\n0::/y\n", placement{cpuOutside: true}, ""},
		{"v1 only", "12:memory:/x\n3:cpu:/x\n", placement{}, ReasonCgroupUnsupported},
		{"empty", "", placement{}, ReasonCgroupUnsupported},
		{"removed cgroup", "0::/job.scope (deleted)\n", placement{deleted: true}, ""},
	}
	for _, tc := range cases {
		got, reason := parsePlacement(tc.data)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%s: got %+v %q, want %+v %q", tc.name, got, reason, tc.want, tc.reason)
		}
	}
}

func TestChain(t *testing.T) {
	if got, complete := chain("m/a/b/c", "m", 10); !complete || !reflect.DeepEqual(got, []string{"m/a/b/c", "m/a/b", "m/a", "m"}) {
		t.Fatalf("chain = %v %v", got, complete)
	}
	if got, complete := chain("m", "m", 10); !complete || !reflect.DeepEqual(got, []string{"m"}) {
		t.Fatalf("chain = %v %v", got, complete)
	}
}

func TestChainStopsAtTheLimitWithoutBuildingMore(t *testing.T) {
	deep := "m" + strings.Repeat("/x", 1000)
	for _, tc := range []struct {
		name     string
		limit    int
		complete bool
	}{{"exactly the levels needed", 1001, true}, {"one level short", 1000, false}, {"tiny limit", 3, false}} {
		got, complete := chain(deep, "m", tc.limit)
		if complete != tc.complete || len(got) > tc.limit {
			t.Errorf("%s: %d levels complete %v, want complete %v within %d", tc.name, len(got), complete, tc.complete, tc.limit)
		}
	}
}

func TestParseLimit(t *testing.T) {
	if got, err := parseLimit("max\n"); err != nil || got.set {
		t.Fatalf("max = %+v %v, want unset", got, err)
	}
	if got, err := parseLimit("1048576\n"); err != nil || got != (limit{bytes: 1 << 20, set: true}) {
		t.Fatalf("number = %+v %v", got, err)
	}
	if _, err := parseLimit("lots"); !errors.Is(err, errMalformed) {
		t.Fatalf("err = %v, want malformed", err)
	}
}

func TestKeyedValue(t *testing.T) {
	data := "anon 100\ninactive_file 4096\nactive_file 8192\n"
	if got, err := keyedValue(data, "inactive_file"); err != nil || got != 4096 {
		t.Fatalf("inactive_file = %d %v", got, err)
	}
	if _, err := keyedValue(data, "slab"); !errors.Is(err, errMalformed) {
		t.Fatalf("missing key err = %v, want malformed", err)
	}
	if _, err := keyedValue("inactive_file x\n", "inactive_file"); !errors.Is(err, errMalformed) {
		t.Fatalf("bad number err = %v, want malformed", err)
	}
}

func TestParseCPUMax(t *testing.T) {
	cases := []struct {
		data string
		cpus float64
		has  bool
		bad  bool
	}{
		{"max 100000\n", 0, false, false},
		{"150000 100000\n", 1.5, true, false},
		{"50000 100000", 0.5, true, false},
		{"max\n", 0, false, true},
		{"100 0\n", 0, false, true},
		{"x 100000\n", 0, false, true},
	}
	for _, tc := range cases {
		cpus, has, err := parseCPUMax(tc.data)
		if cpus != tc.cpus || has != tc.has || (err != nil) != tc.bad {
			t.Errorf("%q: got %v %v %v", tc.data, cpus, has, err)
		}
	}
}

func TestParseCPUSet(t *testing.T) {
	good := map[string]int{"0\n": 1, "0-3\n": 4, "0-3,8,10-11\n": 7, "5-5": 1}
	for data, want := range good {
		if got, err := parseCPUSet(data); err != nil || got != want {
			t.Errorf("%q = %d %v, want %d", data, got, err, want)
		}
	}
	for _, data := range []string{"", "\n", "a", "3-1", "1-", "-1", "0,,2"} {
		if _, err := parseCPUSet(data); !errors.Is(err, errMalformed) {
			t.Errorf("%q err = %v, want malformed", data, err)
		}
	}
}

func TestParsePSISomeTotal(t *testing.T) {
	data := "some avg10=0.00 avg60=0.10 avg300=0.00 total=2499628\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=7\n"
	if got, err := parsePSISomeTotal(data); err != nil || got != 2499628 {
		t.Fatalf("total = %d %v, want the some line", got, err)
	}
	for _, bad := range []string{"", "full avg10=0 total=1\n", "some avg10=0\n", "some total=x\n"} {
		if _, err := parsePSISomeTotal(bad); !errors.Is(err, errMalformed) {
			t.Errorf("%q err = %v, want malformed", bad, err)
		}
	}
}
