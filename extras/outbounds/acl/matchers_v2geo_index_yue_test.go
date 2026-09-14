package acl

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/apernet/hysteria/extras/v2/outbounds/acl/v2geo"
)

// The upstream geosite matcher answers every connection with a linear scan
// over the whole category. The Yue fleet ACL references exactly one category,
// `geosite:category-ads-all`, which carries 189,166 entries — and 100% of them
// are Domain_RootDomain, so a connection to any non-ad host walked the entire
// list before the rule could be declined. In the merged 2026-09-08 fleet CPU
// profile (16 live node profiles, both roles) `geositeMatcher.Match` was the
// largest single application-level function: 2.27% cum / 1.37% flat.
//
// The index patch in matchers_v2geo.go replaces the scan. Indexing is only safe
// if it is *provably* the same predicate: this ACL decides reject/direct, and a
// single disagreement is either "something we must block got through" or "a
// paying user was blocked". So the tests below do not spot-check — they
// re-implement upstream's semantics independently and differential-test the
// two over a corpus built to attack exactly the boundaries an index gets wrong
// (label boundaries, substring-but-not-suffix, empty values, trailing dots,
// case).

// referenceMatch is upstream's pre-patch algorithm, transcribed from
// matchers_v2geo.go as it read before the index was introduced. It is
// deliberately the naive version: it is the oracle, not the fast path.
//
// The engine normalizes the host before any matcher sees it
// (compiledRuleSetImpl.Match: `strings.TrimRight(strings.ToLower(name), ".")`),
// so the oracle has to start from the same string or it compares two different
// questions.
func referenceMatch(site *v2geo.GeoSite, attrs []string, rawHost string) bool {
	host := strings.TrimRight(strings.ToLower(rawHost), ".")
	attrsAllow := func(d *v2geo.Domain) bool {
		if len(attrs) == 0 {
			return true
		}
		have := map[string]bool{}
		for _, a := range d.Attribute {
			have[a.Key] = true
		}
		if len(have) == 0 {
			return false
		}
		for _, want := range attrs {
			if !have[want] {
				return false
			}
		}
		return true
	}
	for _, d := range site.Domain {
		if !attrsAllow(d) {
			continue
		}
		switch d.Type {
		case v2geo.Domain_Plain:
			if strings.Contains(host, d.Value) {
				return true
			}
		case v2geo.Domain_Regex:
			re, err := regexp.Compile(d.Value)
			if err == nil && re.MatchString(host) {
				return true
			}
		case v2geo.Domain_Full:
			if host == d.Value {
				return true
			}
		case v2geo.Domain_RootDomain:
			if host == d.Value || strings.HasSuffix(host, "."+d.Value) {
				return true
			}
		}
	}
	return false
}

type stubGeoSite struct{ sites map[string]*v2geo.GeoSite }

func (stubGeoSite) LoadGeoIP() (map[string]*v2geo.GeoIP, error) {
	return map[string]*v2geo.GeoIP{}, nil
}

func (s stubGeoSite) LoadGeoSite() (map[string]*v2geo.GeoSite, error) { return s.sites, nil }

func indexProbeAttr(keys ...string) []*v2geo.Domain_Attribute {
	out := make([]*v2geo.Domain_Attribute, 0, len(keys))
	for _, k := range keys {
		out = append(out, &v2geo.Domain_Attribute{Key: k})
	}
	return out
}

// indexProbeSite mixes every domain type, attributed and unattributed entries,
// and values chosen so that a wrong index produces a visible disagreement:
// "example.com" vs "notexample.com", a value that is a substring but not a
// suffix, an empty value, and a value with a trailing dot.
func indexProbeSite() *v2geo.GeoSite {
	d := func(t v2geo.Domain_Type, v string, a ...string) *v2geo.Domain {
		return &v2geo.Domain{Type: t, Value: v, Attribute: indexProbeAttr(a...)}
	}
	return &v2geo.GeoSite{CountryCode: "probe", Domain: []*v2geo.Domain{
		d(v2geo.Domain_RootDomain, "example.com"),
		d(v2geo.Domain_RootDomain, "com"),
		d(v2geo.Domain_RootDomain, "internal"),
		d(v2geo.Domain_RootDomain, "google.internal"),
		d(v2geo.Domain_RootDomain, ""),
		d(v2geo.Domain_RootDomain, "example.com."),
		d(v2geo.Domain_RootDomain, "ads.example", "ads"),
		d(v2geo.Domain_RootDomain, "beacon.example", "ads", "tracker"),
		d(v2geo.Domain_Full, "exact.example.org"),
		d(v2geo.Domain_Full, ""),
		d(v2geo.Domain_Full, "full-only.example", "ads"),
		d(v2geo.Domain_Plain, "trackme"),
		d(v2geo.Domain_Plain, "xample"),
		d(v2geo.Domain_Regex, `^ad[0-9]+\.probe\.example$`),
		d(v2geo.Domain_Regex, `\.doubleclick\.`),
	}}
}

