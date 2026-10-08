#!/usr/bin/env bash
set -euo pipefail

for tool in docker trivy syft python3; do
  command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done
docker buildx version >/dev/null

artifact_dir="$(mktemp -d)"
builder_name="settlekit-verify-$$"
cleanup() {
  docker buildx rm --force "$builder_name" >/dev/null 2>&1 || true
  find "$artifact_dir" -depth -delete
}
trap cleanup EXIT
docker buildx create --name "$builder_name" --driver docker-container >/dev/null

docker buildx build --builder "$builder_name" --pull --load --tag settlekit:verify .
image_user="$(docker image inspect settlekit:verify --format '{{.Config.User}}')"
[[ "$image_user" == "65532:65532" ]] || { echo "image must run as non-root" >&2; exit 1; }
trivy fs --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress .
trivy image --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress settlekit:verify
docker save settlekit:verify -o "$artifact_dir/image.docker.tar"
syft "docker-archive:$artifact_dir/image.docker.tar" -o "spdx-json=$artifact_dir/sbom.spdx.json"
docker buildx build --builder "$builder_name" --attest type=provenance,mode=max \
  --output "type=oci,dest=$artifact_dir/image.oci.tar" .
python3 scripts/check_supply_chain.py "$artifact_dir/sbom.spdx.json" "$artifact_dir/image.docker.tar" "$artifact_dir/image.oci.tar"
