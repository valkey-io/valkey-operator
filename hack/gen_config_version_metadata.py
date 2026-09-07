#!/usr/bin/env python3
# Copyright 2025 Valkey Contributors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""
Generate the ConfigIntroducedIn map for the valkey-operator by scanning the
Valkey source tree, tag by tag, to find the first Valkey release that
understands each configuration directive.

Directives are declared in src/config.c as create<Type>Config("name", ...).
For each release tag the script reads that file via `git show <tag>:...`
(no checkout; the working tree is untouched) and records the earliest tag a
directive appears in. HIDDEN_CONFIG directives are skipped: they are internal,
absent from CONFIG GET, and not a supported user-facing setting.

All release candidates and the final X.Y.0 of each minor line are scanned.
Patch releases (X.Y.Z, Z>0) are ignored: Valkey backports new directives into
them (e.g. the 9.1 feature tls-auto-reload-interval also ships in 8.0.8), so
scanning only .0 (and its rcs) attributes a directive to the minor that
introduced it.

A directive is gated at the earliest version it is seen in, with two
adjustments (see combine_gates):
  - If the directive no longer exists in the newest release scanned for its
    minor line, it is dropped (added in an rc but removed before a later rc or
    the final).
  - While only release candidates exist for a line, a directive introduced in
    an rc is gated at that rc, so users testing any rc still get it. Once the
    final X.Y.0 ships, the gate snaps to the final version.

--baseline filters the output only: directives first seen at or before it are
assumed universally known and omitted. It defaults to 7.2.5 (emit everything);
pass the lowest version you gate against, e.g. 8.1.0. Output is deterministic
(tags sorted by semver, keys sorted), so identical inputs produce identical
bytes.

Usage:
    hack/gen_config_version_metadata.py \\
        --valkey-repo path/to/valkey-repo \\
        --baseline 8.1.0 \\
        --out internal/valkey/config_version_metadata.go

Re-run whenever a new Valkey version is released (after fetching its tag).
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass

# First Valkey release, and the scan anchor.
SCAN_FROM = "7.2.5"

# A config declaration, capturing the directive name, optional alias, and the
# flags argument, e.g. createBoolConfig("protected-mode", NULL, MODIFIABLE_CONFIG,
# ...). The leading \s excludes "#define createBoolConfig(name," macro
# definitions. The flags are inspected to skip HIDDEN_CONFIG directives, which
# are internal and do not appear in CONFIG GET.
_CONFIG_RE = re.compile(
    r'create[A-Za-z0-9_]+Config\(\s*"([^"]+)"\s*,\s*(?:"([^"]+)"|NULL)\s*,\s*([^,]+),',
)

# Release-tag matcher: optional leading "v", MAJOR.MINOR.PATCH, optional -rcN.
_TAG_RE = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:-rc(\d+))?$")


@dataclass(frozen=True, order=True)
class Version:
    """A comparable Valkey version.

    A final release sorts after its release candidates (9.1.0-rc1 < 9.1.0):
    non-prereleases use rc == sys.maxsize so they sort last within a patch.
    """

    major: int
    minor: int
    patch: int
    rc: int  # sys.maxsize for a final (non-rc) release

    @property
    def is_prerelease(self) -> bool:
        return self.rc != sys.maxsize

    @property
    def core(self) -> tuple[int, int, int]:
        return (self.major, self.minor, self.patch)

    def go_literal(self) -> str:
        base = f"{self.major}.{self.minor}.{self.patch}"
        return f"{base}-rc{self.rc}" if self.is_prerelease else base

    def __str__(self) -> str:
        return self.go_literal()


def parse_tag(tag: str) -> Version | None:
    m = _TAG_RE.match(tag.strip())
    if not m:
        return None
    rc = int(m.group(4)) if m.group(4) is not None else sys.maxsize
    return Version(int(m.group(1)), int(m.group(2)), int(m.group(3)), rc)


def git(repo: str, *args: str, allow_missing: bool = False) -> str | None:
    """Run git in repo and return stdout; raises on failure, or returns None
    for a path missing from the tree when allow_missing is set."""
    result = subprocess.run(
        ["git", "-C", repo, *args], capture_output=True, text=True
    )
    if result.returncode != 0:
        stderr = result.stderr.strip()
        if allow_missing and (
            "does not exist" in stderr or "exists on disk, but not in" in stderr
        ):
            return None
        raise RuntimeError(
            f"git -C {repo} {' '.join(args)} failed "
            f"(rc={result.returncode}): {stderr}"
        )
    return result.stdout


