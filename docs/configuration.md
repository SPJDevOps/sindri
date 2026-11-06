# Configuration reference

Sindri reads one file, `sindri.yaml`. `sindri serve --config <file>` names it; with
no flag, `serve` reads `./sindri.yaml` if it exists and runs on the defaults below if
it does not. A file you name that is missing is an error. An empty file means
"everything default".

Check a file without a database or a server:

```bash
sindri config check -f sindri.yaml
sindri config show --expanded -f sindri.yaml
```

`config show` prints the server sections with every secret replaced by `<redacted>`.
`--expanded` also prints the repositories, lifecycle, and defaults as the long-form
documents the server acts on, with every preset, inference, and default written out.

This page is written by hand from `internal/config`. It lists what the loader accepts
and what each key does. Where the code sets a default, the page states it; where it does
not, the key is simply unset.

## Rules that apply everywhere

- **Unknown keys are an error.** A typo fails at load instead of silently keeping a
  default.
- **Environment references.** Any string value may contain `${VAR}` or
  `${VAR:-default}`. `$$` writes a literal dollar sign. Expansion happens on parsed
  values, so a password containing `:` or `#` cannot change the meaning of the file.
  An unset variable with no default is an error.
- **Durations** are Go duration strings with a unit: `30s`, `5m`, `1h`, `168h`. A bare
  number such as `3600` is rejected, because guessing a unit is how a five minute
  cache becomes a one hour one. Negative durations are rejected.
- **Short and long forms.** Most sections accept a one-line short form and a mapping
  long form. They produce the same configuration; nothing downstream can tell which was
  written. The tables below give both.
- **Secrets stay out of the database.** The database holds the name of a credential, not
  its value.

## Server settings

These keys configure the process. They have no manifest form: manifests describe
repositories and lifecycle, so `listen`, `storage`, `database`, `log`, `auth`, `worker`,
`credentials`, and `publicUrl` appear in `sindri.yaml` only.

### `listen`

Short form: `listen: :8080` sets `address`.

| Key | Meaning | Default |
|---|---|---|
| `address` | Address the HTTP server binds. Must not be empty. | `:8080` |
| `readHeaderTimeout` | How long a client may take to send request headers. | `10s` |
| `shutdownTimeout` | How long shutdown waits for requests in flight. | `20s` |

### `storage`

Where blobs are written. Short forms: `storage: ./data` is the filesystem backend with
that path; `storage: s3://bucket` or `storage: s3://bucket/prefix` is the S3 backend.

| Key | Meaning | Default |
|---|---|---|
| `backend` | `filesystem` or `s3`. | `filesystem` |
| `path` | Directory for blobs. Filesystem only; required there. A relative path is resolved against the configuration file's directory. | `./data` |
| `bucket` | Bucket name. S3 only; required there. | none |
| `prefix` | Key prefix inside the bucket. S3 only. | none |
| `region` | S3 region. Compatible stores ignore it, but the SDK needs one. | `us-east-1` |
| `endpoint` | URL of an S3-compatible store such as MinIO. S3 only. | none, the AWS endpoint |
| `pathStyle` | Use path-style addressing, which most S3-compatible stores need. S3 only. | `false` |
| `accessKeyId` | Static access key. Set it together with `secretAccessKey`, or set neither. | none |
| `secretAccessKey` | Static secret key. Redacted by `config show`. | none |
| `sessionToken` | Session token for temporary credentials. Redacted by `config show`. | none |

Leave the three credential keys out to use the ambient AWS credential chain. Setting an
S3-only key on the filesystem backend, or `path` on S3, is an error.

### `database`

Postgres is the only supported store. Short form: `database: postgres://...` sets `url`.

| Key | Meaning | Default |
|---|---|---|
| `url` | Connection URL. The scheme must be `postgres` or `postgresql`. Passwords are redacted in logs and `config show`. | `postgres://sindri:sindri@127.0.0.1:55432/sindri?sslmode=disable`, the development stack's |
| `maxConns` | Pool size. At least 1. | `10` |
| `minConns` | Connections kept open. Between 0 and `maxConns`. | `0` |
| `connectTimeout` | How long to wait for a connection. | `10s` |

