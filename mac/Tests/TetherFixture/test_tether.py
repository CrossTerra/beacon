"""Tether fixtures. No installation, sudo, real sources or credentials; connections are loopback or socketpairs."""

import errno
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


def pinned_identity(path):
    """The launch pin Beacon writes: unsigned decimals of the device and inode bits."""
    st = os.stat(path)
    return f"{st.st_dev & 0xFFFFFFFF}:{st.st_ino}"


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
        self.fixture = fixture
        args = [fixture, "session", str(state)] + ([mode] if mode else [])
        self.helper = subprocess.Popen(args, stdin=lease)
        lease.close()
        self.app = subprocess.Popen([sys.executable, "-B", "-c", "import time; time.sleep(120)"], stdin=self.holder)
        self.holder.settimeout(30)
        self.seen = b""

    def receive(self):
        byte = self.holder.recv(1)
        self.seen += byte
        return byte

    def event(self):
        """The next state byte, or a whole runner line ("#started …") as text."""
        byte = self.receive()
        if byte != b"#":
            return byte
        line = b""
        while (byte := self.receive()) not in (b"\n", b""):
            line += byte
        return "#" + line.decode()

    def until(self, prefix):
        """Reads until a runner line that starts with `prefix`; returns its words."""
        while True:
            event = self.event()
            if event == b"":
                raise AssertionError(f"the lease closed before {prefix}")
            if isinstance(event, str) and event.startswith(prefix):
                return event.split()

    def launch(self, state, run, prompt, cwd, extra_env=(), pin=None, identity=None):
        """Writes the run's directory, pinning `cwd`'s identity (or `pin`'s, or `identity`) as the app does, and asks for it."""
        directory = state / "runs" / run
        directory.mkdir(parents=True)
        (directory / "prompt").write_text(prompt)
        argv = [self.fixture, "runner", str(state)]
        env = ["PATH=/usr/bin:/bin", "RUN_VAR=from-the-app", "BEACON_SUPERVISOR=forged", *extra_env]
        try:
            identity = identity if identity is not None else pinned_identity(pin or cwd)
        except FileNotFoundError:
            identity = "1:1"
        fields = [str(cwd), identity, str(len(argv)), *argv, *env]
        (directory / "launch").write_bytes(b"".join(f.encode() + b"\0" for f in fields))
        self.holder.sendall(f"L {run}\n".encode())

    def prepared(self, run):
        _, name, pid = self.until("#prepared")
        assert name == run and int(pid) > 0
        return int(pid)

    def acknowledge(self, run, pid):
        self.holder.sendall(f"A {run}\n".encode())
        reply = self.until("#")
        assert reply == ["#started", run, str(pid)], reply

    def accept_launch(self, run):
        self.acknowledge(run, self.prepared(run))

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


def leaky_watch(fixture, state):
    """A watch that leaves a detached grandchild and exits into a 30 second restart backoff."""
    session, port = started(fixture, state, "leaky")
    wait_for(lambda: (state / "watch-ran").exists(), "the leaking watch")
    leader = {int(p.name.removeprefix("watch-leader-")) for p in state.glob("watch-leader-*")}
    leaked = {int(p.name.removeprefix("leaked-")) for p in state.glob("leaked-*")}
    assert len(leader) == 1 and len(leaked) == 1
    wait_for(lambda: not live(leader), "the watch to exit")
    time.sleep(0.5)  # tether checks every 100 ms: let it reap the leader and enter the backoff.
    return session, port, leaked


def leaked_grandchild_stopped_at_quit(fixture, state):
    session, port, leaked = leaky_watch(fixture, state)
    session.quit()
    assert not live(leaked), "the detached grandchild of a watch in backoff survived Quit"
    assert_clean(state, port)


def leaked_grandchild_stopped_before_restart(fixture, state):
    session, port, leaked = leaky_watch(fixture, state)
    # TERM is ignored, so this takes the 1 second grace period and a KILL, well inside the 30 second backoff.
    wait_for(lambda: not live(leaked), "the watch's leftover to be stopped", timeout=4)
    session.quit()
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


