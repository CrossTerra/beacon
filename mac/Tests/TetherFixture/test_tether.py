"""Tether fixtures. No installation, sudo, real sources or credentials; connections are loopback or socketpairs."""

import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


def wait_for(check, what, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.05)
    raise AssertionError(f"timed out waiting for {what}")


def live(pids):
    """The pids that exist and are not zombies. A zombie reparented to launchd still answers kill(pid, 0)."""
    if not pids:
        return set()
    result = subprocess.run(["ps", "-o", "pid=,stat=", "-p", ",".join(map(str, pids))],
                            capture_output=True, text=True, check=False)
    found = set()
    for line in result.stdout.splitlines():
        pid, stat = line.split()
        if not stat.startswith("Z"):
            found.add(int(pid))
    return found


def pids(state):
    return {int(p.name) for p in (state / "pids").iterdir()}


def kill_all(state):
    for pid in live(pids(state)):
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def listening(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=1):
            return True
    except ConnectionRefusedError:
        return False


class Session:
    """A tether session whose lease is held by a separate stand-in for the app."""

    def __init__(self, fixture, state, mode=None):
        lease, self.holder = socket.socketpair()
        args = [fixture, "session", str(state)] + ([mode] if mode else [])
        self.helper = subprocess.Popen(args, stdin=lease)
        lease.close()
        self.app = subprocess.Popen([sys.executable, "-B", "-c", "import time; time.sleep(120)"], stdin=self.holder)
        self.holder.settimeout(15)
        self.seen = b""

    def receive(self):
        byte = self.holder.recv(1)
        self.seen += byte
        return byte

    def until_closed(self):
        """Reads state reports until the lease closes; returns the last byte, or b"" if none came."""
        last = b""
        while byte := self.receive():
            last = byte
        return last

    def quit(self, verified=True):
        self.holder.shutdown(socket.SHUT_WR)
        assert self.until_closed() == (b"S" if verified else b"")
        assert self.helper.wait(timeout=10) == (0 if verified else 4)
        self.close()

    def crash_app(self):
        self.holder.close()
        self.app.kill()  # The kernel closes the last endpoint; no stop message is sent.
        self.app.wait(timeout=5)
        assert self.helper.wait(timeout=10) == 0

    def close(self):
        self.holder.close()
        for process in (self.app, self.helper):
            if process.poll() is None:
                process.kill()
                process.wait()


def started(fixture, state, mode=None):
    (state / "pids").mkdir(exist_ok=True)
    session = Session(fixture, state, mode)
    assert session.receive() == b"A"
    assert session.receive() == b"R"  # The collector stand-in started.
    wait_for(lambda: (state / "port").exists(), "the detached grandchild")
    port = int((state / "port").read_text())
    assert listening(port)
    return session, port


def assert_clean(state, port):
    wait_for(lambda: not live(pids(state)), "every supervised process to exit", timeout=3)
    assert not listening(port), "the detached listener survived"
    assert not (state / "tether.session").exists()


def quit_after_restart(fixture, state):
    session, port = started(fixture, state)
    wait_for(lambda: (state / "watch-restarted").exists(), "the watch restart")
    log = state / "collector.log"
    wait_for(lambda: log.exists() and log.read_text() == "Items: 0 send\n", "the collector log")
    assert log.stat().st_mode & 0o777 == 0o600
    session.quit()
    assert_clean(state, port)


def quit_with_restart_pending(fixture, state):
    session, port = started(fixture, state, "slow")  # The watch's restart waits 30 seconds.
    wait_for(lambda: (state / "watch-ran").exists(), "the failing watch")
    session.quit()
    assert_clean(state, port)
    assert not (state / "watch-restarted").exists(), "a watch restarted during shutdown"


def app_crash(fixture, state):
    session, port = started(fixture, state)
    session.crash_app()
    assert_clean(state, port)


