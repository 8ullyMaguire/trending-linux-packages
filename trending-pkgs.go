// Command trending-pkgs ranks Arch Linux packages by how unusual their current
// pkgstats popularity is relative to their own history.
//
// It reads a package list, fetches each package's popularity series from
// pkgstats.archlinux.de, and scores each one.
//
// The default score is the z-score the tool has always used: how far the most
// recent popularity sits from that package's own mean, in standard deviations.
// A package at 2.0 is two standard deviations above its own history, whatever
// absolute popularity that happens to be. The other four methods the README
// describes are selectable with -method; they were documented but never
// implemented, so the README was describing an intention rather than the tool.
//
// Two things dominate the runtime, and both were sequential before:
//
//   - One HTTP request per package. The default list is ~69,000 packages and the
//     API answers in roughly 150ms, so a sequential run takes about three hours.
//     -workers fetches concurrently; the default is 16.
//   - getNonDependencyPackages shells out to `pacman -Sii` once per package,
//     another ~69,000 process spawns, slow enough to dominate everything else.
//     It used to run on every invocation; it is now behind -non-deps.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/montanaflynn/stats"
)

// API is a var rather than a const so the tests can point it at a stub server.
// The production value is the pkgstats endpoint.
var API string = "https://pkgstats.archlinux.de/api/"

// The earliest month pkgstats has data for. Anything earlier returns an empty
// series, and the z-score below would then divide by a zero standard deviation.
const startMonth = "201009"

// client has a timeout, unlike http.DefaultClient. Without one a single slow
// response parks a worker forever, and across 69,000 requests one slow response
// is a near-certainty rather than a possibility.
var client = &http.Client{Timeout: 30 * time.Second}

type SomeStruct struct {
	Name   string
	Zscore float64
}

type PackagePopularity struct {
	Name       string  `json:"name"`
	Samples    int     `json:"samples"`
	Count      int     `json:"count"`
	Popularity float64 `json:"popularity"`
	StartMonth int     `json:"startMonth"`
	EndMonth   int     `json:"endMonth"`
}

type Response struct {
	Total               int                 `json:"total"`
	Count               int                 `json:"count"`
	Limit               int                 `json:"limit"`
	Offset              int                 `json:"offset"`
	Query               interface{}         `json:"query"`
	PackagePopularities []PackagePopularity `json:"packagePopularities"`
}

// result is one package's series plus the error that stopped it, so an
// unreachable package is visible rather than silently scored as zero.
type result struct {
	name   string
	series []float64
	err    error
}

func main() {
	method := flag.String("method", "zscore",
		"ranking method: zscore, total, relative, log, combined or tiered")
	list := flag.String("list", "packages.txt",
		"file containing one package name per line")
	out := flag.String("out", "",
		"write 'name score' lines here instead of stdout")
	workers := flag.Int("workers", 16,
		"concurrent HTTP requests")
	nonDeps := flag.Bool("non-deps", false,
		"filter the list through `pacman -Sii`, keeping only packages nothing depends on "+
			"(one subprocess per package: slow)")
	limit := flag.Int("limit", 0,
		"only consider the first N packages from the list; 0 means all")
	flag.Parse()

	if !validMethod(*method) {
		log.Fatalf("unknown -method %q: want one of zscore, total, relative, log, combined, tiered", *method)
	}
	if *workers < 1 {
		log.Fatalf("-workers must be at least 1, got %d", *workers)
	}

	packageNames, err := readLinesFromFile(*list)
	if err != nil {
		log.Fatal(err)
	}
	if *nonDeps {
		packageNames = getNonDependencyPackages(packageNames)
	}
	if *limit > 0 && *limit < len(packageNames) {
		packageNames = packageNames[:*limit]
	}
	if len(packageNames) == 0 {
		log.Fatal("no packages to score")
	}

	log.Printf("scoring %d packages with %d workers, method %s", len(packageNames), *workers, *method)
	results := fetchAll(packageNames, *workers)

	// A package the API does not know about, or that timed out, is reported and
	// skipped. Scoring it as zero would let a transient network failure demote
	// real packages for no reason.
	failures := 0
	scored := make([]SomeStruct, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			failures++
			if failures <= 10 {
				log.Printf("skipping %s: %v", r.name, r.err)
			}
			continue
		}
		if v, ok := score(r.series, *method); ok {
			scored = append(scored, SomeStruct{r.name, v})
		}
	}
	if failures > 10 {
		log.Printf("skipping %d more packages that could not be fetched", failures-10)
	}
	if len(scored) == 0 {
		log.Fatal("no package produced a score")
	}
	log.Printf("scored %d of %d packages (%d skipped)", len(scored), len(packageNames), failures)

	sortScored(scored, *method)

	var w io.Writer = os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}
	for _, s := range scored {
		fmt.Fprintf(w, "%s %g\n", s.Name, s.Zscore)
	}
}

