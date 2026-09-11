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
