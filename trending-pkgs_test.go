package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The five ranking methods are the part of this tool with actual logic, and
// three of the four non-default ones were documented in the README without
// ever existing. These tests pin what each one returns, because "relative" and
// "total" being silently the same function is exactly the bug that a README
// describing intent instead of behaviour would hide.

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScoreTotalIsAbsoluteChange(t *testing.T) {
	// 1, 2, 3, 4 -> last - first = 3
	got, ok := score([]float64{1, 2, 3, 4}, "total")
	if !ok {
		t.Fatal("total should be defined for a rising series")
	}
	approx(t, got, 3)
}

func TestScoreRelativeIsPercentageChange(t *testing.T) {
	// 2 -> 3 is a 50% rise, not an absolute rise of 1.
	got, ok := score([]float64{2, 3}, "relative")
	if !ok {
		t.Fatal("relative should be defined for a series starting above zero")
	}
	approx(t, got, 0.5)
}

// The distinction that matters: the two methods weight differently. A large
// absolute rise scores high on "total"; a large proportional rise scores high on
// "relative". If both returned the same number, "relative" would be a synonym
// for "total" and the -method flag would be a lie.
func TestTotalAndRelativeDisagree(t *testing.T) {
	// A: 1 -> 101,   +100 absolute, +10000% relative
	// B: 50 -> 150,  +100 absolute,  +200% relative
	// The absolute rise is identical, so "total" cannot tell them apart, while
	// "relative" ranks A far above B.
	totalA, _ := score([]float64{1, 101}, "total")
	totalB, _ := score([]float64{50, 150}, "total")
	relA, _ := score([]float64{1, 101}, "relative")
	relB, _ := score([]float64{50, 150}, "relative")

	if math.Abs(totalA-totalB) > 1e-9 {
		t.Errorf("these two series should have the same absolute change, got %v and %v", totalA, totalB)
	}
	if !(relA > relB) {
		t.Errorf("relative: expected A (%v) above B (%v)", relA, relB)
	}

	// And the converse: a big absolute rise on a small base is a small absolute
	// rise on a large one, which "total" ranks in the opposite order.
	small, _ := score([]float64{1, 2}, "total")
	big, _ := score([]float64{50, 60}, "total")
	if !(big > small) {
		t.Errorf("total: expected the +10 on a base of 50 (%v) above the +1 on a base of 1 (%v)", big, small)
	}
}

func TestScoreRelativeUndefinedFromZero(t *testing.T) {
	// A package nobody sampled before has no percentage growth; dividing by the
	// first value would be a division by zero.
	if _, ok := score([]float64{0, 5}, "relative"); ok {
		t.Error("relative must be undefined when the series starts at zero")
	}
}

func TestScoreZscoreIsDeviationFromOwnMean(t *testing.T) {
	// A steady climb to a value well above the mean should be positive.
	got, ok := score([]float64{1, 2, 3, 10}, "zscore")
	if !ok {
		t.Fatal("zscore should be defined for a varying series")
	}
	if got <= 0 {
		t.Errorf("a series ending above its mean should score positive, got %v", got)
	}

	// A series ending below its mean should be negative.
	low, ok := score([]float64{10, 9, 8, 1}, "zscore")
	if !ok {
		t.Fatal("zscore should be defined for a varying series")
	}
	if low >= 0 {
		t.Errorf("a series ending below its mean should score negative, got %v", low)
	}
}

// A flat line has zero standard deviation, so the z-score divides by zero. The
// original get_zscore did exactly that and returned NaN, which sorts
// unpredictably -- sometimes first, sometimes last.
func TestScoreZscoreUndefinedForFlatSeries(t *testing.T) {
	got, ok := score([]float64{5, 5, 5, 5}, "zscore")
	if ok {
		t.Errorf("zscore must be undefined for a constant series, got %v", got)
	}
	if math.IsNaN(got) {
		t.Error("score returned NaN; that is what the caller cannot handle")
	}
}

