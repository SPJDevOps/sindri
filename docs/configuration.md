# Configuration reference

<!-- commands checked by: TestDocs_ConfigurationCommandsRun -->
<!-- commands checked by: TestDocs_ConfigurationExamplesLoad -->

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

The server settings tables are generated from the structs in `internal/config` by
`make reference`, and a test fails when the page and the code disagree. The tables for
repositories, lifecycle, defaults, and manifests are written by hand, and a test checks that they
list exactly the keys the loader accepts. Where the code sets a default, the page states
it; where it does not, the key is unset.

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
`scanning`, `credentials`, and `publicUrl` appear in `sindri.yaml` only.

### `listen`

The HTTP listener.

<!-- BEGIN GENERATED: listen -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `listen: :8080` is the same as `listen: {address: ":8080"}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `address` | string | Address the HTTP server binds. Must not be empty. | `:8080` |
| `readHeaderTimeout` | duration | How long a client may take to send request headers. | `10s` |
| `shutdownTimeout` | duration | How long shutdown waits for requests in flight. | `20s` |
<!-- END GENERATED: listen -->

### `storage`

Where blobs are written.

<!-- BEGIN GENERATED: storage -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `storage: ./data` is the same as `storage: {backend: filesystem, path: ./data}`.

Short form: `storage: s3://bucket/prefix` is the same as `storage: {backend: s3, bucket: bucket, prefix: prefix}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `backend` | string | `filesystem` or `s3`. | `filesystem` |
| `path` | string | Directory for blobs. Filesystem only, and required there. A relative path is resolved against the configuration file's directory. | `./data` |
| `bucket` | string | Bucket name. S3 only, and required there. | unset |
| `prefix` | string | Key prefix inside the bucket. S3 only. | unset |
| `region` | string | S3 region. Compatible stores ignore it, but the SDK needs one. | `us-east-1` on S3 |
| `endpoint` | string | URL of an S3-compatible store such as MinIO. S3 only. | unset, which is AWS |
| `pathStyle` | boolean | Use path-style addressing, which most S3-compatible stores need. S3 only. | `false` |
| `accessKeyId` | string | Static access key. Set it together with `secretAccessKey`, or leave both out to use the ambient AWS credential chain. | unset |
| `secretAccessKey` | string | Static secret key. Redacted by `config show`. | unset |
| `sessionToken` | string | Session token for temporary credentials. Redacted by `config show`. | unset |
<!-- END GENERATED: storage -->

Setting an S3-only key on the filesystem backend, or `path` on S3, is an error.

### `database`

Postgres is the only supported store.

<!-- BEGIN GENERATED: database -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `database: postgres://sindri.example.com/sindri` is the same as `database: {url: "postgres://sindri.example.com/sindri"}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `url` | string | Connection URL. The scheme must be `postgres` or `postgresql`. The password is redacted in logs and by `config show`. The default is the development stack's, with a development-only credential: never deploy it. | `postgres://sindri:sindri@127.0.0.1:55432/sindri?sslmode=disable` |
| `maxConns` | integer | Pool size. At least 1. | `10` |
| `minConns` | integer | Connections kept open. Between 0 and `maxConns`. | `0` |
| `connectTimeout` | duration | How long to wait for a connection. | `10s` |
<!-- END GENERATED: database -->

### `log`

The structured logger.