def runner_runs_and_reports(fixture, state):
    """A run starts in its checkout with its prompt on stdin and the app's environment plus tether's marker;
    its exit code and its end are reported, and its output and errors are kept beside it."""
    session, port = started(fixture, state)
    checkout = state / "checkout"
    checkout.mkdir()
    session.launch(state, "run_a1", "exit 3", checkout)
    pid = session.prepared("run_a1")
    run_dir = state / "runs" / "run_a1"
    # Acknowledging a different run does not release this one. The second command gives us a reply to wait on.
    session.holder.sendall(b"A run_wrong\nL run_missing\n")
    assert session.until("#failed")[1] == "run_missing"
    assert b"#started run_a1 " not in session.seen
    assert subprocess.check_output(["ps", "-o", "stat=", "-p", str(pid)], text=True).strip().startswith("T")
    assert pid not in pids(state) and (run_dir / "output").stat().st_size == 0
    assert not (run_dir / "started").exists()
    session.acknowledge("run_a1", pid)
    # Written only after resume: what the app reads back after a crash.
    assert (run_dir / "started").read_text() == f"{pid}\n"
    assert session.until("#exited") == ["#exited", "run_a1", "3"]
    assert session.until("#gone") == ["#gone", "run_a1"]
    output = (state / "runs" / "run_a1" / "output").read_text()
    assert f"cwd={checkout.resolve()}" in output or f"cwd={checkout}" in output, output
    assert "prompt=exit 3" in output and "run_var=from-the-app" in output
    # tether's own marker for this run, not the one the launch file tried to set.
    marker = next(line for line in output.splitlines() if line.startswith("marker="))
    assert marker.endswith(":rrun_a1") and "forged" not in marker, marker
    assert (state / "runs" / "run_a1" / "errors").read_text() == "runner stderr\n"
    for name in ("output", "errors"):
        assert (state / "runs" / "run_a1" / name).stat().st_mode & 0o777 == 0o600
    # The same run cannot start twice: its output exists.
    session.holder.sendall(b"L run_a1\n")
    assert session.until("#failed")[1] == "run_a1"
    session.quit()
    assert_clean(state, port)


def unacknowledged_launch_times_out(fixture, state):
    """No ack: tether kills and reaps the suspended process, never runs it, and reports a launch failure."""
    session, port = started(fixture, state, "short-ack")
    session.launch(state, "run_no_ack", "exit 0", state)
    pid = session.prepared("run_no_ack")
    assert session.until("#failed") == ["#failed", "run_no_ack", str(errno.ETIMEDOUT)]
    try:
        os.kill(pid, 0)  # A zombie would still answer: the failure must follow the reap.
    except ProcessLookupError:
        pass
    else:
        raise AssertionError("the suspended child was not reaped before the failure reply")
    assert pid not in pids(state)
    directory = state / "runs" / "run_no_ack"
    assert (directory / "output").stat().st_size == 0 and not (directory / "started").exists()
    session.holder.sendall(b"A run_no_ack\nL run_missing\n")
    assert session.until("#failed")[1] == "run_missing"
    assert b"#started run_no_ack " not in session.seen
    assert b"#exited run_no_ack " not in session.seen and b"#gone run_no_ack" not in session.seen
    assert listening(port), "an acknowledgement timeout stopped another service"
    session.quit()
    assert_clean(state, port)


def quit_during_launch_handshake(fixture, state):
    """Closing the write half cancels a suspended run, without waiting for its acknowledgement deadline."""
    session, port = started(fixture, state)
    session.launch(state, "run_quit", "exit 0", state)
    pid = session.prepared("run_quit")
    session.holder.shutdown(socket.SHUT_WR)
    assert session.until("#failed") == ["#failed", "run_quit", str(errno.ECANCELED)]
    assert session.until_closed() == b"S"
    assert session.helper.wait(timeout=10) == 0
    assert b"#started run_quit " not in session.seen and pid not in pids(state)
    assert not (state / "runs" / "run_quit" / "started").exists()
    session.close()
    assert_clean(state, port)


def runner_stopped(fixture, state):
    """Stopping a run stops its whole tree, TERM ignored and a detached grandchild included."""
    session, port = started(fixture, state)
    before = pids(state)
    session.launch(state, "run_b2", "hang", state)
    session.accept_launch("run_b2")
    wait_for(lambda: len(list(state.glob("leaked-*"))) == 1, "the run's detached grandchild")
    run = pids(state) - before
    assert len(live(run)) >= 2
    session.holder.sendall(b"K run_b2\n")
    exited = session.until("#exited")
    assert exited[1] == "run_b2" and int(exited[2]) >= 128, exited
    assert session.until("#gone") == ["#gone", "run_b2"]
    assert not live(run), "a process of the stopped run survived"
    assert listening(port), "stopping a run stopped another service"
    session.quit()
    assert_clean(state, port)


def runners_stop_at_quit(fixture, state):
    """Two runs at once, in two checkouts; Quit stops both, and the next session would sweep them."""
    session, port = started(fixture, state)
    before = pids(state)
    prepared = {}
    for run in ("run_c1", "run_c2"):
        (state / run).mkdir()
        session.launch(state, run, "hang", state / run)
        prepared[run] = session.prepared(run)
    for run, pid in prepared.items():
        session.acknowledge(run, pid)
    wait_for(lambda: len(list(state.glob("leaked-*"))) == 2, "both runs' grandchildren")
    runs = pids(state) - before
    session.quit()
    assert not live(runs), "a run survived Quit"
    assert_clean(state, port)


