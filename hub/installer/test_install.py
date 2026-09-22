#!/usr/bin/env python3
"""Offline lifecycle regression tests for install.sh."""
import os
import pathlib
import pty
import select
import fcntl
import termios
import subprocess
import tempfile
import textwrap
import time
import unittest

HERE = pathlib.Path(__file__).resolve().parent
INSTALLER = HERE / "install.sh"
KEY = "ask_" + "a" * 48
NEW_KEY = "ask_" + "b" * 48


class Harness:
    def __init__(self, version="1.2.0", arch="arm64"):
        self.tmp = tempfile.TemporaryDirectory(prefix="assistant-install-test-")
        self.root = pathlib.Path(self.tmp.name) / "root"
        self.tools = pathlib.Path(self.tmp.name) / "tools"
        self.root.mkdir(); self.tools.mkdir(); (self.root / "run/systemd/system").mkdir(parents=True)
        self.version, self.arch = version, arch
        self.agent = self.tools / "payload-agent"
        self.agent.write_text("#!/usr/bin/env bash\nif [[ ${1:-} == --version ]]; then printf 'assistant-agent %s (linux/%s)\\n' \"$FAKE_VERSION\" \"$FAKE_ARCH\"; fi\n")
        self.agent.chmod(0o755)
        self.calls = self.root / "status.calls"; self.state = self.root / "systemctl.state"; self.state.write_text("inactive\n")
        self._write_tools()
        source = INSTALLER.read_text().replace("INSTALL_ROOT=''", f"INSTALL_ROOT='{self.root}'").replace("WAIT_SECONDS=120", "WAIT_SECONDS=2").replace("STABILITY_SECONDS=3", "STABILITY_SECONDS=0").replace('[[ ${EUID:-$(id -u)} -eq 0 ]]', '[[ 0 -eq 0 ]]')
        # macOS ships bash 3.2; keep the disposable copy compatible with its
        # nounset behavior while production remains tested on Linux bash.
        source = source.replace('for previous in "${seen[@]}";', 'for previous in "${seen[@]-}";')
        self.script = pathlib.Path(self.tmp.name) / "install.sh"; self.script.write_text(source); self.script.chmod(0o755)

    def _write(self, name, body):
        p = self.tools / name; p.write_text("#!/usr/bin/env bash\n" + textwrap.dedent(body)); p.chmod(0o755)

    def _write_tools(self):
        self._write("uname", """
            [[ ${1:-} == -m ]] && printf '%s\\n' "$FAKE_ARCH" || printf 'Linux\\n'
        """)
        self._write("flock", "exit 0\n")
        self._write("timeout", "shift; exec \"$@\"\n")
        self._write("systemctl", """
            state_file=${SYSTEMCTL_STATE:?}; state=$(cat "$state_file" 2>/dev/null || printf inactive); unit=${SYSTEMCTL_UNIT:?}
            case ${1:-} in
              show) prop=""; for arg in "$@"; do case $arg in --property=*) prop=${arg#--property=};; esac; done; case $prop in
                FragmentPath) [[ -f "$unit" ]] && printf '%s\\n' "$unit" || printf '\\n' ;;
                DropInPaths) printf '\\n' ;;
                ActiveState) printf '%s\\n' "$state" ;;
                MainPID) [[ $state == active ]] && printf '4242\\n' || printf '0\\n' ;;
                NRestarts) printf '%s\\n' "${SYSTEMCTL_RESTARTS:-0}" ;;
              esac ;;
              is-active) [[ $state == active ]] ;;
              is-enabled) [[ -f "$unit.enabled" ]] && printf enabled || printf disabled ;;
              stop) printf inactive >"$state_file" ;;
              start) n=0; [[ -f "$state_file.starts" ]] && n=$(cat "$state_file.starts"); n=$((n + 1)); printf '%s' "$n" >"$state_file.starts"; [[ -n ${SYSTEMCTL_QUEUE:-} ]] && printf 'created-by-agent\\n' >"$SYSTEMCTL_QUEUE"; if [[ ${SYSTEMCTL_FAIL_START:-0} == 1 && $n == 1 ]]; then exit 1; fi; printf active >"$state_file" ;;
              enable) : >"$unit.enabled" ;;
              disable) rm -f "$unit.enabled" ;;
              daemon-reload|reset-failed) : ;;
              *) : ;;
            esac
        """)
        self._write("curl", """
            set -eu; out=; url=; cfg=
            while (($#)); do case $1 in -o) out=$2; shift 2;; --config) cfg=$2; shift 2;; *) url=$1; shift;; esac; done
            token=''; [[ -n $cfg && -f $cfg ]] && token=$(sed -n 's/^header = "Authorization: Bearer //p' "$cfg" | sed 's/"$//')
            if [[ $url == */agent/status && (${CURL_FAIL_AUTH:-0} == 1 || (${CURL_REJECT_OLD:-0} == 1 && $token != "$NEW_KEY")) ]]; then exit 22; fi
            if [[ $url == */agent/status ]]; then
              n=0; [[ -f "$STATUS_CALLS" ]] && n=$(cat "$STATUS_CALLS"); n=$((n + 1)); printf '%s' "$n" >"$STATUS_CALLS"; source=${STATUS_SOURCE:-src_test}; [[ $token == "$NEW_KEY" ]] && source=${STATUS_SOURCE_NEW:-src_test}; seq=0; [[ $n -ge ${STATUS_SUCCESS_AT:-3} ]] && seq=1; if [[ ${CURL_FAIL_AFTER_RESTART:-0} == 1 && $n -ge 3 ]]; then exit 28; fi; printf '{"sourceId":"%s","lastMetricsSeq":%s,"agentVersion":"%s"}' "$source" "$seq" "$FAKE_VERSION"
            elif [[ $url == */stable || $url == */agent-manifest.json ]]; then
              h=$(sha256sum "$FAKE_AGENT" | awk '{print $1}'); s=$(wc -c <"$FAKE_AGENT" | tr -d '[:space:]'); [[ ${CURL_BAD_HASH:-0} == 1 ]] && h=0000000000000000000000000000000000000000000000000000000000000000; [[ ${CURL_BAD_SIZE:-0} == 1 ]] && s=$((s + 1)); printf '{"schemaVersion":1,"version":"%s","assets":[{"os":"linux","arch":"amd64","name":"assistant-agent_%s_linux_amd64","size":%s,"sha256":"%s"},{"os":"linux","arch":"arm64","name":"assistant-agent_%s_linux_arm64","size":%s,"sha256":"%s"}]}' "$FAKE_VERSION" "$FAKE_VERSION" "$s" "$h" "$FAKE_VERSION" "$s" "$h" >"$out"
            elif [[ $url == */assistant-agent_* ]]; then cp "$FAKE_AGENT" "$out"; fi
        """)

    def env(self, **extra):
        e = os.environ.copy(); e.update({"PATH": str(self.tools) + os.pathsep + e["PATH"], "FAKE_AGENT": str(self.agent), "FAKE_VERSION": self.version, "FAKE_ARCH": self.arch, "NEW_KEY": NEW_KEY, "STATUS_CALLS": str(self.calls), "SYSTEMCTL_STATE": str(self.state), "SYSTEMCTL_UNIT": str(self.root / "etc/systemd/system/assistant-agent.service"), "STATUS_SOURCE": "src_test", "STATUS_SOURCE_NEW": "src_test", "STATUS_SUCCESS_AT": "3"}); e.update({k: str(v) for k, v in extra.items()}); return e

    def config(self, key=KEY, source="src_test", hub="https://hub.example"):
        p = self.root / "etc/assistant-agent"; p.mkdir(parents=True, exist_ok=True); (p / "agent.env").write_text(f"ASSIST_HUB_URL={hub}\nASSIST_SOURCE_KEY={key}\nASSIST_QUEUE_DIR=/var/lib/assistant-agent\nASSIST_SOURCE_ID={source}\n"); (p / "agent.env").chmod(0o600)

    def queue(self, text="queue-marker"):
        p = self.root / "var/lib/assistant-agent"; p.mkdir(parents=True, exist_ok=True); (p / "agent-queue.db").write_text(text)

    def run(self, *args, **env):
        self.calls.unlink(missing_ok=True); return subprocess.run(["bash", str(self.script), *args], text=True, capture_output=True, errors="replace", env=self.env(**env), timeout=10)

    def run_prompt(self, *args, key=KEY, **extra):
        self.calls.unlink(missing_ok=True)
        env = self.env(**extra); master, slave = pty.openpty(); pid = os.fork()
        if pid == 0:
            os.setsid(); fcntl.ioctl(slave, termios.TIOCSCTTY, 0); os.dup2(slave, 0); os.dup2(slave, 1); os.dup2(slave, 2); os.close(master); os.close(slave); os.execve("/bin/bash", ["bash", str(self.script), *args], env)
        os.close(slave); output = bytearray(); sent = False; deadline = time.time() + 10
        while time.time() < deadline:
            ready, _, _ = select.select([master], [], [], .2)
            if ready:
                try: output.extend(os.read(master, 4096))
                except OSError: pass
                if not sent and ("密钥" in output.decode(errors="replace") or b"key" in output.lower()): os.write(master, (key + "\n").encode()); sent = True
            waited, status = os.waitpid(pid, os.WNOHANG)
            if waited:
                text = output.decode(errors="replace")
                return subprocess.CompletedProcess([], os.waitstatus_to_exitcode(status), text, text)
        os.kill(pid, 9); os.waitpid(pid, 0); raise AssertionError("installer pty timeout: " + output.decode(errors="replace"))

    def close(self): self.tmp.cleanup()