<!-- BEGIN GENERATED: log -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `log: debug` is the same as `log: {level: debug}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `level` | string | `debug`, `info`, `warn`, or `error`. | `info` |
| `format` | string | `json` or `text`. | `json` |
<!-- END GENERATED: log -->

### `auth`

Every request authenticates; there is no anonymous access. See
[ADR 18](adr/0018-authentication-and-rbac-in-v1.md). Without `oidc`, only the bootstrap
token and machine tokens authenticate, and the web UI cannot sign anyone in. Note that
`static_token` is spelled with an underscore, unlike the camelCase keys around it.

<!-- BEGIN GENERATED: auth -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `auth: ${SINDRI_TOKEN}` is the same as `auth: {static_token: "${SINDRI_TOKEN}"}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `static_token` | string | The bootstrap administrator credential: what `sindri apply` uses before an identity provider is configured, and the break-glass path after. Compared in constant time, never logged, and redacted by `config show`. The default logs a warning while it is in use. | `sindri-dev-token` |
| `admins` | list of strings | Identity-provider groups whose members administer the server. Entries must not be empty. Membership is read from the caller's token on each request and never stored. | unset |
| `oidc.issuer` | string | The provider's issuer URL. Required. Must be `https`, or `http` for a loopback address. | unset |
| `oidc.clientId` | string | OAuth client id. Required. | unset |
| `oidc.clientSecret` | string | OAuth client secret. Redacted by `config show`. | unset |
| `oidc.redirectUrl` | string | The browser login callback, for example `https://sindri.example.com/auth/callback`. Setting it enables UI sign-in. Same scheme rule as `issuer`. The session cookie is `Secure` exactly when this is `https`. | unset |
| `oidc.groupsClaim` | string | Token claim listing the caller's groups. | `groups` |
| `oidc.usernameClaim` | string | Token claim recorded as the actor in the audit ledger. | `email` |
| `oidc.scopes` | list of strings | Scopes the browser login requests. Providers that release groups only on request, such as Dex, need `groups` added. | `openid`, `profile`, `email` |
<!-- END GENERATED: auth -->

### `publicUrl`

npm packuments carry absolute tarball URLs, and a request's `Host` header is not trusted
to build them (see [ADR 24](adr/0024-npm-packument-and-dist-tags.md)), so a server with an
npm repository needs to know its own origin. Sindri serves at the root of that origin.
Loading fails when an npm repository is declared, in the short or the long form, and
`publicUrl` is unset.

<!-- BEGIN GENERATED: publicUrl -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `publicUrl` | string | The origin clients reach this server at, for example `https://sindri.example.com`. Required when the file declares any npm repository. Must be `https`, or `http` for a loopback address, and an origin only: no credentials, path, query, or fragment. A trailing slash is removed. | unset |
<!-- END GENERATED: publicUrl -->

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

<!-- BEGIN GENERATED: credentials -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `<name>.username` | string | Upstream user name. Redacted by `config show`. | unset |
| `<name>.password` | string | Upstream password, sent as HTTP Basic with `username`. Set it, or `token`, never both. Redacted by `config show`. | unset |
| `<name>.token` | string | Upstream token, sent as HTTP Bearer. Set it instead of `username` and `password`. An OCI proxy cannot use one. Redacted by `config show`. | unset |
<!-- END GENERATED: credentials -->

A repository refers to a credential with `upstream.credentials: <name>`. Naming a
reference that is not defined here is refused when the repository is written. Reloading
the file with `SIGHUP` swaps the secrets atomically.

A proxy that has credentials fails a virtual repository's walk when its upstream refuses
them, instead of reading the refusal as "no such name". See
[ADR 14](adr/0014-proxy-cache-and-virtual-resolution.md).

A credential is either `username` and `password` (sent as HTTP Basic) or `token` (sent as
HTTP Bearer), never both. A private npm registry that wants a token takes the second form.
Both are sent only to the upstream's own origin, with one exception (ADR 14): an OCI
proxy's username and password also go to the registry's token realm, but only over https
or on the upstream's own origin. An OCI proxy cannot use a token, because
OCI registries exchange username and password at their token realm; naming one is refused
when the repository is written. See [ADR 27](adr/0027-upstream-credentials-may-be-a-bearer-token.md).

```yaml
credentials:
  npm-private:
    token: ${NPM_TOKEN}
```

### `worker`

The background worker evicts stale proxy cache entries and abandoned uploads, and runs
scheduled garbage collection. See
[ADR 20](adr/0020-queue-table-and-background-worker.md).

<!-- BEGIN GENERATED: worker -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `worker: false` is the same as `worker: {enabled: false}`.

