# Sindri

A registry for container images, Python packages, and npm packages, running as one
replica. The chart deploys Sindri and nothing else: bring Postgres, an S3-compatible
bucket or a volume, and optionally an OIDC provider for UI sign-in.

The full guide, with a working `values.yaml`, the Secret it reads, an Ingress, storage,
scanning, and upgrades, is
[Deploying on Kubernetes with Helm](https://spjdevops.github.io/sindri/deploy/helm.html).

## Install

The chart is published to ghcr.io as an OCI artifact, so it needs Helm 3.8 or later and
no `helm repo add`. Chart versions match Sindri releases.

```bash
helm install sindri oci://ghcr.io/spjdevops/charts/sindri --version <version> \
  --namespace sindri --create-namespace --values values.yaml
```

The `config` value is `sindri.yaml`, written to a ConfigMap unchanged, so every key in the
[configuration reference](https://spjdevops.github.io/sindri/configuration.html) works.
The chart refuses to render without `config.database`, `config.storage`, and
`config.auth.static_token`. Keep secrets out of the values: write them as `${VAR}` and
supply them from a Secret through `envFrom`, as the guide shows.

## Values

`helm show values oci://ghcr.io/spjdevops/charts/sindri` lists every value with its
default. `values.schema.json` rejects a misspelled chart value; keys inside `config` are
checked by Sindri itself when it starts.