The default URL carries a development-only credential. Never deploy it.

### `log`

Short form: `log: debug` sets `level`.

| Key | Meaning | Default |
|---|---|---|
| `level` | `debug`, `info`, `warn`, or `error`. | `info` |
| `format` | `json` or `text`. | `json` |

### `auth`

Short form: `auth: ${SINDRI_TOKEN}` sets only `static_token`. Every request
authenticates; there is no anonymous access. See
[ADR 18](adr/0018-authentication-and-rbac-in-v1.md).

| Key | Meaning | Default |
|---|---|---|
| `static_token` | The bootstrap administrator credential, and the break-glass path once an identity provider is set. Compared in constant time, never logged, redacted by `config show`. | `sindri-dev-token`, which logs a warning while it is in use |
| `admins` | Identity-provider groups whose members administer the server. Entries must not be empty. Membership is read from the caller's token on each request and never stored. | none |
| `oidc` | The identity provider. Without it only the bootstrap token and machine tokens authenticate, and the web UI cannot sign anyone in. | absent |

Note that `static_token` is spelled with an underscore, unlike the camelCase keys
around it.

#### `auth.oidc`

| Key | Meaning | Default |
|---|---|---|
| `issuer` | The provider's issuer URL. Required. Must be `https`, or `http` for a loopback address. | none |
| `clientId` | OAuth client id. Required. | none |
| `clientSecret` | OAuth client secret. Redacted by `config show`. | none |
| `redirectUrl` | The browser login callback, for example `https://sindri.example.com/auth/callback`. Setting it enables UI sign-in. Same scheme rule as `issuer`. The session cookie is `Secure` exactly when this is `https`. | none |
| `groupsClaim` | Token claim listing the caller's groups. | `groups` |
| `usernameClaim` | Token claim recorded as the actor in the audit ledger. | `email` |
| `scopes` | Scopes the browser login requests. Providers that release groups only on request, such as Dex, need `groups` added. | `openid`, `profile`, `email` |

### `publicUrl`

The origin clients reach this server at, for example `https://sindri.example.com`.
npm packuments carry absolute tarball URLs, and a request's `Host` header is not
trusted to build them (see [ADR 24](adr/0024-npm-packument-and-dist-tags.md)).

- **Required** when the file declares any npm repository, in the short or the long form.
  Loading fails otherwise.
- Must be an origin: `https` (or `http` for a loopback address), a host, no credentials,
  no path, query, or fragment. Sindri serves at the root of its origin.
- A trailing slash is removed.
- Unset by default.

### `credentials`

Upstream secrets, which proxy repositories name by key. A map from a reference name to
a login. Reference names are lowercase letters, digits, `-`, and `_`, at most 63
characters, and may not start with `-` or `_`. Values are normally written as
`${VAR}`.

```yaml
credentials:
  dockerhub-login:
    username: ${DOCKERHUB_USER}
    password: ${DOCKERHUB_TOKEN}
```

| Key | Meaning | Default |
|---|---|---|
| `<name>.username` | Upstream user name. | none |
| `<name>.password` | Upstream password or token. Required. | none |

A repository refers to a credential with `upstream.credentials: <name>`. Both fields
are redacted by `config show`. Naming a reference that is not defined here is refused
when the repository is written. Reloading the file with `SIGHUP` swaps the secrets
atomically.

A proxy that has credentials fails a virtual repository's walk when its upstream refuses
them, instead of reading the refusal as "no such name". See
[ADR 14](adr/0014-proxy-cache-and-virtual-resolution.md). Token-authenticated
private npm upstreams are not supported yet (SIN-53).

### `worker`

The background worker evicts stale proxy cache entries and abandoned uploads, and runs
scheduled garbage collection. Short form: `worker: false` turns it off and
`worker: true` is the default. See
[ADR 20](adr/0020-queue-table-and-background-worker.md).