Short form: `worker: true` is the same as `worker: {enabled: true}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `enabled` | boolean | Run a worker inside `serve`. With it off, the server still enqueues work, such as `sindri gc`, for a worker elsewhere to claim. An absent `enabled` in the mapping means `true`. | `true` |
| `gc.interval` | duration | How often to schedule garbage collection. Unset never schedules it: "untagged and unpromoted is garbage" is a policy to adopt after a dry run, not one to inherit by upgrading. `sindri gc` works either way. | unset |
| `gc.grace` | duration | Blobs seen more recently than this are kept, whatever references them. | `24h` |
| `retention.interval` | duration | How often the cleanup jobs run. | `1h` |
| `retention.uploadMaxIdle` | duration | How long a staged upload may go unwritten before it is discarded. At least `1m`. | `24h` |
| `retention.proxyStaleAfter` | duration | How long past its TTL a proxy cache entry is kept to serve through an upstream outage, before retention evicts it. Also how long a cached file that upstream no longer lists is kept before its row is removed. | `168h` |
<!-- END GENERATED: worker -->

### `scanning`

Vulnerability scanning is off by default. When it is on, the worker scans hosted
publishes only; proxied content is never scanned. It needs a `trivy` executable, which
the `-trivy` release image ships. See [Scanning and the severity gate](scanning.md) and
[ADR 29](adr/0029-what-a-scan-covers.md).

<!-- BEGIN GENERATED: scanning -->
<!-- Generated from internal/config by `make reference`. Edit the struct comments, not this table. -->

Short form: `scanning: false` is the same as `scanning: {enabled: false}`.

Short form: `scanning: true` is the same as `scanning: {enabled: true}`.

| Key | Type | Meaning | Default |
|---|---|---|---|
| `enabled` | boolean | Scan hosted publishes for known vulnerabilities and attach the result to the artifact. Off when the section is absent. An absent `enabled` in the mapping means `true`, because writing the section signals intent. | `false` |
| `trivy` | string | The Trivy executable. A bare name is looked up on the PATH. Sindri runs it as a separate process and never links it in. | `trivy` |
| `dbRepository` | string | A mirror of the Trivy vulnerability database, passed as `--db-repository`, for a server that cannot reach the public one. Cannot be combined with `skipDBUpdate`. | unset |
| `javaDbRepository` | string | A mirror of Trivy's Java vulnerability database, passed as `--java-db-repository`. Trivy fetches that database separately, and only when a scanned image or package holds a jar. An air-gapped install that scans jar-containing images sets this, or pre-seeds the Java database in `cacheDir`; otherwise, with `skipDBUpdate`, such a scan fails (fail closed). Cannot be combined with `skipDBUpdate`. | unset |
| `cacheDir` | string | Where Trivy keeps its database and caches between scans. Empty uses `sindri/trivy` under the user's cache directory (`$XDG_CACHE_HOME`, or `~/.cache` on Linux). The `-trivy` release image sets `$XDG_CACHE_HOME` to `/var/cache`, so there it is `/var/cache/sindri/trivy`. | unset |
| `timeout` | duration | How long one scan may run before it is cancelled and retried. At least `1m`. | `10m` |
| `skipDBUpdate` | boolean | Never download the vulnerability database, and scan with the one already in `cacheDir`, including the Java database that Trivy fetches separately when it finds a jar. For an air-gapped server with a pre-seeded cache; a jar-containing subject fails to scan if the Java database is not in it. Cannot be combined with `dbRepository` or `javaDbRepository`. | `false` |
| `maxUnpackedBytes` | integer | The most bytes unpacked from one wheel or tarball while scanning it, so that an archive bomb cannot fill the disk. At least 1048576 (1 MiB). | `1073741824` |
| `rescanInterval` | duration | How often every approved subject is scanned again, so a vulnerability published after approval is noticed (ADR 31). Default `24h`; `0s` disables the schedule, and the on-demand re-scan route still works. Otherwise at least `1m`. | `24h` |
<!-- END GENERATED: scanning -->

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

<!-- keys checked against the loader: repos -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `protocol` | string | `oci`, `pypi`, or `npm`. Required for a hosted repository, and for a proxy with an explicit `url`. A proxy with a preset takes the preset's protocol; a virtual repository takes its members'. | inferred, as described |
| `upstream` | preset name or mapping | Makes the repository a **proxy**. A preset name, or a mapping (below). | unset |
| `members` | list of strings | Makes the repository **virtual**: an ordered list of repository names. Every member must share one protocol. | unset |
| `type` | string | `hosted`, `proxy`, or `virtual`. Optional; if written it must agree with what `members` and `upstream` imply. | inferred |
| `read` | list of strings | Groups that may read. Entries are group names as the identity provider reports them, or `authenticated` for any signed-in caller. | `defaults.<type>.read`, else administrators, and machine tokens scoped to it |
| `write` | list of strings | Groups that may write. Write implies read. | `defaults.<type>.write`, else administrators, and machine tokens scoped to it |
| `lifecycle` | list of strings, or mapping | Hosted repositories only: the stages artifacts may be promoted into, as a list, or as a mapping with `stages`. | `defaults.hosted.lifecycle`, else every stage |
| `lifecycle.stages` | list of strings | The same list, in the mapping spelling. | every stage |

An entry with neither `members` nor `upstream` is hosted. An entry with both is an
error: declare two repositories. A member written in another namespace needs its type as
a prefix, such as `hosted/org/internal-py`.

`upstream` takes a preset name or a mapping. A preset is an upstream built into the
binary, so nobody hand-types the URL, and it sets these values:

<!-- BEGIN GENERATED: presets -->
<!-- Generated from internal/config/presets.go by `make reference`. Edit the presets, not this table. -->

| Preset | Protocol | URL | `ttl` | `negativeTtl` |
|---|---|---|---|---|
| `dockerhub` | `oci` | `https://registry-1.docker.io` | `1h` | `5m` |
| `ghcr` | `oci` | `https://ghcr.io` | `1h` | `5m` |
| `maven-central` | `maven`, reserved: this build does not serve it | `https://repo.maven.apache.org/maven2` | `1h` | `5m` |
| `npm` | `npm` | `https://registry.npmjs.org` | `1h` | `5m` |
| `pypi` | `pypi` | `https://pypi.org/simple` | `1h` | `5m` |
<!-- END GENERATED: presets -->

