package naming

import (
	"reflect"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// Already valid.
		{"nginx", "nginx", true},
		{"web-1", "web-1", true},
		{"web1", "web1", true},
		{"a.b.c", "a.b.c", true},
		{"9front", "9front", true},

		// The compose case that motivated this package. The underscore
		// becomes a double hyphen so that "web_1" and "web-1" stay distinct.
		{"proj_db_1", "proj--db--1", true},
		{"my_project_web_1", "my--project--web--1", true},

		// Case folding.
		{"Nginx", "nginx", true},
		{"PROD_Database", "prod--database", true},

		// A run of underscores collapses to the two hyphens of the
		// replacement, a run of other separators to a single one.
		{"a__b", "a--b", true},
		{"a___b", "a--b", true},
		{"a_b", "a--b", true},
		{"a b", "a-b", true},
		{"a--b", "a--b", true}, // hyphens are preserved verbatim

		// Trimming.
		{"-lead", "lead", true},
		{"trail-", "trail", true},
		{"_under", "under", true},

		// Whitespace.
		{"  nginx  ", "nginx", true},
		{"with space", "with-space", true},

		// Unusable.
		{"", "", false},
		{"   ", "", false},
		{"___", "", false},
		{"...", "", false},
		{"-", "", false},
		{"a" + strings.Repeat("_", 100), "a", true},
	}

	for _, tc := range cases {
		got, ok := Sanitize(tc.in)
		if ok != tc.ok {
			t.Errorf("Sanitize(%q) ok = %t, want %t (got %q)", tc.in, ok, tc.ok, got)
			continue
		}
		if got != tc.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Whatever Sanitize accepts must be accepted by IsValidName, otherwise the
// agent would publish names the rest of the stack considers malformed.
func TestSanitizeOutputIsAlwaysValid(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"nginx", "proj_db_1", "_x", "x_", "A.B_C", "---", "a b c",
		"ünïcode", "tab\there", "nl\nhere", "x" + strings.Repeat("y", 200),
		strings.Repeat("z", 100) + "_1",
	}
	for _, in := range inputs {
		got, ok := Sanitize(in)
		if !ok {
			continue
		}
		if !IsValidName(got) {
			t.Errorf("Sanitize(%q) = %q which IsValidName rejects", in, got)
		}
		if len(got) > MaxNameLength {
			t.Errorf("Sanitize(%q) = %q exceeds %d chars", in, got, MaxNameLength)
		}
		for _, label := range strings.Split(got, ".") {
			if len(label) > MaxLabelLength {
				t.Errorf("Sanitize(%q) = %q has label %q of %d chars", in, got, label, len(label))
			}
		}
	}
}

func TestSanitizeTruncatesLongLabels(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 300)
	got, ok := Sanitize(long)
	if !ok {
		t.Fatal("expected a long label to survive")
	}
	if len(got) != MaxLabelLength {
		t.Errorf("len = %d, want %d", len(got), MaxLabelLength)
	}
	if !strings.HasPrefix(got, "aaaa") {
		t.Errorf("truncated name lost its prefix: %q", got)
	}
	if !IsValidName(got) {
		t.Errorf("truncated name %q is invalid", got)
	}
}

// Truncation must not merge two distinct long names into one, or the wrong
// container would win the record.
func TestTruncationKeepsNamesDistinct(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("a", 100)
	a, _ := Sanitize(base + "one")
	b, _ := Sanitize(base + "two")
	if a == b {
		t.Errorf("distinct long names collapsed to %q", a)
	}
	if len(a) != len(b) {
		t.Errorf("lengths differ: %d vs %d", len(a), len(b))
	}
}

func TestSanitizeIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"proj_db_1", "Nginx", "a__b", "-x-", "A.B"} {
		once, ok := Sanitize(in)
		if !ok {
			continue
		}
		twice, ok := Sanitize(once)
		if !ok {
			t.Errorf("Sanitize(%q) = %q but re-sanitising failed", in, once)
			continue
		}
		if once != twice {
			t.Errorf("Sanitize(%q) not idempotent: %q then %q", in, once, twice)
		}
	}
}

