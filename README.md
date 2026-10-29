# twoctl

[![ci](https://github.com/two-inc/twoctl-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/two-inc/twoctl-cli/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/two-inc/twoctl-cli)](https://goreportcard.com/report/github.com/two-inc/twoctl-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Command-line interface for the [Two](https://two.inc) merchant APIs. Generated from the [public OpenAPI specs](https://github.com/two-inc/docs/tree/main/openapi), so every endpoint is exposed as a subcommand.

## Install

### Homebrew

```sh
brew install two-inc/tap/twoctl
```

### Binary releases

Download from [releases](https://github.com/two-inc/twoctl-cli/releases) and place on `$PATH`.

### From source

```sh
go install github.com/two-inc/twoctl-cli/cmd/twoctl@latest
```

## Shell completion

`twoctl` ships completions for bash, zsh, fish, and PowerShell. The binary
generates them on demand via `twoctl completion <shell>`.

### bash

Linux:

```sh
twoctl completion bash | sudo tee /etc/bash_completion.d/twoctl > /dev/null
```

macOS (with `bash-completion@2` from Homebrew):

```sh
twoctl completion bash > "$(brew --prefix)/etc/bash_completion.d/twoctl"
```

If you don't have write access to a system path, source the completion in your
`~/.bashrc`:

```sh
echo 'source <(twoctl completion bash)' >> ~/.bashrc
```

### zsh

```sh
# one-shot (re-run on shell start):
echo 'source <(twoctl completion zsh)' >> ~/.zshrc

# or persist to fpath:
twoctl completion zsh > "${fpath[1]}/_twoctl"
```

If `compinit` hasn't been initialised in your `~/.zshrc`, add:

```sh
autoload -Uz compinit && compinit
```

before the completion source line.

### fish

```sh
twoctl completion fish | source                                # current shell
twoctl completion fish > ~/.config/fish/completions/twoctl.fish # persistent
```

### PowerShell

```powershell
twoctl completion powershell | Out-String | Invoke-Expression                          # current session
twoctl completion powershell >> $PROFILE                                                # persistent
```

After installing, open a new shell. Tab-completion works on subcommands,
flags, and any flag value that has a known enum in the OpenAPI spec
(country codes, currencies, statuses).

## Contexts

`twoctl` uses named contexts (kubectl-style). Each context bundles a base URL
and an OS-keychain entry holding the API key, so you can switch between
sandbox, prod, staging, cyber, perf, release, or any custom environment
without copy-pasting keys.

```sh
twoctl config set-context sandbox --url https://api.sandbox.two.inc --key secret_test_...
twoctl config set-context prod    --url https://api.two.inc        --key secret_prod_...
twoctl config get-contexts                  # show all contexts, * marks current
twoctl config use-context prod              # switch the default
twoctl config current-context               # print the current name
twoctl config delete-context temp           # remove + clear keychain entry
```

Per-invocation overrides (no switching):

```sh
twoctl --env prod company company-search --country NO --q two
twoctl --context cyber checkout get-order --order-id abc   # kubectl-style alias
twoctl --url http://localhost:8080 --api-key secret_test_x ...
```

Scripts can select a context without changing the saved selection:

```sh
TWOCTL_CONTEXT=staging twoctl order get --order-id abc
```

Context selection follows `--context`, then `--env`, then non-empty
`TWOCTL_CONTEXT`, then the saved current context. `--url` overrides the
API URL and requires `--api-key` or `TWO_API_KEY` rather than using a stored
context key. API calls, `auth whoami`, `auth login`, `auth logout`, and
`--dry-run` honor the invocation context. `config current-context` continues
to show the saved selection; the environment override never writes it.

For built-in env names (`prod`, `sandbox`, `staging`, `cyber`, `perf`,
`release`) twoctl knows the URL. For any other name it falls back to
`https://api.<name>.two.inc`; override with `--url`.

API key resolution order (highest precedence first):

1. `--api-key` flag
2. `TWO_API_KEY` env var
3. OS keychain entry for the active context

Keys are obtained from the [Two merchant portal](https://portal.two.inc).

### Configuration directory

Set `TWOCTL_CONFIG_HOME` to use a dedicated twoctl configuration directory:

```sh
TWOCTL_CONFIG_HOME=/path/to/job-config twoctl config get-contexts
```

Directory precedence is `TWOCTL_CONFIG_HOME`, then
`$XDG_CONFIG_HOME/twoctl`, then `~/.config/twoctl`. An empty override falls
back to the next location. Both `config.yaml` (and its lock) and updater
`state.json` use the selected directory; `TWOCTL_CONFIG_HOME` names that
directory directly, with no extra `twoctl/` appended. Existing files are
not copied between locations. API keys stay in the OS keychain, keyed by
context name, so selecting a different directory does not isolate keys.

## Usage

The command tree is resource-first: `twoctl <resource> <action> [flags]`.
Every operation across every Two API is classified into a `(resource,
action)` pair derived from the URL path + operationId, then registered
under the resource. Non-CRUD actions like `cancel`, `refund`, `fulfill`,
`search`, `notify` surface as first-class verbs alongside `get`/`create`/
`edit`/`delete`.

```sh
twoctl order get --order-id abc-123          # GET /v1/order/{order_id}
twoctl order create --file order.json        # POST /v1/order
twoctl order cancel --order-id abc-123       # POST /v1/order/{id}/cancel
twoctl order fulfill --order-id abc-123 --file f.json
twoctl order refund --order-id abc-123 --file r.json
twoctl order edit --order-id abc-123 --file patch.json
twoctl company search --q "two" --country NO
twoctl billing-account get --billing-account-id ba_xxx
twoctl explain order create                  # JSON schema for the operation
twoctl api-resources --resource order        # catalog filtered by resource
twoctl api-resources --action cancel         # everything that's a "cancel"
```

Run `twoctl <resource>` on its own to see every action available. Each leaf
command's `--help` lists flags derived from the OpenAPI operation;
request bodies go via `--file path.json` or `--data '{...}'`.

Top-level non-resource commands:

| `twoctl` | Purpose |
| --- | --- |
| `config set-context / use-context / get-contexts / current-context / delete-context` | Manage contexts (kubectl naming) |
| `auth login / logout / whoami` | Store the API key in the OS keychain |
| `explain <resource> <action>` | JSON schema for an operation, no network call |
| `api-resources` | JSON catalog of every operation, for agent discovery |
| `version` | Print version and build info |
| `upgrade` | Self-upgrade from the latest GitHub release |
| `--context` / `--env` (alias) | Per-invocation context override |

## Agent-friendly features

`twoctl` is designed to be driven by both humans and LLM agents.

- `twoctl api-resources [-r <resource>] [-a <action>]` — emits a JSON catalogue of every operation (command, method, path, flags with kind/type/required, body status). Pipe this to an agent's planner so it can pick the right command without parsing `--help` text.
- `twoctl <resource> <action> --describe` — JSON view of one operation's full spec (parameters, request body schema, response schemas) without a network call. Also reachable as `twoctl explain <resource> <action>`.
- `twoctl <resource> <action> --dry-run` — render the HTTP request that would be sent (URL, redacted headers, body presence) and exit. No credentials needed.
- `-o table|json|yaml` (default `auto`: table on TTY, JSON when piped).
- Errors are emitted as JSON on stderr with shape `{error, message, status, response, request_id}`. Stdout stays clean for piping.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | generic error |
| 2 | usage / missing required input |
| 3 | authentication or authorisation (401/403) |
| 4 | not found (404) |
| 5 | rate limited (429) |
| 6 | server error (5xx) |
| 7 | network / transport failure |

## Self-upgrade

`twoctl` checks for new releases at most once every 24 hours and prompts when one is available. The prompt offers:

- `y` install now
- `n` not now (default)
- `s` skip this version (won't ask again until the next release)

Direct controls:

```sh
twoctl upgrade                      # check + install
twoctl upgrade --check              # only report, don't install
twoctl upgrade --reset-skips        # forget every skipped version
twoctl upgrade --disable-autocheck  # silence the prompt entirely
twoctl upgrade --enable-autocheck   # turn it back on
```

`--no-upgrade-check` is a per-run override for scripts. State lives in `~/.config/twoctl/state.json`.

## APIs covered

Every operation across all twelve published Two specs surfaces as a
`twoctl <resource> <action>` command. Actions are auto-derived from
operationId prefixes (get_/retrieve_/list_/search_, create_/make_/
fulfill_/refund_/cancel_/...) with HTTP-method fall-back when no prefix
matches. Run `twoctl api-resources` for the full machine-readable
catalog (120+ operations).

| Spec | Source |
| --- | --- |
| Checkout (Order) | [checkout-api.yaml](openapi/checkout-api.yaml) |
| Billing Account | [billing-account-api.yaml](openapi/billing-account-api.yaml) |
| Repay | [repay-api.yaml](openapi/repay-api.yaml) |
| Recourse | [recourse-api.yaml](openapi/recourse-api.yaml) |
| Company | [company-api.yaml](openapi/company-api.yaml) |
| Limits | [limits-api.yaml](openapi/limits-api.yaml) |
| Autofill | [autofill-api.yaml](openapi/autofill-api.yaml) |
| Business Registration | [business-registration-api.yaml](openapi/business-registration-api.yaml) |
| Marketplace | [marketplace-api.yaml](openapi/marketplace-api.yaml) |
| Trade Account v2 | [trade-account-v2-api.yaml](openapi/trade-account-v2-api.yaml) |
| Trade Account v3 | [trade-account-v3-api.yaml](openapi/trade-account-v3-api.yaml) |
| Webhooks | [webhooks-api.yaml](openapi/webhooks-api.yaml) |

The deprecated stubs (`search-api.page-deprecated`,
`trade-account-api-deprecated-v1`, `trade-account-api.page-deprecated`)
are not wired up.

## Regeneration

The `openapi/` specs are vendored from [`two-inc/docs`](https://github.com/two-inc/docs/tree/main/openapi).
When that source changes, a `repository_dispatch` event fires the [`regenerate.yml`](.github/workflows/regenerate.yml) workflow,
which pulls fresh specs, runs codegen, and opens a PR.

To regenerate locally:

```sh
./scripts/codegen.sh
go build ./...
```

## Contributing

This repository is generated from the OpenAPI specs in [`two-inc/docs`](https://github.com/two-inc/docs).
Bug reports and PRs for the CLI itself (flags, output formatting, ergonomics) are welcome here.
For API behaviour or schema changes, please open an issue against `two-inc/docs`.

## Telemetry

`twoctl` does not collect telemetry. There are no usage metrics, no
anonymous identifiers, no phone-home. The only outbound calls the CLI
makes are:

1. The Two API endpoint configured by your active context (e.g.
   `https://api.sandbox.two.inc`).
2. `https://api.github.com/repos/two-inc/twoctl-cli/releases/latest` for
   the daily self-upgrade check. Disable with `--no-upgrade-check`,
   `TWOCTL_SKIP_UPGRADE_CHECK=1`, or `twoctl upgrade --disable-autocheck`.

Self-upgrade downloads come from `github.com/two-inc/twoctl-cli/releases`
when you (or the prompt) chooses to install.

## Versioning + deprecation

Releases follow [semver](https://semver.org). Breaking changes to flags,
commands, or behaviour bump the major version.

When a command or flag is renamed, the old name keeps working for at
least one minor cycle and prints a deprecation note to stderr (never
stdout). The note includes the version in which the alias will be
removed.

## Licence

MIT - see [LICENSE](LICENSE).
