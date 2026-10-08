#!/usr/bin/env python3

import json
import hashlib
import pathlib
import sys
import tarfile


def require(condition, message):
    if not condition:
        raise SystemExit(message)


def blob(archive, digest):
    algorithm, value = digest.split(":", 1)
    require(algorithm == "sha256", "unexpected OCI digest algorithm")
    content = archive.extractfile(f"blobs/sha256/{value}").read()
    require(hashlib.sha256(content).hexdigest() == value, "OCI blob digest mismatch")
    return json.loads(content)


def inspect_oci(archive, document):
    found, images = [], []
    for descriptor in document.get("manifests", []):
        child = blob(archive, descriptor["digest"])
        if descriptor.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest":
            for layer in child.get("layers", []):
                if layer.get("mediaType") == "application/vnd.in-toto+json":
                    found.append(blob(archive, layer["digest"]))
        else:
            if child.get("config"):
                images.append(child["config"]["digest"])
            nested, nested_images = inspect_oci(archive, child)
            found.extend(nested)
            images.extend(nested_images)
    return found, images


def main():
    require(len(sys.argv) == 4, "usage: check_supply_chain.py SBOM DOCKER_ARCHIVE OCI_ARCHIVE")
    sbom = json.loads(pathlib.Path(sys.argv[1]).read_text())
    require(sbom.get("spdxVersion") == "SPDX-2.3", "SPDX version is missing")
    require(sbom.get("packages"), "SBOM contains no packages")
    require(sbom.get("documentNamespace"), "SBOM namespace is missing")
    with tarfile.open(sys.argv[2]) as archive:
        docker_manifest = json.load(archive.extractfile("manifest.json"))
        require(len(docker_manifest) == 1, "Docker archive must contain one image")
        config_bytes = archive.extractfile(docker_manifest[0]["Config"]).read()
        image_config = "sha256:" + hashlib.sha256(config_bytes).hexdigest()
    with tarfile.open(sys.argv[3]) as archive:
        documents, image_configs = inspect_oci(archive, json.load(archive.extractfile("index.json")))
    require(image_config in image_configs, "scanned Docker image differs from provenance OCI image")
    provenances = [item for item in documents if item.get("predicateType") == "https://slsa.dev/provenance/v1"]
    require(provenances, "BuildKit SLSA provenance attestation is missing")
    for item in provenances:
        predicate = item.get("predicate", {})
        require(predicate.get("buildDefinition"), "provenance build definition is missing")
        require(predicate.get("runDetails"), "provenance run details are missing")
    print(f"SPDX packages={len(sbom['packages'])}; SLSA provenance attestations={len(provenances)}: PASS")


if __name__ == "__main__":
    main()
