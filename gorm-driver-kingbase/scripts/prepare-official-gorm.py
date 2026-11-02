#!/usr/bin/env python3
"""Extract a checksummed, version-matched official GORM baseline into a fresh directory."""

import hashlib
import json
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import sys
import tarfile


def prepare(driver, destination):
    driver = Path(driver).resolve()
    destination = Path(destination).resolve()
    metadata = json.loads((driver / "scripts/gorm-baseline.json").read_text())
    module = json.loads(subprocess.check_output(["go", "mod", "edit", "-json"], cwd=driver, text=True))
    required = next(item["Version"] for item in module["Require"] if item["Path"] == "gorm.io/gorm")
    if required != metadata["version"]:
        raise RuntimeError(f"GORM baseline {metadata['version']} does not match declared dependency {required}")
    if any(item["Old"]["Path"] == "gorm.io/gorm" for item in module.get("Replace", [])):
        raise RuntimeError("GORM dependency has a replace directive; establish a matching baseline explicitly")
    archive = driver / metadata["archive"]
    if hashlib.sha256(archive.read_bytes()).hexdigest() != metadata["sha256"]:
        raise RuntimeError("official GORM archive checksum mismatch")
    destination.mkdir(parents=True, exist_ok=False)
    with tarfile.open(archive, "r:gz") as source:
        for member in source.getmembers():
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != metadata["archive_root"]:
                raise RuntimeError("unsafe archive path: " + member.name)
            target = destination.joinpath(*path.parts[1:])
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                target.parent.mkdir(parents=True, exist_ok=True)
                with source.extractfile(member) as content, target.open("wb") as output:
                    shutil.copyfileobj(content, output)
                target.chmod(0o755 if member.mode & 0o111 else 0o644)
            else:
                raise RuntimeError("unsupported archive member: " + member.name)
    # The tests submodule's upstream replace points at this root; therefore
    # runtime core and upstream test sources always originate from the same tag.
    if "replace gorm.io/gorm => ../" not in (destination / "tests/go.mod").read_text():
        raise RuntimeError("upstream tests no longer select their adjacent core")
    print(f"Official GORM baseline: {required}; archive SHA256={metadata['sha256']}", flush=True)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: prepare-official-gorm.py NEW_DESTINATION")
    prepare(Path(__file__).resolve().parent.parent, Path(sys.argv[1]))
