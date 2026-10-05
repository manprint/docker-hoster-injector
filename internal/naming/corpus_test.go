package naming

import (
	"strings"
	"testing"
)

// Real container names, collected from what Docker, Compose and Swarm
// actually produce, plus the shapes that break naive implementations.
var realNames = []string{
	// Plain single word, the common case.
	"nginx", "redis", "postgres", "grafana", "traefik",

	// Compose v1: <project>_<service>_<index>.
	"myproject_web_1", "myproject_db_1", "myproject_worker_1",
	"shop_frontend_1", "shop_backend_2",

	// Compose v2 with hyphens and dashes in the project name.
	"my-project_web_1", "my-project_worker-1",
	"a_b_c_d_e",

	// Compose with container_name override.
	"app", "api-server", "frontend_prod",

	// Kubernetes-in-Docker and other tooling.
	"k8s_POD_nginx_abc123", "kind-control-plane",

	// Leading and trailing separators.
	"-leading", "trailing-", "_leading", "trailing_",
	"__double__", "---triple---",

	// Mixed separators.
	"a-b_c", "a_b-c", "a--b", "a__b", "a-_-b",

	// Dots, which are legal in Docker names.
	"my.app", "app.v2", "a.b.c.d", ".leading-dot", "trailing-dot.",

	// Digits and digits-only.
	"1", "123", "9front", "web1", "1web", "web-1", "v2-api",

	// Uppercase, which Docker allows but DNS folds.
	"Nginx", "PROD_Database", "MyApp",

	// Long names, to exercise truncation.
	strings.Repeat("a", 63),
	strings.Repeat("a", 64),
	strings.Repeat("a", 200),
	strings.Repeat("a", 63) + "_1",
	strings.Repeat("very-long-project-name_", 5) + "service_1",

	// Characters that are legal in a Docker name but not in a host name.
	"with space", "tab\there", "hash#tag", "quote'name", "semi;colon",
	"back\\slash", "star*name", "question?name", "paren(name)",
	"bracket[name]", "at@name", "dollar$name", "percent%name",
	"amp&name", "plus+name", "equal=name", "comma,name",

	// Non-ASCII.
	"ünïcode", "日本語", "emoji😀name", "ñandú", "ΑΒΓ",

	// NUL and control characters.
	"null\x00byte", "bell\x07", "newline\nname", "cr\rname",
	"tab\tname", "vertical\vtab",

	// Only separators, which have no sanitisable content.
	"___", "---", "...", "-_-",

	// Empty and whitespace.
	"", " ", "\t", "\n", "   ",

	// Leading digit combinations that look like IP addresses.
	"127.0.0.1", "0.0.0.0", "10.0.0.5",

	// Resembles an existing hosts entry.
	"localhost", "ip6-localhost", "ip6-loopback",

	// Shell and SQL metacharacters, in case a name ever reaches a command.
	"$(id)", "`id`", "a;rm -rf /", "../../etc/passwd", "..",
}

func TestSanitizeNeverProducesAnUnusableName(t *testing.T) {
	t.Parallel()

	for _, name := range realNames {
		t.Run(describe(name), func(t *testing.T) {
			t.Parallel()

			got, ok := Sanitize(name)
			if !ok {
				// Rejecting is only acceptable when the name holds nothing
				// that can survive into a host name. Docker restricts
				// container names to [a-zA-Z0-9][a-zA-Z0-9_.-]*, so a
				// purely non-ASCII or purely separator name cannot reach
				// us in practice; rejecting it is the safe answer.
				if !hasASCIIAlnum(name) {
					return
				}
				t.Fatalf("Sanitize(%q) rejected a name that has usable ASCII content", name)
			}

			// Whatever we publish must satisfy every downstream gate.
			if !IsValidName(got) {
				t.Errorf("Sanitize(%q) = %q, which IsValidName rejects", name, got)
			}
			if !IsPublishable(got) {
				t.Errorf("Sanitize(%q) = %q, which IsPublishable rejects", name, got)
			}
			if len(got) > MaxNameLength {
				t.Errorf("Sanitize(%q) = %q, %d chars exceeds the %d limit",
					name, got, len(got), MaxNameLength)
			}
			for _, label := range strings.Split(got, ".") {
				if len(label) > MaxLabelLength {
					t.Errorf("Sanitize(%q) = %q, label %q is %d chars, limit %d",
						name, got, label, len(label), MaxLabelLength)
				}
				if strings.ContainsAny(label, " \t\r\n\v\f") {
					t.Errorf("Sanitize(%q) = %q, label %q contains whitespace", name, got, label)
				}
				if strings.Contains(label, "#") {
					t.Errorf("Sanitize(%q) = %q, label %q would start a comment", name, got, label)
				}
			}
			if strings.HasPrefix(got, ".") || strings.HasSuffix(got, ".") {
				t.Errorf("Sanitize(%q) = %q has a leading or trailing dot", name, got)
			}
			if strings.Contains(got, "..") {
				t.Errorf("Sanitize(%q) = %q has an empty label", name, got)
			}
		})
	}
}

