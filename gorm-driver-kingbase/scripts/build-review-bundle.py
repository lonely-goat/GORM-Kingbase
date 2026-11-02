#!/usr/bin/env python3
"""Build and verify a complete adapter/SDK source delivery without developer files."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
from urllib.parse import unquote, urlsplit
import zipfile


def local_links(document):
    """Read inline and reference-style Markdown links outside code examples."""
    text = re.sub(r"(?ms)^\s*```.*?^\s*```[^\n]*", "", document.read_text())
    text = re.sub(r"`[^`\n]+`", "", text)
    references = re.findall(r"\]\((<[^>]+>|[^)\n]+)\)", text)
    references += re.findall(r"(?m)^\[[^\]]+\]:\s*(<[^>]+>|\S+)\s*$", text)
    for reference in references:
        parts = urlsplit(reference.strip("<>"))
        if parts.scheme or parts.netloc or not parts.path:
            continue
        yield (document.parent / unquote(parts.path)).resolve()


def validate_document_links(root):
    """Require repository documentation to work without sibling workspace files."""
    root = root.resolve()
    for document in root.rglob("*.md"):
        for target in local_links(document):
            if not target.is_relative_to(root):
                raise RuntimeError(f"document target escapes repository: {document}: {target}")
            if not target.exists():
                raise RuntimeError(f"missing document target: {document}: {target}")


def file_hashes(root):
    return {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(root.rglob("*")) if path.is_file()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path, help="new directory outside the adapter repository")
    parser.add_argument("--race", action="store_true", help="also run the no-database race checks (requires CGO)")
    parser.add_argument("--test-databases", action="store_true", help="run copied integration tests against both dedicated instances")
    args = parser.parse_args()
    driver = Path(__file__).resolve().parent.parent
    repository = driver.parent
    destination = args.destination.resolve()
    archive = destination.with_name(destination.name + ".tar.gz")
    zip_archive = destination.with_name(destination.name + ".zip")
    validation_log = destination.with_name(destination.name + ".validation.log")
    if any(path.exists() for path in (destination, archive, zip_archive, validation_log)):
        parser.error("destination, archives and validation log must not already exist")
    if repository == destination or repository.is_relative_to(destination):
        parser.error("destination must not contain the source repository")
    if destination == driver or driver in destination.parents:
        parser.error("destination must be outside the adapter module")
    if args.test_databases and (os.environ.get("KINGBASE_TEST_ALLOW_DDL") != "yes" or
                               any(not os.environ.get("KINGBASE_" + mode + "_DSN") for mode in ("MYSQL", "ORACLE"))):
        parser.error("--test-databases requires both DSNs and KINGBASE_TEST_ALLOW_DDL=yes")

    environment = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off")
    sdk = Path(subprocess.check_output(
        ["go", "list", "-m", "-f", "{{.Dir}}", "kingbase.com/gokb"],
        cwd=driver, env=environment, text=True).strip()).resolve()
    if not (sdk / "go.mod").is_file():
        parser.error("resolved gokb source is unavailable")
    third_party = repository / "third_party"
    if sdk != (third_party / "gokb").resolve():
        parser.error("gokb must resolve to the repository's third_party/gokb source")
    if destination == third_party or third_party in destination.parents:
        parser.error("destination must be outside third_party")
    provenance = json.loads((third_party / "gokb-source.json").read_text())
    if file_hashes(sdk) != provenance["files"]:
        parser.error("gokb source no longer matches third_party/gokb-source.json")

    adapter_copy = destination / "gorm-driver-kingbase"
    sdk_copy = destination / "third_party/gokb"
    ignore = shutil.ignore_patterns(
        ".git", ".aws", ".claude", ".codex", ".agents", ".idea", ".vscode",
        "__pycache__", "*.pyc", "*.log", ".env", ".env.*", "coverage.out", "coverage.html",
        "*.test", "dist", "test-results", "archive", "deliverables")

    def ignore_adapter_files(directory, names):
        excluded = set(ignore(directory, names))
        # Exclude a root-level go-build executable, while preserving the
        # cmd/autodetect source directory needed by database checks.
        if Path(directory) == driver and (driver / "autodetect").is_file():
            excluded.add("autodetect")
        return excluded

    shutil.copytree(driver, adapter_copy, ignore=ignore_adapter_files)
    shutil.copytree(sdk, sdk_copy, ignore=ignore)
    for name in ("README.md", ".gitignore", "THIRD_PARTY_NOTICES.md"):
        shutil.copy2(repository / name, destination / name)
    for name in ("README.md", "gokb-source.json"):
        shutil.copy2(third_party / name, destination / "third_party" / name)
    (destination / "scripts").mkdir()
    shutil.copy2(repository / "scripts/test.sh", destination / "scripts/test.sh")
    validate_document_links(destination)
    if file_hashes(sdk_copy) != provenance["files"]:
        raise RuntimeError("copied gokb source differs from the original SDK")
    copied_sdk = Path(subprocess.check_output(
        ["go", "list", "-mod=readonly", "-m", "-f", "{{.Dir}}", "kingbase.com/gokb"],
        cwd=adapter_copy, env=environment, text=True).strip()).resolve()
    if copied_sdk != sdk_copy:
        raise RuntimeError("copied module still resolves gokb outside the delivery")
    environment.update(TZ="UTC")
    if not args.test_databases:
        environment.update(KINGBASE_MYSQL_DSN="", KINGBASE_ORACLE_DSN="", KINGBASE_DSN="")
    commands = [["go", "-C", "gorm-driver-kingbase", "list", "-buildvcs=false", "-mod=readonly", "-deps", "./..."],
                ["bash", "scripts/test.sh", "unit"]]
    if args.race:
        commands.append(["bash", "scripts/test.sh", "race"])
    if args.test_databases:
        commands.append(["bash", "scripts/test.sh", "integration"])
    with validation_log.open("w") as log:
        log.write("Delivery validation commands and results\n")
        for command in commands:
            log.write("$ " + " ".join(command) + "\n")
            log.flush()
            subprocess.run(command, cwd=destination, env=environment,
                           stdout=log, stderr=subprocess.STDOUT, check=True)
    manifest = {
        "gokb_module": "kingbase.com/gokb",
        "gokb_source_matches_provenance": True,
        "gokb_public_distribution_permission": provenance["public_distribution_permission"],
        "gorm_version": json.loads((adapter_copy / "scripts/gorm-baseline.json").read_text())["version"],
        "repository_document_links_verified": True,
        "build_unit_tests_and_vet_passed": True,
        "race_tests_passed": args.race,
        "copied_integration_verified_on_both_databases": args.test_databases,
        "official_gorm_database_regression_run": False,
        "files": file_hashes(destination),
    }
    (destination / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
    with tarfile.open(archive, "w:gz") as output:
        output.add(destination, arcname=destination.name)
    with zipfile.ZipFile(zip_archive, "w", compression=zipfile.ZIP_DEFLATED) as output:
        for path in sorted(destination.rglob("*")):
            if path.is_file():
                output.write(path, str(Path(destination.name) / path.relative_to(destination)))
    print("Module resolution, SDK checksums, build, unit tests, vet and document links passed.")
    print("Delivery directory: " + str(destination))
    print("Source archive: " + str(archive))
    print("ZIP archive: " + str(zip_archive))
    print("Validation log: " + str(validation_log))


if __name__ == "__main__":
    main()
