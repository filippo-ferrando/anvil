# Pipelines

A pipeline file (`anvil-ci.yaml` by default) lists jobs. Each job runs one command on a
throwaway clone per distro (see [ephemeral.md](ephemeral.md)), after the jobs it needs
have passed. anvild runs the whole pipeline and keeps its history, logs and collected
files. A pipeline runs by hand with `anvil pipeline run`, or on every new commit of a
watched branch.

```yaml
jobs:
  build:
    distro: ubuntu-24.04
    run: make build              # sh -c, from /work
    setup: ci/setup.sh           # run once in a warm snapshot, see below
    key-files: [go.mod, go.sum]
    env: {CGO_ENABLED: "0"}
    collect: [dist]              # kept with the run
  test:
    needs: [build]
    distros: [ubuntu-24.04, debian-13, alpine-3.22]
    run: make test
    timeout: 20m                 # default 1h
    cpus: 4                      # default 2
    memory: 2048                 # MiB, default 512
apply: deploy/anvil.yaml         # applied once every job passed
```

## Jobs

- `distro` or `distros` (one clone each, all at once) and `run` are required. A distro can
  also be `@<warm key>` ([ephemeral.md](ephemeral.md#picking-a-warm-snapshot-by-key)).
- Every job gets the whole source in `/work`: what git doesn't ignore in a git repo,
  everything otherwise.
- `needs` lists jobs that must pass first. Independent jobs run at the same time. A job
  whose need failed is skipped. A cycle is an error before anything runs.
- `setup` and `key-files` build a warm snapshot with that script, once, and reuse it until
  the script or a key file changes ([ephemeral.md](ephemeral.md#setup-scripts)). Paths are
  relative to the source.
- `collect` takes the same paths and globs as `--collect` and keeps the files under the
  run, as `<job>/<distro>/...`. Jobs don't see each other's collected files.
- A job's clone is deleted when it ends, whether it passed or failed.

## Running by hand

```
anvil pipeline run                      # the current directory
anvil pipeline run ./app -f ci/pipeline.yaml --artifacts out
anvil pipeline runs
anvil pipeline show 01K9                # a unique start of the run id
anvil pipeline logs 01K9 test --distro debian-13
anvil pipeline artifacts 01K9 out
anvil pipeline rerun 01K9
anvil pipeline cancel 01K9
```

`run` uploads the directory, then prints every job's output with a `[job@distro]` prefix
and a summary, like `anvil matrix`. It exits 1 when the run failed. Ctrl-C, or a dropped
connection, cancels the run and deletes its clones. `--artifacts` downloads the collected
files at the end. Over `--remote` the source is uploaded to the remote anvild, and logs and
files come back the same way.

## Watching a branch

```
anvil pipeline watch add app --repo git@code.example.com:acme/app.git --branch main
anvil pipeline watch list
anvil pipeline watch trigger app        # run now, even without a new commit
anvil pipeline watch pause app
anvil pipeline watch rm app
```

anvild checks the branch with `git ls-remote` every `--interval` (default 1 minute, at
least 10 seconds). On a new commit it makes a shallow clone, packs what git tracks and runs
the pipeline file (`--file`, default `anvil-ci.yaml`). A commit counts as seen once its run
starts, so a failing pipeline isn't retried until the next commit or a trigger.

- anvild runs `git` as its own user (`anvil`), without prompts. A private repo needs that
  user's SSH key or a git credential helper set up for it.
- The repo is an `https://`, `http://`, `ssh://` or `git://` URL, `user@host:path`, or a
  local path. A local repo must be owned by whoever adds the watch, since anvild reads it
  with its own rights.

## Apply on success

`apply:` names an `anvil.yaml` in the source, applied with `anvil apply` once every job
passed (see [apply.md](apply.md)). Host paths in its volumes are checked against whoever
started the run, or added the watch, the same way `anvil apply` checks its caller: only
directories that user owns can be shared. The result shows in `anvil pipeline show` as
`apply: applied <file>` or `apply: failed: <why>`, and a failed apply fails the run.

## History

anvild keeps the last 50 runs, each with its source, logs and collected files, under
`/var/lib/anvil/pipeline-runs/<id>`. A run that was going when anvild stopped shows as
failed after the restart. The TUI's CI screen lists runs (`enter` shows their jobs, `x`
cancels one) and watches (`t` triggers, `p` pauses).