def helper_crash_then_recovery(fixture, state):
    session, port = started(fixture, state)
    session.helper.kill()  # Nothing cleans up now; the next session must.
    session.helper.wait()
    session.close()
    old = live(pids(state))
    assert old and listening(port), "the fixture left nothing to recover"
    (state / "port").unlink()
    session, new_port = started(fixture, state)
    assert not live(old), "the next session did not stop the old processes"
    assert not listening(port)
    session.quit()
    assert_clean(state, new_port)


def second_session_refused(fixture, state):
    session, port = started(fixture, state)
    other = Session(fixture, state)
    assert other.receive() == b"B"
    assert other.helper.wait(timeout=5) == 3
    other.close()
    assert listening(port)
    session.quit()
    assert_clean(state, port)


def collector_restart_reported(fixture, state):
    session, _ = started(fixture, state)
    collector = (state / "collector.log")
    wait_for(collector.exists, "the collector stand-in")
    stand_in = next(pid for pid in live(pids(state))
                    if subprocess.run(["ps", "-o", "command=", "-p", str(pid)],
                                      capture_output=True, text=True).stdout.split()[1:2] == ["idle"])
    os.kill(stand_in, signal.SIGKILL)  # A crash: restarted after the backoff.
    assert session.receive() == b"W"
    assert session.receive() == b"R"
    session.quit()


def stubborn_shutdown_gives_up(fixture, state):
    """A process that does not stop cannot hold Quit: tether gives up and reports nothing verified."""
    try:
        session, _ = started(fixture, state, "stubborn")
        begun = time.monotonic()
        session.quit(verified=False)
        assert time.monotonic() - begun < 1 + 5 + 3
        assert (state / "tether.session").exists(), "the next session must sweep again"
    finally:
        kill_all(state)


def stubborn_sweep_gives_up(fixture, state):
    try:
        session, _ = started(fixture, state)
        session.helper.kill()
        session.helper.wait()
        session.close()
        other = Session(fixture, state, "stubborn")
        assert other.until_closed() == b"", "a session started while old processes remained"
        assert other.helper.wait(timeout=10) == 4
        other.close()
    finally:
        kill_all(state)


def collector_lifetime(collector, scratch):
    """The real Go collector: SIGTERM is a clean stop, and EOF on its lifetime pipe ends it."""
    config = scratch / "config.yaml"
    config.write_text(
        "signal: {url: 'http://127.0.0.1:1'}\n"
        "person: {handle: '@fixture'}\n"
        "mode: dry-run\nrules: rules.yaml\nstate: state\nsources: []\n"
    )
    (scratch / "rules.yaml").write_text("rules: []\n")
    for graceful in (True, False):
        with subprocess.Popen(
            [collector, "run", "--dry-run", "--summary", "--config", str(config)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0, cwd=scratch,
            env={"HOME": str(scratch), "BEACON_TETHERED": "1"},
        ) as process:
            try:
                assert process.stdout.readline() == b"Dry run: nothing is sent.\n"
                if graceful:
                    process.send_signal(signal.SIGTERM)
                    assert process.wait(timeout=5) == 0
                else:
                    process.stdin.close()
                    assert process.wait(timeout=5) == 1
            finally:
                if process.poll() is None:
                    process.kill()
    assert not (scratch / "state").exists()


if __name__ == "__main__":
    fixture, collector = sys.argv[1:3]
    for case in (quit_after_restart, quit_with_restart_pending, app_crash, helper_crash_then_recovery,
                 second_session_refused, collector_restart_reported, stubborn_shutdown_gives_up,
                 stubborn_sweep_gives_up):
        state = Path(tempfile.mkdtemp(prefix="tether-"))
        try:
            case(fixture, state)
        finally:
            kill_all(state)
            shutil.rmtree(state, ignore_errors=True)
        print(f"ok {case.__name__}")
    scratch = Path(tempfile.mkdtemp(prefix="collector-"))
    try:
        collector_lifetime(collector, scratch)
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    print("ok collector_lifetime")