def list_release_tags(
    repo: str, minimum: Version
) -> tuple[list[tuple[Version, str]], dict[tuple[int, int], Version]]:
    """Return the (version, tag) pairs to scan and the released finals per line.

    All release candidates and the final X.Y.0 of each minor line at or above
    `minimum` are scanned, in ascending order, so a directive can be attributed
    to the earliest rc it appears in. Patch releases (X.Y.Z, Z>0) are ignored --
    see the module docstring. `minimum` itself is always kept as the scan
    anchor, even if it is a patch release.

    The second return value maps each minor line (major, minor) that has a
    released final X.Y.0 to that final's Version. The caller uses it to apply
    the hybrid gating rule: while only rcs exist, a directive is gated at the
    earliest rc that introduced it; once the final ships, the gate snaps to the
    final version (see combine_gates).
    """
    # Candidate tags per minor line: the .0 final and its -rcN prereleases.
    # value: {"final": (v, tag), "rcs": [(v, tag), ...]}
    by_line: dict[tuple[int, int], dict] = {}
    anchor: tuple[Version, str] | None = None
    for tag in (git(repo, "tag") or "").splitlines():
        v = parse_tag(tag)
        if v is None or v < minimum:
            continue
        if v.core == minimum.core and not v.is_prerelease:
            anchor = (v, tag)  # scan anchor, kept regardless of patch level
            continue
        if v.patch != 0:
            continue  # patch releases are ignored (backport attribution)
        line = by_line.setdefault((v.major, v.minor), {"final": None, "rcs": []})
        if v.is_prerelease:
            line["rcs"].append((v, tag))
        else:
            line["final"] = (v, tag)

    chosen: list[tuple[Version, str]] = []
    finals: dict[tuple[int, int], Version] = {}
    if anchor is not None:
        chosen.append(anchor)
    for key, line in by_line.items():
        # Scan every rc so a directive is attributed to the earliest rc it
        # appears in, plus the final if released.
        chosen.extend(line["rcs"])
        if line["final"] is not None:
            chosen.append(line["final"])
            finals[key] = line["final"][0]
    return sorted(chosen, key=lambda vt: vt[0]), finals


def directives_at_tag(repo: str, tag: str) -> set[str]:
    """Return the directive names (and aliases) declared at a tag.

    A tag whose tree has no src/config.c yields an empty set; any other git
    failure propagates (see git's allow_missing).
    """
    source = git(repo, "show", f"refs/tags/{tag}:src/config.c", allow_missing=True)
    if source is None:
        return set()
    names: set[str] = set()
    for name, alias, flags in _CONFIG_RE.findall(source):
        # HIDDEN_CONFIG directives are internal: absent from CONFIG GET and not
        # a supported user-facing setting, so they are not gated.
        if "HIDDEN_CONFIG" in flags:
            continue
        names.add(name)
        if alias:
            names.add(alias)
    return names


def compute_first_seen(
    repo: str, tags: list[tuple[Version, str]], debug: bool
) -> tuple[dict[str, Version], dict[tuple[int, int], set[str]]]:
    """Scan tags in ascending order and return two mappings.

    - first_seen: each directive name -> the earliest scanned version that
      declares it.
    - latest_line_directives: each minor line (major, minor) -> the directive
      set of the newest tag scanned for that line. The caller uses this to drop
      directives that no longer exist in the latest release of their line (see
      combine_gates), so a directive added in an rc but removed before a later
      rc or the final is not emitted.
    """
    first_seen: dict[str, Version] = {}
    latest_line_directives: dict[tuple[int, int], set[str]] = {}
    for version, tag in tags:
        directives = directives_at_tag(repo, tag)
        # Tags arrive in ascending order, so the last one seen for a line is
        # its newest scanned release.
        latest_line_directives[(version.major, version.minor)] = directives
        new_here = sorted(n for n in directives if n not in first_seen)
        for name in new_here:
            first_seen[name] = version
        if debug:
            print(
                f"[debug] {str(version):<12} tag={tag:<12} "
                f"parsed={len(directives):>3} new={len(new_here)}"
                + (f" -> {', '.join(new_here)}" if new_here else ""),
                file=sys.stderr,
            )
    return first_seen, latest_line_directives


def combine_gates(
    first_seen: dict[str, Version],
    finals: dict[tuple[int, int], Version],
    latest_line_directives: dict[tuple[int, int], set[str]],
) -> dict[str, Version]:
    """Apply the hybrid gating rule to first-seen versions.

    For each directive, gated at the version it was first seen in:
      - Drop it if it no longer exists in the newest release scanned for its
        minor line (added in an rc but removed before a later rc or the final).
      - If that line has a released final X.Y.0, snap the gate up to the final
        version, so a released image is stated once available.
      - Otherwise (rc-only phase) keep the earliest rc version, so images
        running any rc of that line still get the directive.
    """
    result: dict[str, Version] = {}
    for name, version in first_seen.items():
        line = (version.major, version.minor)
        # Drop directives that vanished from the newest scanned release.
        latest = latest_line_directives.get(line, set())
        if name not in latest:
            continue
        final = finals.get(line)
        if version.is_prerelease and final is not None:
            result[name] = final  # released final wins once it ships
        else:
            result[name] = version  # rc-only phase, or introduced at a final
    return result


def go_quote(s: str) -> str:
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def gofmt(source: str) -> str:
    """Format Go source with gofmt so output matches the committed (gofmt'd)
    file. Requires gofmt on PATH; raises RuntimeError if it is missing or fails.
    """
    try:
        proc = subprocess.run(
            ["gofmt"], input=source, capture_output=True, text=True
        )
    except FileNotFoundError as e:
        raise RuntimeError("gofmt not found on PATH") from e
    if proc.returncode != 0:
        raise RuntimeError(f"gofmt failed: {proc.stderr.strip()}")
    return proc.stdout