func TestScoreLogUsesAThirdOfTheWayBack(t *testing.T) {
	// A long flat history with a single rise at the very end. Comparing against
	// a point a third of the way back is the whole point: if the method compared
	// against the first sample instead, the history would still give the same
	// base here, so the test uses a series that rises early AND late and checks
	// the base is the a-third-back one.
	//
	// len 12, so idx = 12/3 = 4. series[4] is the base, and the last value is 8.
	series := []float64{1, 1, 1, 1, 1, 8, 8, 8, 8, 8, 8, 8}
	got, ok := score(series, "log")
	if !ok {
		t.Fatal("log should be defined for a positive series")
	}
	// base = series[4] = 1, last = 8 -> log(8) = 2.0794...
	// Against the first sample (also 1) this happens to agree, so also assert it
	// is NOT the whole-history ratio when the base differs.
	approx(t, got, math.Log(8))

	// Here the base genuinely differs from the first sample: series[4] = 4 while
	// series[0] = 1, so the two definitions give different answers.
	series2 := []float64{1, 1, 1, 1, 4, 4, 4, 4, 4, 4, 4, 4}
	got2, ok := score(series2, "log")
	if !ok {
		t.Fatal("log should be defined for a positive series")
	}
	// Against the first sample this would be log(4/1) = 1.386; against a third
	// back it is log(4/4) = 0.
	approx(t, got2, 0)
	if approxEq(got2, math.Log(4)) {
		t.Errorf("log scored %v, which is the whole-history ratio; the base should be a third of the way back", got2)
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScoreCombinedIsBoundedAndNotEqualToTotal(t *testing.T) {
	// Normalising both terms is what stops combined collapsing into total.
	got, ok := score([]float64{1, 101}, "combined")
	if !ok {
		t.Fatal("combined should be defined")
	}
	if got < 0 || got > 1 {
		t.Errorf("combined should be normalised to [0,1], got %v", got)
	}

	total, _ := score([]float64{1, 101}, "total")
	if math.Abs(got-total) < 1e-9 {
		t.Errorf("combined (%v) collapsed into total (%v); the relative term is not contributing", got, total)
	}
}

func TestScoreNeedsTwoPoints(t *testing.T) {
	for _, m := range []string{"zscore", "total", "relative", "log", "combined", "tiered"} {
		if _, ok := score([]float64{1}, m); ok {
			t.Errorf("%s: a single point should not be scoreable", m)
		}
		if _, ok := score(nil, m); ok {
			t.Errorf("%s: an empty series should not be scoreable", m)
		}
	}
}

func TestScoreUnknownMethod(t *testing.T) {
	if _, ok := score([]float64{1, 2, 3}, "nonsense"); ok {
		t.Error("an unknown method must not return a score")
	}
}

func TestValidMethod(t *testing.T) {
	for _, m := range []string{"zscore", "total", "relative", "log", "combined", "tiered"} {
		if !validMethod(m) {
			t.Errorf("%q should be a valid method", m)
		}
	}
	for _, m := range []string{"", "ZSCORE", "zscore ", "hot", "nonsense"} {
		if validMethod(m) {
			t.Errorf("%q should not be a valid method", m)
		}
	}
}

// Ties must break deterministically, or two runs over the same data produce
// different files and a diff of the output means nothing.
func TestSortScoredIsDeterministicOnTies(t *testing.T) {
	in := []SomeStruct{
		{"zebra", 1}, {"apple", 1}, {"mango", 1}, {"banana", 2},
	}
	want := []string{"banana", "apple", "mango", "zebra"}

	for run := 0; run < 20; run++ {
		got := append([]SomeStruct(nil), in...)
		sortScored(got, "zscore")
		for i, w := range want {
			if got[i].Name != w {
				t.Fatalf("run %d: position %d = %s, want %s (full order %v)", run, i, got[i].Name, w, names(got))
			}
		}
	}
}

func names(s []SomeStruct) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = v.Name
	}
	return out
}

// fetchSeries is exercised against a stub rather than the live API, so the
// decode path and the error paths are covered without a network dependency.
//
// Two things are being checked here. That a non-200 is an error rather than a
// silently empty result: the original code did not check the status at all, so
// an API error page decoded into an empty series and the package scored as
// nothing. And that an unknown package is reported, not silently dropped.
func TestFetchSeriesRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := API
	API = srv.URL + "/"
	defer func() { API = old }()

	if _, err := fetchSeries("anything"); err == nil {
		t.Fatal("a 500 must be an error, not an empty series")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error should name the status, got %v", err)
	}
}

func TestFetchSeriesReportsUnknownPackage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A well-formed 200 with no series: the shape the API returns for a
		// package it has never heard of.
		w.Write([]byte(`{"name":"x","packagePopularities":[]}`))
	}))
	defer srv.Close()

	old := API
	API = srv.URL + "/"
	defer func() { API = old }()

	if _, err := fetchSeries("nonexistent"); err == nil {
		t.Fatal("an empty series should be an error, so the package is reported rather than scored as zero")
	}
}

func TestFetchSeriesDecodesValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"packagePopularities":[
			{"popularity":1.5},{"popularity":2.5},{"popularity":9.0}]}`))
	}))
	defer srv.Close()

	old := API
	API = srv.URL + "/"
	defer func() { API = old }()

	got, err := fetchSeries("anything")
	if err != nil {
		t.Fatalf("fetchSeries: %v", err)
	}
	want := []float64{1.5, 2.5, 9.0}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		approx(t, got[i], want[i])
	}
}

// fetchAll must keep results in input order, and must not lose a package when
// one of them fails.
func TestFetchAllPreservesOrderAndReportsFailures(t *testing.T) {
	names := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	// 8 so the five requests genuinely overlap rather than running serially.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/packages/beta/") {
			http.Error(w, "boom", http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"packagePopularities":[{"popularity":1},{"popularity":2},{"popularity":5}]}`))
	}))
	defer srv.Close()

	old := API
	API = srv.URL + "/"
	defer func() { API = old }()

	results := fetchAll(names, 4)
	if len(results) != len(names) {
		t.Fatalf("got %d results, want %d", len(results), len(names))
	}
	for i, r := range results {
		if r.name != names[i] {
			t.Errorf("position %d = %s, want %s (concurrent fetches reordered the results)", i, r.name, names[i])
		}
	}
	if results[1].err == nil {
		t.Error("the failing package should carry its error")
	}
	for i, r := range results {
		if i != 1 && r.err != nil {
			t.Errorf("%s should have succeeded, got %v", r.name, r.err)
		}
	}
}

// A failed fetch must not become a score. If it did, a network blip would
// demote real packages.
func TestFailedFetchIsNotScored(t *testing.T) {
	failed := result{name: "ghost", series: nil, err: http.ErrServerClosed}
	if v, ok := score(failed.series, "zscore"); ok {
		t.Errorf("a failed fetch scored %v; it must be skipped", v)
	}
}

func TestReadLinesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/list.txt"
	if err := writeToFile([]string{"a", "b", "c"}, path); err != nil {
		t.Fatal(err)
	}
	got, err := readLinesFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("got %v, want [a b c]", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := readLinesFromFile(t.TempDir() + "/nope.txt"); err == nil {
		t.Error("reading a missing file should be an error")
	}
}