def runner_bad_launches(fixture, state):
    """A launch that cannot start is reported and starts nothing; malformed commands are ignored."""
    session, port = started(fixture, state)
    session.holder.sendall(b"L run_missing\n")
    assert session.until("#failed")[1] == "run_missing"
    session.launch(state, "run_d1", "exit 0", state / "no-such-directory")
    assert session.until("#failed")[1] == "run_d1"
    bad = state / "runs" / "run_d2"
    bad.mkdir(parents=True)
    (bad / "prompt").write_text("exit 0")
    (bad / "launch").write_bytes(b"relative\0" + b"1:1\0" + b"1\0" + b"/bin/true\0")
    session.holder.sendall(b"L run_d2\n" + b"L ../escape\n" + b"X\n" + b"L " + b"a" * 300 + b"\n")
    assert session.until("#failed")[1] == "run_d2"
    # A good run after all that still starts.
    session.launch(state, "run_d3", "exit 0", state)
    session.accept_launch("run_d3")
    assert session.until("#exited") == ["#exited", "run_d3", "0"]
    assert listening(port)
    session.quit()
    assert_clean(state, port)


def signed_device_pin_is_refused(fixture, state):
    """A signed device decimal is not the volume's bits. The unsigned pin of the same directory still starts."""
    session, port = started(fixture, state)
    checkout = state / "neg"
    checkout.mkdir()
    inode = os.stat(checkout).st_ino
    session.launch(state, "run_signed", "exit 0", checkout, identity=f"-1:{inode}")
    failed = session.until("#failed")
    assert failed == ["#failed", "run_signed", str(errno.EINVAL)], failed
    assert not (state / "runs" / "run_signed" / "started").exists()
    session.launch(state, "run_bits", "exit 0", checkout)
    session.accept_launch("run_bits")
    assert session.until("#exited") == ["#exited", "run_bits", "0"]
    session.quit()
    assert_clean(state, port)


def runner_checkout_swapped(fixture, state):
    """A directory swapped in at the checkout's path after the app pinned it is refused: nothing starts."""
    session, port = started(fixture, state)
    checkout, other = state / "checkout", state / "other"
    checkout.mkdir()
    other.mkdir()
    pinned = os.stat(checkout)
    checkout.rename(state / "moved-away")
    other.rename(checkout)
    session.launch(state, "run_e1", "exit 0", checkout, pin=state / "moved-away")
    assert os.stat(checkout).st_ino != pinned.st_ino
    failed = session.until("#failed")
    assert failed[1] == "run_e1" and int(failed[2]) == 70, failed  # ESTALE
    assert not (state / "runs" / "run_e1" / "started").exists()
    assert not (state / "runs" / "run_e1" / "output").exists()
    session.quit()
    assert_clean(state, port)


def run_left_by_a_killed_tether(fixture, state):
    """tether killed while a run works: the next session sweeps the run's processes before it reports ready."""
    session, port = started(fixture, state)
    before = pids(state)
    session.launch(state, "run_k1", "hang", state)
    session.accept_launch("run_k1")
    wait_for(lambda: len(list(state.glob("leaked-*"))) == 1, "the run's detached grandchild")
    run = pids(state) - before
    session.helper.kill()
    session.helper.wait()
    session.close()
    assert live(run), "the fixture left no run to sweep"
    (state / "port").unlink()
    session, new_port = started(fixture, state)
    assert not live(run), "the next session did not stop the run"
    session.quit()
    assert_clean(state, new_port)


def many_runs_in_one_session(fixture, state):
    """Only live runs count against the limit: 300 runs one after another all start, past the 256 at once."""
    session, port = started(fixture, state)
    for n in range(300):
        run = f"run_m{n}"
        session.launch(state, run, "exit 0", state)
        session.accept_launch(run)
        assert session.until("#gone")[1] == run
    session.quit()
    assert_clean(state, port)


def runs_without_a_collector(fixture, state):
    """With the retired LaunchAgent installed there is no collector job; the first run is job 0. Its start, exit
    and end come only as its own lines, never as the collector's state bytes."""
    (state / "pids").mkdir(exist_ok=True)
    session = Session(fixture, state, "legacy")
    assert session.receive() == b"L"
    for run in ("run_l1", "run_l2"):
        session.launch(state, run, "exit 1", state)
        session.accept_launch(run)
        assert session.until("#exited") == ["#exited", run, "1"]
        assert session.until("#gone") == ["#gone", run]
    session.holder.shutdown(socket.SHUT_WR)
    assert session.until_closed() == b"S"
    assert session.helper.wait(timeout=10) == 0
    session.close()
    # Every byte outside a "#…\n" line: only the ready byte and the confirmation, no R, W or X.
    bare, inside = b"", False
    for byte in session.seen:
        if inside:
            inside = byte != ord("\n")
        elif byte == ord("#"):
            inside = True
        else:
            bare += bytes([byte])
    assert bare == b"LS", session.seen


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
    for case in (quit_after_restart, quit_with_restart_pending, leaked_grandchild_stopped_at_quit,
                 leaked_grandchild_stopped_before_restart, app_crash, helper_crash_then_recovery,
                 second_session_refused, collector_restart_reported, stubborn_shutdown_gives_up,
                  stubborn_sweep_gives_up, runner_runs_and_reports, unacknowledged_launch_times_out,
                  quit_during_launch_handshake, runner_stopped, runners_stop_at_quit,
                 runner_bad_launches, signed_device_pin_is_refused, runner_checkout_swapped,
                 run_left_by_a_killed_tether, many_runs_in_one_session,
                 runs_without_a_collector):
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