// indexProbeHosts is the differential corpus. Every entry exists to make some
// plausible-but-wrong index disagree with the oracle.
func indexProbeHosts() []string {
	return []string{
		"", ".", "..", "a", "com", "example.com", "www.example.com",
		"a.b.c.example.com", "notexample.com", "example.com.evil.net",
		".example.com", "example.com.", "EXAMPLE.COM", "xample.com",
		"e.com", "..com", "a..com", "metadata.google.internal",
		"google.internal", "internal", "myinternal", "x.internal",
		"exact.example.org", "www.exact.example.org", "exact.example.org.uk",
		"full-only.example", "sub.full-only.example",
		"ads.example", "sub.ads.example", "notads.example",
		"beacon.example", "deep.sub.beacon.example",
		"trackme.net", "www.trackme.net", "please-trackme-now.org",
		"ad1.probe.example", "ad.probe.example", "ad12345.probe.example",
		"xad1.probe.example", "ad1.probe.example.evil",
		"static.doubleclick.net", "doubleclick.net", "a.doubleclick.example",
		"tld", "a.tld", "a.b.tld", strings.Repeat("a.", 40) + "example.com",
		"probe", "probe.example", "1.2.3.4",
	}
}

func compileIndexProbeACL(t testing.TB, rule string, site *v2geo.GeoSite) CompiledRuleSet[string] {
	t.Helper()
	rules, err := ParseTextRules("reject(" + rule + ")\ndirect(all)\n")
	if err != nil {
		t.Fatalf("probe ACL does not parse: %v", err)
	}
	// Cache size 1 so consecutive probes cannot be answered from the LRU: the
	// matcher itself has to decide every host, which is the code under test.
	set, err := Compile[string](rules,
		map[string]string{"reject": "reject", "direct": "direct"}, 1,
		stubGeoSite{sites: map[string]*v2geo.GeoSite{"probe": site}})
	if err != nil {
		t.Fatalf("probe ACL does not compile: %v", err)
	}
	return set
}

// TestGeositeIndexMatchesLinearScan is the load-bearing test for the index
// patch: the indexed matcher must be the same predicate as the scan it
// replaced, including for the attribute-gated form.
func TestGeositeIndexMatchesLinearScan(t *testing.T) {
	site := indexProbeSite()
	for _, tc := range []struct {
		rule  string
		attrs []string
	}{
		{"geosite:probe", nil},
		{"geosite:probe@ads", []string{"ads"}},
		{"geosite:probe@ads@tracker", []string{"ads", "tracker"}},
		{"geosite:probe@absent", []string{"absent"}},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			set := compileIndexProbeACL(t, tc.rule, site)
			var matched int
			for _, host := range indexProbeHosts() {
				got, _ := set.Match(HostInfo{Name: host}, ProtocolTCP, 443)
				want := "direct"
				if referenceMatch(site, tc.attrs, host) {
					want = "reject"
					matched++
				}
				if got != want {
					t.Errorf("host %q: indexed matcher said %q, linear scan says %q",
						host, got, want)
				}
			}
			// A corpus that never matches would make this test vacuously green
			// for any broken index that always returns false.
			if tc.rule != "geosite:probe@absent" && matched == 0 {
				t.Fatalf("%s matched nothing; the differential test proves nothing", tc.rule)
			}
			if tc.rule == "geosite:probe@absent" && matched != 0 {
				t.Fatalf("an attribute no entry carries must match nothing, got %d", matched)
			}
		})
	}
}

// bigRootSite mirrors the real fleet category: many entries, all RootDomain.
func bigRootSite(n int) *v2geo.GeoSite {
	domains := make([]*v2geo.Domain, 0, n)
	for i := range n {
		domains = append(domains, &v2geo.Domain{
			Type:  v2geo.Domain_RootDomain,
			Value: fmt.Sprintf("ad-%06d.tracker.example", i),
		})
	}
	return &v2geo.GeoSite{CountryCode: "probe", Domain: domains}
}

