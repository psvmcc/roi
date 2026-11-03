"""Validate release tags before using them as filenames or registry tags."""
import re
import sys


def validate_tag(tag):
    # OCI tags cannot contain SemVer build metadata (+).
    match = re.fullmatch(r"v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?", tag)
    if not match or len(tag) > 128:
        raise ValueError("expected a version tag such as v1.2.3 or v1.2.3-rc.1 (no build metadata)")
    prerelease = match[4]
    if prerelease and any(part.isdigit() and len(part) > 1 and part[0] == "0" for part in prerelease.split(".")):
        raise ValueError("numeric prerelease identifiers must not contain leading zeroes")
    return bool(prerelease)


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2:
            raise ValueError("usage: release.py TAG")
        print("prerelease=" + str(validate_tag(sys.argv[1])).lower())
    except ValueError as error:
        sys.exit(str(error))
