[![Logo Image](https://cdn.pterodactyl.io/logos/new/pterodactyl_logo.png)](https://pterodactyl.io)

![Discord](https://img.shields.io/discord/122900397965705216?label=Discord&logo=Discord&logoColor=white)
![GitHub Releases](https://img.shields.io/github/downloads/pterodactyl/wings/latest/total)
[![Go Report Card](https://goreportcard.com/badge/github.com/pterodactyl/wings)](https://goreportcard.com/report/github.com/pterodactyl/wings)

# Pterodactyl Wings

Wings is Pterodactyl's server control plane, built for the rapidly changing gaming industry and designed to be
highly performant and secure. Wings provides an HTTP API allowing you to interface directly with running server
instances, fetch server logs, generate backups, and control all aspects of the server lifecycle.

In addition, Wings ships with a built-in SFTP server allowing your system to remain free of Pterodactyl specific
dependencies, and allowing users to authenticate with the same credentials they would normally use to access the Panel.

## Fork Notes

This repository is a fork of [pterodactyl/wings](https://github.com/pterodactyl/wings) with a small set of daemon-side
fixes and hardening changes. For the Rust rewrite, see [Wings-rs](https://github.com/calagopus/wings).

Notable changes in this fork include:

* File download requests return a proper not-found response when the target file does not exist, instead of bubbling up
  as an internal server error.
* File downloads use stricter path and file-type validation before serving content.
* File downloads and stream responses use `http.ServeContent`, enabling HTTP range and conditional request handling.
* File downloads are capped to the file size observed when the request starts, matching the fixed-byte behavior used by
  Wings-rs-style downloads so later file growth does not extend the response.
* Download responses include safer headers such as `X-Content-Type-Options: nosniff`.

## Clone this fork

Clone this repository's `develop` branch into a new `daemon` directory:

```bash
git clone --branch develop https://github.com/Tomaxikz/daemon.git
cd daemon
```

This is the development branch and already includes this fork's changes; do not
apply the bundled upstream patches to this checkout. Cloning downloads the source
only—it does not install or restart Wings. Keep the full Git history (no
`--depth` option) if you plan to run the installer validation commands below.

## Release pipeline

`.github/workflows/push.yaml` owns testing, packaging and publishing. Go versions
come from `go.mod`. Pull requests run with read-only repository permissions; only
the final publishing job on this repository has `contents: write`.

Before a release can publish, the workflow must complete:

- Native Linux amd64 and arm64 tests, race tests, release/debug builds, and binary
  version smoke tests.
- Clean application and builds of `betterfiles.patch`, `betterconsole.patch`,
  `nsm.patch`, and `tomaxikz.patch` against supported upstream commit `6987d5e`.
- Clean, repeat, and upgrade runs of all three smart installers, using source
  files from the same checkout. The upgrade fixtures use patches from `60f5d30`.
- Release-tool regression tests, source consistency checks, and bundle checksum
  verification. CodeQL remains a separate scheduled analysis workflow.

Every bundle contains both architectures' normal/debug binaries, all four patches,
all three installers, `README.md`, `RELEASE.json`, and portable `SHA256SUMS`.
Installers attached to releases fetch their source files from that exact commit;
the repository copies continue to default to `develop`. `TOMAXIKZ_RAW_BASE` still
allows an explicit source override.

Pushes to `develop` publish a versioned prerelease named `dev-<full commit SHA>`
before updating the existing `dev-latest` download URLs. Versioned releases are
never overwritten by this pipeline. Publishing is serialized across branches,
and an older run cannot promote itself when `develop` has advanced. The alias is
temporarily hidden while its assets are replaced and verified. Ordinary failures
restore the previous alias; a forced runner termination may require rerunning the
publishing job. Versioned snapshots remain available during alias updates.

For a stable release, push a tag such as `v1.2.3`. Tags such as `v1.2.3-rc.1` publish
prereleases. Both use the same checks and bundling process. Older stable versions
do not displace a newer stable version as GitHub's latest release. There are no
automatic release-branch commits or requests to Pterodactyl's upstream CDN.

No custom publishing secret is needed: the workflow uses its repository-scoped
`GITHUB_TOKEN`. The repository's rolling `dev-latest` release must remain mutable;
enabling repository-wide release immutability is incompatible with this alias.
The workflow itself refuses to move or replace versioned releases. To roll back,
download a binary from a previous versioned release, replace Wings, and restart it.

Local validation on Linux with Go from `go.mod`:

```bash
python3 -m unittest discover -s .github/scripts -p 'test_*.py' -v
python3 .github/scripts/validate_installers.py betterfiles
python3 .github/scripts/validate_installers.py betterconsole
python3 .github/scripts/validate_installers.py networkstats
python3 .github/scripts/validate_installers.py full
python3 .github/scripts/release.py build --arch amd64 --directory /tmp/wings-build
```

The build command requires the matching native Linux architecture. Publishing is
restricted to trusted GitHub workflow events; local validation never publishes.

## Sponsors

I would like to extend my sincere thanks to the following sponsors for helping fund Pterodactyl's development.
[Interested in becoming a sponsor?](https://github.com/sponsors/pterodactyl)

| Company                                                                           | About                                                                                                                                                                                                                                           |
|-----------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| [**Infraly, LLC**](https://infraly.co/)                                           | Infraly is an infrastructure company powering the next generation of online services. Through their brands, Infraly delivers cutting-edge solutions across multiple markets. Their vertically integrated approach provides unmatched performance, scalability, and reliability, giving our customers full control.                                                                                     |
| [**Hosturly**](https://hosturly.com/)                                             | Hosturly is an enterprise hosting provider. They provide cost-effective, high-performance, and reliable services, including VPS, Web, Dedicated, and Colocation.                                                                                |
| [**Physgun**](https://physgun.com/)                                               | Physgun is a game server hosting provider. Most providers rent rack space and rebrand a panel. At Physgun, they engineer the performance, write the features, and staff the support. Physgun truly is game hosting perfected!                   |
| [**WISP**](https://wisp.gg/)                                                      | WISP is an industry-leading SaaS platform for game server management, designed for hosting companies, gaming organizations, and enthusiasts. WISP combines modern, intuitive interfaces with powerful tools, making server deployment and administration seamless, scalable, and efficient.                                                                                                                 |
| [**Buildurly**](https://buildurly.com/)                                           | Buildurly is a hardware procurement company. They deliver tailored, enterprise-grade hardware solutions designed around your unique needs. From sourcing to delivery, Buildurly's white-glove service ensures a seamless, worry-free, professional experience.                                                                                                                                          |
| [**indifferent broccoli**](https://indifferentbroccoli.com/)                      | indifferent broccoli is a game server hosting and rental company. With them, you get top-notch computer power for your gaming sessions. They destroy lag, latency, and complexity--letting you focus on the fun stuff.                         |

## Documentation

### Advanced file search (Better Files / Wings-rs V2)

The existing authenticated `POST /api/servers/{server}/files/search` accepts the
BFM V2 JSON contract. The legacy `GET` search remains unchanged. For example:

```json
{
  "root": "/plugins",
  "path_filter": {
    "include": ["**/*.yml"],
    "exclude": ["**/cache/**"],
    "case_insensitive": true
  },
  "size_filter": {"min": 0, "max": 10485760},
  "content_filter": {
    "query": "enabled",
    "max_search_size": 2097152,
    "include_unmatched": false,
    "case_insensitive": true
  },
  "per_page": 100
}
```

All three filters may be omitted or `null`. Empty, omitted or null `root` means
`/`; omitted or null `per_page` defaults to 100. Results use the existing directory
entry fields: `name`, `size`, `size_physical`, `directory`, `file`, `symlink`,
`mime`, `created`, and `modified`. Only regular files are returned. A file at
`/plugins/a/config.yml` has `name: "a/config.yml"` for this root; another at
`/plugins/b/config.yml` remains a separate result. Empty results are exactly
`{"results":[]}`. Ordering is unspecified; `per_page` caps results, not a pageable
cursor. Physical size and creation/change time fall back to logical size and
modification time when that metadata is unavailable.

Compatibility follows the BFM-consumed subset of Calagopus Wings'
[search implementation at d3eced7](https://github.com/calagopus/wings/blob/d3eced76461845ce4833eb0c849b46e3c9a4b572/application/src/routes/api/servers/_server_/files/search.rs):

- Globs use gitignore-style rules against paths from the **server filesystem
  root**, not the requested search directory. `*.yml` matches basenames at any
  depth; `/plugins/*.yml` is root-anchored. `**`, character classes, single-component
  brace alternatives, escaped leading `#`/`!`, comments and ordered `!` negation
  are supported. Exclusions win over includes. Excluded directories are pruned,
  so later rules cannot re-include their descendants. A trailing `/` matches
  directories only; `cache/**` matches descendants, not `cache` itself.
- Empty/omitted/null include and exclude arrays apply no restriction. Other
  include arrays require a positive matching rule (an array containing only
  comments or negative rules matches no files). Path and content case-insensitive
  matching fold ASCII only. Glob `?` matches one byte, not one Unicode character.
- Size `min` is inclusive and `max` is exclusive. Omitted/null bounds are
  unbounded. Equal bounds match nothing; negative or reversed bounds are errors.
- Content queries are literal UTF-8, not regular expressions. The first 128 bytes
  provide the upstream UTF-8 heuristic: an incomplete trailing character is
  accepted, NUL bytes are allowed, and invalid UTF-8 after that prefix does not
  disqualify a file. Matching streams through bounded buffers, preserves matches
  across buffer boundaries, and stops on the first match.
- `max_search_size` defaults to 2 MiB when omitted/null. Files larger than it are
  not content-searched. Explicit zero is preserved. `include_unmatched: true`
  includes these oversized/unsearched files (even binary files), **not** searched
  nonmatches or files rejected by the UTF-8 head check. Empty/omitted/null queries
  match eligible files passing that check, including empty files. Both content
  booleans default to false, as does path `case_insensitive`.

Search uses Wings' sandbox and denylist, refuses symlink roots and skips symlinks
and special files. Directory traversal uses bounded batches and pinned directory
handles. Unopenable regular files are skipped; read failures abort the request.
Resource limits deliberately bound the upstream-compatible subset:

| Limit | Value |
| --- | --- |
| JSON body | 1 MiB |
| Results | Default 100; maximum 500 (larger values capped); zero returns `[]` |
| Include + exclude patterns | 128 total; 64 KiB combined; 4096 UTF-8 bytes each |
| Brace expansion | 64 combinations per pattern; no nested/empty or cross-directory alternatives |
| Root / content query | 4096 UTF-8 bytes each; root path components at most 255 bytes |
| Entries / directories / depth below root | 250,000 / 16,384 including root / 256 |
| Content size threshold | Default 2 MiB; accepted range 0–1 GiB per file |
| Total file reads | 1 GiB per request, including up to 128 MIME bytes per unsearched file |
| Search execution | 30 seconds, also canceled when the request context ends |

Per-file content reads stay within `max_search_size`, even if the file grows.
Unknown fields (including `match_context`), V1 POST payloads, malformed JSON and
wrong types return `400`; an oversized body returns `413`. Invalid roots, filters
or exhausted traversal/read budgets return `422`, forbidden roots `403`, missing
roots `404`, and cancellation/timeouts `408`. Execution errors return no partial
result list. Archive/backup mounting and optional match context are unsupported;
this endpoint searches only the live server filesystem. Unsupported or invalid
globs are rejected, never silently dropped.

BFM detects advanced search from the actual `GET /openapi.json` response:

```javascript
const properties = openapi.paths?.['/api/servers/{server}/files/search']?.post
    ?.requestBody?.content?.['application/json']?.schema?.properties;
const canSearchV2 = ['path_filter', 'size_filter', 'content_filter', 'per_page']
    .every((key) => Object.prototype.hasOwnProperty.call(properties ?? {}, key));
```

A version string, legacy GET operation or schema-less POST is insufficient. The
inline schema describes the nested contract and response; `x-search-limits`
reports execution bounds. No Panel API changes or capability-check weakening are
needed. Rebuild and replace the daemon binary, then restart it when deploying.
BFM's backend capability cache lasts 60 seconds and its frontend cache 30 seconds;
allow both to expire (up to about 90 seconds in sequence) and reload the file manager.

Source installations can use the updated `betterfiles.patch` or `tomaxikz.patch`.
`betterfiles_patch.sh` already downloads these handler, OpenAPI and test files in
place, so its logic does not need changing. Use files from the same updated
revision/release, then rebuild; updating documentation alone changes no running
daemon. Binary installations need only the newly built daemon, not a source patch.

### Multipart folder uploads (Better Files)

`POST /upload/file` accepts the existing signed upload URL and `directory` query
parameter. A request without a `paths` field keeps the normal flat-upload behavior.
Resumable `HEAD`/`PATCH` uploads keep their existing contract.

Folder batches add **one multipart text field named `paths`**, containing a JSON
array of relative paths. Its entries match the repeated `files` parts **in order**.
The field may appear before or after the file parts. Do not rely on a filename such
as `mods/a/config.yml`: Go's multipart parser strips its directory components.

```javascript
const form = new FormData();
form.append('files', firstConfigFile, 'config.yml');
form.append('files', secondConfigFile, 'config.yml');
form.append('paths', JSON.stringify(['mods/a/config.yml', 'mods/b/config.yml']));

const url = new URL(signedUploadUrl);
url.searchParams.set('directory', '/uploads');
await fetch(url, { method: 'POST', body: form });
// Let the browser supply Content-Type and its multipart boundary.
```

This writes `/uploads/mods/a/config.yml` and `/uploads/mods/b/config.yml`. Missing
parent directories are created with server ownership. Paths use `/`, are relative
to `directory`, and must have the same basename as the corresponding file part.
They must not contain empty, `.` or `..` components, backslashes, NUL, a leading
slash, or a Windows drive prefix. Paths are literal JSON strings, not URL-encoded
values. Invalid UTF-8, paths over 4096 bytes, components over 255 bytes, duplicate
destinations, and file/directory conflicts are rejected. Duplicate basenames in
different directories are supported.

Folder batches accept at most **512 files per request**. File bytes must fit both
the configured upload limit (per file and in total) and the server's disk quota.
The HTTP body is additionally bounded by the upload limit plus 8 MiB for multipart
metadata. The client-supplied `total_size` does not override these checks. Ignored
files and directories are enforced, and writes refuse symlinks, hardlinks and
non-regular targets. Disk growth is reserved before overwriting each file.

Use **one fresh single-use upload token for the entire POST request**. Tokens are
consumed even when validation fails; never reuse one for a retry or for resumable
uploads. Success remains an empty `200` response. Invalid input/limits return
`400`, denied paths `403`, unavailable/consumed tokens `404`, an active same-file
upload `409`, and an oversized HTTP body `413`. Batch writes are not transactional:
validation is performed before writing, but a runtime failure may leave earlier
files written. Retry with a fresh token after inspecting the error.

BFM must detect support from `GET /openapi.json` before sending `paths`:

```javascript
const support = openapi.paths?.['/upload/file']?.post?.['x-betterfiles-folder-upload'];
const canUploadFolders = support?.version === 1 && support.paths_field === 'paths';
// support.max_files is 512; split larger selections into separate requests/tokens.
```

Checking for `POST /upload/file` alone is insufficient: older Wings versions may
ignore `paths` and flatten the batch. If the capability is absent, keep the
existing per-directory multipart uploads or per-file resumable uploads.

Source installations can use `betterfiles_patch.sh`, `betterfiles.patch`, or the
full `tomaxikz.patch`, then rebuild and replace Wings. The smart installer updates
recognized upload implementations in place; custom upload modifications require
a reviewed merge. The new supporting files are `server/filesystem/upload.go`,
`server/filesystem/upload_test.go`, and `router/router_upload_batch_test.go`.

* [Panel Documentation](https://pterodactyl.io/panel/1.0/getting_started.html)
* [Wings Documentation](https://pterodactyl.io/wings/1.0/installing.html)
* [Community Guides](https://pterodactyl.io/community/about.html)
* Or, get additional help [via Discord](https://discord.gg/pterodactyl)

## Reporting Issues

Please use the [pterodactyl/panel](https://github.com/pterodactyl/panel) repository to report any issues or make
feature requests for Wings. In addition, the [security policy](https://github.com/pterodactyl/panel/security/policy) listed
within that repository also applies to Wings.
