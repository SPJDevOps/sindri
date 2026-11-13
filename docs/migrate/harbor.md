# Migrating from Harbor

<!-- commands checked by: scripts/migrate-harbor-gate.sh -->

You can move a team from Harbor to Sindri without a flag day. Sindri proxies your
Harbor, hosts the images you move or push itself, and serves both through one
repository, so clients change one registry name and Harbor can be switched off once
nothing reads it. This page shows each step, and the script that checks it runs the
commands below with the substitutions listed in
[What this was checked against](#what-this-was-checked-against).

**Read the last section first.** The check ran against a registry standing in for
Harbor that uses Harbor's token flow, not against Harbor itself. Statements about
Harbor's own behavior, which the check could not see, are marked "not checked".

The method uses three OCI repositories:

- a **proxy**, `harbor`, whose upstream is your Harbor, signed in as a robot account,
  so every image you already have stays pullable;
- a **hosted** repository, `internal`, where new images are pushed and where the
  history you want to keep is copied; and
- a **virtual** repository, `images`, over those two, which is the only address
  clients use.

The virtual repository lists the hosted repository first, and that order is a rule,
not a preference. An image name that hosted has ever held is **claimed**: the proxy
is never asked for its manifests. The claim is per image name (the whole path after
the repository name, such as `acme/app`), it covers every tag and digest of that
name, and it applies to manifests, not to blobs. The consequence is described in
[Move the history before you switch anything](#5-move-the-history-before-you-switch-anything)
and decides the order of the steps: copy first, then switch clients, then push.

You need a Sindri server that can reach your Harbor over HTTPS, a Harbor robot account
that can pull from the projects you are moving, and Docker on the clients. Examples
write the two servers as `https://harbor.example.com` and
`https://sindri.example.com`, and the Harbor project as `acme`.

## How Harbor concepts map

| In Harbor | In Sindri |
|---|---|
| Project `acme`, repository `app` | The image name `acme/app`. The project is the first part of the name, and passes through the `harbor` proxy unchanged. |
| A robot account `robot$acme+sindri` | For Sindri's own reads of Harbor: a `username` and `password` credential on the `harbor` proxy. For your clients and CI: [machine tokens](#3-give-your-clients-and-ci-tokens), which are Sindri's own. |
| A project's members and roles | `read` and `write` on a repository, by group; see below. |
| Project quotas, retention rules, immutability rules | Not carried over; see [What Sindri does not do yet](#what-sindri-does-not-do-yet). |

Sindri's access rules attach to a repository, not to a path inside one. If your Harbor
projects have different audiences, give each audience its own hosted repository and
list each in the virtual repository's `members`. Image names inside a hosted repository
must then match what the proxy serves: a name is `acme/app` in both. One
`internal` repository, as below, is the right start if one group may read everything.

## 1. Give Sindri a robot account on Harbor

Sindri reads your images as a Harbor robot account with pull permission on the
projects you are moving. Create it in Harbor; how is Harbor's business and was not
checked. Harbor robot names look like `robot$<project>+<name>` (not checked). The
`$` is the part that goes wrong: an unquoted `$acme` is an empty variable to the shell, which silently turns the
name into `robot+sindri`. Single-quote the name, and keep the secret in your secret
store:

<!-- migrate:robot-env -->
```bash
export HARBOR_ROBOT_USER='robot$acme+sindri'
# HARBOR_ROBOT_PASSWORD comes from your secret store.
```
<!-- /migrate:robot-env -->

If the robot's secret is revoked or stops working, Sindri's reads of Harbor fail with a
refusal that appears in Sindri's log as a warning, and the proxy no longer serves tags it
has not cached. That is the behavior of Sindri's side, and the check exercises the
refusal path only through the stand-in.

## 2. Declare the repositories

Add this to the Sindri configuration, next to your `listen`, `storage`, `database`, and
`auth` settings:

<!-- migrate:config -->
```yaml
credentials:
  harbor-robot:
    username: ${HARBOR_ROBOT_USER}
    password: ${HARBOR_ROBOT_PASSWORD}

defaults:
  hosted:  { read: [authenticated], write: [team:platform] }
  proxy:   { read: [authenticated] }
  virtual: { read: [authenticated] }

repos:
  harbor:   { protocol: oci, upstream: { url: "https://harbor.example.com", ttl: 10m, negativeTtl: 1m, credentials: harbor-robot } }
  internal: { protocol: oci }
  images:   { members: [internal, harbor] }
```
<!-- /migrate:config -->

A robot account is a `username` and `password` credential. A `token:` credential is
refused on an OCI proxy: an OCI registry exchanges the name and secret at its token
realm for a short-lived token, and Sindri does that exchange for you. See
[ADR 27](../adr/0027-upstream-credentials-may-be-a-bearer-token.md) and the
[`credentials` reference](../configuration.md#credentials). The values are read from
the environment when the server loads the file, so the `$` in the robot's name needs
no escaping in the file.

The credential goes only to Harbor's own origin. Harbor's token realm, usually
`https://harbor.example.com/service/token`, is on that origin, so it gets the robot's
login. A realm on another host gets it only over HTTPS, and a download that an upstream
redirects to another host is fetched without it (Sindri's own tests cover the redirect;
the stand-in does not redirect). Check where your Harbor's realm is: `curl -sI https://harbor.example.com/v2/`
prints it in `www-authenticate`.

`ttl` is how long Sindri trusts a tag it cached from Harbor before asking again. A
tag that moves in Harbor (`latest`) is therefore up to `ttl` stale through the proxy.
Digests do not move. Replace `team:platform` with the group that pushes.

## 3. Give your clients and CI tokens

A client needs the registry name and a token, and the copy in step 5 needs the push
token, so make the tokens first. Sindri issues machine tokens through
its API, which only an administrator may call. A puller needs `read` on everything it
pulls. A pusher needs `write` on `internal`:

<!-- migrate:tokens -->
```bash
printf 'Authorization: Bearer %s\n' "$SINDRI_ADMIN_TOKEN" |
  curl -fsS -X POST https://sindri.example.com/api/v1/tokens \
    -H @- -H 'Content-Type: application/json' \
    -d '{"name":"ci-pull","role":"read"}'
printf 'Authorization: Bearer %s\n' "$SINDRI_ADMIN_TOKEN" |
  curl -fsS -X POST https://sindri.example.com/api/v1/tokens \
    -H @- -H 'Content-Type: application/json' \
    -d '{"name":"ci-push","role":"write","repository":"internal"}'
```
<!-- /migrate:tokens -->

Each response holds the token once, in `token`. Put it in your secret store as
`SINDRI_PULL_TOKEN` and `SINDRI_PUSH_TOKEN`. These replace Harbor's robot accounts for
your clients and CI: one token per use, revocable on its own. Give a puller an
**unscoped** `read` token (as above). A token scoped to one repository, such as
`images`, cannot pull through it, because a virtual repository answers from its
members and the token is not scoped to them.

The header goes to curl on its standard input (`-H @-`), so the token never appears
on a command line, where `ps` and shell history would show it.

## 4. Take the inventory

Before you copy anything, list what Harbor holds. A robot account can list repositories
and tags over the registry API, with `crane`, a single static binary. The commands
below run `crane` from its published image and need only the two credentials:

<!-- migrate:login -->
```bash
printf '%s' "$HARBOR_ROBOT_PASSWORD" | crane auth login harbor.example.com -u "$HARBOR_ROBOT_USER" --password-stdin
printf '%s' "$SINDRI_PUSH_TOKEN" | crane auth login sindri.example.com -u token --password-stdin
```
<!-- /migrate:login -->

`crane auth login` stores each credential for its own registry, in
`~/.docker/config.json`. This page has no script of its own that handles credentials: a
home-made script that keeps its `Authorization` header across a redirect would send the
robot's secret to whatever host the registry redirects a download to (some registries
redirect to object storage). Use a maintained client such as `crane`, `skopeo`, or
`oras` instead. Whether `crane` drops the header on a cross-host redirect was not
checked here, because the stand-in does not redirect.

<!-- migrate:inventory -->
```bash
for repo in $(crane catalog harbor.example.com | grep '^acme/'); do
  for tag in $(crane ls "harbor.example.com/$repo"); do
    echo "$repo $tag"
  done
done
```
<!-- /migrate:inventory -->

This prints one `repository tag` line for every tag of every repository in the `acme`
project. Check the list against what you expect.

The stand-in lets the robot call `/v2/_catalog`. **Not checked:** whether your Harbor
does. If it refuses the robot, list the project's repositories in another way, such as
Harbor's own API or UI, and feed the names to the `crane ls` and `crane copy` lines
below in place of `crane catalog`. The check did not run any Harbor API call.

An image is a manifest, and for a multi-architecture image the manifest is an index
that points at one manifest per platform. The inventory lists tags, and a tag can be an
index. The copy below handles both.

## 5. Move the history before you switch anything

Do this before any client reads from Sindri and before the first push to `internal`.
Pushing `acme/app` to `internal` claims `acme/app`. From then on, through `images`,
Harbor's tags of `acme/app` are not served, even a tag you never pushed to Sindri:
`images/acme/app:1.0` answers 404 while `harbor/acme/app:1.0` still serves it. There is
no merge of hosted and proxied tags for one name. Other image names are unaffected:
the claim is per name, so a push to `acme/app` does not hide `acme/tools`. So copy
every image and every tag you keep first. Until you switch the clients in step 6 they
keep reading Harbor, and nothing looks different to them while this runs.

Copying is a registry-to-registry copy of the manifests and blobs, not a
`docker pull`, `docker tag`, and `docker push`, because a docker round trip can
rewrite an index: the local daemon keeps only your platform's image, and pushes one
platform back as a single manifest with a different digest. `crane copy` copies the
manifest bytes as they are, and for an index every manifest it lists, so the digests
are the same on both sides. `--all-tags` copies every tag of a repository, and
`--no-clobber` leaves a tag that already exists in Sindri alone, so the command can be
run again after an interruption without moving a tag someone has pushed to `internal`:

<!-- migrate:copy -->
```bash
for repo in $(crane catalog harbor.example.com | grep '^acme/'); do
  crane copy --all-tags --no-clobber "harbor.example.com/$repo" "sindri.example.com/internal/$repo"
done
```
<!-- /migrate:copy -->

Then compare, tag by tag, the digest Harbor reports with the digest Sindri reports.
The command exits non-zero on the first difference:

<!-- migrate:verify -->
```bash
for repo in $(crane catalog harbor.example.com | grep '^acme/'); do
  for tag in $(crane ls "harbor.example.com/$repo"); do
    a=$(crane digest "harbor.example.com/$repo:$tag")
    b=$(crane digest "sindri.example.com/internal/$repo:$tag")
    if [ "$a" = "$b" ]; then echo "ok $repo:$tag $a"; else echo "MISMATCH $repo:$tag $a $b"; exit 1; fi
  done
done
```
<!-- /migrate:verify -->

What is preserved: every manifest and index byte, so every digest, and every tag you
list. A deployment that pins `image@sha256:...` keeps working. What is not: Harbor's
own metadata. Tags' push and pull times, scan results, labels, signatures and SBOM
attachments (they are separate artifacts that refer to the image, see below), and
immutability rules do not move. `crane copy` copies the image it is given; it does not look for other
artifacts that refer to it. Copy signatures and SBOMs separately if you rely on them
(not checked).

An imported image has Sindri's push time, and its blobs are shared with every other
image that has the same layers.

An image first pushed to Harbor after your copy is not in `internal`, so through
`images` it is still served by the `harbor` proxy. Run the copy again before the first
push to that image: `--no-clobber` skips every tag already there.

## 6. Switch the clients

The history is in Sindri, so clients can change. Change the registry name in image references, from `harbor.example.com/acme/app`
to `sindri.example.com/images/acme/app`, in Dockerfiles, Compose files, and
Kubernetes manifests. A client signs in with the token as the password; the user name
is not checked:

<!-- migrate:pull -->
```bash
printf '%s' "$SINDRI_PULL_TOKEN" | docker login sindri.example.com -u token --password-stdin
docker pull sindri.example.com/images/acme/app:1.0
```
<!-- /migrate:pull -->

Kubernetes `imagePullSecret`s and container runtime mirror settings were not checked.

The pulled image has the digest Harbor had for the tag, because Sindri passed Harbor's
manifest through unchanged. `docker pull` prints it on its `Digest:` line.

## 7. Check the clients without Harbor's credential

A client holds only the Sindri token. Sindri holds the robot, so a read that reaches
Harbor comes from the robot account. The images you copied are now Sindri's own, so
pulling one does not reach Harbor at all. An image that only Harbor has, such as one
pushed to Harbor after the copy, is read through the `harbor` proxy. In the check, the
stand-in's request log shows the robot's login for such an image, no read of a copied
one, and no request from the client. Look at your Harbor's access logs the same
way (not checked): a person's account still pulling the images you moved means that
client still talks to Harbor.

## 8. Push new images to Sindri

CI pushes to `internal`, with the push token. Clients that pull through `images` see
the new tag beside the history, because both are in `internal` once the history is
copied. Pushes go to `internal`, never to `images`: a push to a virtual repository
is refused.

<!-- migrate:push -->
```bash
printf '%s' "$SINDRI_PUSH_TOKEN" | docker login sindri.example.com -u token --password-stdin
docker tag acme-app:2.0 sindri.example.com/internal/acme/late:2.0
docker push sindri.example.com/internal/acme/late:2.0
```
<!-- /migrate:push -->

The example pushes `acme/late`, an image first pushed to Harbor after the copy above,
to show why the copy has to be complete. After the push, ask for its Harbor tag
through `images`:

<!-- migrate:claim-check -->
```bash
printf 'Authorization: Bearer %s\n' "$SINDRI_PULL_TOKEN" |
  curl -s -o /dev/null -w '%{http_code}\n' -H @- \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    https://sindri.example.com/v2/images/acme/late/manifests/1.0
```
<!-- /migrate:claim-check -->

It prints `404`: the push claimed `acme/late`, and Harbor's `1.0` is no longer
served, while `harbor/acme/late:1.0` still has it. Run the copy again, and `1.0` is
in `internal` beside the new tag.

## 9. Pull by digest, with Harbor off

When nothing but Sindri reads Harbor, stop pushing to it, remove `harbor` from the
`images` members, and then stop Harbor. Do not remove the proxy before the history is
copied (step 5): until an image is copied, the proxy is what keeps its tags pullable. Remove the robot account when the
server goes.

Pulling by digest from `internal` is the proof that the history is Sindri's own:

<!-- migrate:pull-digest -->
```bash
printf '%s' "$SINDRI_PULL_TOKEN" | docker login sindri.example.com -u token --password-stdin
docker pull "sindri.example.com/images/acme/app@$DIGEST"
```
<!-- /migrate:pull-digest -->

`DIGEST` is the value `crane digest` printed for the tag you copied, and `docker pull`
prints the same value on its `Digest:` line.

## What Sindri does not do yet

Be sure none of these is on your list before you switch Harbor off:

- **Vulnerability scanning.** Sindri does not scan images until M11a, so anything Harbor's scanner gives you today has no equivalent.
  Scan the images some other way in the meantime; see
  [ADR 23](../adr/0023-scanning-as-a-queue-job-and-the-open-core-line.md).
- **Replication.** Sindri has no replication between servers.
- **Referrers from before the move.** Sindri serves the OCI referrers API from 1.5, for
  what is pushed to a hosted repository. A proxy repository answers with an empty list,
  so signatures and SBOMs attached to an image in Harbor are not discoverable through
  the proxy: attach them again after you publish to hosted. See
  [Signatures and SBOMs](../clients/referrers.md).
- **Project quotas.** Sindri has no per-project storage limit.
- **Tag retention and immutability rules.** Nothing is carried over from Harbor's. Sindri has
  its own lifecycle stages and garbage collection, which work differently.
- **Harbor's robot-account permission model.** A Sindri machine token has a role
  (`read`, `write`, or `admin`) and optionally one repository or namespace.
- **`/v2/_catalog`.** Sindri does not serve it, so clients that list repositories
  through it do not work. Use the API or the UI.
- **Cross-repository blob mounts.** A client that tries one is given a normal upload
  session, which costs a re-upload of the layer.
- **A proxy's tag list.** `tags/list` through `images` or `harbor` shows the tags
  Sindri has cached or hosted, not what Harbor holds, so a client that lists tags to
  discover versions sees only what has already been pulled or copied.

## What this was checked against

The commands above ran against a **registry standing in for Harbor**, not against
Harbor. The stand-in is the Distribution registry 2.8.3 (`registry:2`; Harbor's registry
component is based on it, which was not checked) behind nginx 1.28, with `cesanta/docker_auth` 1.14.0 as its token server. It
reproduces the part of Harbor this page depends on: an anonymous request is refused
with a Bearer challenge, a robot named `robot$acme+sindri` exchanges its secret at a
token realm for a token that is good for pulling from the `acme` project only, and the
token goes to `/v2/`. nginx serves the registry and the realm from one origin, as
Harbor does, and the check fails if the stand-in lets an anonymous caller in. It has
none of Harbor's API, projects, quotas, replication, scanner, audit log, or
redirect-to-object-storage blob downloads, and a real Harbor can differ in ways this
does not show. The robot-account listing through the API in step 4 was not run for that
reason. Run the steps against a Harbor you can afford to read from before you trust
them with a production one.

The check ran Sindri from this repository's build, Docker Desktop's Docker 29 for the
pulls and the push, and `crane` (go-containerregistry v0.21.0) from its published
image for the copy. Every image is pinned by digest in the script. It reads the
commands out of this page and makes only these substitutions: `harbor.example.com`
becomes the stand-in and `sindri.example.com` becomes the server on `:8080`, each as
the address the command runs against (this host for `curl`, the host's name for
`docker` and `crane`); the page says HTTPS and the check speaks plain HTTP; and `crane`
runs through a wrapper in a container that adds `--insecure` and nothing else. The
check also seeds the stand-in itself, builds the local `acme-app:2.0` image, and
pushes an extra image to the stand-in after the copy, as the page's step 8 describes. What it asserts:

- The proxy authenticates to the stand-in with a `username` and `password` credential
  whose name contains a `$`, quoted as shown. A client with no Harbor credential pulls
  `acme/app:1.0` through Sindri at Harbor's digest.
- A token scoped to the `images` repository cannot pull through it.
- The inventory lists exactly the tags of the `acme` project and leaves out the
  project the robot may not read.
- After the copy, every tag has the digest Harbor reports (the images are
  multi-architecture indexes), the copy can be run again, and `images/acme/app` serves
  its tags, all before any client is switched.
- A pull of a copied image through `images` is served from `internal`, and the
  stand-in never sees a request for it. An image pushed to the stand-in after the
  copy is read through the proxy with the robot's login.
- One push to `internal/acme/late`, an image not yet copied, makes
  `images/acme/late:1.0` answer 404 while `harbor/acme/late:1.0` still answers, and
  `acme/tools:1.0` is unaffected. Running the copy again serves it.
- With the stand-in stopped, a pull of the image by digest through `images` succeeds
  from `internal`.
- The robot's password and the machine tokens appear in no log.

The check is one process for Sindri and one container each for the registry, the token
server, nginx, and each `crane` run, on one host. It does not isolate the machine's
Docker daemon, whose image cache and insecure-registry list are the machine's, or its
network: the seed images come from Docker Hub. It runs plain HTTP where the page says
HTTPS. `crane` reads no configuration from the machine that runs the check.