| Key | Meaning | Default |
|---|---|---|
| `enabled` | Run a worker inside `serve`. With it off, the server still enqueues work, such as `sindri gc`, for a worker elsewhere to claim. An absent `enabled` in the mapping means `true`. | `true` |
| `gc.interval` | How often to schedule garbage collection. Zero never schedules it: "untagged and unpromoted is garbage" is a policy to adopt after a dry run. `sindri gc` works either way. | unset (never) |
| `gc.grace` | Blobs seen more recently than this are kept, whatever references them. | `24h` |
| `retention.interval` | How often the cleanup jobs run. | `1h` |
| `retention.uploadMaxIdle` | How long a staged upload may go unwritten before it is discarded. At least `1m`. | `24h` |
| `retention.proxyStaleAfter` | How long past its TTL a proxy cache entry is kept to serve through an upstream outage. Also how long a cached file that upstream no longer lists is kept before its row is removed. | `168h` (7 days) |

## Repositories, lifecycle, and defaults

These three sections describe desired state. `serve` reconciles them against the
database on start and on `SIGHUP`. `sindri apply` converges the same documents from CI.
A file with none of these sections leaves repositories API-managed. See
[ARCHITECTURE.md section 8](https://github.com/SPJDevOps/sindri/blob/main/ARCHITECTURE.md#8-configuration-as-code) and
[ADR 17](adr/0017-config-kinds-namespaces-and-reconciliation.md).

The short form below desugars into the long form (enveloped manifests) one way and
completely. `testdata/config/sindri.yaml` and `testdata/manifests/` spell one setup both
ways, and a golden test keeps them identical.

### `repos`

A map from repository name to its entry. A hosted, proxy, or virtual repository is
distinguished by what the entry contains, not by a flag.

| Key | Meaning |
|---|---|
| `protocol` | `oci`, `pypi`, or `npm`. Required for a hosted repository, and for a proxy with an explicit `url`. A proxy with a preset takes the preset's protocol; a virtual repository takes its members'. |
| `upstream` | Makes the repository a **proxy**. A preset name, or a mapping (below). |
| `members` | Makes the repository **virtual**: an ordered list of repository names. Every member must share one protocol. |
| `type` | `hosted`, `proxy`, or `virtual`. Optional; if written it must agree with what `members` and `upstream` imply. |
| `read` | Groups that may read. Entries are group names as the identity provider reports them, or `authenticated` for any signed-in caller. |
| `write` | Groups that may write. Write implies read. |
| `lifecycle` | Hosted repositories only: the stages artifacts may be promoted into, as a list, or as a mapping with `stages`. Empty means every stage. |

An entry with neither `members` nor `upstream` is hosted. An entry with both is an
error: declare two repositories. A member written in another namespace needs its type as
a prefix, such as `hosted/org/internal-py`.

`upstream` takes a preset name, which is one of `pypi`, `dockerhub`, `ghcr`, `npm`, or
`maven-central` (reserved; this build does not serve Maven). Or it takes a mapping:

| Key | Meaning | Default |
|---|---|---|
| `preset` | A preset name, as above. | none |
| `url` | The upstream's URL. Needed when there is no preset. | the preset's |
| `ttl` | How long a cached answer is fresh. `0` never expires, so the cache is never refreshed and Sindri never notices upstream removing a file. Always set it for an explicit `url`. | the preset's, `1h` for every preset; `0` for an explicit `url` |
| `negativeTtl` | How long "not found" is remembered. `0` means it is not cached. | the preset's, `5m` for every preset; `0` for an explicit `url` |
| `credentials` | A key of the top-level `credentials:` map. | none |

Precedence, lowest first: the preset, then `defaults.proxy`, then what the repository
writes. A mapping with an explicit `url` and no `ttl` or `negativeTtl` leaves both at
`0`. An unset `ttl` of `0` never expires: a cached hit is always fresh and never
revalidated, so that proxy never refreshes its cache, and since SIN-54 never notices
upstream removing a file, because a refresh is what flags it. An unset `negativeTtl` of
`0` means "not found" is not cached. Always set `ttl` for an explicit `url`.

The presets expand to these URLs: `pypi` is `https://pypi.org/simple`, `dockerhub` is
`https://registry-1.docker.io`, `ghcr` is `https://ghcr.io`, and `npm` is
`https://registry.npmjs.org`.

### `lifecycle`

The ordered stages and the evidence each requires.

| Key | Meaning |
|---|---|
| `stages` | An ordered list. Each entry is a stage name, or a mapping with `name` and `gates`. |
| `gates` | A map from a stage name to the evidence types required to promote into it, for example `prod: [sbom, signature]`. The stage must be declared in `stages`, and a stage's gates are written in one place, not both. |

Lifecycle applies to hosted repositories only. Stages are organisation-wide.

### `defaults`

Values applied to every repository of a type that does not set them itself. There is
no `extends:`; YAML anchors cover the rest. The keys are the repository types.

| Key | Allowed fields |
|---|---|
| `defaults.hosted` | `read`, `write`, `lifecycle` |
| `defaults.proxy` | `read`, `write`, `ttl`, `negativeTtl`, `credentials` |
| `defaults.virtual` | `read`, `write` |

## Long form: manifests

`sindri apply -f <file-or-directory>` and `sindri diff` read enveloped documents,
several per file separated by `---`. Every document has `kind`, `metadata`, and `spec`.
All three kinds describe repositories and lifecycle, so none of them has a place for
server settings.

### `kind: Repository`

`metadata.name` is required. `metadata.namespace` names the owning team; omit it for an
organisation repository. A namespaced repository's full name is `namespace/name`.

| Key | Meaning |
|---|---|
| `spec.type` | `hosted`, `proxy`, or `virtual`. Required. |
| `spec.protocol` | `oci`, `pypi`, or `npm`. Required. |
| `spec.upstream.url` | Proxy only. The upstream URL. |
| `spec.upstream.ttl` | Proxy only. Fresh time. Unset is `0`, which never expires and never refreshes the cache, so always set it. |
| `spec.upstream.negativeTtl` | Proxy only. How long "not found" is remembered. Unset is `0`: not cached. |
| `spec.upstream.credentials` | Proxy only. A key of the server's `credentials:` map. |
| `spec.members[].ref` | Virtual only. `<type>/<name>`, for example `hosted/internal-js`; `org/<name>` or `<team>/<name>` reaches across a namespace. |
| `spec.access.read` | Groups that may read, or `authenticated`. |
| `spec.access.write` | Groups that may write. |
| `spec.lifecycle.stages` | Hosted only. The stages artifacts may be promoted into. |

### `kind: Lifecycle`

| Key | Meaning |
|---|---|
| `spec.stages[].name` | The stage name. |
| `spec.stages[].gates` | Evidence types required to promote into the stage. |

### `kind: OrgPolicy`

Optional. Absent means every team declares freely. Only one may exist.

| Key | Meaning |
|---|---|
| `spec.mode` | `centralized` (only organisation documents declare repositories), `delegated` (teams declare freely, the default), or `hybrid` (teams declare their own, and the mandatory repositories must exist). |
| `spec.mandatoryRepos[].name` | An organisation repository that must exist. |
| `spec.mandatoryRepos[].locked` | Teams may reference it and never override it. |
| `spec.namespaces[].name` | A team namespace. |
| `spec.namespaces[].admins` | Groups that may apply that namespace's manifests. |

## Ownership

The reconciler owns rows marked `managed_by = config`, and records which trigger owns
each: `serve --config` or `sindri apply`. The API, the reconciler, and the two triggers
never change a row another owns. The reconciler never deletes without `--prune`. See
[ADR 17](adr/0017-config-kinds-namespaces-and-reconciliation.md).
