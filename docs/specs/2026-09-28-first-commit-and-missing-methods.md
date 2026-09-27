# trending-pkgs — first commit, and three things the README had promised

`go/trending-linux-packages` had a complete working tool and **no commits at
all**. `git log` on `main` reported "your current branch 'main' does not have
any commits yet". Everything — source, `go.mod`, `go.sum`, a 68,664-line package
list, a README, an 8.6MB aarch64 binary — was sitting in the working tree,
half of it staged, `go.mod` staged as added-then-deleted so the tree did not
even build.

`go.mod` and `go.sum` were recoverable from the index (`git checkout-index`),
which is the only reason this repo had a `go.mod` at all. The module is
`trending-pkgs` on Go 1.19 with a single dependency,
`github.com/montanaflynn/stats v0.6.6`, used only for `stats.Mean` and
`stats.StandardDeviation`.

## What was actually built

A tool that reads a list of Arch package names, fetches each one's monthly
popularity series from `https://pkgstats.archlinux.de/api/packages/<pkg>/series`,
scores it, and prints the packages highest-first.

### 1. The README described a tool that did not exist

It opened by listing five sorting methods — "Total Gain", "Relative Gain",
"Logarithmic Gain", "Combined Score", "Tiered". The code had **no `flag`
import, no `os.Args`, and no flag handling of any kind.** The only score
computed was a z-score. The rest of the README was a tutorial: "Here's a
step-by-step explanation of how you can implement the script yourself" —
including the step "Use the `flag` package to handle command-line arguments",
which had not been done.

Running the binary with `-h` did not print usage. It ignored the argument,
loaded `packages.txt`, and started fetching.

All five methods now exist behind `-method`, and the README is a real README.
Two details the tutorial got wrong, now fixed in the implementation:

- **Tiered** is described as "sort by total gain, then relative gain for ties".
  As a single score it cannot express two sort keys, so `tiered` scores on
  `total` and `sortScored` breaks ties on name. Ordering by total is right;
  the tiebreak is not relative, and the README now says so.
- **Combined** is described as combining total and relative. Added naively it
  *is* total: `(last-first)/100 + (last-first)/first` divided by two is
  dominated by the first term, because the absolute term is unbounded and the
  relative one usually sits near 0.2. Both terms are now clamped to `[0,1]`
  before averaging, and `TestScoreCombinedIsBoundedAndNotEqualToTotal` fails if
  the two ever collapse into the same number again.

The `zscore` default is unchanged: `(last - mean) / stddev` over the package's
own history, via `montanaflynn/stats`.

### 2. A full run took about three hours, and most of it was not the API

`get_trending_packages` looped over every package and called
`get_package_popularities` synchronously. Measured against the live API, one
`series` request is ~150ms, and `packages.txt` has 68,664 entries: roughly
**2.9 hours**, serial.

Worse, `main` called `getNonDependencyPackages` unconditionally, which runs
`pacman -Sii` once per package. That is 68,664 process spawns — slower in total
than the HTTP work, and it produced a file nothing downstream read, because
`writeToFile(nonDependencyPackages, "non_dependency_packages.txt")` was the
only consumer and `nonDependencyPackages` went on to be discarded.

Changes:

- **`fetchAll`** fans the list out over `-workers` (default 16, capped at
  4× NumCPU), writing results back by index so output order does not depend on
  which worker finished first. At 16 workers a full run is roughly 11 minutes.
- **`-non-deps`** is now opt-in. The default path no longer spawns anything.
- **`http.DefaultClient` → a client with a 30s timeout.** `http.Get` has no
  timeout at all; across 69,000 requests one slow response is a near-certainty
  rather than a possibility, and it parks the loop forever.
- **`requestJSON` never checked the status code.** A 404 or a 502 body
  decoded into an empty `Response`, the package scored as nothing, and the
  failure was indistinguishable from a package with no history. `fetchSeries`
  now returns an error naming the status, and a well-formed-but-empty series is
  an explicit error too.
- **Failed fetches are reported and skipped, not scored as zero.** Otherwise a
  transient network failure demotes real packages for no reason.
- **Ties break on package name**, so two runs over the same data produce
  byte-identical output and a diff of the results means something.
- `ioutil.ReadFile`/`WriteFile` → `os`, and `get_zscore` was replaced by
  `score`, which returns `ok=false` when the method is undefined rather than
  `NaN`.

### 3. `API` had to become a var

`const API` could not be repointed, which made the HTTP layer untestable without
a real network call. It is now a `var` with a comment saying why. The tests use
`httptest` stubs and never touch pkgstats.

## Tests

19 tests, no network, `go test ./...` in 4ms.

The two that matter most:

- `TestTotalAndRelativeDisagree` — a series with a large absolute rise
  (1→101) and one with a large proportional rise (50→150) have *identical*
  `total` scores, so `total` cannot tell them apart, while `relative` ranks
  them far apart. If both methods ever returned the same number, `-method`
  would be a flag that lies.
- `TestScoreZscoreUndefinedForFlatSeries` — the original `get_zscore` divided
  by a zero standard deviation for a constant series and returned `NaN`, which
  sorts unpredictably. Now returns `ok=false`, and the test asserts the value
  is not `NaN`.

Two of my own tests failed first and both were the test's fault, not the code's:
I had expected `relative` to prefer a small-base package over a large-base one
(backwards — 1→101 is +10000%), and I had picked a series where the `log`
method's step happened before its comparison point, so the test asserted `0` and
the code correctly returned `0`. Worth recording: the second one passed for the
wrong reason. It was rewritten to use a series where the base genuinely differs
from the first sample, so it distinguishes the two definitions.

## .gitignore

Was a generic Python/Node/Rust template with nothing Go-specific, which is why
an 8.6MB aarch64 binary was staged. Now excludes the build output, the
cross-compiles, and the two files a run writes (`popularities.txt`,
`trending.txt`). `packages.txt` stays tracked — it is the input.

The binary had to be explicitly unstaged, not just ignored, since `.gitignore`
only affects untracked files.

## Verification

```
gofmt -l .          clean
go vet ./...        clean
go build            ok
go test ./...       ok, twice, 19 tests
live smoke test     6 packages, 192ms, correctly ranked
all 6 methods       run, each produces a different ordering
unknown -method     rejected with the list of valid ones
unknown package     reported and skipped, not scored as zero
```

Live ranking from the smoke test, z-score: `vim 2.489`, `ffmpeg 1.889`,
`neovim 1.567`, `python 1.002`, `bash 0.617`, `linux -0.063`.

`API` is a var so the tests can point it at a stub. A private host or
rate-limit concern is the reason to consider making it a flag instead; it is
not one today.