// TestGeositeIndexMatchesLinearScanAtFleetScale repeats the differential check
// against a category the size of the one production actually loads, because the
// index only engages meaningfully there.
func TestGeositeIndexMatchesLinearScanAtFleetScale(t *testing.T) {
	site := bigRootSite(20000)
	set := compileIndexProbeACL(t, "geosite:probe", site)
	hosts := append(indexProbeHosts(),
		"ad-000000.tracker.example",
		"sub.ad-019999.tracker.example",
		"ad-020000.tracker.example",
		"tracker.example",
		"xad-000001.tracker.example",
		"ad-000001.tracker.example.evil.net",
	)
	var matched int
	for _, host := range hosts {
		got, _ := set.Match(HostInfo{Name: host}, ProtocolTCP, 443)
		want := "direct"
		if referenceMatch(site, nil, host) {
			want = "reject"
			matched++
		}
		if got != want {
			t.Errorf("host %q: indexed matcher said %q, linear scan says %q", host, got, want)
		}
	}
	if matched == 0 {
		t.Fatal("fleet-scale corpus matched nothing; the test proves nothing")
	}
}

// BenchmarkGeositeMissFleetScale measures the case production spends its time
// in: a host that matches nothing, against a category the size of
// category-ads-all. Before the patch this walked every entry.
func BenchmarkGeositeMissFleetScale(b *testing.B) {
	site := bigRootSite(189166)
	set := compileIndexProbeACL(b, "geosite:probe", site)
	host := HostInfo{Name: "www.example.com"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got, _ := set.Match(host, ProtocolTCP, 443); got != "direct" {
			b.Fatalf("unexpected verdict %q", got)
		}
	}
}

// BenchmarkGeositeMissFleetScaleLinear is the same workload through the oracle,
// i.e. what the fleet ran before this patch. Keeping it next to the indexed
// benchmark is what makes the speed-up a measurement rather than a claim.
func BenchmarkGeositeMissFleetScaleLinear(b *testing.B) {
	site := bigRootSite(189166)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if referenceMatch(site, nil, "www.example.com") {
			b.Fatal("unexpected match")
		}
	}
}

// TestGeositeIndexDoesNotCostMemory guards the other half of the trade. The
// fleet containers run under a hard mem_limit on 2 GiB hosts, so an index that
// bought CPU with resident memory would be a bad deal made silently.
//
// It is not a wash: it is negative. Upstream's newGeositeMatcher calls
// domainAttributeToMap for *every* entry, so a 189,166-entry category holds
// 189,166 separate map[string]bool allocations plus the 40-byte-per-entry
// slice. Releasing them costs more than the two index maps.
func TestGeositeIndexDoesNotCostMemory(t *testing.T) {
	site := bigRootSite(189166)
	set := compileIndexProbeACL(t, "geosite:probe", site)

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	if got, _ := set.Match(HostInfo{Name: "www.example.com"}, ProtocolTCP, 443); got != "direct" {
		t.Fatalf("verdict %q", got)
	}
	after := heap()
	runtime.KeepAlive(set)

	deltaMiB := (float64(after) - float64(before)) / (1 << 20)
	t.Logf("189,166-entry category: heap %.1f MiB -> %.1f MiB (%+.1f MiB)",
		float64(before)/(1<<20), float64(after)/(1<<20), deltaMiB)
	if deltaMiB > 1.0 {
		t.Fatalf("building the index grew the heap by %+.1f MiB; it is supposed to "+
			"release more than it allocates. Check that buildIndex still clears "+
			"m.Domains — holding both representations is the likely cause.", deltaMiB)
	}
}

// TestGeositeIndexIsRaceFreeUnderConcurrentFirstMatch: the index is built
// lazily under sync.Once and buildIndex clears m.Domains. In production the ACL
// is consulted concurrently by every connection, so the *first* Match is itself
// concurrent; any read or write of m.Domains / m.root outside the Once is what
// -race catches here.
func TestGeositeIndexIsRaceFreeUnderConcurrentFirstMatch(t *testing.T) {
	site := bigRootSite(5000)
	set := compileIndexProbeACL(t, "geosite:probe", site)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				host := fmt.Sprintf("ad-%06d.tracker.example", (i*50+j)%6000)
				got, _ := set.Match(HostInfo{Name: host}, ProtocolTCP, 443)
				want := "direct"
				if referenceMatch(site, nil, host) {
					want = "reject"
				}
				if got != want {
					t.Errorf("concurrent verdict mismatch host=%q got=%q want=%q", host, got, want)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