// hasASCIIAlnum reports whether a name contains at least one ASCII letter or
// digit, which is the minimum for a sanitised form to carry any meaning.
func hasASCIIAlnum(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

func describe(name string) string {
	if name == "" {
		return "empty"
	}
	// Keep subtest names short and printable; the raw string is used for the
	// actual assertions.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 32 && r < 127:
			b.WriteRune(r)
		default:
			b.WriteString("\\x")
			b.WriteString(hexDigit(byte(r)))
		}
		if b.Len() > 40 {
			b.WriteString("...")
			return b.String()
		}
	}
	return b.String()
}

func hexDigit(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xf]})
}

// A sanitised name must be stable: the agent recomputes names on every
// resync, and an unstable result would make the hosts file flap.
func TestSanitizeIsStableForRealNames(t *testing.T) {
	t.Parallel()

	for _, name := range realNames {
		first, ok := Sanitize(name)
		if !ok {
			continue
		}
		for i := 0; i < 5; i++ {
			again, ok := Sanitize(name)
			if !ok || again != first {
				t.Fatalf("Sanitize(%q) is not stable: %q then %q", name, first, again)
			}
		}
		// Sanitising its own output must be a no-op, otherwise the second
		// resync would produce a different name than the first.
		second, ok := Sanitize(first)
		if !ok {
			t.Fatalf("Sanitize(%q) failed on already sanitised name %q", name, first)
		}
		if second != first {
			t.Errorf("Sanitize(%q) = %q but Sanitize(%q) = %q", name, first, first, second)
		}
	}
}

// Distinct container names must never collapse onto one host name, or the
// wrong container would answer for the other's traffic.
// Sanitisation cannot be made injective over Docker's whole name alphabet, and
// pretending otherwise would be worse than stating it.
//
// Docker permits [a-zA-Z0-9][a-zA-Z0-9_.-]*, RFC 1123 permits only
// [a-z0-9-]. The output alphabet is strictly smaller than the input, so by the
// pigeonhole principle some distinct names must collapse. "a__b" and "a--b"
// are the clearest example.
//
// What matters for correctness is not that every name is unique, but that no
// container can become unreachable: Variants always publishes the raw name
// alongside the sanitised one, and Docker guarantees raw names are unique.
// So every container keeps at least one name of its own, and any contested
// sanitised name is settled deterministically by Assign rather than by map
// iteration order.
//
// This test pins both halves of that argument.
func TestSanitizeCollisionsAreBoundedAndDocumented(t *testing.T) {
	t.Parallel()

	// Names that are expected to collapse, with the documented reason.
	known := map[string]string{
		"a__b": "a--b", // two underscores vs two hyphens: same output
		"a--b": "a--b",
		"a b":  "a-b", // Docker rejects spaces, so this cannot occur in practice
		"a-b":  "a-b",
	}
	for in, want := range known {
		got, ok := Sanitize(in)
		if !ok || got != want {
			t.Errorf("Sanitize(%q) = (%q, %t), want %q", in, got, ok, want)
		}
	}

	// The separator that actually occurs in Compose names must never
	// collide with the hyphen an operator would type by hand.
	mustDiffer := [][2]string{
		{"web_1", "web-1"},
		{"proj_web_1", "proj-web-1"},
		{"my_proj_db_1", "my-proj-db-1"},
		{"a_b", "a-b"},
		{"nginx_1", "nginx-1"},
	}
	for _, pair := range mustDiffer {
		a, _ := Sanitize(pair[0])
		b, _ := Sanitize(pair[1])
		if a == b {
			t.Errorf("Sanitize(%q) and Sanitize(%q) both give %q, but they name different containers", pair[0], pair[1], a)
		}
	}
}

// Every container must keep at least one published name that no other
// container can claim, because Docker container names are unique.
func TestEveryContainerRetainsAUniqueName(t *testing.T) {
	t.Parallel()

	// A realistic compose project with several services.
	containers := [][]string{
		{"myproject_web_1"},
		{"myproject_web_2"},
		{"myproject_db_1"},
		{"myproject_db_2"},
		{"myproject_worker_1"},
		{"myproject_worker_2"},
		{"standalone-nginx"},
		{"a_b"},
		{"a-b"},
		{"a__b"},
		{"a--b"},
	}

	claims := make([]Claim, 0, 32)

	for _, c := range containers {
		for _, raw := range c {
			if !IsPublishable(strings.ToLower(raw)) {
				t.Fatalf("container name %q is not publishable verbatim, so it could be orphaned", raw)
			}
			claims = append(claims, Claim{
				Name:     strings.ToLower(raw) + ".docker.local",
				Owner:    c[0],
				Priority: 0,
			})
			for _, v := range Variants(raw) {
				claims = append(claims, Claim{
					Name:     v.Name + ".docker.local",
					Owner:    c[0],
					Priority: 0,
				})
			}
		}
	}

	got := Assign(claims)

	// Every container must own at least one name outright.
	for _, c := range containers {
		owned := 0
		for name, claim := range got.Owners {
			if claim.Owner == c[0] && strings.HasSuffix(name, ".docker.local") {
				owned++
			}
		}
		if owned == 0 {
			t.Errorf("container %q owns no name at all; it would be unreachable", c[0])
		}
	}
}

