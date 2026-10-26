#!/usr/bin/env python3
"""Interactively replace one node key. No credentials in argv, stdout or backups."""
import getpass
import json
import os
import re
import tempfile
import warnings
from pathlib import Path


def replace_key(path, index, secret):
    path = Path(path)
    if path.is_symlink() or not path.is_file():
        raise ValueError("Config must be a regular file")
    if not re.fullmatch(r"nk_[a-f0-9]{32}\.[a-f0-9]{64}", secret):
        raise ValueError("Invalid independent credential format")
    with path.open(encoding="utf-8") as stream:
        data = json.load(stream)
    nodes = data.get("Nodes", [])
    if index < 0 or index >= len(nodes):
        raise ValueError("Invalid node selection")
    nodes[index]["ApiKey"] = secret
    fd, tmp = tempfile.mkstemp(prefix=".credential-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(data, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def main():
    if os.geteuid() != 0:
        raise ValueError("Run as root")
    path = Path("/etc/v2box/config.json")
    with path.open(encoding="utf-8") as stream:
        nodes = json.load(stream).get("Nodes", [])
    for index, node in enumerate(nodes):
        print(f"{index + 1}: {node.get('ApiHost', '')} / {node.get('NodeType', 'v2node')} #{node.get('NodeID')}")
    index = int(input("Select node number: ")) - 1
    # Require a real terminal: never fall back to echoing a secret through stdin.
    with open("/dev/tty", "r+") as terminal:
        with warnings.catch_warnings():
            warnings.simplefilter("error", getpass.GetPassWarning)
            secret = getpass.getpass("New independent key: ", stream=terminal)
    replace_key(path, index, secret)
    print("Key saved. With file watching enabled, only this node reloads. Verify its new fingerprint in the panel; restart is only needed when file watching is disabled.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, EOFError, KeyboardInterrupt, getpass.GetPassWarning):
        raise SystemExit("Credential update failed; check selection, key format and config permissions.")
