# Migrating from devpi

<!-- commands checked by: scripts/migrate-devpi-gate.sh -->

You can move a team from devpi to Sindri without a flag day. Sindri proxies your
devpi index, hosts new uploads itself, and serves both through one repository, so
clients change one line and devpi can be switched off once nothing reads it. This
page shows each step, and the script that checks it runs the commands below with the substitutions listed in
[What this was checked against](#what-this-was-checked-against).

The method uses three PyPI repositories, and a fourth that is optional:

- a **proxy**, `devpi`, whose upstream is your devpi index, so everything you have
  already uploaded stays installable;
- a **hosted** repository, `internal-py`, where new releases are uploaded;
- a **proxy** of the public index, `pypi`, if your devpi index inherits from
  `root/pypi` and your clients rely on that; and
- a **virtual** repository, `python`, over those, which is the only address
  clients use.

The virtual repository lists the hosted repository first, and that order is a
rule, not a preference. A project name that hosted has ever held is **claimed**:
the proxies are never asked for it. The claim is per project, by its normalized
name (PEP 503, so `Acme_Lib` and `acme-lib` are one project), and it covers the
whole project, not a version. The consequence is described in
[Move the history before you switch anything](#3-move-the-history-before-you-switch-anything)
and decides the order of the steps: import first, then switch clients, then upload.

You need a Sindri server that can reach your devpi over HTTP or HTTPS, a devpi
server that serves PEP 691 JSON indexes, which is what Sindri asks for (check with
`curl -H 'Accept: application/vnd.pypi.simple.v1+json' <index>/+simple/<project>/`),
and pip and twine on the clients. These steps were checked with devpi-server 6.21,
pip 26.3, and twine 7.0. Examples write the two servers as
`https://devpi.example.com` and `https://sindri.example.com`, and the old index as
`acme/prod`.

## 1. Give Sindri an account on devpi

Sindri reads your packages as a devpi user. A dedicated account such as
`sindri-proxy` keeps Sindri's reads apart from people's in devpi's logs. Create it
as `root`, with a password from your secret store (here `DEVPI_PASSWORD`):

<!-- migrate:devpi-user -->
```bash
devpi user -c sindri-proxy password="$DEVPI_PASSWORD"
```
<!-- /migrate:devpi-user -->

devpi-server 6.21 has no per-index read permission of its own: an index is readable
by anyone who can reach the server, and uploads are limited by the index's `acl_upload`.
Whether Sindri needs a login at all depends on how you front devpi. If devpi is
open on a private network, leave out `credentials` below. If a reverse proxy
requires a login for reads (the `devpi-lockdown` plugin with nginx's
`auth_request` does this, and is what this page was checked against), the account
needs only to be able to sign in. Sindri sends it as HTTP Basic.

## 2. Declare the repositories

Add this to the Sindri configuration, next to your `listen`, `storage`,
`database`, and `auth` settings:

<!-- migrate:config -->
```yaml
credentials:
  devpi:
    username: sindri-proxy
    password: ${DEVPI_PASSWORD}

defaults:
  hosted:  { read: [authenticated], write: [team:platform] }
  proxy:   { read: [authenticated] }
  virtual: { read: [authenticated] }

repos:
  devpi:       { protocol: pypi, upstream: { url: "https://devpi.example.com/acme/prod/+simple", ttl: 10m, negativeTtl: 1m, credentials: devpi } }
  internal-py: { protocol: pypi }
  pypi:        { upstream: pypi }
  python:      { members: [internal-py, devpi, pypi] }
```
<!-- /migrate:config -->

The upstream URL is the index's `+simple` address, with the user and index in it.
Sindri adds the project name and asks for the JSON form. The credential goes only
to the upstream's own origin (scheme and host): a file link in a devpi document
that names another host is fetched without it. See the
[`credentials` reference](../configuration.md#credentials).

Links in devpi's documents are relative (`../../+f/...`). Sindri resolves them
against the project page they came from, as PEP 503 says, so devpi needs no
`--outside-url` setting for Sindri's sake.

`pypi` is last, so a project that devpi does not hold still installs from the
public index. Leave it out if devpi's `bases` are empty and you want no public
source. If your devpi index inherits from `root/pypi`, devpi already serves the
public packages through the `devpi` member, and `pypi` is a second path to them;
keep it only if you plan to drop `root/pypi` from devpi first. Replace
`team:platform` with the group that uploads.

If your index inherits from other devpi indexes that you own, declare one proxy
for each, and list each in `members` after `devpi`.

## 3. Move the history before you switch anything

Do this before any client reads from Sindri and before the first upload to
`internal-py`. Uploading `acme-lib` 1.5.0 to `internal-py` claims `acme-lib`. From
then on, through `python`, the devpi versions of `acme-lib` are not in the index
and are not served, even a version you never uploaded to Sindri. There is no merge
of hosted and proxied files for one project. Other projects are unaffected: the
claim is per project, by its normalized name, so an upload to `acme-lib` does not
hide `acme-legacy`. So import every project, with every file, first. Until you
switch the clients in step 4 they keep reading devpi, and nothing looks different
to them while this runs.

Importing is a download from devpi and an upload to Sindri, for every file. This
script lists every project on the index and every file of each, which is every
version in the form devpi stores it, and downloads them. It prints one line for
each file, so its output is the inventory. It uses only the Python standard
library. It sends the login only to devpi's own origin (scheme and host): a file
link that names another host is fetched without it, and a redirect to another host
drops it. The account is the one from step 1, or any that can read the index:

<!-- migrate:export -->
```python
import base64, json, os, pathlib, urllib.parse, urllib.request

base = os.environ["DEVPI_INDEX_URL"].rstrip("/")
index = base + "/+simple/"
login = base64.b64encode(f"{os.environ['DEVPI_USER']}:{os.environ['DEVPI_PASSWORD']}".encode()).decode()
out = pathlib.Path("devpi-export")
out.mkdir(exist_ok=True)

def origin(url):
    parts = urllib.parse.urlsplit(url)
    return parts.scheme, parts.netloc

class DropLoginOffOrigin(urllib.request.HTTPRedirectHandler):
    """Drop the login from a redirect that leaves devpi's origin."""
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None and origin(newurl) != origin(index):
            new.remove_header("Authorization")
        return new

opener = urllib.request.build_opener(DropLoginOffOrigin)

def get(url, kind="application/vnd.pypi.simple.v1+json"):
    headers = {"Accept": kind}
    if origin(url) == origin(index):  # a link to another host gets no login
        headers["Authorization"] = "Basic " + login
    return opener.open(urllib.request.Request(url, headers=headers))

with get(index) as page:
    projects = [p["name"] for p in json.load(page)["projects"]]
for name in projects:
    with get(index + urllib.parse.quote(name) + "/") as page:
        detail = json.load(page)
        base_url = page.geturl()  # relative links resolve against the page, after redirects
    for f in detail["files"]:
        print(name, f["filename"])
        with get(urllib.parse.urljoin(base_url, f["url"]), "*/*") as src:
            (out / f["filename"]).write_bytes(src.read())
```
<!-- /migrate:export -->

<!-- migrate:export-run -->
```bash
DEVPI_INDEX_URL=https://devpi.example.com/acme/prod DEVPI_USER=sindri-proxy \
  DEVPI_PASSWORD="$DEVPI_PASSWORD" python3 export-devpi.py
```
<!-- /migrate:export-run -->

Save the script as `export-devpi.py` first. An index that inherits from others
lists only its own projects here, so run it for each index you own, and not for
`root/pypi`. Check the list against what you expect, then upload the files. Set
`SINDRI_TOKEN` to a token that may write to `internal-py`; twine reads the
password from `TWINE_PASSWORD`, which keeps it off the command line:

<!-- migrate:import -->
```bash
TWINE_USERNAME=token TWINE_PASSWORD="$SINDRI_TOKEN" \
  twine upload \
  --repository-url https://sindri.example.com/pypi/internal-py/upload \
  devpi-export/*
```
<!-- /migrate:import -->

An imported file keeps its bytes, and so its hash, and has Sindri's upload time.
Sindri's files are immutable. Uploading a file again with the same bytes succeeds,
so an interrupted import can be run again; a file with the same name and different
bytes is refused, and the upload fails for it. devpi's
test results and documentation are not part of the index and are not moved.

A project first uploaded to devpi after your export is not in `internal-py`, so
through `python` it is still served from the `devpi` proxy. Run the script and the
upload again before the first upload to that project: the files already
there are uploaded again unchanged.

## 4. Switch the clients

A client needs the index address and a token. `pip.conf` holds the address, and
holds no secret:

<!-- migrate:pip-conf -->
```ini
[global]
index-url = https://sindri.example.com/pypi/python/simple/
```
<!-- /migrate:pip-conf -->

pip finds `pip.conf` at `~/.config/pip/pip.conf` (`%APPDATA%\pip\pip.ini` on
Windows), in `/etc/pip.conf`, or where `PIP_CONFIG_FILE` points. `pip config
debug` lists the files it read. The token goes in `~/.netrc` (mode 600), which pip
reads when the index URL has no credentials. Sindri ignores the user name, so
`token` is only a label:

<!-- migrate:netrc -->
```text
machine sindri.example.com
  login token
  password YOUR_SINDRI_TOKEN
```
<!-- /migrate:netrc -->

If you would rather keep the token in a keyring than in a file, name the user in
the index URL and tell pip to ask the `keyring` command:

<!-- migrate:pip-conf-keyring -->
```ini
[global]
index-url = https://token@sindri.example.com/pypi/python/simple/
keyring-provider = subprocess
```
<!-- /migrate:pip-conf-keyring -->

<!-- migrate:keyring-pip -->
```bash
keyring set https://sindri.example.com/pypi/python/simple/ token
```
<!-- /migrate:keyring-pip -->

`keyring set` asks for the token. pip runs the `keyring` command from its `PATH`,
and deliberately ignores one installed in its own environment, so install it
apart from pip, for example with `pipx install keyring`. (twine, which loads
`keyring` as a library, needs it in twine's own environment instead; step 7.)

Then look for everything that still names devpi and remove it. Each of these keeps
a client reading devpi directly, or worse, beside Sindri:

- `index-url` and `extra-index-url` lines in `pip.conf`, `requirements.txt`
  (`-i`, `--index-url`, `--extra-index-url`), and `constraints.txt`;
- the `PIP_INDEX_URL`, `PIP_EXTRA_INDEX_URL`, and `UV_*` index variables in CI and
  Dockerfiles;
- `[easy_install] index_url` in `setup.cfg` or `~/.pydistutils.cfg`;
- `index_url` for devpi in `tox.ini` and `pyproject.toml` (`[tool.uv]`,
  `[[tool.poetry.source]]`); and
- the `devpi use` setting on a machine: `devpi` CLI users keep working against
  devpi until the server goes, but their `devpi upload` should become `twine
  upload` (step 7).

## 5. Do not keep `--extra-index-url`

The old habit with devpi is a second index on the command line:

<!-- migrate:extra-index -->
```bash
pip install --index-url https://sindri.example.com/pypi/internal-py/simple/ \
  --extra-index-url https://sindri.example.com/pypi/devpi/simple/ acme-lib
```
<!-- /migrate:extra-index -->

Do not do this, even if both addresses are Sindri's. pip does not treat the
indexes as an ordered list. It reads every one, merges all the files it finds for
the name, and installs the highest version, wherever that came from. Whoever can
publish a higher version of `acme-lib` to any of those indexes, including the
public one, is installed in place of the version you meant. That is dependency
confusion, and an extra index is how it enters.

A Sindri virtual repository is the replacement. It asks its members in order and
answers from the first one that holds the project, and a project that `internal-py`
has ever held is answered from `internal-py` alone. pip is given one index and
there is nothing to merge. Give every tool (pip, uv, tox, CI) the one `python`
address and no second index.

With the example above, a version of `acme-lib` that exists only on devpi is
installed, and a hosted version that is lower is ignored. Through `python`, the
hosted version is what you get. The gate shows both.

## 6. Check the clients without devpi

A client that holds no devpi credential cannot go around Sindri. Install a project
the old index holds:

<!-- migrate:install -->
```bash
pip install --no-cache-dir acme-legacy==1.0.0
```
<!-- /migrate:install -->

With the page's `pip.conf`, `~/.netrc`, and no devpi address anywhere, the file
comes from Sindri's `python` repository, from the copy you imported in step 3. The
same filename has the same SHA-256 on both sides. A project that only devpi holds
is served by the proxy, which reads it from devpi as `sindri-proxy`.

## 7. Upload new releases to Sindri

Uploads go to the hosted repository, not the virtual one. Name it once in
`~/.pypirc`, without the token:

<!-- migrate:pypirc -->
```ini
[distutils]
index-servers = sindri

[sindri]
repository = https://sindri.example.com/pypi/internal-py/upload
username = token
```
<!-- /migrate:pypirc -->

twine asks the keyring for the missing password, under the repository URL and the
user name. Store the token there once:

<!-- migrate:keyring-twine -->
```bash
keyring set https://sindri.example.com/pypi/internal-py/upload token
```
<!-- /migrate:keyring-twine -->

<!-- migrate:publish -->
```bash
python -m build
twine upload --repository sindri dist/*
```
<!-- /migrate:publish -->

On CI, set `TWINE_USERNAME` and `TWINE_PASSWORD` from the secret store instead,
and leave the keyring out. `devpi upload` and `devpi push` do not talk to Sindri;
replace them with the two commands above. Clients that install through `python`
see the new release beside the history, because both are in `internal-py` once the
history is imported.

## 8. Switch devpi off

When nothing reads devpi but Sindri, stop uploading to it, remove `devpi` from the
`python` members, and then stop devpi. Do not remove the proxy before the history
is imported: the proxy is what keeps those versions installable. Remove the
`sindri-proxy` account when the server goes.

## What this was checked against

The commands above run against a real devpi-server 6.21.0 in a pinned `python`
image, with devpi-web 5.2.0 and devpi-lockdown 2.0.0 behind nginx 1.28 so that
reads need a login. Sindri authenticates to it as `sindri-proxy` with a
`username` and `password` credential, and the check fails if Sindri cannot read
devpi's index or its files. pip 26.3 and twine 7.0.0 run in a second container of
that image, with an empty home directory and a generated `pip.conf`, so nothing
from the machine that runs the check is read.

- A client with no devpi credential installs through Sindri, and nginx's log in
  front of devpi shows the file was read by `sindri-proxy` and not by the client.
- `pip.conf` with `~/.netrc`, `pip.conf` with `keyring-provider = subprocess`, and
  `twine upload` with the password in a keyring all authenticate; the pip keyring case
  fails when the keyring is emptied.
- The `--extra-index-url` form installs the devpi-only version of a project that
  hosted also holds, and the `python` repository installs the hosted one.
- A project is hidden from the proxy by one upload to hosted, and returns when its
  history is imported.

The check runs one process for Sindri and one container each for devpi, nginx,
and the clients, on one host; it does not isolate the machine's network, and it
needs access to pypi.org, where pip, twine, and the public `pypi` member read
from. The keyring is a plain file backend (`keyrings.alt`), which tests the
`keyring` command's contract, not an operating system's secret store.

Not checked here: a devpi that serves HTTPS with a private certificate authority,
devpi's replica mode, and indexes that inherit from other devpi indexes.

The check runs the page's configuration, client files, and commands out of the
page itself, with these substitutions: the example hosts `devpi.example.com` and
`sindri.example.com` become the hosts the check runs on, the netrc's
`YOUR_SINDRI_TOKEN` becomes a token, `machine` names the container's address for
the host, the check adds one `trusted-host` line to `pip.conf` because its Sindri
speaks plain HTTP, and in the publish block `dist/*` names the directory the check
built into, with `python -m build` run by the check's own pinned build step. The
export script is also run against two small local listeners, one that serves a
devpi-style index whose file links point to a second host and redirect to it, to
see that the login never reaches that host.
