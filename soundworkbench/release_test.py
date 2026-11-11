"""Offline integration tests: real Git histories, mocked Docker/GitHub boundaries."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parent
BASE = "v1.5.2-beta.1"
VERSION = "v2.0.0"
PREVIOUS = "v1.5.2-beta.1-sw.167-sw.14"
NAME = f"cc-connect-{VERSION}-linux-amd64"


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.repo = self.root / "src"
        self.remote = self.root / "origin.git"
        self.bin = self.root / "bin"
        self.repo.mkdir()
        self.bin.mkdir()
        self.trace = self.root / "trace"
        # Callers may use an alternate Git directory; fixtures must never inherit it.
        clean_env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env = dict(clean_env, TEST_TRACE=str(self.trace), TEST_REMOTE=str(self.remote),
                        TEST_GIT=shutil.which("git"), TEST_VERSION=VERSION, TEST_PREVIOUS=PREVIOUS,
                        PATH=str(self.bin) + os.pathsep + os.environ["PATH"])
        self.git("init", "-q", "-b", "local")
        self.git("config", "user.name", "Release Test")
        self.git("config", "user.email", "release-test@example.invalid")
        self.git("init", "--bare", "-q", str(self.remote))
        self.git("remote", "add", "origin", "https://github.com/soundworkbench/cc-connect.git")
        (self.repo / "soundworkbench").mkdir()
        for name in ("build.sh", "release.sh"):
            shutil.copy2(SCRIPTS / name, self.repo / "soundworkbench" / name)
        (self.repo / "soundworkbench/VERSION").write_text(VERSION + "\n")
        (self.repo / ".gitignore").write_text("dist/\n")
        self.git("add", ".")
        self.git("commit", "-qm", "Previous runtime")
        self.git("tag", BASE)
        self.git("tag", PREVIOUS)
        self.git("push", "-q", str(self.remote), "local", "--tags")
        (self.repo / "change").write_text("new release tooling\n")
        self.git("add", "change")
        self.git("commit", "-qm", "build: adopt semantic releases")
        self.write_tool("git", '''import os, subprocess, sys
args = sys.argv[1:]
if args[0] in ("fetch", "ls-remote"):
    args = [os.environ["TEST_REMOTE"] if a == "origin" else a for a in args]
sys.exit(subprocess.call([os.environ["TEST_GIT"], *args]))
''')
        self.write_tool("gh", '''import json, os, sys
with open(os.environ["TEST_TRACE"], "a") as f: f.write(json.dumps(["gh", *sys.argv[1:]]) + "\\n")
if sys.argv[1] == "api":
    if os.environ.get("TEST_API_FAILURE"): sys.exit("GitHub unavailable")
    releases = [{"tag_name": os.environ["TEST_PREVIOUS"], "draft": False, "published_at": "2026-11-10T00:00:00Z"}]
    if os.environ.get("TEST_RELEASE_EXISTS"):
        releases.append({"tag_name": os.environ["TEST_VERSION"], "draft": True, "published_at": None})
    print(json.dumps([releases]))
elif sys.argv[1:3] == ["release", "view"]:
    version = os.environ["TEST_VERSION"]
    name = "cc-connect-" + version + "-linux-amd64"
    if os.environ.get("TEST_BAD_ASSETS"): name = "wrong-name"
    print(json.dumps({"tagName": version, "name": version, "isDraft": False,
                      "assets": [{"name": name}, {"name": name + ".sha256"}]}))
elif sys.argv[1:3] != ["release", "create"]:
    sys.exit("unexpected gh operation")
''')
        self.write_tool("docker", '''import io, json, os, re, sys, tarfile
args = sys.argv[1:]
with open(os.environ["TEST_TRACE"], "a") as f: f.write(json.dumps(["docker", *args]) + "\\n")
archive = tarfile.open(fileobj=io.BytesIO(sys.stdin.buffer.read()))
version = archive.extractfile("soundworkbench/VERSION").read().decode().strip()
if "go build" in " ".join(args):
    assert "-X main.version=" + version in args[-1], args
    print("#!/bin/sh\\nprintf '%s\\\\n' 'cc-connect " + version + "'")
elif os.environ.get("TEST_FAIL_TESTS"):
    sys.exit(1)
''')

    def git(self, *args):
        return subprocess.check_output([shutil.which("git"), *args], cwd=self.repo, text=True,
                                       env=self.env, stderr=subprocess.STDOUT).strip()

    def write_tool(self, name, body):
        path = self.bin / name
        path.write_text("#!/usr/bin/env python3\n" + body)
        path.chmod(0o755)

    def run_script(self, name, *args, ok=True):
        proc = subprocess.run([str(self.repo / "soundworkbench" / name), *args], cwd=self.repo,
                              env=self.env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if ok:
            self.assertEqual(proc.returncode, 0, proc.stdout)
        else:
            self.assertNotEqual(proc.returncode, 0, proc.stdout)
        return proc

    def calls(self):
        return [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []

    def push_source(self):
        self.git("push", "-q", str(self.remote), "local")

    def test_unpushed_commit_is_rejected_before_build(self):
        # The cached tracking ref lies; the script must refresh the real remote.
        self.git("update-ref", "refs/remotes/origin/local", "HEAD")
        result = self.run_script("release.sh", "--dry-run", ok=False)
        self.assertIn("not pushed to origin/local", result.stdout)
        self.assertEqual(self.calls(), [])

    def test_bypass_cannot_publish(self):
        result = self.run_script("release.sh", "--allow-unpushed", ok=False)
        self.assertIn("requires --dry-run", result.stdout)
        self.assertEqual(self.calls(), [])

    def test_dry_run_checks_builds_and_prepares_exact_assets(self):
        self.env["SKIP_TESTS"] = "1"  # Release must ignore this inherited shortcut.
        result = self.run_script("release.sh", "--dry-run", "--allow-unpushed")
        self.assertIn("bypassing unpushed-commit check", result.stdout)
        self.assertIn("Dry run passed", result.stdout)
        docker = [call for call in self.calls() if call[0] == "docker"]
        self.assertEqual(len(docker), 2, self.calls())  # Tests, then build.
        self.assertIn("go test", " ".join(docker[0]))
        self.assertEqual(docker[0][docker[0].index("--cpus") + 1], "4")
        self.assertFalse(any(call[1:3] == ["release", "create"] for call in self.calls()))
        self.assertEqual(self.git("ls-remote", str(self.remote), "refs/tags/" + VERSION), "")
        self.assertFalse(self.git("tag", "--list", VERSION))
        self.assertEqual(subprocess.check_output([str(self.repo / "dist" / NAME), "--version"], text=True),
                         "cc-connect " + VERSION + "\n")
        receipt = json.loads((self.repo / "dist" / f"{VERSION}-receipt.json").read_text())
        self.assertEqual(receipt["commit"], self.git("rev-parse", "HEAD"))
        self.assertEqual(receipt["previous"], PREVIOUS)
        notes = (self.repo / "dist" / f"{VERSION}-notes.md").read_text()
        for text in (BASE, PREVIOUS, receipt["commit"], "build: adopt semantic releases"):
            self.assertIn(text, notes)
        subprocess.run(["sha256sum", "-c", NAME + ".sha256"], cwd=self.repo / "dist", check=True,
                       stdout=subprocess.PIPE)

    def test_pushed_commit_passes_without_bypass(self):
        self.push_source()
        result = self.run_script("release.sh", "--dry-run")
        self.assertNotIn("bypassing", result.stdout)
        self.assertIn("Dry run passed", result.stdout)

    def test_existing_local_tag_is_rejected(self):
        self.push_source()
        self.git("tag", VERSION)
        result = self.run_script("release.sh", "--dry-run", ok=False)
        self.assertIn("already exists locally", result.stdout)
        self.assertEqual(self.calls(), [])

    def test_existing_remote_tag_is_rejected(self):
        self.push_source()
        self.git("tag", VERSION)
        self.git("push", "-q", str(self.remote), "refs/tags/" + VERSION)
        self.git("tag", "-d", VERSION)
        result = self.run_script("release.sh", "--dry-run", ok=False)
        self.assertIn("already exists on origin", result.stdout)
        self.assertEqual(self.calls(), [])

    def test_existing_draft_release_is_rejected_without_a_tag(self):
        self.push_source()
        self.env["TEST_RELEASE_EXISTS"] = "1"
        result = self.run_script("release.sh", "--dry-run", ok=False)
        self.assertIn("release " + VERSION + " already exists", result.stdout)
        self.assertFalse(any(call[0] == "docker" for call in self.calls()))

    def test_github_failure_is_not_treated_as_missing_release(self):
        self.push_source()
        self.env["TEST_API_FAILURE"] = "1"
        self.run_script("release.sh", "--dry-run", ok=False)
        self.assertFalse(any(call[0] == "docker" for call in self.calls()))

    def test_failed_tests_prevent_build_and_release(self):
        self.push_source()
        self.env["TEST_FAIL_TESTS"] = "1"
        self.run_script("release.sh", "--dry-run", ok=False)
        self.assertEqual(len([call for call in self.calls() if call[0] == "docker"]), 1)
        self.assertFalse(any(call[1:3] == ["release", "create"] for call in self.calls()))

    def test_build_uses_archived_version_not_worktree_or_environment(self):
        (self.repo / "soundworkbench/VERSION").write_text("v9.9.9\n")
        self.env["VERSION"] = "v8.8.8"
        self.run_script("build.sh", "HEAD")
        self.assertTrue((self.repo / "dist" / NAME).is_file())
        self.assertFalse((self.repo / "dist/cc-connect-v9.9.9-linux-amd64").exists())

    def test_publish_checks_title_and_asset_names_after_create(self):
        # gh is a local stub in this test; nothing is published to GitHub.
        self.push_source()
        self.env["TEST_BAD_ASSETS"] = "1"
        result = self.run_script("release.sh", ok=False)
        self.assertIn("do not match VERSION", result.stdout)
        create = [call for call in self.calls() if call[1:3] == ["release", "create"]]
        self.assertEqual(len(create), 1)
        self.assertEqual(create[0][3], VERSION)
        self.assertEqual(create[0][create[0].index("--target") + 1], self.git("rev-parse", "HEAD"))
        self.assertEqual([Path(asset).name for asset in create[0][-2:]], [NAME, NAME + ".sha256"])


if __name__ == "__main__":
    unittest.main()
