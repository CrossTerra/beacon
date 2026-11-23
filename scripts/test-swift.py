#!/usr/bin/env python3
"""Run Swift Testing serially; a blocked test cannot cancel its own wait."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

TEST_SECONDS = 120
RUN_SECONDS = 480


def watch(child, events):
    run_deadline = time.monotonic() + RUN_SECONDS
    functions = set()
    active = {}
    finished = False
    last_test = None
    while True:
        status = child.poll()

        # Swift Testing flushes this versioned JSONL stream even when SwiftPM's console output is buffered.
        # The writer may be between a record and its newline; leave incomplete records for the next read.
        while True:
            pos = events.tell()
            line = events.readline()
            if not line.endswith(b"\n"):
                events.seek(pos)
                break
            record = json.loads(line)
            payload = record["payload"]
            if record["kind"] == "test" and payload["kind"] == "function":
                functions.add(payload["id"])
            elif record["kind"] == "event":
                kind, test = payload["kind"], payload.get("testID")
                if kind == "testStarted" and test in functions:
                    active[test] = time.monotonic() + TEST_SECONDS
                    last_test = test
                elif kind == "testEnded":
                    active.pop(test, None)
                elif kind == "runEnded":
                    finished = True

        if status is not None:
            if status == 0 and (not last_test or not finished or active):
                raise RuntimeError("missing or incomplete Swift Testing event stream")
            return status if status >= 0 else 128 - status

        now = time.monotonic()
        for test, deadline in active.items():
            if now >= deadline:
                raise TimeoutError(f"{test} exceeded its {TEST_SECONDS}s test deadline")
        if now >= run_deadline:
            detail = next(iter(active), last_test or "no tests started")
            raise TimeoutError(f"swift test exceeded {RUN_SECONDS}s ({detail})")
        time.sleep(0.1)


def stop_session(child):
    # SIGINT lets SwiftPM cancel its test runner. On timeout, kill any survivors in this invocation's private
    # session, not just SwiftPM's process group: the runner and fixtures can make their own groups.
    child.send_signal(signal.SIGINT)
    try:
        child.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass
    try:
        pids = subprocess.check_output(["/bin/ps", "-axo", "pid="], timeout=3).split()
        for entry in pids:
            pid = int(entry)
            try:
                if os.getsid(pid) == child.pid:
                    os.kill(pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                continue
    finally:
        if child.poll() is None:
            child.kill()
            child.wait(timeout=3)


def main(args):
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="beacon-swift-test-") as directory:
        path = Path(directory) / "events.jsonl"
        path.touch()
        # Keep the console live as well: without this SwiftPM can stop printing at a 16 KiB boundary, several
        # tests before the actual stall. The watchdog uses the event stream, not the human-readable output.
        env = dict(os.environ, NSUnbufferedIO="YES")
        child = subprocess.Popen(
            ["swift", "test", "--package-path", "mac", *args, "--no-parallel",
             "--event-stream-output-path", str(path), "--event-stream-version", "0"],
            cwd=root, env=env, start_new_session=True,
        )
        result = None
        try:
            with path.open("rb") as events:
                result = watch(child, events)
            return result
        except TimeoutError as error:
            print(f"Swift test watchdog: {error}", file=sys.stderr, flush=True)
            return 124
        except (ValueError, KeyError, RuntimeError) as error:
            print(f"Swift test watchdog: {error}", file=sys.stderr, flush=True)
            return 1
        finally:
            if result != 0:
                stop_session(child)


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, terminate)
    sys.exit(main(sys.argv[1:]))
