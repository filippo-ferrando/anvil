# Forge runners

`anvil runner` runs Forgejo/Gitea Actions or GitHub Actions jobs on this host, each in its
own throwaway clone (see [ephemeral.md](ephemeral.md)). anvild polls the forge for
waiting jobs, starts a clone for each job it can take, and runs the forge's own runner
inside it for that one job. The runner handles the workflow file and actions
(`actions/checkout` and the rest), so anvil doesn't reimplement them.

```
anvil runner add ci --forge forgejo --url https://code.example.com --scope orgs/acme \
    --label debian=debian-13 --label ubuntu=ubuntu-24.04 --token-file ~/.forgejo-token
ANVIL_FORGE_TOKEN=ghp_... anvil runner add gh --forge github --scope repos/acme/app --label anvil=ubuntu-24.04
anvil runner list
anvil runner jobs ci
anvil runner pause ci      # running jobs go on, no new ones
anvil runner resume ci
anvil runner rm ci         # stops its running jobs too
```

## Labels

Each `--label <job label>=<distro>` maps a label a workflow asks for (`runs-on:`) to the
distro its clone starts from. A job is taken only when every one of its labels is mapped
(GitHub's `self-hosted`, `linux`, `x64` and `arm64` don't count). `@<warm key>` in place of
a distro picks one warm snapshot, such as one built with a setup script or saved from a
clone ([ephemeral.md](ephemeral.md#picking-a-warm-snapshot-by-key)).

Job steps run on the clone itself, as root, not in a container: on Forgejo every label is
registered as `<label>:host`.

## The runner in the clone

For a label mapped to a distro, the forge's runner is installed once, through a warm
snapshot with a setup script (shown as `forgejo-runner latest` or `actions-runner latest`
in `anvil warm list`). The script also installs `curl`, `git` and `node`, which JavaScript
actions need. `--version` pins a runner release; without it the latest one at build time is
used, and stays until that warm snapshot is removed. For a label mapped to `@<warm key>`
nothing is installed: that snapshot must already have the runner (`forgejo-runner` on
`PATH`, or GitHub's runner in `/opt/actions-runner`).

## Forgejo and Gitea

Needs Forgejo 15 or later, which lists waiting jobs (`GET <scope>/actions/runners/jobs`)
and registers ephemeral runners (`POST <scope>/actions/runners`). For each job anvild
registers a single-use runner and runs `forgejo-runner one-job --handle <job>` in the
clone, so the runner takes that exact job. Forgejo invalidates its credentials once the job
is done; anvild also removes a registration whose clone never took its job.

- `--scope` is `repos/<owner>/<repo>`, `orgs/<org>` or `admin` (the whole instance).
- The token needs the rights to manage runners in that scope.

## GitHub

Only repository scopes (`repos/<owner>/<repo>`). anvild lists queued workflow runs and
their jobs, and for each matching job makes a just-in-time runner config
(`generate-jitconfig`, default runner group), which the clone's runner uses for one job.
A just-in-time runner takes any queued job with its labels, not only the one that started
it. The token needs administration rights on the repository. `--url` points at a GitHub
Enterprise Server instead of github.com.

## Limits and safety

- `--max-jobs` (default 2) bounds jobs at once; each clone also goes through the usual
  memory check. `--cpus` and `--memory` (default 2 CPUs, 2048 MiB) size the clones.
- A job's clone is deleted when the job ends, and after 6 hours at most.
- The API token is kept in anvild's database and never sent back to clients. Credentials
  for a job reach its clone through the environment, not the command line.
- Clones reach outside through NAT and nothing reaches them, the same as other ephemeral
  runs, so they suit jobs from pull requests.
- `anvil runner jobs` keeps the last 100 finished jobs per runner, with the end of the
  runner's output for failed ones. Job logs are on the forge.
- When anvild restarts, the clones of running jobs are gone; those jobs show as failed here
  and are left to the forge's own timeout.