class InstallerTests(unittest.TestCase):
    def setUp(self): self.h = Harness()
    def tearDown(self): self.h.close()

    def test_fresh_prompt_and_repeat_preserve_queue(self):
        r = self.h.run_prompt("--hub", "https://hub.example", "--version", "1.2.0"); self.assertEqual(r.returncode, 0, r.stdout); self.assertIn("接入成功", r.stdout)
        env = self.h.root / "etc/assistant-agent/agent.env"; self.assertEqual(env.stat().st_mode & 0o777, 0o600); self.h.queue(); r = self.h.run("--hub", "https://hub.example", "--version", "1.2.0"); self.assertEqual(r.returncode, 0, r.stderr); self.assertEqual((self.h.root / "var/lib/assistant-agent/agent-queue.db").read_text(), "queue-marker")

    def test_auth_failure_before_mutation_and_invalid_inputs(self):
        self.h.config(); self.h.queue("before"); before = (self.h.root / "etc/assistant-agent/agent.env").read_bytes(); r = self.h.run("--hub", "https://hub.example", CURL_FAIL_AUTH=1); self.assertNotEqual(r.returncode, 0); self.assertEqual((self.h.root / "etc/assistant-agent/agent.env").read_bytes(), before)
        for args in (("--hub", "http://hub.example"), ("--hub", "https://hub.example/?x=1"), ("--hub", "https://hub.example", "--version", "1.2")): self.assertNotEqual(self.h.run(*args).returncode, 0)

    def test_rotation_same_source_succeeds_other_source_rejected(self):
        self.h.config(); self.h.queue(); r = self.h.run_prompt("--hub", "https://hub.example", "--reconfigure", key=NEW_KEY, STATUS_SOURCE_NEW="src_test", CURL_REJECT_OLD=1); self.assertEqual(r.returncode, 0, r.stderr)
        self.h.config(); self.h.queue(); r = self.h.run_prompt("--hub", "https://hub.example", "--reconfigure", key=NEW_KEY, STATUS_SOURCE_NEW="src_other"); self.assertNotEqual(r.returncode, 0); self.assertIn("不同来源", r.stderr)

    def test_reject_unknown_config_queue_dir_and_downgrade(self):
        self.h.config(); self.h.queue("keep"); p = self.h.root / "etc/assistant-agent/agent.env"; p.write_text(p.read_text() + "UNKNOWN=1\n"); self.assertNotEqual(self.h.run("--hub", "https://hub.example").returncode, 0); self.assertEqual((self.h.root / "var/lib/assistant-agent/agent-queue.db").read_text(), "keep")
        self.h.config(); p.write_text(p.read_text().replace("ASSIST_QUEUE_DIR=/var/lib/assistant-agent", "ASSIST_QUEUE_DIR=/tmp/other")); self.assertNotEqual(self.h.run("--hub", "https://hub.example").returncode, 0)
        self.h.config(); self.h.queue("keep"); b = self.h.root / "usr/local/bin/assistant-agent"; b.parent.mkdir(parents=True); b.write_text("#!/usr/bin/env bash\nprintf 'assistant-agent 9.9.9 (linux/arm64)\\n'\n"); b.chmod(0o755); r = self.h.run_prompt("--hub", "https://hub.example", "--version", "1.2.0", "--reconfigure"); self.assertNotEqual(r.returncode, 0); self.assertIn("拒绝降级", r.stderr); self.assertEqual((self.h.root / "var/lib/assistant-agent/agent-queue.db").read_text(), "keep")

    def test_legacy_unit_migrates(self):
        p = self.h.root / "etc/assistant-agent"; p.mkdir(parents=True); u = self.h.root / "etc/systemd/system"; u.mkdir(parents=True); (u / "assistant-agent.service").write_text(textwrap.dedent(f"""\
            [Unit]
            Description=Assistant Agent
            After=network-online.target

            [Service]
            Environment=ASSIST_HUB_URL=https://hub.example
            Environment=ASSIST_SOURCE_KEY={KEY}
            ExecStart=/usr/local/bin/assistant-agent
            Restart=always
            RestartSec=5

            [Install]
            WantedBy=multi-user.target
        """)); r = self.h.run("--hub", "https://hub.example", STATUS_SUCCESS_AT=4); self.assertEqual(r.returncode, 0, r.stderr); self.assertIn("ASSIST_SOURCE_ID=src_test", (p / "agent.env").read_text())

    def test_start_failure_rolls_back_and_network_timeout_keeps_new(self):
        self.h.config(); self.h.queue("keep"); oldbin = self.h.root / "usr/local/bin/assistant-agent"; oldbin.parent.mkdir(parents=True); oldbin.write_text("#!/usr/bin/env bash\nprintf 'assistant-agent 1.1.0 (linux/arm64)\\n'\n"); oldbin.chmod(0o755); r = self.h.run("--hub", "https://hub.example", SYSTEMCTL_FAIL_START=1); self.assertNotEqual(r.returncode, 0); self.assertIn("1.1.0", oldbin.read_text()); self.assertEqual((self.h.root / "var/lib/assistant-agent/agent-queue.db").read_text(), "keep")
        self.h.config(); self.h.queue("keep"); r = self.h.run("--hub", "https://hub.example", CURL_FAIL_AFTER_RESTART=1); self.assertEqual(r.returncode, 3, r.stderr); self.assertIn("待确认上报", r.stderr); self.assertEqual((self.h.root / "var/lib/assistant-agent/agent-queue.db").read_text(), "keep")

    def test_fresh_failure_keeps_binding_for_retry_and_rejects_other_source(self):
        queue = self.h.root / "var/lib/assistant-agent/agent-queue.db"
        r = self.h.run_prompt("--hub", "https://hub.example", SYSTEMCTL_FAIL_START=1, SYSTEMCTL_QUEUE=queue)
        self.assertNotEqual(r.returncode, 0)
        self.assertTrue(queue.exists())
        self.assertTrue((self.h.root / "etc/assistant-agent/source.json").exists())
        self.assertFalse((self.h.root / "etc/assistant-agent/agent.env").exists())
        r = self.h.run_prompt("--hub", "https://hub.example", key=KEY)
        self.assertEqual(r.returncode, 0, r.stderr)

        self.h.close(); self.h = Harness()
        queue = self.h.root / "var/lib/assistant-agent/agent-queue.db"
        r = self.h.run_prompt("--hub", "https://hub.example", SYSTEMCTL_FAIL_START=1, SYSTEMCTL_QUEUE=queue)
        self.assertNotEqual(r.returncode, 0)
        r = self.h.run_prompt("--hub", "https://hub.example", key=NEW_KEY, STATUS_SOURCE_NEW="src_other")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("不同来源", r.stderr)

    def test_manifest_integrity_custom_unit_dropin_and_async_restart(self):
        self.h.config(); self.h.queue("keep")
        for option, message in (("CURL_BAD_HASH", "SHA256"), ("CURL_BAD_SIZE", "大小")):
            r = self.h.run("--hub", "https://hub.example", **{option: 1})
            self.assertNotEqual(r.returncode, 0); self.assertIn(message, r.stderr)
        unit = self.h.root / "etc/systemd/system/assistant-agent.service"; unit.parent.mkdir(parents=True, exist_ok=True); unit.write_text("[Service]\nExecStart=/custom/agent\n")
        r = self.h.run("--hub", "https://hub.example"); self.assertNotEqual(r.returncode, 0); self.assertIn("自定义内容", r.stderr)
        unit.unlink(); (pathlib.Path(str(unit) + ".d")).mkdir()
        r = self.h.run("--hub", "https://hub.example"); self.assertNotEqual(r.returncode, 0); self.assertIn("drop-in", r.stderr)
        (pathlib.Path(str(unit) + ".d")).rmdir(); unit.unlink(missing_ok=True)
        r = self.h.run("--hub", "https://hub.example", SYSTEMCTL_RESTARTS=1); self.assertNotEqual(r.returncode, 0); self.assertIn("恢复", r.stderr)


if __name__ == "__main__": unittest.main(verbosity=2)