func TestIsValidName(t *testing.T) {
	t.Parallel()

	valid := []string{"nginx", "web-1", "a.b.c", "x1", "9lives", "a"}
	for _, n := range valid {
		if !IsValidName(n) {
			t.Errorf("IsValidName(%q) = false, want true", n)
		}
	}

	invalid := []string{
		"", ".", "..", ".a", "a.", "a..b", "-a", "a-",
		"a_b", "A", "a b", "héllo", strings.Repeat("a", 64),
		strings.Repeat("a", 64) + ".local",
		strings.Repeat("a.", 200),
	}
	for _, n := range invalid {
		if IsValidName(n) {
			t.Errorf("IsValidName(%q) = true, want false", n)
		}
	}
}

func TestVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		want  []Variant
	}{
		{
			name:  "simple name needs no variant",
			input: []string{"nginx"},
			want:  []Variant{{Name: "nginx"}},
		},
		{
			name:  "compose name gets a sanitised twin",
			input: []string{"proj_db_1"},
			want:  []Variant{{Name: "proj_db_1"}, {Name: "proj--db--1", Sanitized: true}},
		},
		{
			name:  "uppercase folds to a single entry",
			input: []string{"Nginx"},
			want:  []Variant{{Name: "nginx"}},
		},
		{
			name:  "duplicates collapse",
			input: []string{"nginx", "NGINX", " nginx "},
			want:  []Variant{{Name: "nginx"}},
		},
		{
			name:  "blank entries are ignored",
			input: []string{"nginx", "", "   "},
			want:  []Variant{{Name: "nginx"}},
		},
		{
			name:  "nothing usable yields nothing",
			input: []string{"", "   ", "\t\n"},
			want:  []Variant{},
		},
		{
			name:  "several names keep their order",
			input: []string{"web", "db", "cache"},
			want:  []Variant{{Name: "web"}, {Name: "db"}, {Name: "cache"}},
		},
		{
			name:  "a sanitised twin never precedes its source",
			input: []string{"a_b", "a-b"},
			want:  []Variant{{Name: "a_b"}, {Name: "a--b", Sanitized: true}, {Name: "a-b"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Variants(tc.input...)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Variants(%q) =\n  %+v\nwant\n  %+v", tc.input, got, tc.want)
			}
		})
	}
}

