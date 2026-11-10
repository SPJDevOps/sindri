# Using Sindri as a pip index

<!-- commands checked by: scripts/pypi-page-gate.sh -->

Sindri is usable as a private Python index today, for both publishing and installing.
Starting from the [quickstart](../quickstart.md), declare a repository once, in the configuration
file or through the API. PyPI repositories are never auto-created, so a typo in a
`--repository-url` fails loudly instead of silently creating a second index:

```bash
curl -fsS -X POST localhost:8080/api/v1/repositories \
  -H "Authorization: Bearer $SINDRI_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"internal","type":"hosted","protocol":"pypi"}'
```

Publish with twine, and install with pip:

```bash
python -m twine upload \
  --repository-url http://127.0.0.1:8080/pypi/internal/upload \
  -u token -p "$SINDRI_TOKEN" dist/*

pip install --index-url "http://token:$SINDRI_TOKEN@127.0.0.1:8080/pypi/internal/simple/" \
  --trusted-host 127.0.0.1:8080 your-package
```

Or run [`scripts/pypi-twine-gate.sh`](https://github.com/SPJDevOps/sindri/blob/main/scripts/pypi-twine-gate.sh), which does all of the
above against a checked-in sample package.

To avoid repeating the URLs, `~/.pypirc`:

```ini
[distutils]
index-servers = sindri

[sindri]
repository = http://127.0.0.1:8080/pypi/internal/upload
username = token
password = <your token>
```

Worth knowing before you point real work at it:

- **Every request authenticates**, the index and downloads included. The username is
  ignored; the password is the token. Uploading needs the write role on the repository.
- **Use one virtual index, not `--extra-index-url`.** With two indexes, pip chooses by
  version and not by trust. A virtual repository listing your hosted repository before a
  pypi.org proxy never serves a name you publish from the proxy, whatever version the
  proxy offers. That is the dependency-confusion defence of invariant I5.
- **A published file is immutable.** Re-uploading identical bytes is a no-op while the
  file exists; different bytes under a filename already published is a 409, because
  consumers pin the hash the index advertised. A deleted file's name stays taken too: it
  is refused even with identical bytes. Bump the version to republish. See
  [ADR 13](../adr/0013-pypi-write-time-validation-and-immutable-files.md).
- **Wheels and sdists only** (`.whl`, `.tar.gz`, `.tgz`), and an upload must be
  self-consistent: the filename, the packaged metadata, and twine's form fields have to
  agree on project and version. Build tools satisfy that; a hand-rolled `curl -F` may not.
- **No yanking.** Removing a file is a `DELETE` on its management API path,
  `/api/v1/repositories/{repo}/packages/{name}/versions/{version}/files/{filename}`.
