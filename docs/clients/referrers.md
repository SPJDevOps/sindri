# Signatures and SBOMs with cosign and oras

<!-- commands checked by: scripts/referrers-gate.sh -->

cosign, oras, Notation, and Trivy attach a signature or an SBOM to an image by pushing a
second manifest that names the image as its `subject`. They find it again through the OCI
referrers API, `GET /v2/<name>/referrers/<digest>`. Sindri serves that API for hosted
repositories from 1.5, so these tools work with their defaults.

The examples use the [quickstart](../quickstart.md)'s `docker` repository on plain HTTP,
hence `--plain-http`. Behind TLS, leave it out.

## Attach and discover with oras

Any manifest can be a subject. This pushes a stand-in for an image, then attaches an SBOM
to it:

<!-- referrers:attach -->
```bash
echo 'hello' > hello.txt
oras push --plain-http -u token -p "$SINDRI_TOKEN" \
  localhost:8080/docker/demo/app:1.0 hello.txt

echo '{"spdxVersion":"SPDX-2.3"}' > sbom.spdx.json
oras attach --plain-http -u token -p "$SINDRI_TOKEN" \
  --artifact-type application/spdx+json \
  localhost:8080/docker/demo/app:1.0 sbom.spdx.json:application/spdx+json
```
<!-- /referrers:attach -->

The `:application/spdx+json` after the file name is the layer's media type. Give one:
without it oras labels the file as a container image layer, and Sindri then treats the
attachment as an image and scans it.

`oras discover` lists what refers to the image:

<!-- referrers:discover -->
```bash
oras discover --plain-http -u token -p "$SINDRI_TOKEN" \
  localhost:8080/docker/demo/app:1.0
```
<!-- /referrers:discover -->

## Sign and verify with cosign

cosign 2 stores a signature as a referrer when asked to. This example signs with a local
key and stays out of the public transparency log so that it runs offline. For real use,
leave out `--tlog-upload=false` and `--insecure-ignore-tlog`.

<!-- referrers:sign -->
```bash
cosign login localhost:8080 -u token -p "$SINDRI_TOKEN"
cosign generate-key-pair

DIGEST="$(oras resolve --plain-http -u token -p "$SINDRI_TOKEN" localhost:8080/docker/demo/app:1.0)"
COSIGN_EXPERIMENTAL=1 cosign sign --key cosign.key --tlog-upload=false \
  --registry-referrers-mode=oci-1-1 -y "localhost:8080/docker/demo/app@$DIGEST"
```
<!-- /referrers:sign -->

<!-- referrers:verify -->
```bash
cosign verify --key cosign.pub --insecure-ignore-tlog --experimental-oci11 \
  "localhost:8080/docker/demo/app@$DIGEST"
```
<!-- /referrers:verify -->

## The API itself

The answer is an OCI image index with one descriptor per referrer. It is empty, with
status 200, when nothing refers to the digest. `artifactType` filters the list:

<!-- referrers:api -->
```bash
curl -fsS -u "token:$SINDRI_TOKEN" \
  "http://localhost:8080/v2/docker/demo/app/referrers/$DIGEST?artifactType=application/spdx%2Bjson"
```
<!-- /referrers:api -->

A long list is paged with a `Link` header, as the tags list is.

## What to know

- **A referrer is not evidence.** A lifecycle gate that requires a `signature` or an
  `sbom` reads only what was attached through
  `POST /api/v1/subjects/{digest}/evidence`. A cosign signature pushed as a referrer is
  discoverable by cosign and does not satisfy the gate. Sindri does not verify
  signatures yet.
- **Each repository has its own list.** A list holds the referrers pushed to that
  repository and no others, even when another repository holds the same image.
- **Hosted repositories only.** A proxy repository answers with an empty list and does
  not ask its upstream. A virtual repository answers from its first hosted member that
  holds the image and that you may read.
- **A referrer lives as long as its subject.** Garbage collection keeps an untagged
  signature while a repository holds both it and the image it signs. It is collected
  with the image, or once you delete it from the repository.
- **Signatures and SBOMs are not scanned on publish.** That means a manifest that names
  a subject and has neither an image config nor an image layer. Anything that looks like
  an image is scanned, whether or not it names a subject. See [Scanning](../scanning.md).
- **Pushed before 1.5?** Manifests pushed to an older Sindri are not listed. Attach them
  again. See [Upgrading](../upgrading.md#behaviour-changes-in-15).