// The same name arriving from both the container name and a network alias
// must not produce two identical records in the hosts file.
func TestVariantsDedupesAcrossSources(t *testing.T) {
	t.Parallel()

	got := Variants("proj_db_1", "proj_db_1", "proj--db--1", "PROJ_DB_1")
	want := []Variant{
		{Name: "proj_db_1"},
		{Name: "proj--db--1", Sanitized: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Variants = %+v, want %+v", got, want)
	}
}

// IsPublishable is the gate for writing a name verbatim into the hosts file,
// so it must accept exactly the names a resolver can look up there.
func TestIsPublishable(t *testing.T) {
	t.Parallel()

	valid := []string{
		"nginx",
		"proj_db_1", // underscore: exact match in a hosts file
		"web-1",
		"a.b.c",
		"x_", // trailing underscore is fine
		"_x", // leading underscore is fine
		"-x", // leading hyphen is fine
		"x-", // trailing hyphen is fine
		"9lives",
		"a1_b2.c3_d4",
		"MixedCase_1", // hosts file lookups are case insensitive
	}
	for _, n := range valid {
		if !IsPublishable(n) {
			t.Errorf("IsPublishable(%q) = false, want true", n)
		}
	}

	invalid := []string{
		"", " ", "a b", "#comment", "a#b", "a\tb", "a\nb",
		".lead", "trail.", "a..b",
		"héllo", "a\x00b",
		strings.Repeat("a", 64),   // single label over 63 chars
		strings.Repeat("a.", 200), // 400 chars, over the 253 limit
	}
	for _, n := range invalid {
		if IsPublishable(n) {
			t.Errorf("IsPublishable(%q) = true, want false", n)
		}
	}
}

// A name accepted by IsPublishable must round-trip through Sanitize into an
// IsValidName, which is the invariant that keeps the two predicates aligned.
func TestPublishableImpliesValidAfterSanitize(t *testing.T) {
	t.Parallel()

	for _, n := range []string{"proj_db_1", "web-1", "a.b.c", "x_", "9lives"} {
		if !IsPublishable(n) {
			t.Fatalf("precondition: IsPublishable(%q) = false", n)
		}
		s, ok := Sanitize(n)
		if !ok {
			t.Errorf("Sanitize(%q) failed", n)
			continue
		}
		if !IsValidName(s) {
			t.Errorf("Sanitize(%q) = %q which IsValidName rejects", n, s)
		}
	}
}

func TestFQDN(t *testing.T) {
	t.Parallel()

	got, ok := FQDN("nginx", "docker.local")
	if !ok || got != "nginx.docker.local" {
		t.Errorf("FQDN = (%q, %t), want (nginx.docker.local, true)", got, ok)
	}
	if _, ok := FQDN("", "docker.local"); ok {
		t.Error("empty name must be rejected")
	}
	if _, ok := FQDN("nginx", ""); ok {
		t.Error("empty suffix must be rejected")
	}

	// A name that would overflow the 253 character limit must be refused
	// rather than published truncated, since we cannot answer for it.
	long := strings.Repeat("a", 60)
	full := strings.TrimSuffix(long, "") + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".local"
	if _, ok := FQDN(full, "local"); ok {
		t.Errorf("FQDN accepted a %d character name", len(full))
	}
}

func TestRecords(t *testing.T) {
	t.Parallel()

	got := Records("docker.local", "proj_db_1", "db", "")
	want := []string{
		"proj_db_1.docker.local",
		"proj--db--1.docker.local",
		"db.docker.local",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Records =\n  %q\nwant\n  %q", got, want)
	}

	if got := Records("docker.local"); len(got) != 0 {
		t.Errorf("Records with no names = %q, want empty", got)
	}
	// A name that is only whitespace is unusable and must be dropped.
	if got := Records("docker.local", "  ", "\t"); len(got) != 0 {
		t.Errorf("Records with blank name = %q, want empty", got)
	}
	// A blank name next to a good one must not suppress the good one.
	if got := Records("docker.local", "", "nginx", "  "); len(got) != 1 || got[0] != "nginx.docker.local" {
		t.Errorf("Records with mixed names = %q, want [nginx.docker.local]", got)
	}
}

// The output of Records is what gets written to the hosts file, so it must be
// stable: the same input always yields the same slice, which is what makes the
// writer idempotent and lets it detect "nothing changed".
func TestRecordsIsDeterministic(t *testing.T) {
	t.Parallel()

	names := []string{"proj_web_1", "web", "cache_1", "Z", "a_b_c"}
	first := Records("docker.local", names...)
	for i := 0; i < 50; i++ {
		again := Records("docker.local", names...)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("iteration %d differs:\n  %q\n  %q", i, first, again)
		}
	}
}

func TestAssign(t *testing.T) {
	t.Parallel()

	got := Assign([]Claim{
		{Name: "a.docker.local", Owner: "bbb", Priority: 10},
		{Name: "a.docker.local", Owner: "aaa", Priority: 10},
		{Name: "a.docker.local", Owner: "ccc", Priority: 1},
		{Name: "b.docker.local", Owner: "ddd", Priority: 5},
	})

	if len(got.Owners) != 2 {
		t.Fatalf("got %d owners, want 2: %+v", len(got.Owners), got.Owners)
	}
	// Lowest priority number wins, regardless of owner id.
	if w := got.Owners["a.docker.local"]; w.Owner != "ccc" {
		t.Errorf("a.docker.local won by %q, want ccc (lowest priority)", w.Owner)
	}
	if w := got.Owners["b.docker.local"]; w.Owner != "ddd" {
		t.Errorf("b.docker.local won by %q, want ddd", w.Owner)
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(got.Conflicts), got.Conflicts)
	}
	c := got.Conflicts[0]
	if c.Name != "a.docker.local" || c.Winner.Owner != "ccc" || len(c.Losers) != 2 {
		t.Errorf("unexpected conflict: %+v", c)
	}
}

