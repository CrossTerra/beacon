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


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


def pids(state):
    return {int(p.name) for p in (state / "pids").iterdir()}


def listening(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=1):
            return True
    except ConnectionRefusedError:
        return False


class Session:
    """A tether session whose lease is held by a separate stand-in for the app."""

    def __init__(self, fixture, state):
        lease, self.holder = socket.socketpair()
        self.helper = subprocess.Popen([fixture, "session", str(state)], stdin=lease)
        lease.close()
        self.app = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(120)"], stdin=self.holder, pass_fds=())
        self.holder.settimeout(10)

    def receive(self):
        return self.holder.recv(1)

    def quit(self):
        self.holder.shutdown(socket.SHUT_WR)
        assert self.receive() == b"S"
        assert self.helper.wait(timeout=10) == 0
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


def started(fixture, state):
    (state / "pids").mkdir(exist_ok=True)
    session = Session(fixture, state)
    assert session.receive() == b"A"
    wait_for(lambda: (state / "port").exists(), "the detached grandchild")
    port = int((state / "port").read_text())
    assert listening(port)
    return session, port


def assert_clean(state, port):
    assert not any(alive(pid) for pid in pids(state)), "a supervised process survived"
    assert not listening(port), "the detached listener survived"
    assert not (state / "tether.session").exists()


def quit_after_restart(fixture, state):
    session, port = started(fixture, state)
    wait_for(lambda: (state / "watch-restarted").exists(), "the watch restart")
    session.quit()
    assert_clean(state, port)


def quit_with_restart_pending(fixture, state):
    session, port = started(fixture, state)
    wait_for(lambda: (state / "watch-ran").exists(), "the failing watch")
    session.quit()
    time.sleep(1.5)  # Past the 1 second backoff.
    assert not (state / "watch-restarted").exists(), "a watch restarted during shutdown"
    assert_clean(state, port)


def app_crash(fixture, state):
    session, port = started(fixture, state)
    session.crash_app()
    assert_clean(state, port)


def helper_crash_then_recovery(fixture, state):
    session, port = started(fixture, state)
    session.helper.kill()  # Nothing cleans up now; the next session must.
    session.helper.wait()
    session.close()
    old = {pid for pid in pids(state) if alive(pid)}
    assert old and listening(port), "the fixture left nothing to recover"
    (state / "port").unlink()
    session, new_port = started(fixture, state)
    assert not any(alive(pid) for pid in old), "the next session did not stop the old processes"
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
                 second_session_refused):
        state = Path(tempfile.mkdtemp(prefix="tether-"))
        try:
            case(fixture, state)
        finally:
            shutil.rmtree(state, ignore_errors=True)
        print(f"ok {case.__name__}")
    scratch = Path(tempfile.mkdtemp(prefix="collector-"))
    try:
        collector_lifetime(collector, scratch)
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    print("ok collector_lifetime")
