package version

import "testing"

func TestCompareIsNumericNotLexicographic(t *testing.T) {
	t.Parallel()

	// The case that a string comparison gets wrong, and that matters because
	// it is the difference between "ancient daemon" and "supported daemon".
	if Compare("1.9", "1.44") >= 0 {
		t.Error(`Compare("1.9", "1.44") >= 0, want negative: 1.9 is older than 1.44`)
	}
	if Compare("1.44", "1.9") <= 0 {
		t.Error(`Compare("1.44", "1.9") <= 0, want positive`)
	}
	if Compare("1.20", "1.3") <= 0 {
		t.Error(`Compare("1.20", "1.3") <= 0, want positive`)
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"1.44", "1.44", 0},
		{"1.44", "1.44.0", 0}, // missing parts are zeroes
		{"1.44.0", "1.44", 0},
		{"1.44", "1.45", -1},
		{"1.45", "1.44", 1},
		{"1.44", "1.44.1", -1},
		{"2.0", "1.99", 1},
		{"1.44", "1.9", 1},
		{"1.24", "1.44", -1},

		// Pre-release suffixes must not break the comparison.
		{"1.44.0-rc.1", "1.44", 0},
		{"1.44.0-rc.1", "1.44.0", 0},
		{"1.45.0-beta", "1.44.0", 1},
		{"v1.44.0", "1.44.0", 0},

		// Junk must not panic and must not pretend to be newer.
		{"", "1.44", -1},
		{"garbage", "1.44", -1},
		{"1.44", "", 1},
	}

	for _, tc := range cases {
		got := Compare(tc.a, tc.b)
		if sign(got) != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// minAPIVersion mirrors dockerclient.MinAPIVersion. It is duplicated here
// deliberately: this package must not import the Docker client, and a test
// that cross-checks the two keeps the floor from drifting apart silently.
const minAPIVersion = "1.44"

// AtLeast is the check the agent actually performs, so it gets its own table.
func TestAtLeast(t *testing.T) {
	t.Parallel()

	cases := []struct {
		version string
		want    bool
	}{
		// The supported floor, Docker 25.
		{"1.44", true},
		{"1.44.0", true},
		{"1.45", true},
		{"1.51", true},
		{"2.0", true},

		// Below the floor: rejected even though some are lexicographically
		// larger.
		{"1.43", false},
		{"1.24", false},
		{"1.9", false},
		{"1.4", false},
		{"", false},
		{"nonsense", false},
	}

	for _, tc := range cases {
		if got := AtLeast(tc.version, minAPIVersion); got != tc.want {
			t.Errorf("AtLeast(%q, %q) = %t, want %t", tc.version, minAPIVersion, got, tc.want)
		}
	}
}

// The floor must stay put: raising it silently would lock out users, and
// lowering it would promise support that was never tested.
func TestFloorIsDocker25(t *testing.T) {
	t.Parallel()

	if minAPIVersion != "1.44" {
		t.Errorf("floor = %q, want 1.44 (Docker 25)", minAPIVersion)
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"1.44":        "1.44.0",
		"1.44.0":      "1.44.0",
		"1.51.3":      "1.51.3",
		"v1.44.0":     "1.44.0",
		"1.44.0-rc.1": "1.44.0",
		"":            "0.0.0",
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

// Compare must not be fooled by a version with extra components.
func TestCompareWithExtraComponents(t *testing.T) {
	t.Parallel()

	if Compare("1.44.0.1", "1.44.0.2") != 0 {
		t.Error("a fourth component must be ignored")
	}
	if Compare("1.44.0.2", "1.44") != 0 {
		t.Error("a fourth component must not make a version newer")
	}
}
