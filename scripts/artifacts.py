#!/usr/bin/env python3
"""Create checksums and a CycloneDX Go-runtime SBOM from the actual binary.

Uses only Python's standard library and `go version`; it does not execute the
binary, download dependencies, or add packages to the application.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
from urllib.parse import quote


def component(module):
    effective = module.get("Replace", module)
    if "Replace" in module and not effective.get("Version"):
        raise ValueError("local module replacements cannot be packaged")
    name, version = effective["Path"], effective.get("Version", "(devel)")
    result = {"type": "library", "name": name, "version": version,
              "bom-ref": name + "@" + version,
              "purl": "pkg:golang/" + quote(name, safe="/") + "@" + quote(version, safe="")}
    if effective.get("Sum"):
        result["properties"] = [{"name": "go:module-sum", "value": effective["Sum"]}]
    return result


def make_bom(info, digest):
    app = component(info["Main"])
    app["type"] = "application"
    app["hashes"] = [{"alg": "SHA-256", "content": digest}]
    runtime = {"type": "framework", "name": "Go runtime", "version": info["GoVersion"],
               "bom-ref": "go-runtime@" + info["GoVersion"]}
    dependencies = [component(m) for m in info.get("Deps", [])]
    dependencies = sorted({c["bom-ref"]: c for c in dependencies}.values(), key=lambda c: c["bom-ref"])
    properties = [{"name": "go:compiler", "value": info["GoVersion"]}]
    for setting in info.get("Settings", []):
        if setting["Key"] in {"GOOS", "GOARCH", "CGO_ENABLED", "vcs.revision", "vcs.modified"}:
            properties.append({"name": "go:" + setting["Key"], "value": setting["Value"]})
    return {"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1,
            "metadata": {"component": app, "properties": properties},
            "components": [runtime] + dependencies,
            "dependencies": [{"ref": app["bom-ref"], "dependsOn": [runtime["bom-ref"]] + [c["bom-ref"] for c in dependencies]}]}


def generate(binary):
    binary = Path(binary)
    info = json.loads(subprocess.check_output(["go", "version", "-m", "-json", str(binary)], text=True))
    with binary.open("rb") as f:
        digest = hashlib.file_digest(f, "sha256").hexdigest()
    bom = make_bom(info, digest)
    sbom = binary.with_name(binary.name + ".cdx.json")
    sbom.write_text(json.dumps(bom, indent=2, sort_keys=True) + "\n")
    checksum = binary.with_name(binary.name + ".sha256")
    checksum.write_text(digest + "  " + binary.name + "\n")
    return sbom, checksum


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    args = parser.parse_args()
    for artifact in generate(args.binary):
        print(artifact)


if __name__ == "__main__":
    main()
