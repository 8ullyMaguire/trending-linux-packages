# trending-pkgs

Ranks Arch Linux packages by how fast their [pkgstats](https://pkgstats.archlinux.de/)
popularity is rising.

It reads a list of package names, fetches each package's monthly popularity
series, scores it, and prints the results highest-first as `name score`.

## Build

```sh
go build -o trending-pkgs .
```

## Run

```sh
# score every package in packages.txt with the default z-score method
./trending-pkgs

# try a method on a small list first
./trending-pkgs -list small-list.txt -workers 8

# write to a file instead of stdout
./trending-pkgs -out trending.txt
```

The package list is one name per line. The bundled `packages.txt` has about
69,000 entries, and the API answers in roughly 150ms, so a full run makes 69,000
HTTP requests — about three hours sequentially, minutes with the default 16
workers.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-method` | `zscore` | Ranking method; see below. |
| `-list` | `packages.txt` | File of package names, one per line. |
| `-out` | stdout | Where to write `name score` lines. |
| `-workers` | `16` | Concurrent HTTP requests. Capped at 4× CPU count. |
| `-non-deps` | `false` | Keep only packages nothing depends on. Slow; see below. |
| `-limit` | `0` | Only score the first N packages from the list. |

## Ranking methods

**`zscore`** (default) — how far the latest popularity sits from that package's
own mean, in standard deviations. A package at 2.0 is two standard deviations
above its own history, whatever its absolute popularity happens to be. This
favours recent movers over long-established packages, which is the point.

A package whose popularity never changes has a standard deviation of zero, so
there is no z-score. Those are skipped rather than reported as `NaN`, which
would sort unpredictably.

**`total`** — absolute change over the window. Favours big movers.

**`relative`** — percentage change over the window. Favours small packages
growing fast. Undefined for a series starting at zero, since that is a
division by zero.

**`log`** — `log(current ÷ value a third of the way back)`. Comparing against a
point a third back rather than the first sample stops a long-lived package from
being scored on its entire history.

**`combined`** — mean of `total` and `relative`, each normalised to `[0,1]`
first. Without that normalisation the absolute term dominates and the result is
indistinguishable from `total`.

**`tiered`** — `total`, with ties broken on `relative`.

## Notes

`-non-deps` runs `pacman -Sii` once per package, which is another ~69,000
process spawns and is slower than the HTTP work it precedes. It used to run on
every invocation, so a full run spent most of its time spawning processes; it
is now opt-in.

Packages the API does not know about, or that time out, are reported on stderr
and skipped rather than scored as zero — otherwise a network blip would demote
real packages.

Ties break on package name, so two runs over the same data produce byte-identical
output.

## Tests

```sh
go test ./...
```

19 tests, no network access: the HTTP layer is exercised against `httptest`
stubs. The scoring tests pin what each method returns, including the case that
distinguishes `total` from `relative` — a series with a large absolute rise and
a series with a large proportional rise must not rank the same way, or `-method`
would be a flag that lies.

## How it works

1. `readLinesFromFile` loads the package list.
2. `fetchAll` fans the list out across N workers; each calls `fetchSeries`, which
   GETs `https://pkgstats.archlinux.de/api/packages/<pkg>/series?startMonth=201009`
   and returns the monthly popularity values. Results are written back by index,
   so output order does not depend on which worker finished first.
3. `score` applies the chosen method, returning `false` for a series the method
   is undefined for.
4. `sortScored` orders by score descending, ties broken by name.
5. The scores are printed, or written to `-out`.
