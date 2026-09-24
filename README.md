# Better Wings

[Pterodactyl Wings](https://github.com/pterodactyl/wings) fork with Better Files Manager,
Better Console, and Network Statistics Manager support. Release target: `better-wings-v1.2`.

## Install

Download the latest development binary for Linux amd64 or arm64:

```bash
(case "$(uname -m)" in x86_64) wings_arch=amd64 ;; aarch64|arm64) wings_arch=arm64 ;; *) printf 'Unsupported architecture\n' >&2; exit 1 ;; esac; curl -fL --retry 3 -o wings "https://github.com/Tomaxikz/daemon/releases/download/dev-latest/wings_linux_${wings_arch}" && chmod 0755 wings)
```

`dev-latest` is a prerelease. Pinned versions and `SHA256SUMS` are on the
[releases page](https://github.com/Tomaxikz/daemon/releases). Binary installs need no source patch or Go toolchain.

Back up the installed binary and configuration before replacing Wings:

```bash
sudo cp -p /usr/local/bin/wings "/usr/local/bin/wings.backup.$(date -u +%Y%m%dT%H%M%SZ)"
sudo systemctl stop wings
sudo install -o root -g root -m 0755 wings /usr/local/bin/wings
sudo systemctl start wings
sudo systemctl is-active wings
```

## Source installs

Use Go 1.26.7, or the version specified in `go.mod` for another release.

| Addon | Installer | Patch |
| --- | --- | --- |
| Better Files Manager | `betterfiles_patch.sh` | `betterfiles.patch` |
| Better Console | `betterconsolepatch.sh` | `betterconsole.patch` |
| Network Statistics Manager | `install_networkstats.sh` | `nsm.patch` |
| Full bundle | — | `tomaxikz.patch` |
| NSM upgrade for this fork | `install_networkstats.sh` | `nsm-networking.patch` |

Stock patches target `6987d5e6f0612133e031255a99655e822d30579a`.
The NSM fork-upgrade patch targets `d0e07134d8862c22ca2594526f5b879280eacff1`.
Choose one matching patch; don't stack the full bundle with addon patches.

For a clean matching checkout:

```bash
git apply --check nsm.patch
git apply nsm.patch
go test ./...
go build -o wings .
```

For NSM installation or supported upgrades, run `bash install_networkstats.sh` from the
Git source root. It requires Git, curl, Python 3 and Go; backs up changes; refuses
merge conflicts; and tests/builds without installing the binary or restarting services.
Release-attached installers use commit-pinned sources. See the
[NSM manifest](nsm-networking.manifest.json) for supported bases.

## Network Statistics Manager policies

NSM provides allocation-bound firewall rules and separate per-container upload/download
caps. Linux handles shaping with TBF + FQ-CoDel, configured through native Netlink.
Firewall management uses `nft`.

Requirements: rootful Linux Docker, a local Unix Docker socket, ordinary bridge networking,
explicit non-loopback DNS, host NET_ADMIN/SYS_ADMIN privileges, nftables bridge/conntrack
support, Linux 4.20+ Netlink strict checking, and kernel TBF/FQ-CoDel memory-limit support.
Rootless, privileged, host/shared/overlay-network and extra-network containers are unsupported,
as are ISPN and `force_outgoing_ip`.

Merge these settings into the existing Wings YAML `docker` section:

```yaml
docker:
  network_policy:
    enabled: true
    ipv6: false
    max_upload_bps: 0
    max_download_bps: 0
    runtime: ""
```

Missing settings use these defaults; explicit values are preserved. Rates are decimal
bits/s (`100000000` = 100 Mbps). Zero node ceilings mean no additional ceiling;
each server still has its own limits. Enabling support alone creates no server policy.

Stop an unmanaged server before first applying a policy. NSM creates dedicated bridges;
check host ACLs and any dependencies on old private addresses or bridge names.

The Panel backend uses these node-authenticated endpoints under `/api/servers/{uuid}`:

| Endpoint | Purpose |
| --- | --- |
| `GET /network-policy/capabilities` | Check version-1 firewall/bandwidth support |
| `PUT /network-policy` | Save a complete policy; acceptance does not mean application is finished |
| `GET /network-policy` | Check `state`, `applied_revision`, and `applied_hash` |

Use the [policy fixture](internal/networkpolicy/testdata/shared.json) for the request format.
Revisions must increase when changing policy. Omitted/null Panel configuration retains
saved restrictions; clearing requires a newer valid policy. Wait for `state: applied`.

Important limits:

- Upload leaves the container; download enters it. Caps permit brief bursts and are not bandwidth reservations.
- Host-side SFTP, HTTP transfers and backups are outside these caps.
- Firewall rules have no implicit established-connection bypass; include needed return-traffic and DNS rules.
- Foreign firewall/queue rules are not overwritten. Enforcement failures can block startup or quarantine a server.
- This does not prevent upstream saturation or protect against a host/Docker administrator.

Back up the entire `<system.root_directory>/network-policy` directory. Before rolling back
to a binary without NSM, stop affected servers, clear policies and node ceilings through
the supported controls, and verify restrictions are removed. Do not delete saved policies
or flush the host firewall to bypass enforcement. Test on a disposable node before production.

### Optional pre-execution runtime

The default `runtime: ""` uses Wings-managed startup and reconciliation. The optional
`nsm-runtime` wrapper verifies saved limits before the game executes, including when Wings
is unavailable. It uses the existing `runc` and creates no helper containers.

Confirm the native path with `command -v runc`. Merge this into Docker's existing
`/etc/docker/daemon.json`, adjusting paths; do not replace other settings or the default runtime:

```json
{
  "runtimes": {
    "wings-nsm": {
      "path": "/usr/local/bin/wings",
      "runtimeArgs": [
        "nsm-runtime", "--config", "/etc/pterodactyl/config.yml",
        "--runc", "/usr/bin/runc", "--"
      ]
    }
  }
}
```

Validate and reload Docker configuration during a maintenance window using
[Docker's runtime instructions](https://docs.docker.com/engine/daemon/alternative-runtimes/).
The registered binary must be the same one Wings runs; the config path must match too.
Keep these files and their directories root-controlled and inaccessible to tenants.

Stop affected servers, set `docker.network_policy.runtime: wings-nsm`, and start them through
Wings to recreate their containers. Keep their Docker restart policy at `no`.
Missing or mismatched saved state blocks startup. `runc run` and checkpoint restore are unsupported.

Existing kernel queues survive a Wings crash in either mode. Live updates and drift recovery
still need Wings. To remove the wrapper, stop affected servers, clear `runtime`, and recreate
them through Wings before removing Docker registration or replacing the binary.

## Better Files APIs

### Advanced file search (Better Files / Wings-rs V2)

`POST /api/servers/{server}/files/search` supports `root`, `path_filter`, `size_filter`,
`content_filter`, and `per_page`. Optional filters accept omitted/null values. Legacy GET search remains available.

The response is `{"results": [...]}` with each name relative to `root`. Globs match paths
from the server filesystem root; size minimums are inclusive and maximums exclusive.
Content searches use literal UTF-8 queries. `include_unmatched` includes oversized,
unsearched files—not searched nonmatches.

Default/maximum results: 100/500. Default content threshold: 2 MiB. Execution is limited to
30 seconds and 1 GiB of total reads. Archive mounting and match context are unsupported.
The full schema and `x-search-limits` are in `/openapi.json`.

BFM must detect the POST schema's `path_filter`, `size_filter`, `content_filter`, and
`per_page` properties, not just the version or route. After upgrading Wings, allow capability
caches to expire and reload the file manager.

### Multipart folder uploads (Better Files)

`POST /upload/file` keeps the existing signed URL and `directory` query parameter.
For folders, send repeated `files` parts plus one text field, `paths`, containing a JSON
array of relative paths in the same order:

```json
["mods/a/config.yml", "mods/b/config.yml"]
```

Do not rely on multipart filenames to preserve directories. Paths must use `/`, remain
relative, and match each file part's basename. Missing parent directories are created.
Traversal, duplicate destinations and symlink targets are rejected; denylist, upload-size
and disk-quota checks apply. Maximum batch size is 512 files.

Use one fresh single-use token per request, including retries. Batch writes are not
transactional. Flat and resumable uploads retain their existing contracts.

Check `/openapi.json` at `paths['/upload/file'].post['x-betterfiles-folder-upload']` for
`version: 1` and `paths_field: "paths"`. Without that capability, keep per-directory
uploads to avoid older daemons flattening folder paths.

## Development and releases

```bash
go test ./...
go vet ./...
python3 -m unittest discover -s .github/scripts -p 'test_*.py'
python3 .github/scripts/validate_installers.py networkstats
```

The release workflow tests native Linux amd64/arm64 builds and installer fixtures, then
packages binaries, patches, installers and checksums. `develop` publishes prereleases;
version tags such as `better-wings-v1.2` publish versioned releases.

## Links

- [Wings installation](https://pterodactyl.io/wings/1.0/installing.html)
- [Fork issues](https://github.com/Tomaxikz/daemon/issues)
- [Upstream project](https://github.com/pterodactyl/wings)
- [License](LICENSE)