A preset names an upstream, not a repository: the repository's name is its key under
`repos:`, so `public-js: { upstream: npm }` is as valid as `npm: { upstream: npm }`.
`sindri config show --expanded` prints what each preset became.

The mapping takes these keys:

<!-- keys checked against the loader: repos.upstream -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `preset` | string | A preset name, as above. | unset |
| `url` | string | The upstream's URL. Needed when there is no preset. | the preset's |
| `ttl` | duration | How long a cached answer is fresh. `0` never expires, so the cache is never refreshed and Sindri never notices upstream removing a file. Always set it for an explicit `url`. | the preset's; else `defaults.proxy.ttl`; else `0` |
| `negativeTtl` | duration | How long "not found" is remembered. `0` means it is not cached. | the preset's; else `defaults.proxy.negativeTtl`; else `0` |
| `credentials` | string | A key of the top-level `credentials:` map. | unset |

Precedence, lowest first: the preset, then `defaults.proxy`, then what the repository
writes. A mapping with an explicit `url`, and no `ttl` or `negativeTtl` here or in
`defaults.proxy`, leaves both at `0`. An unset `ttl` of `0` never expires: a cached hit is always fresh and never
revalidated, so that proxy never refreshes its cache, and since SIN-54 never notices
upstream removing a file, because a refresh is what flags it. An unset `negativeTtl` of
`0` means "not found" is not cached. Always set `ttl` for an explicit `url`.

### `lifecycle`

The ordered stages and the evidence each requires.

<!-- keys checked against the loader: lifecycle -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `stages` | list | An ordered list. Each entry is a stage name, or a mapping with `name` and `gates`. | no stages |
| `stages[].name` | string | The stage name, in the mapping spelling. | |
| `stages[].gates` | list of gates | Rules to promote into the stage, in the mapping spelling. Each entry is an evidence type (a string) or a `scan` mapping. | no gates |
| `gates` | map of stage to list of gates | A map from a stage name to the rules to promote into it, for example `prod: [sbom, signature, {scan: {maxSeverity: high}}]`. The stage must be declared in `stages`, and a stage's gates are written in one place, not both. | no gates |

A gate is an evidence type, or a `scan` mapping that refuses a subject whose newest
Sindri scan of any platform holds a finding above a severity ceiling. A stage takes at
most one `scan` rule.

<!-- keys checked against the loader: lifecycle.gates -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `scan` | mapping | A scan rule, written in place of an evidence type. | |
| `scan.maxSeverity` | string | The ceiling: `critical`, `high`, `medium`, or `low`. `high` refuses only critical findings; an unrated (`UNKNOWN`) finding is refused at every ceiling. | |