def render_go(
    gated: dict[str, Version],
    package: str,
    baseline: Version,
    scanned_to: Version,
    regen_command: str,
) -> str:
    lines = [
        "// Code generated by hack/gen_config_version_metadata.py. DO NOT EDIT.",
        "//",
        "// Regenerate with:",
        f"//   {regen_command}",
        "//",
        f"// Lists directives introduced after {baseline}, up to {scanned_to}.",
        "",
        f"package {package}",
        "",
        'import semver "github.com/Masterminds/semver/v3"',
        "",
        "// ConfigIntroducedIn maps user-facing config directives to the minimum",
        "// Valkey version that understands them. Only directives introduced after",
        f"// the {baseline} baseline are listed; anything not present is assumed",
        "// supported by every Valkey version the operator supports.",
        "var ConfigIntroducedIn = map[string]*semver.Version{",
    ]
    for name in sorted(gated):
        lines.append(
            f"\t{go_quote(name)}: semver.MustParse({go_quote(gated[name].go_literal())}),"
        )
    lines.append("}")
    lines.append("")
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--valkey-repo",
        default="_valkey",
        help="Path to the Valkey source git checkout (default: _valkey)",
    )
    parser.add_argument(
        "--baseline",
        default="7.2.5",
        help=(
            "Emit only directives first seen after this version; earlier ones "
            "are assumed universally known (default: 7.2.5)"
        ),
    )
    parser.add_argument(
        "--package",
        default="valkey",
        help="Go package name for the generated file (default: valkey)",
    )
    parser.add_argument(
        "--out",
        default="-",
        help="Output Go file path, or '-' for stdout (default: -)",
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help=(
            "Do not write; compare freshly generated output against --out and "
            "exit non-zero if they differ."
        ),
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help="Log per-tag directive counts and new directives to stderr",
    )
    args = parser.parse_args()

    baseline = parse_tag(args.baseline)
    if baseline is None:
        print(f"error: invalid --baseline {args.baseline!r}", file=sys.stderr)
        return 2
    scan_from = parse_tag(SCAN_FROM)

    tags, finals = list_release_tags(args.valkey_repo, scan_from)
    if not tags:
        print(
            f"error: no release tags >= {scan_from} in {args.valkey_repo}; is it "
            "a Valkey git checkout with tags fetched?",
            file=sys.stderr,
        )
        return 1

    if args.debug:
        order = " ".join(str(v) for v, _ in tags)
        print(f"[debug] scan order ({len(tags)} tags): {order}", file=sys.stderr)

    first_seen, latest_line_directives = compute_first_seen(
        args.valkey_repo, tags, debug=args.debug
    )
    first_seen = combine_gates(first_seen, finals, latest_line_directives)

    # Sanity guard: the newest tag must parse a non-trivial directive set, or
    # the checkout/parser is broken (wrong path, shallow clone, format change).
    # Reuses the set compute_first_seen already parsed for that tag's line.
    newest_version, newest_tag = tags[-1]
    newest_line = (newest_version.major, newest_version.minor)
    if len(latest_line_directives.get(newest_line, set())) < 10:
        print(
            f"error: <10 directives parsed from {newest_tag}:src/config.c; the "
            "checkout or parser is likely broken.",
            file=sys.stderr,
        )
        return 1

    gated = {
        name: version
        for name, version in first_seen.items()
        if version.core > baseline.core
    }

    # Reconstruct the invocation for the "Regenerate with" header.
    regen_parts = [
        "hack/gen_config_version_metadata.py",
        "--valkey-repo <path>",
        f"--baseline {args.baseline}",
    ]
    if args.package != "valkey":
        regen_parts.append(f"--package {args.package}")
    regen_parts.append("--out <this file>")

    output = render_go(
        gated,
        package=args.package,
        baseline=baseline,
        scanned_to=tags[-1][0],
        regen_command=" ".join(regen_parts),
    )
    output = gofmt(output)

    if args.check:
        if args.out == "-":
            print("error: --check requires --out <file>", file=sys.stderr)
            return 2
        try:
            with open(args.out, "r", encoding="utf-8") as f:
                existing = f.read()
        except FileNotFoundError:
            print(f"error: --check: {args.out} does not exist", file=sys.stderr)
            return 1
        if existing != output:
            regen = (
                f"hack/gen_config_version_metadata.py --valkey-repo {args.valkey_repo} "
                f"--baseline {args.baseline} --out {args.out}"
            )
            print(
                f"error: {args.out} is out of date; regenerate with:\n  {regen}",
                file=sys.stderr,
            )
            return 1
        print(f"{args.out} is up to date", file=sys.stderr)
        return 0

    if args.out == "-":
        sys.stdout.write(output)
    else:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(output)
        print(
            f"wrote {len(gated)} gated directive(s) to {args.out} "
            f"(scanned from {tags[0][0]} to {tags[-1][0]})",
            file=sys.stderr,
        )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except RuntimeError as e:
        print(f"error: {e}", file=sys.stderr)
        raise SystemExit(1)