// Two containers created in the same second must always resolve the same way,
// otherwise the hosts file would flap across restarts.
func TestAssignTieBreakIsStable(t *testing.T) {
	t.Parallel()

	claims := []Claim{
		{Name: "x.docker.local", Owner: "zzz", Priority: 1},
		{Name: "x.docker.local", Owner: "aaa", Priority: 1},
		{Name: "x.docker.local", Owner: "mmm", Priority: 1},
	}
	want := "aaa"
	for i := 0; i < 100; i++ {
		got := Assign(claims)
		if w := got.Owners["x.docker.local"]; w.Owner != want {
			t.Fatalf("iteration %d: winner %q, want %q", i, w.Owner, want)
		}
	}
}

func TestAssignInputIsNotMutated(t *testing.T) {
	t.Parallel()

	// The reconciler may reuse the claim slice, so Assign must not sort it
	// in place.
	claims := []Claim{
		{Name: "b.docker.local", Owner: "b", Priority: 1},
		{Name: "a.docker.local", Owner: "a", Priority: 1},
	}
	snapshot := append([]Claim(nil), claims...)
	Assign(claims)
	for i := range claims {
		if claims[i] != snapshot[i] {
			t.Fatalf("Assign mutated its input at %d: %+v vs %+v", i, claims[i], snapshot[i])
		}
	}
}

func TestAssignIgnoresIncompleteClaims(t *testing.T) {
	t.Parallel()

	got := Assign([]Claim{
		{Name: "", Owner: "a"},
		{Name: "a.docker.local", Owner: ""},
		{Name: "b.docker.local", Owner: "b"},
	})
	if len(got.Owners) != 1 {
		t.Errorf("got %d owners, want 1: %+v", len(got.Owners), got.Owners)
	}
	if _, ok := got.Owners["b.docker.local"]; !ok {
		t.Error("the valid claim was dropped")
	}
	if len(got.Conflicts) != 0 {
		t.Errorf("got %d conflicts, want 0", len(got.Conflicts))
	}
}

func TestAssignConflictsAreSorted(t *testing.T) {
	t.Parallel()

	got := Assign([]Claim{
		{Name: "z.docker.local", Owner: "o1", Priority: 1},
		{Name: "z.docker.local", Owner: "o2", Priority: 1},
		{Name: "a.docker.local", Owner: "o3", Priority: 1},
		{Name: "a.docker.local", Owner: "o4", Priority: 1},
	})
	if len(got.Conflicts) != 2 {
		t.Fatalf("got %d conflicts, want 2", len(got.Conflicts))
	}
	if got.Conflicts[0].Name != "a.docker.local" || got.Conflicts[1].Name != "z.docker.local" {
		t.Errorf("conflicts not sorted: %q, %q", got.Conflicts[0].Name, got.Conflicts[1].Name)
	}
}

func TestAssignEmpty(t *testing.T) {
	t.Parallel()

	got := Assign(nil)
	if got.Owners == nil {
		t.Error("Owners must be non-nil so callers can read it safely")
	}
	if len(got.Owners) != 0 || len(got.Conflicts) != 0 {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestDedupe(t *testing.T) {
	t.Parallel()

	got := Dedupe([]string{"a", "b", "a", "", "  ", " c ", "c"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Dedupe = %q, want %q", got, want)
	}
	if got := Dedupe(nil); got != nil {
		t.Errorf("Dedupe(nil) = %q, want nil", got)
	}
}

// BenchmarkSanitize guards against an accidental O(n^2) in the hot path,
// which runs for every container on every resync.
func BenchmarkSanitize(b *testing.B) {
	names := []string{"proj_web_1", "nginx", "a.b.c", "cache", "UPPER_CASE_NAME"}
	b.ReportAllocs()
	for b.Loop() {
		for _, n := range names {
			_, _ = Sanitize(n)
		}
	}
}