```yaml
lifecycle:
  stages: [dev, integration, prod]
  gates:
    prod:
      - sbom
      - signature
      - scan: {maxSeverity: high}
```

The rule is a ceiling read from the scan evidence the worker records; [ADR 30](adr/0030-severity-gate.md)
gives the reasoning.

Lifecycle applies to hosted repositories only. Stages are organisation-wide.

### `defaults`

Values applied to every repository of a type that does not set them itself. There is
no `extends:`; YAML anchors cover the rest. The keys are the repository types, and each
takes the fields of the same name on a repository.

<!-- keys checked against the loader: defaults -->

| Key | Fields it may set |
|---|---|
| `hosted` | `read`, `write`, `lifecycle` |
| `proxy` | `read`, `write`, `ttl`, `negativeTtl`, `credentials` |
| `virtual` | `read`, `write` |

## Long form: manifests

`sindri apply -f <file-or-directory>` and `sindri diff` read enveloped documents,
several per file separated by `---`. Every document has `kind`, `metadata`, and `spec`.
All three kinds describe repositories and lifecycle, so none of them has a place for
server settings.

### `kind: Repository`

A namespaced repository's full name is `namespace/name`.

<!-- keys checked against the loader: Repository -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `kind` | string | `Repository`. | |
| `metadata.name` | string | The repository's name. Required. | |
| `metadata.namespace` | string | The owning team. Omit it for an organisation repository. | unset |
| `spec.type` | string | `hosted`, `proxy`, or `virtual`. Required. | |
| `spec.protocol` | string | `oci`, `pypi`, or `npm`. Required. | |
| `spec.upstream.url` | string | Proxy only. The upstream URL. | |
| `spec.upstream.ttl` | duration | Proxy only. Fresh time. Unset is `0`, which never expires and never refreshes the cache, so always set it. | `0` |
| `spec.upstream.negativeTtl` | duration | Proxy only. How long "not found" is remembered. Unset is `0`: not cached. | `0` |
| `spec.upstream.credentials` | string | Proxy only. A key of the server's `credentials:` map. | unset |
| `spec.members[].ref` | string | Virtual only. `<type>/<name>`, for example `hosted/internal-js`; `org/<name>` or `<team>/<name>` reaches across a namespace. | |
| `spec.access.read` | list of strings | Groups that may read, or `authenticated`. | administrators, and machine tokens scoped to it |
| `spec.access.write` | list of strings | Groups that may write. | administrators, and machine tokens scoped to it |
| `spec.lifecycle.stages` | list of strings | Hosted only. The stages artifacts may be promoted into. | every stage |

The long form has no presets and no inference: what is written is what the server acts
on. `sindri config show --expanded` prints a short-form file in this spelling.

### `kind: Lifecycle`

<!-- keys checked against the loader: Lifecycle -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `kind` | string | `Lifecycle`. | |
| `metadata.name` | string | A name for the document. Optional. | unset |
| `spec.stages[].name` | string | The stage name. | |
| `spec.stages[].gates` | list of gates | Rules to promote into the stage: evidence types (strings) or `scan` mappings, as in the short form. | no gates |

### `kind: OrgPolicy`

Optional. Absent means every team declares freely. Only one may exist.

<!-- keys checked against the loader: OrgPolicy -->

| Key | Type | Meaning | Default |
|---|---|---|---|
| `kind` | string | `OrgPolicy`. | |
| `metadata.name` | string | A name for the document. Optional. | unset |
| `spec.mode` | string | `centralized` (only organisation documents declare repositories), `delegated` (teams declare freely), or `hybrid` (teams declare their own, and the mandatory repositories must exist). | `delegated` |
| `spec.mandatoryRepos[].name` | string | An organisation repository that must exist. | |
| `spec.mandatoryRepos[].locked` | boolean | Teams may reference it and never override it. | `false` |
| `spec.namespaces[].name` | string | A team namespace. | |
| `spec.namespaces[].admins` | list of strings | Groups that may apply that namespace's manifests. | administrators only |

## Ownership

The reconciler owns rows marked `managed_by = config`, and records which trigger owns
each: `serve --config` or `sindri apply`. The API, the reconciler, and the two triggers
never change a row another owns. The reconciler never deletes without `--prune`. See
[ADR 17](adr/0017-config-kinds-namespaces-and-reconciliation.md).