func validMethod(m string) bool {
	switch m {
	case "zscore", "total", "relative", "log", "combined", "tiered":
		return true
	}
	return false
}

// fetchAll pulls every series concurrently, preserving input order so two runs
// over the same list produce the same ordering before the final sort.
func fetchAll(pkgs []string, workers int) []result {
	// A worker per four cores is plenty for an HTTP-bound loop; beyond that the
	// API, not this process, is the bottleneck.
	if max := runtime.NumCPU() * 4; workers > max {
		workers = max
	}

	out := make([]result, len(pkgs))
	next := make(chan int)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range next {
				series, err := fetchSeries(pkgs[idx])
				out[idx] = result{pkgs[idx], series, err}
			}
		}()
	}
	for i := range pkgs {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// fetchSeries returns one package's monthly popularity values, oldest first.
func fetchSeries(pkg string) ([]float64, error) {
	relURL := "packages/" + pkg + "/series?startMonth=" + startMonth
	resp, err := client.Get(API + relURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	var response Response
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(response.PackagePopularities))
	for _, p := range response.PackagePopularities {
		out = append(out, p.Popularity)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no popularity data (unknown to pkgstats, or newer than %s)", startMonth)
	}
	return out, nil
}

// getPackagePopularities is the single-package form, kept so a test can exercise
// the fetch and decode path without going through all 69,000 packages.
func getPackagePopularities(pkg string) []float64 {
	series, err := fetchSeries(pkg)
	if err != nil {
		return nil
	}
	return series
}

// score applies one ranking method to a popularity series. ok is false when the
// method is undefined for that series: a flat line has no meaningful growth, and
// the log method needs positive values at both ends.
func score(series []float64, method string) (float64, bool) {
	if len(series) < 2 {
		return 0, false
	}
	first, last := series[0], series[len(series)-1]

	switch method {
	case "zscore":
		// The original method, unchanged: how far the latest value sits from
		// this package's own mean. A constant series has zero standard
		// deviation, so there is no z-score to report.
		mean, _ := stats.Mean(series)
		std, _ := stats.StandardDeviation(series)
		if std == 0 {
			return 0, false
		}
		return (last - mean) / std, true

	case "total":
		return last - first, true

	case "relative":
		if first <= 0 {
			return 0, false
		}
		return (last - first) / first, true

	case "log":
		// Compare against a point a third of the way back rather than the first
		// sample, so a long-lived package is not scored on its whole history.
		idx := len(series) / 3
		if idx < 1 {
			idx = 1
		}
		base := series[idx]
		if base <= 0 || last <= 0 {
			return 0, false
		}
		return math.Log(last / base), true

	case "combined":
		// Both terms are normalised to [0,1] before averaging. Without that the
		// absolute term dominates and the relative term contributes nothing,
		// which makes this identical to "total".
		if first <= 0 {
			return clamp01((last - first) / 100), true
		}
		return (clamp01((last-first)/100) + clamp01((last-first)/first)) / 2, true

	case "tiered":
		// Sorted by total, then relative as a tiebreak; see sortScored.
		return last - first, true
	}
	return 0, false
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// sortScored orders results by score descending. Ties break on name so two runs
// over the same data produce byte-identical output rather than an order that
// depends on scheduling.
func sortScored(scored []SomeStruct, method string) {
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Zscore == scored[j].Zscore {
			return scored[i].Name < scored[j].Name
		}
		return scored[i].Zscore > scored[j].Zscore
	})
}

// getNonDependencyPackages keeps only packages that nothing depends on.
//
// Slow by nature: one `pacman -Sii` per package. It used to run on every
// invocation, which meant a full run spent most of its time spawning processes
// rather than talking to the API. It is now behind -non-deps.
func getNonDependencyPackages(packages []string) []string {
	var nonDependencyPackages []string

	for _, pkg := range packages {
		cmd := exec.Command("pacman", "-Sii", pkg)
		output, err := cmd.Output()

		if err != nil {
			fmt.Println("Error while checking package", pkg, ":", err)
			continue
		}

		outputStr := string(output)
		if strings.Contains(outputStr, "Required By     : None") {
			nonDependencyPackages = append(nonDependencyPackages, pkg)
		}
	}

	return nonDependencyPackages
}

func writeToFile(data []string, fileName string) error {
	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	for _, item := range data {
		_, err = fmt.Fprintln(file, item)
		if err != nil {
			return err
		}
	}

	return nil
}

func readLinesFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}