// Whatever collision resolution decides, it must decide the same thing every
// time, or the hosts file would flap between restarts.
func TestCollisionResolutionIsStableAcrossRuns(t *testing.T) {
	t.Parallel()

	containers := [][]string{{"a__b"}, {"a--b"}, {"web_1"}, {"web-1"}}
	build := func() map[string]string {
		var claims []Claim
		for _, c := range containers {
			for _, raw := range c {
				claims = append(claims, Claim{Name: raw + ".docker.local", Owner: raw, Priority: 0})
				for _, v := range Variants(raw) {
					claims = append(claims, Claim{Name: v.Name + ".docker.local", Owner: raw, Priority: 0})
				}
			}
		}
		out := map[string]string{}
		for name, claim := range Assign(claims).Owners {
			out[name] = claim.Owner
		}
		return out
	}

	want := build()
	for i := 0; i < 200; i++ {
		got := build()
		if len(got) != len(want) {
			t.Fatalf("iteration %d: %d owners, want %d", i, len(got), len(want))
		}
		for name, owner := range want {
			if got[name] != owner {
				t.Fatalf("iteration %d: %q owned by %q, want %q", i, name, got[name], owner)
			}
		}
	}
}

// Every record that reaches the hosts file must be a legal hosts file line.
// This is the invariant that protects the host's name resolution.
func TestRecordsProduceWellFormedHostsLines(t *testing.T) {
	t.Parallel()

	const suffix = "docker.local"

	for _, name := range realNames {
		t.Run(describe(name), func(t *testing.T) {
			t.Parallel()

			records := Records(suffix, name)
			if len(records) == 0 {
				return
			}

			seen := make(map[string]struct{}, len(records))
			for _, fqdn := range records {
				if _, dup := seen[fqdn]; dup {
					t.Errorf("Records(%q) produced the duplicate %q", name, fqdn)
				}
				seen[fqdn] = struct{}{}

				host, ok := cutSuffix(fqdn, suffix)
				if !ok {
					t.Errorf("record %q does not end with .%s", fqdn, suffix)
					continue
				}
				if !IsPublishable(host) {
					t.Errorf("record %q has host part %q that IsPublishable rejects", fqdn, host)
				}
				// A record must never be able to inject a comment or a new
				// line into the hosts file.
				if strings.ContainsAny(fqdn, " \t\r\n\v\f#") {
					t.Errorf("record %q contains a character that breaks the hosts format", fqdn)
				}
			}
		})
	}
}

func cutSuffix(s, suffix string) (string, bool) {
	if !strings.HasSuffix(s, "."+suffix) {
		return "", false
	}
	return strings.TrimSuffix(s, "."+suffix), true
}

// The host part is what the resolver looks up, so it must never be a name the
// host already uses for something else. Docker allows a container to be called
// "localhost"; publishing that would be a footgun worth refusing outright.
func TestReservedHostNames(t *testing.T) {
	t.Parallel()

	reserved := []string{
		"localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback",
		"broadcasthost", "allhosts", "allnodes", "local",
	}

	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The sanitiser is not responsible for policy: these names must
			// pass through unchanged so the reconciler can recognise and
			// refuse them on the original value.
			got, ok := Sanitize(name)
			if !ok {
				t.Fatalf("Sanitize(%q) rejected a reserved name", name)
			}
			if got != name {
				t.Errorf("Sanitize(%q) = %q, reserved names must pass through unchanged", name, got)
			}
			for _, variant := range []string{name, strings.ToUpper(name), name + ".x"} {
				if !IsReserved(variant) {
					t.Errorf("IsReserved(%q) = false, want true", variant)
				}
			}
		})
	}
}

// Names that merely resemble a reserved one must still be publishable: the
// filter works on the whole name, not on a substring.
func TestReservedMatchingIsNotOverEager(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"localhost-proxy", "my-localhost", "localhosting",
		"locally", "notlocalhost", "mylocal",
		"ip6-localhost-2",
	}
	for _, name := range allowed {
		if IsReserved(name) {
			t.Errorf("IsReserved(%q) = true, want false", name)
		}
	}
}
