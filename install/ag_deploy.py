#!/usr/bin/env python3
r"""
Minimal AI Gateway deployment helper.

Remote SSH uses Paramiko by default:
    pip install paramiko

GUI:
    python install/ag_deploy.py

CLI examples:
    python install/ag_deploy.py build
    python install/ag_deploy.py install --host root@example.com --key C:\path\id_rsa --site example.com
    python install/ag_deploy.py install --host root@example.com --key C:\path\id_rsa --master-key-stdin
    python install/ag_deploy.py upgrade --host root@example.com --key C:\path\id_rsa
    python install/ag_deploy.py master-key --host root@example.com --key C:\path\id_rsa --ask-master-key
    python install/ag_deploy.py caddy show --host root@example.com --key C:\path\id_rsa
    python install/ag_deploy.py caddy set --host root@example.com --key C:\path\id_rsa --site example.com
    python install/ag_deploy.py uninstall --host root@example.com --key C:\path\id_rsa --keep-files
    python install/ag_deploy.py status --host root@example.com --key C:\path\id_rsa
    python install/ag_deploy.py status --host root@example.com --key C:\path\id_rsa --transport openssh
"""

from __future__ import annotations

import argparse
import base64
import ctypes
import getpass
import gzip
import json
import os
import posixpath
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterable, Optional
from ctypes import wintypes


REPO_ROOT = Path(__file__).resolve().parents[1]
INSTALL_DIR = REPO_ROOT / "install"

DEFAULT_REMOTE_TMP = "/tmp"
DEFAULT_INSTALL_DIR = "/opt/ai-gateway"
DEFAULT_CONFIG_FILE = "/etc/ai-gateway/config.json"
DEFAULT_CONFIG_DIR = "/etc/ai-gateway"
DEFAULT_SERVICE_NAME = "ai-gateway"
DEFAULT_APP_HOST = "127.0.0.1"
DEFAULT_APP_PORT = "18088"
DEFAULT_SITE = ":80"
DEFAULT_TIMEOUT = "60s"
DEFAULT_SSH_TRANSPORT = "paramiko"
SSH_TRANSPORT_CHOICES = ("paramiko", "openssh", "auto")
SSH_KEEPALIVE_INTERVAL_SECONDS = 15
SSH_SERVER_ALIVE_COUNT_MAX = 3
DEFAULT_GUI_HOST = "root@allcode.cc"
DEFAULT_GUI_SITE = "allcode.cc"
DEFAULT_GUI_KEY = r"C:\Users\Administrator\.ssh\ag_ssh"
GUI_CONFIG_FILENAME = "ag_deploy.json"
GUI_PASSWORD_FIELD = "password"
GUI_PROTECTED_PASSWORD_FIELD = "password_protected"
GUI_MASTER_KEY_FIELD = "master_key"
GUI_PROTECTED_MASTER_KEY_FIELD = "master_key_protected"
GUI_PASSWORD_PROTECTION_WINDOWS_DPAPI = "windows-dpapi"
GUI_SECRET_FIELDS = (
    (GUI_PASSWORD_FIELD, GUI_PROTECTED_PASSWORD_FIELD),
    (GUI_MASTER_KEY_FIELD, GUI_PROTECTED_MASTER_KEY_FIELD),
)


LogFn = Callable[[str], None]


class DeployError(RuntimeError):
    pass


CONNECTION_LOST_ERRNOS = {32, 54, 104, 110, 111, 113, 10053, 10054, 10060, 10061}
CONNECTION_LOST_TEXT = (
    "10054",
    "10053",
    "connection reset",
    "connection reset by peer",
    "forcibly closed",
    "远程主机强迫关闭",
    "broken pipe",
    "connection aborted",
    "connection lost",
    "connection timed out",
    "socket exception",
    "socket is closed",
    "socket closed",
    "server connection dropped",
    "ssh session is not connected",
    "ssh session not active",
    "transport is not connected",
    "paramiko ssh session is not connected",
)


def is_remote_connection_lost_error(exc: BaseException) -> bool:
    seen: set[int] = set()
    stack: list[BaseException] = [exc]
    while stack:
        current = stack.pop()
        marker = id(current)
        if marker in seen:
            continue
        seen.add(marker)
        if isinstance(current, (ConnectionError, BrokenPipeError, EOFError, TimeoutError)):
            return True
        if isinstance(current, OSError):
            numbers = [current.errno, getattr(current, "winerror", None)]
            numbers.extend(arg for arg in current.args if isinstance(arg, int))
            if any(number in CONNECTION_LOST_ERRNOS for number in numbers if number is not None):
                return True
        text = str(current).lower()
        if any(marker in text for marker in CONNECTION_LOST_TEXT):
            return True
        cause = getattr(current, "__cause__", None)
        context = getattr(current, "__context__", None)
        if isinstance(cause, BaseException):
            stack.append(cause)
        if isinstance(context, BaseException):
            stack.append(context)
    return False


def user_config_dir() -> Path:
    if sys.platform == "win32":
        base = os.environ.get("APPDATA") or os.environ.get("LOCALAPPDATA")
        if base:
            return Path(base) / "AI Gateway"
        return Path.home() / "AppData" / "Roaming" / "AI Gateway"
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "AI Gateway"
    base = os.environ.get("XDG_CONFIG_HOME")
    if base:
        return Path(base) / "ai-gateway"
    return Path.home() / ".config" / "ai-gateway"


def gui_config_path() -> Path:
    return user_config_dir() / GUI_CONFIG_FILENAME


class DataBlob(ctypes.Structure):
    _fields_ = [
        ("cbData", wintypes.DWORD),
        ("pbData", ctypes.POINTER(ctypes.c_char)),
    ]


def protect_password_windows_dpapi(password: str) -> str:
    raw = password.encode("utf-8")
    crypt32 = ctypes.WinDLL("crypt32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    in_buffer = ctypes.create_string_buffer(raw)
    in_blob = DataBlob(len(raw), ctypes.cast(in_buffer, ctypes.POINTER(ctypes.c_char)))
    out_blob = DataBlob()
    ok = crypt32.CryptProtectData(
        ctypes.byref(in_blob),
        "AI Gateway Deploy",
        None,
        None,
        None,
        0,
        ctypes.byref(out_blob),
    )
    if not ok:
        raise OSError(ctypes.get_last_error(), "CryptProtectData failed")
    try:
        encrypted = ctypes.string_at(out_blob.pbData, out_blob.cbData)
    finally:
        kernel32.LocalFree(out_blob.pbData)
    return base64.b64encode(encrypted).decode("ascii")


def unprotect_password_windows_dpapi(value: str) -> str:
    encrypted = base64.b64decode(value.encode("ascii"))
    crypt32 = ctypes.WinDLL("crypt32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
    in_buffer = ctypes.create_string_buffer(encrypted)
    in_blob = DataBlob(len(encrypted), ctypes.cast(in_buffer, ctypes.POINTER(ctypes.c_char)))
    out_blob = DataBlob()
    ok = crypt32.CryptUnprotectData(
        ctypes.byref(in_blob),
        None,
        None,
        None,
        None,
        0,
        ctypes.byref(out_blob),
    )
    if not ok:
        raise OSError(ctypes.get_last_error(), "CryptUnprotectData failed")
    try:
        raw = ctypes.string_at(out_blob.pbData, out_blob.cbData)
    finally:
        kernel32.LocalFree(out_blob.pbData)
    return raw.decode("utf-8")


def protect_gui_password(password: str) -> dict[str, str] | None:
    if not password:
        return None
    if sys.platform == "win32":
        return {
            "kind": GUI_PASSWORD_PROTECTION_WINDOWS_DPAPI,
            "value": protect_password_windows_dpapi(password),
        }
    return None


def unprotect_gui_password(value: object) -> str:
    if not isinstance(value, dict):
        return ""
    kind = str(value.get("kind") or "")
    payload = str(value.get("value") or "")
    if not payload:
        return ""
    if kind == GUI_PASSWORD_PROTECTION_WINDOWS_DPAPI and sys.platform == "win32":
        try:
            return unprotect_password_windows_dpapi(payload)
        except Exception:
            return ""
    return ""


def load_gui_config() -> dict[str, object]:
    path = gui_config_path()
    try:
        with path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
    except FileNotFoundError:
        return {}
    except (OSError, json.JSONDecodeError):
        return {}
    if not isinstance(data, dict):
        return {}
    for field, protected_field in GUI_SECRET_FIELDS:
        data[field] = unprotect_gui_password(data.get(protected_field))
    return data


def save_gui_config(data: dict[str, object]) -> None:
    path = gui_config_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = dict(data)
    for field, protected_field in GUI_SECRET_FIELDS:
        secret = str(payload.pop(field, "") or "")
        payload.pop(protected_field, None)
        protected_secret = protect_gui_password(secret)
        if protected_secret is not None:
            payload[protected_field] = protected_secret
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2)
        handle.write("\n")


@dataclass
class RemoteConnection:
    host: str
    port: int = 22
    key_path: str = ""
    password: str = ""
    remote_tmp: str = DEFAULT_REMOTE_TMP
    transport: str = DEFAULT_SSH_TRANSPORT

    @property
    def uses_password(self) -> bool:
        return bool(self.password)


@dataclass
class InstallOptions:
    site: str = DEFAULT_SITE
    install_dir: str = DEFAULT_INSTALL_DIR
    config_file: str = DEFAULT_CONFIG_FILE
    master_key: str = ""
    service_name: str = DEFAULT_SERVICE_NAME
    app_host: str = DEFAULT_APP_HOST
    app_port: str = DEFAULT_APP_PORT
    install_caddy: bool = True
    configure_caddy: bool = True


@dataclass
class ServiceOptions:
    install_dir: str = DEFAULT_INSTALL_DIR
    config_dir: str = DEFAULT_CONFIG_DIR
    service_name: str = DEFAULT_SERVICE_NAME
    timeout: str = DEFAULT_TIMEOUT
    keep_files: bool = True


@dataclass
class CaddyOptions:
    site: str = ""
    install_dir: str = DEFAULT_INSTALL_DIR
    config_file: str = DEFAULT_CONFIG_FILE
    service_name: str = DEFAULT_SERVICE_NAME
    app_host: str = ""
    app_port: str = ""
    timeout: str = DEFAULT_TIMEOUT
    public_base_url: str = ""
    reload_caddy: bool = True
    reload_app: bool = True


@dataclass
class MasterKeyOptions:
    install_dir: str = DEFAULT_INSTALL_DIR
    config_file: str = DEFAULT_CONFIG_FILE
    master_key: str = ""
    service_name: str = DEFAULT_SERVICE_NAME
    restart_app: bool = True


def rollback_version_label(version: dict[str, object]) -> str:
    release = str(version.get("release") or "").strip()
    markers: list[str] = []
    if bool(version.get("previous")):
        markers.append("previous")
    marker_text = f" ({', '.join(markers)})" if markers else ""
    backup_path = str(version.get("backup_path") or "").strip()
    backup_name = posixpath.basename(backup_path) if backup_path else "no data backup"
    backup_created_at = str(version.get("backup_created_at") or "").strip()
    backup_size = int(version.get("backup_size") or 0)
    backup_bits = [backup_name]
    if backup_created_at:
        backup_bits.append(backup_created_at)
    if backup_size > 0:
        backup_bits.append(format_bytes(backup_size))
    return f"{release}{marker_text} | " + " | ".join(backup_bits)


def log_line(log: Optional[LogFn], message: str) -> None:
    if log:
        log(message)


def run_process(
    args: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    input_text: str | None = None,
    log: Optional[LogFn] = None,
) -> str:
    log_line(log, "$ " + " ".join(shlex.quote(str(arg)) for arg in args))
    proc = subprocess.Popen(
        args,
        cwd=str(cwd) if cwd else None,
        env=env,
        stdin=subprocess.PIPE if input_text is not None else None,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    output, _ = proc.communicate(input_text)
    output = output or ""
    for line in output.splitlines():
        log_line(log, line)
    if proc.returncode != 0:
        raise DeployError(f"command failed with exit code {proc.returncode}")
    return output


def format_bytes(value: int) -> str:
    size = float(max(0, value))
    for unit in ("B", "KB", "MB", "GB"):
        if size < 1024 or unit == "GB":
            if unit == "B":
                return f"{int(size)} {unit}"
            return f"{size:.1f} {unit}"
        size /= 1024
    return f"{size:.1f} GB"


class UploadProgressLogger:
    def __init__(self, label: str, total: int, log: Optional[LogFn], step_percent: int = 5) -> None:
        self.label = label
        self.total = max(0, total)
        self.log = log
        self.step_percent = max(1, step_percent)
        self.next_percent = 0
        self.last_logged = -1

    def update(self, transferred: int, total: int | None = None, *, force: bool = False) -> None:
        if total is not None and total > 0:
            self.total = total
        transferred = max(0, transferred)
        if self.total <= 0:
            if force and transferred != self.last_logged:
                self.last_logged = transferred
                log_line(self.log, f"{self.label}: {format_bytes(transferred)}")
            return
        percent = min(100, int((transferred * 100) / self.total))
        if not force and percent < self.next_percent:
            return
        self.last_logged = percent
        while self.next_percent <= percent:
            self.next_percent += self.step_percent
        if percent >= 100:
            self.next_percent = 101
        log_line(self.log, f"{self.label}: {percent}% ({format_bytes(transferred)} / {format_bytes(self.total)})")


def first_existing_path(paths: Iterable[str]) -> str:
    for value in paths:
        if value and Path(value).exists():
            return value
    return ""


def find_tool(name: str, common_paths: Iterable[str] = ()) -> str:
    found = shutil.which(name)
    if found:
        return found
    found = first_existing_path(common_paths)
    if found:
        return found
    raise DeployError(f"{name} not found in PATH")


def default_binary_path() -> Path:
    return INSTALL_DIR / "ag-linux-amd64"


def temp_binary_path() -> Path:
    stamp = time.strftime("%Y%m%d-%H%M%S")
    return Path(tempfile.gettempdir()) / f"ag-{stamp}-linux-amd64"


def build_linux_amd64(output: Path | None = None, skip_tests: bool = False, log: Optional[LogFn] = None) -> Path:
    output = (output or default_binary_path()).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    go = find_tool(
        "go",
        [
            r"C:\Program Files\Go\bin\go.exe",
            r"C:\Program Files (x86)\Go\bin\go.exe",
        ],
    )

    if not skip_tests:
        test_env = os.environ.copy()
        test_env.pop("GOOS", None)
        test_env.pop("GOARCH", None)
        test_env.pop("CGO_ENABLED", None)
        run_process([go, "test", "./..."], cwd=REPO_ROOT, env=test_env, log=log)

    env = os.environ.copy()
    env["GOOS"] = "linux"
    env["GOARCH"] = "amd64"
    env["CGO_ENABLED"] = "0"
    run_process(
        [go, "build", "-trimpath", "-ldflags", "-s -w", "-o", str(output), "./cmd/server"],
        cwd=REPO_ROOT,
        env=env,
        log=log,
    )

    if not output.exists():
        raise DeployError(f"build output was not created: {output}")
    log_line(log, f"Built linux/amd64 binary: {output}")
    return output


def parse_remote_host(value: str, fallback_port: int) -> tuple[str, str, int]:
    raw = value.strip()
    if not raw:
        raise DeployError("remote host is required")

    user = "root"
    target = raw
    if "@" in target:
        user, target = target.rsplit("@", 1)
        user = user.strip() or "root"

    port = fallback_port
    if target.startswith("[") and "]" in target:
        end = target.index("]")
        host = target[1:end]
        rest = target[end + 1 :]
        if rest.startswith(":") and rest[1:]:
            port = int(rest[1:])
    elif target.count(":") == 1:
        host, port_text = target.rsplit(":", 1)
        if port_text.isdigit():
            port = int(port_text)
        else:
            host = target
    else:
        host = target

    if not user or not host:
        raise DeployError(f"invalid SSH host: {value}")
    return user, host, port


def ssh_target(conn: RemoteConnection) -> str:
    user, host, _ = parse_remote_host(conn.host, conn.port)
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    return f"{user}@{host}"


def ssh_args(conn: RemoteConnection) -> list[str]:
    _, _, port = parse_remote_host(conn.host, conn.port)
    ssh = find_tool(
        "ssh",
        [
            r"C:\Program Files\Git\usr\bin\ssh.exe",
            r"C:\Windows\System32\OpenSSH\ssh.exe",
        ],
    )
    args = [
        ssh,
        "-p",
        str(port),
        "-o",
        "StrictHostKeyChecking=no",
        "-o",
        f"ServerAliveInterval={SSH_KEEPALIVE_INTERVAL_SECONDS}",
        "-o",
        f"ServerAliveCountMax={SSH_SERVER_ALIVE_COUNT_MAX}",
        "-o",
        "TCPKeepAlive=yes",
    ]
    if conn.key_path.strip():
        args.extend(["-i", conn.key_path.strip()])
    return args


def scp_args(conn: RemoteConnection) -> list[str]:
    _, _, port = parse_remote_host(conn.host, conn.port)
    scp = find_tool(
        "scp",
        [
            r"C:\Program Files\Git\usr\bin\scp.exe",
            r"C:\Windows\System32\OpenSSH\scp.exe",
        ],
    )
    args = [
        scp,
        "-P",
        str(port),
        "-o",
        "StrictHostKeyChecking=no",
        "-o",
        f"ServerAliveInterval={SSH_KEEPALIVE_INTERVAL_SECONDS}",
        "-o",
        f"ServerAliveCountMax={SSH_SERVER_ALIVE_COUNT_MAX}",
        "-o",
        "TCPKeepAlive=yes",
    ]
    if conn.key_path.strip():
        args.extend(["-i", conn.key_path.strip()])
    return args


def normalize_ssh_transport(value: str) -> str:
    transport = (value or DEFAULT_SSH_TRANSPORT).strip().lower()
    if transport not in SSH_TRANSPORT_CHOICES:
        raise DeployError(f"invalid SSH transport: {value}")
    return transport


def require_paramiko():
    try:
        import paramiko  # type: ignore

        return paramiko
    except ImportError as exc:
        raise DeployError("Paramiko SSH transport requires: pip install paramiko") from exc


def paramiko_client(conn: RemoteConnection):
    paramiko = require_paramiko()
    user, host, port = parse_remote_host(conn.host, conn.port)
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    kwargs = {
        "hostname": host,
        "port": port,
        "username": user,
        "timeout": 20,
    }
    if conn.password:
        kwargs["password"] = conn.password
        kwargs["look_for_keys"] = False
        kwargs["allow_agent"] = False
    elif conn.key_path.strip():
        kwargs["pkey"] = load_paramiko_private_key(paramiko, conn.key_path.strip())
        kwargs["look_for_keys"] = False
        kwargs["allow_agent"] = False
    else:
        kwargs["look_for_keys"] = True
        kwargs["allow_agent"] = True
    client.connect(**kwargs)
    configure_paramiko_keepalive(client)
    return client


def configure_paramiko_keepalive(client) -> None:
    transport = client.get_transport()
    if transport is None:
        return
    try:
        transport.set_keepalive(SSH_KEEPALIVE_INTERVAL_SECONDS)
    except Exception:
        pass
    sock = getattr(transport, "sock", None)
    if sock is None:
        return
    try:
        import socket

        sock.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
    except Exception:
        pass


def paramiko_transport_active(client, *, probe: bool = False) -> bool:
    if client is None:
        return False
    try:
        transport = client.get_transport()
        if transport is None or not transport.is_active():
            return False
        if probe:
            transport.send_ignore()
        return True
    except Exception:
        return False


def load_paramiko_private_key(paramiko, key_path: str):
    errors: list[str] = []
    for key_cls_name in ("RSAKey", "Ed25519Key", "ECDSAKey", "DSSKey"):
        key_cls = getattr(paramiko, key_cls_name, None)
        if key_cls is None:
            continue
        try:
            return key_cls.from_private_key_file(key_path)
        except Exception as exc:  # noqa: BLE001 - try all key formats before failing.
            errors.append(f"{key_cls_name}: {exc}")
    detail = "; ".join(errors) if errors else "unsupported key type"
    raise DeployError(f"could not load SSH private key {key_path}: {detail}")


def short_remote_command(command: str) -> str:
    display_command = command
    if "\n" in display_command or len(display_command) > 180:
        display_command = display_command.splitlines()[0] + " ..."
    return display_command


def run_paramiko_client_command(client, conn: RemoteConnection, command: str, input_text: str | None, log: Optional[LogFn]) -> str:
    display_command = short_remote_command(command)
    log_line(log, "$ paramiko ssh " + shlex.quote(conn.host) + " " + shlex.quote(display_command))
    transport = client.get_transport()
    if transport is None or not transport.is_active():
        raise DeployError("Paramiko SSH transport is not connected")
    channel = transport.open_session()
    try:
        channel.exec_command(command)
        if input_text:
            channel.sendall(input_text.encode("utf-8"))
        channel.shutdown_write()

        chunks: list[str] = []
        while True:
            if channel.recv_ready():
                text = channel.recv(4096).decode("utf-8", "replace")
                chunks.append(text)
                for line in text.splitlines():
                    log_line(log, line)
            if channel.recv_stderr_ready():
                text = channel.recv_stderr(4096).decode("utf-8", "replace")
                chunks.append(text)
                for line in text.splitlines():
                    log_line(log, line)
            if channel.exit_status_ready() and not channel.recv_ready() and not channel.recv_stderr_ready():
                break
            time.sleep(0.05)
        code = channel.recv_exit_status()
        output = "".join(chunks)
        if code != 0:
            raise DeployError(f"remote command failed with exit code {code}")
        return output
    finally:
        channel.close()


def run_paramiko_command(conn: RemoteConnection, command: str, input_text: str | None, log: Optional[LogFn]) -> str:
    client = paramiko_client(conn)
    try:
        return run_paramiko_client_command(client, conn, command, input_text, log)
    finally:
        client.close()


def upload_with_paramiko_sftp(sftp, conn: RemoteConnection, local_path: Path, remote_path: str, log: Optional[LogFn]) -> None:
    total = local_path.stat().st_size
    log_line(log, f"Uploading via Paramiko SFTP {local_path} -> {conn.host}:{remote_path} ({format_bytes(total)})")
    progress = UploadProgressLogger("Upload progress", total, log)
    progress.update(0, total)
    sftp.put(str(local_path), remote_path, callback=lambda sent, remote_total: progress.update(sent, remote_total))
    progress.update(total, total, force=True)
    sftp.chmod(remote_path, 0o755)


def upload_with_paramiko(conn: RemoteConnection, local_path: Path, remote_path: str, log: Optional[LogFn]) -> None:
    client = paramiko_client(conn)
    try:
        sftp = client.open_sftp()
        try:
            upload_with_paramiko_sftp(sftp, conn, local_path, remote_path, log)
        finally:
            sftp.close()
    finally:
        client.close()


def run_openssh_command(conn: RemoteConnection, command: str, input_text: str | None, log: Optional[LogFn]) -> str:
    if conn.uses_password:
        raise DeployError("password authentication requires Paramiko transport")
    return run_process(ssh_args(conn) + [ssh_target(conn), command], input_text=input_text, log=log)


def upload_with_openssh(conn: RemoteConnection, local_path: Path, remote_path: str, log: Optional[LogFn]) -> None:
    if conn.uses_password:
        raise DeployError("password authentication requires Paramiko transport")
    total = local_path.stat().st_size
    command = f"cat > {shell_quote(remote_path)} && chmod 0755 {shell_quote(remote_path)}"
    args = ssh_args(conn) + [ssh_target(conn), command]
    log_line(log, f"Uploading via OpenSSH stream {local_path} -> {conn.host}:{remote_path} ({format_bytes(total)})")
    log_line(log, "$ " + " ".join(shlex.quote(str(arg)) for arg in args))
    progress = UploadProgressLogger("Upload progress", total, log)
    progress.update(0, total)
    proc = subprocess.Popen(
        args,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    assert proc.stdin is not None
    output = b""
    try:
        with local_path.open("rb") as handle:
            while True:
                chunk = handle.read(1024 * 1024)
                if not chunk:
                    break
                proc.stdin.write(chunk)
                progress.update(handle.tell(), total)
        proc.stdin.close()
        output = proc.stdout.read() if proc.stdout is not None else b""
        code = proc.wait()
    except BrokenPipeError:
        try:
            proc.stdin.close()
        except Exception:
            pass
        output = proc.stdout.read() if proc.stdout is not None else b""
        code = proc.wait()
    finally:
        if proc.stdin and not proc.stdin.closed:
            proc.stdin.close()
    text = output.decode("utf-8", "replace") if output else ""
    for line in text.splitlines():
        log_line(log, line)
    if code != 0:
        raise DeployError(f"upload failed with exit code {code}")
    progress.update(total, total, force=True)


class RemoteSession:
    def __init__(self, conn: RemoteConnection, log: Optional[LogFn] = None) -> None:
        self.conn = conn
        self.log = log
        self.transport = normalize_ssh_transport(conn.transport)
        self.client = None
        self.sftp = None

    def __enter__(self) -> "RemoteSession":
        if self.transport == "openssh":
            return self
        return self.connect()

    def connect(self) -> "RemoteSession":
        if self.transport == "openssh":
            return self
        self.close()
        try:
            self.client = paramiko_client(self.conn)
            log_line(self.log, f"Connected via Paramiko SSH: {self.conn.host}")
            self.transport = "paramiko"
            return self
        except DeployError as exc:
            if self.transport == "auto" and "pip install paramiko" in str(exc).lower() and not self.conn.uses_password:
                log_line(self.log, "Paramiko is not installed; using OpenSSH transport")
                self.transport = "openssh"
                return self
            raise

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def close(self) -> None:
        if self.sftp is not None:
            try:
                self.sftp.close()
            finally:
                self.sftp = None
        if self.client is not None:
            try:
                self.client.close()
            finally:
                self.client = None

    def is_connected(self) -> bool:
        if self.transport != "paramiko":
            return True
        return paramiko_transport_active(self.client)

    def ensure_connected(self, *, probe: bool = False) -> None:
        if self.transport != "paramiko":
            return
        if paramiko_transport_active(self.client, probe=probe):
            return
        log_line(self.log, f"SSH connection lost; reconnecting to {self.conn.host}")
        self.connect()

    def run(self, command: str, input_text: str | None = None) -> str:
        try:
            if self.transport == "paramiko":
                self.ensure_connected(probe=True)
                return run_paramiko_client_command(self.client, self.conn, command, input_text, self.log)
            return run_openssh_command(self.conn, command, input_text, self.log)
        except Exception as exc:
            if is_remote_connection_lost_error(exc):
                self.close()
            raise

    def upload(self, local_path: Path, remote_path: str) -> None:
        try:
            if self.transport == "paramiko":
                self.ensure_connected(probe=True)
                if self.sftp is None:
                    self.sftp = self.client.open_sftp()
                upload_with_paramiko_sftp(self.sftp, self.conn, local_path, remote_path, self.log)
                return
            upload_with_openssh(self.conn, local_path, remote_path, self.log)
        except Exception as exc:
            if is_remote_connection_lost_error(exc):
                self.close()
            raise


def run_remote(conn: RemoteConnection, command: str, input_text: str | None = None, log: Optional[LogFn] = None) -> str:
    with RemoteSession(conn, log) as remote:
        return remote.run(command, input_text)


def upload_file(conn: RemoteConnection, local_path: Path, remote_path: str, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        remote.upload(local_path, remote_path)


def remote_binary_path(conn: RemoteConnection) -> str:
    stamp = time.strftime("%Y%m%d-%H%M%S")
    return posixpath.join(conn.remote_tmp.rstrip("/") or "/tmp", f"ag-deploy-{stamp}-linux-amd64")


def gzip_binary_for_upload(binary: Path, log: Optional[LogFn] = None) -> Path:
    source = binary.resolve()
    if not source.exists():
        raise DeployError(f"binary does not exist: {source}")
    fd, name = tempfile.mkstemp(prefix=source.name + "-", suffix=".gz")
    os.close(fd)
    archive = Path(name)
    try:
        with source.open("rb") as src, gzip.open(archive, "wb", compresslevel=9) as dst:
            shutil.copyfileobj(src, dst, length=1024 * 1024)
    except Exception:
        try:
            archive.unlink()
        except FileNotFoundError:
            pass
        raise
    log_line(
        log,
        f"Compressed upload artifact: {format_bytes(source.stat().st_size)} -> {format_bytes(archive.stat().st_size)}",
    )
    return archive


def cleanup_remote_paths(remote: RemoteSession, *paths: str) -> None:
    quoted = [shell_quote(path) for path in paths if path]
    if not quoted:
        return
    try:
        remote.run("rm -f " + " ".join(quoted))
    except Exception as exc:  # noqa: BLE001 - cleanup should not hide the deployment result.
        log_line(remote.log, f"WARNING: failed to clean remote temporary files: {exc}")


def upload_compressed_binary(remote: RemoteSession, local_binary: Path, remote_binary: str) -> str:
    remote_archive = remote_binary + ".gz"
    local_archive = gzip_binary_for_upload(local_binary, remote.log)
    try:
        remote.upload(local_archive, remote_archive)
    finally:
        try:
            local_archive.unlink()
        except FileNotFoundError:
            pass
    command = " ".join(
        [
            "gzip",
            "-dc",
            shell_quote(remote_archive),
            ">",
            shell_quote(remote_binary),
            "&&",
            "rm",
            "-f",
            shell_quote(remote_archive),
            "&&",
            "chmod",
            "0755",
            shell_quote(remote_binary),
        ]
    )
    remote.run(command)
    return remote_archive


def shell_quote(value: str) -> str:
    return shlex.quote(value)


def bool_answer(value: bool) -> str:
    return "y" if value else "n"


def install_answers(options: InstallOptions) -> str:
    values = [
        options.site,
        options.install_dir,
        options.config_file,
        options.service_name,
        options.app_host,
        options.app_port,
        bool_answer(options.install_caddy),
        bool_answer(options.configure_caddy),
        "y",
    ]
    return "\n".join(values) + "\n"


def build_for_remote(binary: Path | None, skip_tests: bool, no_build: bool, log: Optional[LogFn]) -> Path:
    if no_build:
        if binary is None:
            binary = default_binary_path()
        binary = binary.resolve()
        if not binary.exists():
            raise DeployError(f"binary does not exist: {binary}")
        return binary
    return build_linux_amd64(binary or temp_binary_path(), skip_tests=skip_tests, log=log)


def install_with_session(
    remote: RemoteSession,
    install_options: InstallOptions,
    *,
    binary: Path | None = None,
    skip_tests: bool = False,
    no_build: bool = False,
    log: Optional[LogFn] = None,
) -> None:
    remote.log = log
    local_binary = build_for_remote(binary, skip_tests, no_build, log)
    remote_binary = remote_binary_path(remote.conn)
    remote_archive = remote_binary + ".gz"
    try:
        upload_compressed_binary(remote, local_binary, remote_binary)
        remote.run(f"{shell_quote(remote_binary)} install", input_text=install_answers(install_options))
    finally:
        cleanup_remote_paths(remote, remote_binary, remote_archive)
    if install_options.master_key.strip():
        set_master_key_with_session(
            remote,
            MasterKeyOptions(
                install_dir=install_options.install_dir,
                config_file=install_options.config_file,
                master_key=install_options.master_key,
                service_name=install_options.service_name,
                restart_app=True,
            ),
            log=log,
        )


def install_remote(
    conn: RemoteConnection,
    install_options: InstallOptions,
    *,
    binary: Path | None = None,
    skip_tests: bool = False,
    no_build: bool = False,
    log: Optional[LogFn] = None,
) -> None:
    with RemoteSession(conn, log) as remote:
        install_with_session(remote, install_options, binary=binary, skip_tests=skip_tests, no_build=no_build, log=log)


def master_key_remote_payload(options: MasterKeyOptions) -> dict[str, object]:
    return {
        "install_dir": options.install_dir,
        "config_file": options.config_file,
        "service_name": options.service_name,
        "restart_app": options.restart_app,
    }


def master_key_remote_command(payload: dict[str, object]) -> str:
    payload_json = json.dumps(payload, separators=(",", ":"))
    script = r'''
import json
import os
import subprocess
import sys
import time
from pathlib import Path

payload = json.loads(PAYLOAD)
master_key = sys.stdin.read().rstrip("\r\n")
install_dir = Path(str(payload.get("install_dir") or "/opt/ai-gateway")).resolve()
record_path = install_dir / "install.json"


def read_json(path):
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def read_record():
    if not record_path.exists():
        return {}
    data = read_json(record_path)
    if not isinstance(data, dict):
        raise RuntimeError("install record is invalid: " + str(record_path))
    return data


def write_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp." + str(time.time_ns()))
    mode = 0o600
    try:
        mode = path.stat().st_mode & 0o777
    except FileNotFoundError:
        pass
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def config_path(record):
    configured = str(payload.get("config_file") or "").strip()
    if configured:
        return Path(configured)
    recorded = str(record.get("config_file") or "").strip()
    if recorded:
        return Path(recorded)
    return Path("/etc/ai-gateway/config.json")


def service_name(record):
    name = str(payload.get("service_name") or record.get("service_name") or "ai-gateway").strip() or "ai-gateway"
    slot = str(record.get("active_slot") or "").strip()
    if slot:
        return name + "-" + slot
    return name


def run(args):
    proc = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = (proc.stdout or "").strip()
    if output:
        print(output)
    if proc.returncode != 0:
        raise RuntimeError("command failed: " + " ".join(args))


try:
    master_key = master_key.strip()
    if not master_key:
        raise RuntimeError("master_key is required")
    if "\n" in master_key or "\r" in master_key:
        raise RuntimeError("master_key must be a single line")

    record = read_record()
    cfg_file = config_path(record)
    config = read_json(cfg_file)
    if not isinstance(config, dict):
        raise RuntimeError("config file is invalid: " + str(cfg_file))

    previous = str(config.get("master_key") or "").strip()
    config["master_key"] = master_key
    write_json(cfg_file, config)
    print("Config file: " + str(cfg_file))
    if previous == master_key:
        print("Config master_key: unchanged")
    else:
        print("Config master_key: configured")
        if previous:
            print("WARNING: existing master_key was replaced; encrypted credentials require the matching key")

    if bool(payload.get("restart_app", True)):
        name = service_name(record)
        run(["systemctl", "restart", name])
        print("App service restarted: " + name)
except Exception as exc:
    print("error: " + str(exc), file=sys.stderr)
    raise SystemExit(1)
'''
    return "python3 -c " + shell_quote("PAYLOAD = " + repr(payload_json) + "\n" + script)


def set_master_key_with_session(remote: RemoteSession, options: MasterKeyOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    master_key = options.master_key.strip()
    if not master_key:
        raise DeployError("master_key is required")
    if "\n" in master_key or "\r" in master_key:
        raise DeployError("master_key must be a single line")
    remote.run(master_key_remote_command(master_key_remote_payload(options)), input_text=master_key)


def set_master_key_remote(conn: RemoteConnection, options: MasterKeyOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        set_master_key_with_session(remote, options, log=log)


def upgrade_with_session(
    remote: RemoteSession,
    service_options: ServiceOptions,
    *,
    binary: Path | None = None,
    skip_tests: bool = False,
    no_build: bool = False,
    log: Optional[LogFn] = None,
) -> None:
    remote.log = log
    local_binary = build_for_remote(binary, skip_tests, no_build, log)
    remote_binary = remote_binary_path(remote.conn)
    remote_archive = remote_binary + ".gz"
    command = " ".join(
        [
            shell_quote(remote_binary),
            "upgrade",
            "-service",
            shell_quote(service_options.service_name),
            "-install-dir",
            shell_quote(service_options.install_dir),
            "-timeout",
            shell_quote(service_options.timeout),
        ]
    )
    try:
        upload_compressed_binary(remote, local_binary, remote_binary)
        remote.run(command)
    finally:
        cleanup_remote_paths(remote, remote_binary, remote_archive)


def upgrade_remote(
    conn: RemoteConnection,
    service_options: ServiceOptions,
    *,
    binary: Path | None = None,
    skip_tests: bool = False,
    no_build: bool = False,
    log: Optional[LogFn] = None,
) -> None:
    with RemoteSession(conn, log) as remote:
        upgrade_with_session(remote, service_options, binary=binary, skip_tests=skip_tests, no_build=no_build, log=log)


def uninstall_with_session(remote: RemoteSession, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    binary = posixpath.join(service_options.install_dir.rstrip("/"), "current", "ag")
    parts = [
        shell_quote(binary),
        "uninstall",
        "-service",
        shell_quote(service_options.service_name),
        "-install-dir",
        shell_quote(service_options.install_dir),
        "-config-dir",
        shell_quote(service_options.config_dir),
    ]
    if service_options.keep_files:
        parts.append("-keep-files")
    command = (
        f"test -x {shell_quote(binary)} || "
        f"(echo 'installed binary not found: {shell_quote(binary)}' >&2; exit 1); "
        + " ".join(parts)
    )
    remote.run(command)


def uninstall_remote(conn: RemoteConnection, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        uninstall_with_session(remote, service_options, log=log)


def stop_with_session(remote: RemoteSession, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    binary = posixpath.join(service_options.install_dir.rstrip("/"), "current", "ag")
    command = (
        f"test -x {shell_quote(binary)} || "
        f"(echo 'installed binary not found: {shell_quote(binary)}' >&2; exit 1); "
        + " ".join(
            [
                shell_quote(binary),
                "stop",
                "-service",
                shell_quote(service_options.service_name),
                "-install-dir",
                shell_quote(service_options.install_dir),
            ]
        )
    )
    remote.run(command)


def stop_remote(conn: RemoteConnection, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        stop_with_session(remote, service_options, log=log)


def start_with_session(remote: RemoteSession, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    install_json = posixpath.join(service_options.install_dir.rstrip("/"), "install.json")
    python_read = (
        "import json,sys;"
        "data=json.load(open(sys.argv[1]));"
        "key=sys.argv[2];"
        "print(str(data.get(key) or '').strip())"
    )
    command = "\n".join(
        [
            f"install_json={shell_quote(install_json)}",
            f"name={shell_quote(service_options.service_name)}",
            "if [ -f \"$install_json\" ]; then",
            f"  record_name=$(python3 -c {shell_quote(python_read)} \"$install_json\" service_name 2>/dev/null || true)",
            f"  active_slot=$(python3 -c {shell_quote(python_read)} \"$install_json\" active_slot 2>/dev/null || true)",
            "  if [ -n \"$record_name\" ]; then name=\"$record_name\"; fi",
            "  if [ -n \"$active_slot\" ]; then name=\"$name-$active_slot\"; fi",
            "fi",
            "systemctl start \"$name\"",
            "echo \"Service started: $name\"",
        ]
    )
    remote.run(command)


def start_remote(conn: RemoteConnection, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        start_with_session(remote, service_options, log=log)


def status_with_session(remote: RemoteSession, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    install_json = posixpath.join(service_options.install_dir.rstrip("/"), "install.json")
    blue = service_options.service_name + "-blue"
    green = service_options.service_name + "-green"
    command = "\n".join(
        [
            f"if [ -f {shell_quote(install_json)} ]; then cat {shell_quote(install_json)}; else echo 'install record not found: {install_json}'; fi",
            f"systemctl is-active {shell_quote(blue)} || true",
            f"systemctl is-active {shell_quote(green)} || true",
            "ss -ltnp 2>/dev/null | grep -E ':(18088|18089)' || true",
        ]
    )
    remote.run(command)


def status_remote(conn: RemoteConnection, service_options: ServiceOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        status_with_session(remote, service_options, log=log)


def rollback_remote_payload(action: str, service_options: ServiceOptions, release: str = "") -> dict[str, object]:
    return {
        "action": action,
        "install_dir": service_options.install_dir,
        "service_name": service_options.service_name,
        "timeout": service_options.timeout,
        "release": release,
    }


def rollback_remote_command(payload: dict[str, object]) -> str:
    payload_json = json.dumps(payload, separators=(",", ":"))
    script = r'''
import json
import os
import subprocess
import shutil
import sys
import tarfile
import time
from pathlib import Path

payload = json.loads(PAYLOAD)
action = str(payload.get("action") or "")
install_dir = Path(str(payload.get("install_dir") or "/opt/ai-gateway")).resolve()
record_path = install_dir / "install.json"
releases_dir = install_dir / "releases"


def read_json(path):
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def write_json_atomic(path, data):
    path = Path(path)
    tmp = path.with_name(path.name + ".tmp." + str(time.time_ns()))
    mode = 0o600
    try:
        mode = path.stat().st_mode & 0o777
    except FileNotFoundError:
        pass
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def backup_manifest(path):
    try:
        with tarfile.open(path, "r:gz") as archive:
            member = archive.getmember("manifest.json")
            handle = archive.extractfile(member)
            if handle is None:
                return {}
            try:
                return json.loads(handle.read().decode("utf-8", "replace"))
            finally:
                handle.close()
    except Exception:
        return {}


def backup_release_from_name(name):
    prefix = "pre-upgrade-"
    suffix = ".agbak"
    if name.startswith(prefix) and name.endswith(suffix):
        return name[len(prefix):-len(suffix)]
    return ""


def backup_dirs(record):
    dirs = [Path("/var/lib/ai-gateway/backups")]
    state_path = str(record.get("state_path") or "").strip()
    if state_path:
        dirs.append(Path(state_path).parent / "backups")
    seen = set()
    out = []
    for path in dirs:
        key = str(path)
        if key not in seen:
            seen.add(key)
            out.append(path)
    return out


def discover_backups(record):
    backups = {}
    for directory in backup_dirs(record):
        if not directory.exists():
            continue
        for path in directory.glob("*.agbak"):
            try:
                stat = path.stat()
            except OSError:
                continue
            manifest = backup_manifest(path)
            release = backup_release_from_name(path.name) or str(manifest.get("app_version") or "").strip()
            if not release:
                continue
            created_at = str(manifest.get("created_at") or "").strip()
            if not created_at:
                created_at = time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime(stat.st_mtime))
            current = backups.get(release)
            if current and float(current.get("mtime", 0)) >= stat.st_mtime:
                continue
            backups[release] = {
                "path": str(path),
                "created_at": created_at,
                "size": stat.st_size,
                "mtime": stat.st_mtime,
            }
    return backups


def read_record():
    if not record_path.exists():
        raise RuntimeError("install record not found: " + str(record_path))
    record = read_json(record_path)
    if not isinstance(record, dict):
        raise RuntimeError("install record is invalid: " + str(record_path))
    return record


def release_is_usable(release):
    if not release:
        return False
    return (releases_dir / release / "ag").is_file() and os.access(releases_dir / release / "ag", os.X_OK)


def release_dir_for_delete(release):
    if not release or "/" in release or "\\" in release or release in (".", ".."):
        raise RuntimeError("invalid release id: " + release)
    release_dir = (releases_dir / release).resolve()
    if release_dir.parent != releases_dir.resolve():
        raise RuntimeError("invalid release path: " + release)
    return release_dir


def list_versions():
    record = read_record()
    current = str(record.get("current_release") or "").strip()
    previous = str(record.get("previous_release") or "").strip()
    backups = discover_backups(record)
    versions = []
    if releases_dir.exists():
        for release_path in sorted(releases_dir.iterdir(), key=lambda item: item.name, reverse=True):
            if not release_path.is_dir():
                continue
            release = release_path.name
            if release == current or not release_is_usable(release):
                continue
            backup = backups.get(release, {})
            versions.append({
                "release": release,
                "previous": release == previous,
                "backup_path": backup.get("path", ""),
                "backup_created_at": backup.get("created_at", ""),
                "backup_size": int(backup.get("size", 0) or 0),
            })
    print(json.dumps({
        "install_dir": str(install_dir),
        "current_release": current,
        "previous_release": previous,
        "versions": versions,
    }, ensure_ascii=False, separators=(",", ":")))


def run_rollback():
    release = str(payload.get("release") or "").strip()
    if not release:
        raise RuntimeError("rollback release is required")
    record = read_record()
    current = str(record.get("current_release") or "").strip()
    if release == current:
        raise RuntimeError("selected release is already current: " + release)
    if not release_is_usable(release):
        raise RuntimeError("selected release is not usable: " + release)

    original = dict(record)
    record["previous_release"] = release
    write_json_atomic(record_path, record)
    binary = install_dir / "current" / "ag"
    if not binary.is_file():
        write_json_atomic(record_path, original)
        raise RuntimeError("installed binary not found: " + str(binary))
    command = [
        str(binary),
        "rollback",
        "-service",
        str(payload.get("service_name") or "ai-gateway"),
        "-install-dir",
        str(install_dir),
        "-timeout",
        str(payload.get("timeout") or "60s"),
    ]
    print("Selected rollback release: " + release)
    proc = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = proc.stdout or ""
    if output:
        print(output.rstrip())
    if proc.returncode != 0:
        write_json_atomic(record_path, original)
        raise RuntimeError("rollback failed with exit code " + str(proc.returncode))


def delete_version():
    release = str(payload.get("release") or "").strip()
    if not release:
        raise RuntimeError("delete release is required")
    record = read_record()
    current = str(record.get("current_release") or "").strip()
    previous = str(record.get("previous_release") or "").strip()
    if release == current:
        raise RuntimeError("refusing to delete current release: " + release)
    release_dir = release_dir_for_delete(release)
    backups = discover_backups(record)
    deleted = []
    if release_dir.exists():
        shutil.rmtree(release_dir)
        deleted.append(str(release_dir))
    backup = backups.get(release, {})
    backup_path_text = str(backup.get("path") or "").strip()
    if backup_path_text:
        backup_path = Path(backup_path_text)
    else:
        backup_path = None
    if backup_path is not None and backup_path.is_file():
        backup_path.unlink()
        deleted.append(str(backup_path))
    if release == previous:
        updated = dict(record)
        updated["previous_release"] = ""
        write_json_atomic(record_path, updated)
        deleted.append(str(record_path) + " previous_release")
    if not deleted:
        print("Rollback version not found: " + release)
        return
    print("Deleted rollback version: " + release)
    for path in deleted:
        print("Deleted: " + path)


try:
    if action == "list":
        list_versions()
    elif action == "run":
        run_rollback()
    elif action == "delete":
        delete_version()
    else:
        raise RuntimeError("unsupported rollback action: " + action)
except Exception as exc:
    if action == "list":
        print(json.dumps({"error": str(exc), "versions": []}, ensure_ascii=False, separators=(",", ":")))
        raise SystemExit(0)
    print("error: " + str(exc), file=sys.stderr)
    raise SystemExit(1)
'''
    return "python3 - <<'PY'\nPAYLOAD = " + repr(payload_json) + "\n" + script + "\nPY\n"


def list_rollback_versions_with_session(
    remote: RemoteSession,
    service_options: ServiceOptions,
    log: Optional[LogFn] = None,
) -> dict[str, object]:
    remote.log = log
    output = remote.run(rollback_remote_command(rollback_remote_payload("list", service_options)))
    try:
        payload = json.loads(output.strip() or "{}")
    except json.JSONDecodeError as exc:
        raise DeployError("could not parse remote rollback version list") from exc
    if not isinstance(payload, dict):
        raise DeployError("remote rollback version list is invalid")
    error = str(payload.get("error") or "").strip()
    if error:
        raise DeployError(error)
    return payload


def rollback_with_session(
    remote: RemoteSession,
    service_options: ServiceOptions,
    release: str,
    log: Optional[LogFn] = None,
) -> None:
    remote.log = log
    remote.run(rollback_remote_command(rollback_remote_payload("run", service_options, release)))


def delete_rollback_version_with_session(
    remote: RemoteSession,
    service_options: ServiceOptions,
    release: str,
    log: Optional[LogFn] = None,
) -> None:
    remote.log = log
    remote.run(rollback_remote_command(rollback_remote_payload("delete", service_options, release)))


def rollback_remote(conn: RemoteConnection, service_options: ServiceOptions, release: str, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        rollback_with_session(remote, service_options, release, log=log)


def caddy_remote_payload(action: str, options: CaddyOptions) -> dict[str, object]:
    return {
        "action": action,
        "site": options.site,
        "install_dir": options.install_dir,
        "config_file": options.config_file,
        "service_name": options.service_name,
        "app_host": options.app_host,
        "app_port": options.app_port,
        "public_base_url": options.public_base_url,
        "reload_caddy": options.reload_caddy,
    }


def caddy_remote_command(payload: dict[str, object]) -> str:
    payload_json = json.dumps(payload, separators=(",", ":"))
    script = r'''
import ipaddress
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import urlparse

payload = json.loads(PAYLOAD)
action = str(payload.get("action") or "")
install_dir = str(payload.get("install_dir") or "/opt/ai-gateway").rstrip("/") or "/opt/ai-gateway"
record_path = Path(install_dir) / "install.json"
caddyfile_path = Path("/etc/caddy/Caddyfile")


def read_json(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError:
        return {}


def write_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp." + str(time.time_ns()))
    mode = 0o600
    try:
        mode = path.stat().st_mode & 0o777
    except FileNotFoundError:
        pass
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def write_text(path, text, mode=0o644):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp." + str(time.time_ns()))
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write(text)
    os.chmod(tmp, mode)
    os.replace(tmp, path)


def run(args):
    proc = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    output = (proc.stdout or "").strip()
    if output:
        print(output)
    if proc.returncode != 0:
        raise RuntimeError("command failed: " + " ".join(args))


def first_site_address(site):
    for item in site.replace(",", " ").split():
        item = item.strip()
        if item:
            return item
    return ""


def host_from_address(address):
    if "://" in address:
        parsed = urlparse(address)
        return parsed.netloc, parsed.hostname or ""
    hostport = address.strip()
    if hostport.startswith("[") and "]" in hostport:
        host = hostport[1:hostport.index("]")]
        return hostport, host
    host = hostport.split(":", 1)[0]
    return hostport, host


def public_base_url_for_site(site):
    address = first_site_address(site)
    if not address or address.startswith(":"):
        return ""
    hostport, host = host_from_address(address)
    host = host.strip("[]")
    if host.startswith("*."):
        host = host[2:]
        hostport = hostport.replace("*.", "", 1)
    if not host or host.lower() == "localhost":
        return ""
    try:
        ipaddress.ip_address(host)
        return ""
    except ValueError:
        pass
    return "https://" + hostport.rstrip("/")


def normalize_public_base_url(value):
    value = str(value or "").strip().rstrip("/")
    if not value:
        return ""
    parsed = urlparse(value)
    if parsed.scheme not in ("http", "https") or not parsed.netloc or parsed.path not in ("", "/"):
        raise RuntimeError("public base url must be http(s)://host without a path")
    return value


def caddyfile_body(site, app_host, app_port):
    return f"""{site} {{
    encode gzip

    reverse_proxy {app_host}:{app_port} {{
        health_uri /readyz
        transport http {{
            dial_timeout 30s
            read_timeout 0
            write_timeout 0
        }}
    }}
}}
"""


def config_path(record):
    configured = str(payload.get("config_file") or "").strip()
    return configured or str(record.get("config_file") or "/etc/ai-gateway/config.json")


def state_path(record, config):
    return str(record.get("state_path") or config.get("state_path") or "").strip()


def database_backend(config):
    backend = str(config.get("database_backend") or "").strip().lower()
    return backend or "sqlite"


def runtime_public_base_url(data):
    runtime = data.get("Runtime")
    if isinstance(runtime, dict):
        return str(runtime.get("PublicBaseURL") or "")
    runtime = data.get("runtime")
    if isinstance(runtime, dict):
        return str(runtime.get("public_base_url") or "")
    return ""


def update_runtime_public_base_url(db_path, public_base_url, backend):
    if backend == "postgres":
        print("Runtime settings: skipped; PostgreSQL backend is updated by the running service on reload/restart")
        return
    if not db_path or not Path(db_path).exists():
        print("Runtime settings: skipped; state database not found")
        return
    con = sqlite3.connect(db_path)
    try:
        row = con.execute("SELECT payload FROM system_settings WHERE key = ?", ("global",)).fetchone()
        if not row:
            print("Runtime settings: skipped; no system_settings row")
            return
        data = json.loads(row[0] or "{}")
        runtime = data.setdefault("Runtime", {})
        if not isinstance(runtime, dict):
            runtime = {}
            data["Runtime"] = runtime
        runtime["PublicBaseURL"] = public_base_url
        data["UpdatedAt"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        con.execute(
            "UPDATE system_settings SET payload = ?, updated_at_ns = ? WHERE key = ?",
            (json.dumps(data, ensure_ascii=False, separators=(",", ":")), time.time_ns(), "global"),
        )
        con.commit()
        print("Runtime settings: PublicBaseURL = " + (public_base_url or "-"))
    finally:
        con.close()


def show():
    record = read_json(record_path)
    cfg_file = config_path(record)
    config = read_json(cfg_file)
    print("Install record: " + str(record_path))
    if record:
        print("  caddy_site: " + str(record.get("caddy_site") or "-"))
        print("  app: " + str(record.get("app_host") or "-") + ":" + str(record.get("app_port") or "-"))
        print("  active_slot: " + str(record.get("active_slot") or "-"))
        print("  current_release: " + str(record.get("current_release") or "-"))
    else:
        print("  missing")
    print("Config file: " + cfg_file)
    print("  public_base_url: " + str(config.get("public_base_url") or "-"))
    backend = database_backend(config)
    print("  database_backend: " + backend)
    db_path = state_path(record, config)
    if db_path and backend != "postgres":
        try:
            con = sqlite3.connect(db_path)
            row = con.execute("SELECT payload FROM system_settings WHERE key = ?", ("global",)).fetchone()
            con.close()
            if row:
                print("Runtime PublicBaseURL: " + (runtime_public_base_url(json.loads(row[0] or "{}")) or "-"))
        except Exception as exc:
            print("Runtime PublicBaseURL: unavailable: " + str(exc))
    elif backend == "postgres":
        print("Runtime PublicBaseURL: skipped for PostgreSQL backend")
    print("Caddyfile: " + str(caddyfile_path))
    if caddyfile_path.exists():
        print(caddyfile_path.read_text(encoding="utf-8", errors="replace").rstrip())
    else:
        print("  missing")
    if shutil.which("systemctl"):
        proc = subprocess.run(["systemctl", "is-active", "caddy"], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print("Caddy service: " + (proc.stdout or "").strip())


def set_caddy():
    site = str(payload.get("site") or "").strip()
    if not site or any(ch in site for ch in "{}\r\n"):
        raise RuntimeError("invalid Caddy site: " + site)
    record = read_json(record_path)
    cfg_file = config_path(record)
    config = read_json(cfg_file)
    app_host = str(payload.get("app_host") or record.get("app_host") or "127.0.0.1").strip()
    app_port = str(payload.get("app_port") or record.get("app_port") or config.get("port") or "18088").strip()
    public_base_url = normalize_public_base_url(payload.get("public_base_url") or public_base_url_for_site(site))
    body = caddyfile_body(site, app_host, app_port)

    backup_path = ""
    if caddyfile_path.exists():
        backup_path = str(caddyfile_path.with_name("Caddyfile.bak." + str(int(time.time()))))
        shutil.copy2(caddyfile_path, backup_path)
    write_text(caddyfile_path, body)
    try:
        if shutil.which("caddy"):
            run(["caddy", "validate", "--config", str(caddyfile_path)])
        if bool(payload.get("reload_caddy", True)):
            run(["systemctl", "reload", "caddy"])
    except Exception:
        if backup_path:
            shutil.copy2(backup_path, caddyfile_path)
            if bool(payload.get("reload_caddy", True)):
                subprocess.run(["systemctl", "reload", "caddy"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        raise

    if record:
        record["caddy_site"] = site
        record["caddy_configured"] = True
        record["app_host"] = app_host
        record["app_port"] = app_port
        write_json(record_path, record)
        print("Install record: caddy_site = " + site)
    else:
        print("Install record: skipped; not found")

    config["public_base_url"] = public_base_url
    write_json(cfg_file, config)
    print("Config: public_base_url = " + (public_base_url or "-"))
    update_runtime_public_base_url(state_path(record, config), public_base_url, database_backend(config))
    print("Caddy: " + site + " -> " + app_host + ":" + app_port)


try:
    if action == "show":
        show()
    elif action == "set":
        set_caddy()
    else:
        raise RuntimeError("unsupported caddy action: " + action)
except Exception as exc:
    print("error: " + str(exc), file=sys.stderr)
    raise SystemExit(1)
'''
    return "python3 - <<'PY'\nPAYLOAD = " + repr(payload_json) + "\n" + script + "\nPY\n"


def caddy_show_with_session(remote: RemoteSession, options: CaddyOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    remote.run(caddy_remote_command(caddy_remote_payload("show", options)))


def caddy_show_remote(conn: RemoteConnection, options: CaddyOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        caddy_show_with_session(remote, options, log=log)


def caddy_set_with_session(remote: RemoteSession, options: CaddyOptions, log: Optional[LogFn] = None) -> None:
    remote.log = log
    remote.run(caddy_remote_command(caddy_remote_payload("set", options)))
    if options.reload_app:
        binary = posixpath.join(options.install_dir.rstrip("/"), "current", "ag")
        command = " ".join(
            [
                f"test -x {shell_quote(binary)}",
                "&&",
                shell_quote(binary),
                "upgrade",
                "-service",
                shell_quote(options.service_name),
                "-install-dir",
                shell_quote(options.install_dir),
                "-timeout",
                shell_quote(options.timeout),
            ]
        )
        remote.run(command)


def caddy_set_remote(conn: RemoteConnection, options: CaddyOptions, log: Optional[LogFn] = None) -> None:
    with RemoteSession(conn, log) as remote:
        caddy_set_with_session(remote, options, log=log)


def add_remote_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--host", required=True, help="SSH target, for example root@example.com")
    parser.add_argument("--port", type=int, default=22, help="SSH port")
    parser.add_argument("--key", default="", help="SSH private key path")
    parser.add_argument("--password", default="", help="SSH password; prefer --ask-password or --password-stdin")
    parser.add_argument("--ask-password", action="store_true", help="prompt for SSH password")
    parser.add_argument("--password-stdin", action="store_true", help="read SSH password from stdin")
    parser.add_argument("--remote-tmp", default=DEFAULT_REMOTE_TMP, help="remote temporary upload directory")
    parser.add_argument(
        "--transport",
        choices=SSH_TRANSPORT_CHOICES,
        default=DEFAULT_SSH_TRANSPORT,
        help="SSH transport implementation",
    )


def add_build_args(parser: argparse.ArgumentParser, *, include_no_build: bool) -> None:
    parser.add_argument("--output", default="", help="linux/amd64 binary output path")
    parser.add_argument("--skip-tests", action="store_true", help="skip go test before build")
    if include_no_build:
        parser.add_argument("--no-build", action="store_true", help="reuse --output instead of compiling")


def add_install_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--site", default=DEFAULT_SITE, help="Caddy site/domain passed to ./ag install")
    parser.add_argument("--install-dir", default=DEFAULT_INSTALL_DIR)
    parser.add_argument("--config-file", default=DEFAULT_CONFIG_FILE)
    add_master_key_value_args(parser)
    parser.add_argument("--service", default=DEFAULT_SERVICE_NAME, help="systemd service name")
    parser.add_argument("--app-host", default=DEFAULT_APP_HOST)
    parser.add_argument("--app-port", default=DEFAULT_APP_PORT)
    parser.add_argument("--install-caddy", dest="install_caddy", action="store_true", default=True)
    parser.add_argument("--no-install-caddy", dest="install_caddy", action="store_false")
    parser.add_argument("--configure-caddy", dest="configure_caddy", action="store_true", default=True)
    parser.add_argument("--no-configure-caddy", dest="configure_caddy", action="store_false")


def add_service_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--install-dir", default=DEFAULT_INSTALL_DIR)
    parser.add_argument("--config-dir", default=DEFAULT_CONFIG_DIR)
    parser.add_argument("--service", default=DEFAULT_SERVICE_NAME, help="systemd service name")
    parser.add_argument("--timeout", default=DEFAULT_TIMEOUT, help="upgrade readiness timeout")


def add_master_key_value_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--master-key", default="", help="set config master_key; prefer --ask-master-key or --master-key-stdin")
    parser.add_argument("--ask-master-key", action="store_true", help="prompt for config master_key")
    parser.add_argument("--master-key-stdin", action="store_true", help="read config master_key from stdin")


def add_master_key_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--install-dir", default=DEFAULT_INSTALL_DIR)
    parser.add_argument("--config-file", default="", help="app config path; defaults to install record")
    parser.add_argument("--service", default=DEFAULT_SERVICE_NAME, help="systemd service name")
    parser.add_argument("--restart-app", dest="restart_app", action="store_true", default=True)
    parser.add_argument("--no-restart-app", dest="restart_app", action="store_false")
    add_master_key_value_args(parser)


def add_caddy_args(parser: argparse.ArgumentParser, *, require_site: bool) -> None:
    if require_site:
        parser.add_argument("--site", required=True, help="Caddy site/domain, for example gateway.example.com")
    else:
        parser.add_argument("--site", default="", help="Caddy site/domain")
    parser.add_argument("--install-dir", default=DEFAULT_INSTALL_DIR)
    parser.add_argument("--config-file", default="", help="app config path; defaults to install record")
    parser.add_argument("--service", default=DEFAULT_SERVICE_NAME, help="systemd service name")
    parser.add_argument("--app-host", default="", help="reverse proxy target host; defaults to install record")
    parser.add_argument("--app-port", default="", help="reverse proxy target port; defaults to install record")
    parser.add_argument("--timeout", default=DEFAULT_TIMEOUT, help="app reload readiness timeout")
    parser.add_argument("--public-base-url", default="", help="override app Public Base URL; defaults to https://first-site")
    parser.add_argument("--reload-caddy", dest="reload_caddy", action="store_true", default=True)
    parser.add_argument("--no-reload-caddy", dest="reload_caddy", action="store_false")
    parser.add_argument("--reload-app", dest="reload_app", action="store_true", default=True)
    parser.add_argument("--no-reload-app", dest="reload_app", action="store_false")


def connection_from_args(args: argparse.Namespace) -> RemoteConnection:
    password = args.password or ""
    if args.ask_password:
        password = getpass.getpass("SSH password: ")
    if args.password_stdin:
        password = sys.stdin.readline().rstrip("\r\n")
    if password and args.key:
        raise DeployError("choose either --key or password authentication, not both")
    return RemoteConnection(
        host=args.host,
        port=args.port,
        key_path=args.key,
        password=password,
        remote_tmp=args.remote_tmp,
        transport=args.transport,
    )


def install_options_from_args(args: argparse.Namespace) -> InstallOptions:
    return InstallOptions(
        site=args.site,
        install_dir=args.install_dir,
        config_file=args.config_file,
        master_key=master_key_from_args(args),
        service_name=args.service,
        app_host=args.app_host,
        app_port=args.app_port,
        install_caddy=args.install_caddy,
        configure_caddy=args.configure_caddy,
    )


def service_options_from_args(args: argparse.Namespace) -> ServiceOptions:
    return ServiceOptions(
        install_dir=args.install_dir,
        config_dir=getattr(args, "config_dir", DEFAULT_CONFIG_DIR),
        service_name=args.service,
        timeout=getattr(args, "timeout", DEFAULT_TIMEOUT),
        keep_files=getattr(args, "keep_files", True),
    )


def caddy_options_from_args(args: argparse.Namespace) -> CaddyOptions:
    return CaddyOptions(
        site=getattr(args, "site", "") or "",
        install_dir=args.install_dir,
        config_file=getattr(args, "config_file", "") or "",
        service_name=args.service,
        app_host=getattr(args, "app_host", "") or "",
        app_port=getattr(args, "app_port", "") or "",
        timeout=getattr(args, "timeout", DEFAULT_TIMEOUT),
        public_base_url=getattr(args, "public_base_url", "") or "",
        reload_caddy=getattr(args, "reload_caddy", True),
        reload_app=getattr(args, "reload_app", True),
    )


def master_key_from_args(args: argparse.Namespace) -> str:
    master_key = getattr(args, "master_key", "") or ""
    ask_master_key = bool(getattr(args, "ask_master_key", False))
    master_key_stdin = bool(getattr(args, "master_key_stdin", False))
    sources = sum(1 for enabled in (bool(master_key), ask_master_key, master_key_stdin) if enabled)
    if sources > 1:
        raise DeployError("choose only one of --master-key, --ask-master-key, or --master-key-stdin")
    if ask_master_key:
        master_key = getpass.getpass("Config master_key: ")
    elif master_key_stdin:
        master_key = sys.stdin.readline().rstrip("\r\n")
    return str(master_key).strip()


def master_key_options_from_args(args: argparse.Namespace) -> MasterKeyOptions:
    master_key = master_key_from_args(args)
    if not master_key:
        raise DeployError("master_key is required")
    return MasterKeyOptions(
        install_dir=args.install_dir,
        config_file=getattr(args, "config_file", "") or "",
        master_key=master_key,
        service_name=args.service,
        restart_app=getattr(args, "restart_app", True),
    )


def binary_arg(args: argparse.Namespace) -> Path | None:
    value = getattr(args, "output", "") or ""
    return Path(value) if value else None


def run_cli(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="AI Gateway linux/amd64 deploy helper")
    subparsers = parser.add_subparsers(dest="command")

    build = subparsers.add_parser("build", help="compile linux/amd64 binary")
    add_build_args(build, include_no_build=False)

    install = subparsers.add_parser("install", help="build, upload, and run ./ag install remotely")
    add_remote_args(install)
    add_build_args(install, include_no_build=True)
    add_install_args(install)

    upgrade = subparsers.add_parser("upgrade", help="build, upload, and run ./ag upgrade remotely")
    add_remote_args(upgrade)
    add_build_args(upgrade, include_no_build=True)
    add_service_args(upgrade)

    master_key = subparsers.add_parser("master-key", help="set remote config master_key")
    add_remote_args(master_key)
    add_master_key_args(master_key)

    uninstall = subparsers.add_parser("uninstall", help="run installed ./ag uninstall remotely")
    add_remote_args(uninstall)
    add_service_args(uninstall)
    uninstall.add_argument("--keep-files", dest="keep_files", action="store_true", default=True)
    uninstall.add_argument("--delete-files", dest="keep_files", action="store_false")

    status = subparsers.add_parser("status", help="show remote install record and service status")
    add_remote_args(status)
    add_service_args(status)

    caddy = subparsers.add_parser("caddy", help="inspect or update managed Caddy reverse proxy")
    caddy_subparsers = caddy.add_subparsers(dest="caddy_command")

    caddy_show = caddy_subparsers.add_parser("show", help="show managed Caddy and Public Base URL settings")
    add_remote_args(caddy_show)
    add_caddy_args(caddy_show, require_site=False)

    caddy_set = caddy_subparsers.add_parser("set", help="update Caddy site and synced app Public Base URL")
    add_remote_args(caddy_set)
    add_caddy_args(caddy_set, require_site=True)

    if not argv:
        return run_gui()

    args = parser.parse_args(argv)
    if args.command is None:
        parser.print_help()
        return 2

    try:
        if args.command == "build":
            build_linux_amd64(binary_arg(args), skip_tests=args.skip_tests, log=print)
        elif args.command == "install":
            install_remote(
                connection_from_args(args),
                install_options_from_args(args),
                binary=binary_arg(args),
                skip_tests=args.skip_tests,
                no_build=args.no_build,
                log=print,
            )
        elif args.command == "upgrade":
            upgrade_remote(
                connection_from_args(args),
                service_options_from_args(args),
                binary=binary_arg(args),
                skip_tests=args.skip_tests,
                no_build=args.no_build,
                log=print,
            )
        elif args.command == "master-key":
            set_master_key_remote(connection_from_args(args), master_key_options_from_args(args), log=print)
        elif args.command == "uninstall":
            uninstall_remote(connection_from_args(args), service_options_from_args(args), log=print)
        elif args.command == "status":
            status_remote(connection_from_args(args), service_options_from_args(args), log=print)
        elif args.command == "caddy":
            if args.caddy_command == "show":
                caddy_show_remote(connection_from_args(args), caddy_options_from_args(args), log=print)
            elif args.caddy_command == "set":
                caddy_set_remote(connection_from_args(args), caddy_options_from_args(args), log=print)
            else:
                caddy.print_help()
                return 2
        else:
            parser.error(f"unknown command: {args.command}")
    except DeployError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    return 0


def load_qt():
    try:
        from PyQt6 import QtCore, QtGui, QtWidgets

        return QtCore, QtGui, QtWidgets
    except ImportError:
        try:
            from PyQt5 import QtCore, QtGui, QtWidgets

            return QtCore, QtGui, QtWidgets
        except ImportError as exc:
            raise DeployError("GUI requires PyQt6 or PyQt5. Install with: pip install PyQt6") from exc


def run_gui() -> int:
    try:
        QtCore, QtGui, QtWidgets = load_qt()
    except DeployError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    def configure_qt_rendering() -> None:
        os.environ.setdefault("QT_ENABLE_HIGHDPI_SCALING", "1")
        os.environ.setdefault("QT_AUTO_SCREEN_SCALE_FACTOR", "1")
        for attr_name in ("AA_EnableHighDpiScaling", "AA_UseHighDpiPixmaps", "AA_DontShowIconsInMenus"):
            attr = getattr(QtCore.Qt, attr_name, None)
            if attr is not None:
                try:
                    QtWidgets.QApplication.setAttribute(attr, True)
                except Exception:
                    pass
        rounding = getattr(QtCore.Qt, "HighDpiScaleFactorRoundingPolicy", None)
        if rounding is not None:
            policy = getattr(rounding, "RoundPreferFloor", None) or getattr(rounding, "PassThrough", None)
            if policy is not None and hasattr(QtWidgets.QApplication, "setHighDpiScaleFactorRoundingPolicy"):
                try:
                    QtWidgets.QApplication.setHighDpiScaleFactorRoundingPolicy(policy)
                except Exception:
                    pass

    def apply_app_font(app) -> None:
        preferred = ["Segoe UI Variable", "Segoe UI", "Microsoft YaHei UI", "Arial"]
        families = set()
        try:
            families = set(QtGui.QFontDatabase.families())
        except Exception:
            try:
                families = set(QtGui.QFontDatabase().families())
            except Exception:
                families = set()
        family = next((name for name in preferred if not families or name in families), "Segoe UI")
        font = QtGui.QFont(family, 10)
        hinting = getattr(QtGui.QFont, "HintingPreference", None)
        if hinting is not None and hasattr(font, "setHintingPreference"):
            font.setHintingPreference(getattr(hinting, "PreferFullHinting"))
        elif hasattr(QtGui.QFont, "PreferFullHinting") and hasattr(font, "setHintingPreference"):
            font.setHintingPreference(QtGui.QFont.PreferFullHinting)
        app.setFont(font)

    class Bridge(QtCore.QObject):
        line = QtCore.pyqtSignal(str)
        done = QtCore.pyqtSignal(bool, str)

    class DeployWindow(QtWidgets.QWidget):
        def __init__(self) -> None:
            super().__init__()
            self.gui_config_path = gui_config_path()
            self.gui_config = load_gui_config()
            self.bridge = Bridge()
            self.bridge.line.connect(self.append_log)
            self.bridge.done.connect(self.finish_task)
            self.worker: threading.Thread | None = None
            self.buttons: list[object] = []
            self.remote_session: RemoteSession | None = None
            self.connection_inputs: list[object] = []
            self.active_task_title = ""
            self.setWindowTitle("AI Gateway Deploy")
            self.setObjectName("root")
            self.resize(1180, 1200)
            self.setMinimumSize(1040, 1200)
            self.build_ui()

        def config_text(self, key: str, default: str = "") -> str:
            value = self.gui_config.get(key, default)
            if value is None:
                return default
            return str(value)

        def config_bool(self, key: str, default: bool = False) -> bool:
            value = self.gui_config.get(key, default)
            if isinstance(value, bool):
                return value
            if isinstance(value, str):
                return value.strip().lower() in {"1", "true", "yes", "on"}
            return bool(value)

        def build_ui(self) -> None:
            layout = QtWidgets.QVBoxLayout(self)
            layout.setContentsMargins(28, 24, 28, 22)
            layout.setSpacing(12)

            header = QtWidgets.QHBoxLayout()
            header.setSpacing(12)
            layout.addLayout(header)
            title = QtWidgets.QLabel("AI Gateway Deploy")
            title.setObjectName("title")
            header.addWidget(title)
            header.addStretch(1)

            subtitle = QtWidgets.QLabel("Build linux/amd64, manage the remote service, and apply Caddy domain routing.")
            subtitle.setObjectName("subtitle")
            layout.addWidget(subtitle)

            default_key = self.config_text("key_path", "")
            if not default_key and Path(DEFAULT_GUI_KEY).exists():
                default_key = DEFAULT_GUI_KEY

            self.host = QtWidgets.QLineEdit(self.config_text("host", DEFAULT_GUI_HOST))
            self.port = QtWidgets.QLineEdit(self.config_text("port", "22"))
            self.key_path = QtWidgets.QLineEdit(default_key)
            self.password = QtWidgets.QLineEdit(self.config_text("password", ""))
            self.password.setEchoMode(QtWidgets.QLineEdit.EchoMode.Password if hasattr(QtWidgets.QLineEdit, "EchoMode") else QtWidgets.QLineEdit.Password)
            self.remote_tmp = QtWidgets.QLineEdit(self.config_text("remote_tmp", DEFAULT_REMOTE_TMP))
            self.transport = QtWidgets.QComboBox()
            self.transport.addItems(list(SSH_TRANSPORT_CHOICES))
            transport = self.config_text("transport", DEFAULT_SSH_TRANSPORT)
            if transport not in SSH_TRANSPORT_CHOICES:
                transport = DEFAULT_SSH_TRANSPORT
            self.transport.setCurrentText(transport)

            self.host.setPlaceholderText("root@example.com")
            self.key_path.setPlaceholderText("SSH private key path")
            self.password.setPlaceholderText("SSH password")
            self.port.setMaximumWidth(96)

            key_row_widget = QtWidgets.QWidget()
            key_row = QtWidgets.QHBoxLayout(key_row_widget)
            key_row.setContentsMargins(0, 0, 0, 0)
            key_row.setSpacing(8)
            key_row.addWidget(self.key_path)
            browse = QtWidgets.QPushButton("Browse")
            browse.clicked.connect(self.browse_key)
            key_row.addWidget(browse)

            auth_row = QtWidgets.QWidget()
            auth_layout = QtWidgets.QHBoxLayout(auth_row)
            auth_layout.setContentsMargins(0, 0, 0, 0)
            auth_layout.setSpacing(0)
            self.auth_key_button = QtWidgets.QPushButton("Private Key")
            self.auth_password_button = QtWidgets.QPushButton("Password")
            self.auth_key_button.setCheckable(True)
            self.auth_password_button.setCheckable(True)
            auth_mode = self.config_text("auth_mode", "key").lower()
            self.auth_password_button.setChecked(auth_mode == "password")
            self.auth_key_button.setChecked(auth_mode != "password")
            self.auth_key_button.setProperty("segmentRole", "left")
            self.auth_password_button.setProperty("segmentRole", "right")
            for button in (self.auth_key_button, self.auth_password_button):
                button.setProperty("segment", "true")
                auth_layout.addWidget(button)
            self.auth_group = QtWidgets.QButtonGroup(self)
            self.auth_group.setExclusive(True)
            self.auth_group.addButton(self.auth_key_button)
            self.auth_group.addButton(self.auth_password_button)
            self.auth_key_button.clicked.connect(self.sync_auth_mode)
            self.auth_password_button.clicked.connect(self.sync_auth_mode)

            self.site = QtWidgets.QLineEdit(self.config_text("site", DEFAULT_GUI_SITE))
            self.public_base_url = QtWidgets.QLineEdit(self.config_text("public_base_url", ""))
            self.install_dir = QtWidgets.QLineEdit(self.config_text("install_dir", DEFAULT_INSTALL_DIR))
            self.config_file = QtWidgets.QLineEdit(self.config_text("config_file", DEFAULT_CONFIG_FILE))
            self.master_key = QtWidgets.QLineEdit(self.config_text("master_key", ""))
            self.master_key.setEchoMode(QtWidgets.QLineEdit.EchoMode.Password if hasattr(QtWidgets.QLineEdit, "EchoMode") else QtWidgets.QLineEdit.Password)
            self.service = QtWidgets.QLineEdit(self.config_text("service_name", DEFAULT_SERVICE_NAME))
            self.app_host = QtWidgets.QLineEdit(self.config_text("app_host", DEFAULT_APP_HOST))
            self.app_port = QtWidgets.QLineEdit(self.config_text("app_port", DEFAULT_APP_PORT))
            self.timeout = QtWidgets.QLineEdit(self.config_text("timeout", DEFAULT_TIMEOUT))
            self.app_port.setMaximumWidth(120)
            self.timeout.setMaximumWidth(120)
            self.master_key.setPlaceholderText("Config master_key")
            self.public_base_url.setPlaceholderText("Auto from Site")
            self.site.textChanged.connect(self.sync_public_url_placeholder)
            self.skip_tests = QtWidgets.QCheckBox("Skip tests")
            self.skip_tests.setChecked(self.config_bool("skip_tests", False))
            self.install_caddy = QtWidgets.QCheckBox("Install Caddy if missing")
            self.install_caddy.setChecked(self.config_bool("install_caddy", True))
            self.configure_caddy = QtWidgets.QCheckBox("Write Caddy reverse proxy")
            self.configure_caddy.setChecked(self.config_bool("configure_caddy", True))
            self.reload_app_after_caddy = QtWidgets.QCheckBox("Reload app after Caddy change")
            self.reload_app_after_caddy.setChecked(self.config_bool("reload_app_after_caddy", True))
            master_key_widget = QtWidgets.QWidget()
            master_key_row = QtWidgets.QHBoxLayout(master_key_widget)
            master_key_row.setContentsMargins(0, 0, 0, 0)
            master_key_row.setSpacing(8)
            master_key_row.addWidget(self.master_key, 1)
            master_key_apply = self.make_action_button("Apply", self.action_master_key_set, "neutral")
            master_key_apply.setObjectName("compactButton")
            master_key_row.addWidget(master_key_apply)

            content = QtWidgets.QHBoxLayout()
            content.setSpacing(14)
            layout.addLayout(content)

            left_column = QtWidgets.QVBoxLayout()
            left_column.setSpacing(12)
            right_column = QtWidgets.QVBoxLayout()
            right_column.setSpacing(12)
            content.addLayout(left_column, 3)
            content.addLayout(right_column, 2)

            connection_card = QtWidgets.QFrame()
            connection_card.setObjectName("card")
            connection_card.setMinimumHeight(120)
            connection_layout = QtWidgets.QVBoxLayout(connection_card)
            connection_layout.setContentsMargins(18, 16, 18, 18)
            connection_layout.setSpacing(10)
            connection_header = QtWidgets.QHBoxLayout()
            connection_header.setSpacing(10)
            connection_title = QtWidgets.QLabel("Connection")
            connection_title.setObjectName("sectionTitle")
            connection_header.addWidget(connection_title)
            self.connection_status = QtWidgets.QLabel("Not connected")
            self.connection_status.setObjectName("connectionStatus")
            connection_header.addWidget(self.connection_status, 1)
            connection_header.addStretch(1)
            self.connection_toggle = QtWidgets.QPushButton("Details")
            self.connection_toggle.setObjectName("compactButton")
            self.connection_toggle.setProperty("role", "ghost")
            self.connection_toggle.clicked.connect(self.toggle_connection_details)
            connection_header.addWidget(self.connection_toggle)
            self.connect_button = self.make_action_button("Connect", self.action_connect, "primary")
            self.connect_button.setObjectName("compactButton")
            connection_header.addWidget(self.connect_button)
            connection_layout.addLayout(connection_header)

            self.connection_details = QtWidgets.QWidget()
            connection = QtWidgets.QGridLayout(self.connection_details)
            connection.setContentsMargins(0, 0, 0, 0)
            connection.setHorizontalSpacing(10)
            connection.setVerticalSpacing(9)
            self.add_field(connection, 0, "SSH", self.host)
            self.add_field(connection, 1, "Port", self.port)
            self.add_field(connection, 2, "Auth", auth_row)
            self.add_field(connection, 3, "Key", key_row_widget)
            self.add_field(connection, 4, "Password", self.password)
            self.add_field(connection, 5, "Remote tmp", self.remote_tmp)
            self.add_field(connection, 6, "Transport", self.transport)
            self.connection_details.setVisible(False)
            connection_layout.addWidget(self.connection_details)
            self.connection_inputs = [
                self.host,
                self.port,
                self.auth_key_button,
                self.auth_password_button,
                self.key_path,
                browse,
                self.password,
                self.remote_tmp,
                self.transport,
            ]
            left_column.addWidget(connection_card)

            service_card, service = self.make_card("Service")
            service_card.setMinimumHeight(260)
            self.add_section_title(service, 0, "Service")
            self.add_field(service, 1, "Install dir", self.install_dir)
            self.add_field(service, 2, "Config file", self.config_file)
            self.add_field(service, 3, "Master key", master_key_widget)
            self.add_field(service, 4, "Service", self.service)
            self.add_field(service, 5, "Timeout", self.timeout)
            service_checks = QtWidgets.QFrame()
            service_checks.setObjectName("checks")
            service_checks_layout = QtWidgets.QVBoxLayout(service_checks)
            service_checks_layout.setContentsMargins(0, 4, 0, 0)
            service_checks_layout.setSpacing(6)
            for checkbox in (self.skip_tests,):
                service_checks_layout.addWidget(checkbox)
            service.addWidget(service_checks, 6, 1)
            left_column.addWidget(service_card)
            left_column.addStretch(1)

            caddy_card, caddy = self.make_card("Caddy Reverse Proxy", "caddyCard")
            caddy_card.setMinimumHeight(250)
            self.add_section_title(caddy, 0, "Caddy Reverse Proxy")
            self.add_field(caddy, 1, "Site address", self.site)
            self.add_field(caddy, 2, "Public URL", self.public_base_url)
            endpoint = QtWidgets.QWidget()
            endpoint_layout = QtWidgets.QHBoxLayout(endpoint)
            endpoint_layout.setContentsMargins(0, 0, 0, 0)
            endpoint_layout.setSpacing(8)
            endpoint_layout.addWidget(self.app_host, 1)
            colon = QtWidgets.QLabel(":")
            colon.setObjectName("fieldLabel")
            endpoint_layout.addWidget(colon)
            endpoint_layout.addWidget(self.app_port)
            self.add_field(caddy, 3, "Upstream", endpoint)
            caddy_checks = QtWidgets.QFrame()
            caddy_checks.setObjectName("checks")
            caddy_checks_layout = QtWidgets.QVBoxLayout(caddy_checks)
            caddy_checks_layout.setContentsMargins(0, 4, 0, 0)
            caddy_checks_layout.setSpacing(6)
            for checkbox in (self.install_caddy, self.configure_caddy, self.reload_app_after_caddy):
                caddy_checks_layout.addWidget(checkbox)
            caddy.addWidget(caddy_checks, 4, 1)
            caddy_buttons = QtWidgets.QHBoxLayout()
            caddy_buttons.setSpacing(8)
            caddy_buttons.addWidget(self.make_action_button("Apply Caddy", self.action_caddy_set, "primary"))
            caddy_buttons.addWidget(self.make_action_button("Caddy Show", self.action_caddy_show, "neutral"))
            caddy.addLayout(caddy_buttons, 5, 1)
            right_column.addWidget(caddy_card)

            actions_card, actions = self.make_card("Actions")
            actions_card.setMinimumHeight(260)
            self.add_section_title(actions, 0, "Actions")
            action_grid = QtWidgets.QGridLayout()
            action_grid.setContentsMargins(0, 2, 0, 0)
            action_grid.setHorizontalSpacing(8)
            action_grid.setVerticalSpacing(8)
            action_specs = [
                ("Install", self.action_install, "neutral", 0, 0),
                ("Uninstall", self.action_uninstall, "danger", 0, 1),
                ("Start", self.action_start, "neutral", 1, 0),
                ("Stop", self.action_stop, "danger", 1, 1),
                ("Status", self.action_status, "neutral", 2, 0),
                ("Rollback", self.action_rollback, "danger", 2, 1),
                ("Upgrade", self.action_upgrade, "primary", 3, 0),
            ]
            for label, handler, role, row, column in action_specs:
                button = self.make_action_button(label, handler, role)
                column_span = 2 if label == "Upgrade" else 1
                action_grid.addWidget(button, row, column, 1, column_span)
            action_grid.setColumnStretch(0, 1)
            action_grid.setColumnStretch(1, 1)
            actions.addLayout(action_grid, 1, 0, 1, 2)
            right_column.addWidget(actions_card)
            right_column.addStretch(1)

            output_label = QtWidgets.QLabel("Output")
            output_label.setObjectName("sectionTitle")
            layout.addWidget(output_label)
            self.log = QtWidgets.QTextEdit()
            self.log.setObjectName("log")
            self.log.setReadOnly(True)
            self.log.setMinimumHeight(110)
            layout.addWidget(self.log, 1)

            self.setStyleSheet(
                """
                QWidget#root {
                    background: #171719;
                    color: #f2f2f7;
                }
                QWidget {
                    color: #f2f2f7;
                }
                QLabel#title {
                    color: #f2f2f7;
                    font-size: 24px;
                    font-weight: 600;
                }
                QLabel#subtitle {
                    color: #98989d;
                    padding-bottom: 6px;
                }
                QLabel#sectionTitle {
                    color: #f2f2f7;
                    font-size: 12px;
                    font-weight: 600;
                    letter-spacing: 0.4px;
                    text-transform: uppercase;
                }
                QLabel#fieldLabel {
                    color: #98989d;
                    background: transparent;
                    padding-top: 7px;
                }
                QLabel#connectionStatus {
                    color: #98989d;
                    background: transparent;
                }
                QFrame#card {
                    background: #242426;
                    border: 1px solid #3a3a3c;
                    border-radius: 14px;
                }
                QFrame#caddyCard {
                    background: #242426;
                    border: 1px solid #0a84ff;
                    border-color: #0a84ff;
                    border-radius: 14px;
                }
                QFrame#checks {
                    background: transparent;
                    border: 0;
                }
                QLineEdit, QTextEdit {
                    min-height: 20px;
                    background: #1b1b1d;
                    border: 1px solid #48484a;
                    border-radius: 8px;
                    padding: 7px 10px;
                    color: #f2f2f7;
                    selection-background-color: #0a84ff;
                    selection-color: #ffffff;
                }
                QLineEdit:focus, QTextEdit:focus {
                    border-color: #0a84ff;
                    background: #1f1f21;
                }
                QLineEdit:disabled {
                    color: #636366;
                    background: #202022;
                    border-color: #3a3a3c;
                }
                QComboBox {
                    min-height: 20px;
                    background: #1b1b1d;
                    border: 1px solid #48484a;
                    border-radius: 8px;
                    padding: 7px 10px;
                    color: #f2f2f7;
                }
                QComboBox:focus {
                    border-color: #0a84ff;
                    background: #1f1f21;
                }
                QComboBox::drop-down {
                    border: 0;
                    width: 24px;
                }
                QComboBox QAbstractItemView {
                    background: #2c2c2e;
                    border: 1px solid #515154;
                    border-radius: 8px;
                    padding: 4px;
                    color: #f2f2f7;
                    selection-background-color: #0a84ff;
                    selection-color: #ffffff;
                    outline: 0;
                }
                QPushButton {
                    min-height: 24px;
                    background: #2c2c2e;
                    border: 1px solid #48484a;
                    border-radius: 8px;
                    padding: 8px 14px;
                    color: #f2f2f7;
                    font-weight: 500;
                }
                QPushButton:hover {
                    background: #3a3a3c;
                    border-color: #5a5a5e;
                }
                QPushButton:pressed {
                    background: #242426;
                }
                QPushButton:disabled {
                    color: #636366;
                    background: #242426;
                    border-color: #3a3a3c;
                }
                QPushButton#compactButton {
                    min-height: 20px;
                    min-width: 92px;
                    padding: 6px 12px;
                }
                QPushButton[role="ghost"] {
                    background: transparent;
                    border-color: transparent;
                    color: #c7c7cc;
                }
                QPushButton[role="ghost"]:hover {
                    background: #2c2c2e;
                    border-color: #3a3a3c;
                    color: #f2f2f7;
                }
                QPushButton[role="primary"] {
                    background: #0a84ff;
                    border-color: #0a84ff;
                    color: #ffffff;
                }
                QPushButton[role="primary"]:hover {
                    background: #2997ff;
                    border-color: #2997ff;
                }
                QPushButton[role="danger"] {
                    color: #ffb3b8;
                    border-color: #5c2c31;
                    background: #2f1f22;
                }
                QPushButton[role="danger"]:hover {
                    background: #43262b;
                    border-color: #7a343b;
                }
                QPushButton[segment="true"] {
                    border-radius: 0;
                    background: #1b1b1d;
                    color: #98989d;
                    border-color: #48484a;
                }
                QPushButton[segmentRole="left"] {
                    border-top-left-radius: 8px;
                    border-bottom-left-radius: 8px;
                }
                QPushButton[segmentRole="right"] {
                    border-top-right-radius: 8px;
                    border-bottom-right-radius: 8px;
                }
                QPushButton[segment="true"]:checked {
                    background: #3a3a3c;
                    border-color: #0a84ff;
                    color: #f2f2f7;
                }
                QCheckBox {
                    color: #f2f2f7;
                    background: transparent;
                    padding: 1px 0;
                }
                QCheckBox::indicator {
                    width: 14px;
                    height: 14px;
                    border-radius: 4px;
                    border: 1px solid #636366;
                    background: #1b1b1d;
                }
                QCheckBox::indicator:checked {
                    background: #0a84ff;
                    border-color: #0a84ff;
                }
                QMenu {
                    background: #2c2c2e;
                    border: 1px solid #515154;
                    border-radius: 10px;
                    padding: 7px;
                    color: #f2f2f7;
                }
                QMenu::item {
                    min-width: 150px;
                    min-height: 22px;
                    padding: 7px 12px;
                    margin: 1px 0;
                    border-radius: 6px;
                    color: #f2f2f7;
                    background: transparent;
                }
                QMenu::icon {
                    width: 0;
                    height: 0;
                    padding: 0;
                    margin: 0;
                }
                QMenu::indicator {
                    width: 0;
                    height: 0;
                }
                QMenu::item:selected {
                    background: #0a84ff;
                    color: #ffffff;
                }
                QMenu::item:disabled {
                    color: #7a7a80;
                    background: transparent;
                }
                QMenu::separator {
                    height: 1px;
                    background: #3a3a3c;
                    margin: 6px 4px;
                }
                QTextEdit#log {
                    font-family: Consolas, "Courier New", monospace;
                    font-size: 12px;
                    min-height: 110px;
                    background: #111113;
                }
                """
            )
            self.sync_public_url_placeholder()
            self.sync_auth_mode()
            self.refresh_connection_ui()
            self.append_log(f"Config: {self.gui_config_path}")

        def make_card(self, title_text: str, object_name: str = "card"):
            card = QtWidgets.QFrame()
            card.setObjectName(object_name)
            card_layout = QtWidgets.QGridLayout(card)
            card_layout.setContentsMargins(18, 16, 18, 18)
            card_layout.setHorizontalSpacing(10)
            card_layout.setVerticalSpacing(9)
            return card, card_layout

        def make_action_button(self, label: str, handler, role: str):
            button = QtWidgets.QPushButton(label)
            button.setProperty("role", role)
            button.clicked.connect(handler)
            try:
                button.setCursor(QtCore.Qt.CursorShape.PointingHandCursor)
            except Exception:
                try:
                    button.setCursor(QtCore.Qt.PointingHandCursor)
                except Exception:
                    pass
            self.buttons.append(button)
            return button

        def add_section_title(self, grid, row: int, title_text: str) -> None:
            title = QtWidgets.QLabel(title_text)
            title.setObjectName("sectionTitle")
            grid.addWidget(title, row, 0, 1, 2)

        def add_field(self, grid, row: int, label_text: str, widget) -> None:
            label = QtWidgets.QLabel(label_text)
            label.setObjectName("fieldLabel")
            grid.addWidget(label, row, 0)
            grid.addWidget(widget, row, 1)
            grid.setColumnStretch(1, 1)

        def clear_input_selection(self) -> None:
            for field in self.findChildren(QtWidgets.QLineEdit):
                field.deselect()

        def sync_public_url_placeholder(self) -> None:
            site = self.site.text().strip()
            if site and not site.startswith(":"):
                address = site.replace(",", " ").split()[0]
                if "://" in address:
                    address = address.split("://", 1)[1]
                address = address.split("/", 1)[0]
                address = address.strip("[]")
                if address:
                    self.public_base_url.setPlaceholderText(f"Auto: https://{address}")
                    return
            self.public_base_url.setPlaceholderText("Auto from Site")

        def sync_auth_mode(self) -> None:
            if self.remote_session is not None:
                self.key_path.setEnabled(False)
                self.password.setEnabled(False)
                return
            use_password = self.auth_password_button.isChecked()
            self.key_path.setEnabled(not use_password)
            self.password.setEnabled(use_password)

        def toggle_connection_details(self) -> None:
            visible = not self.connection_details.isVisible()
            self.connection_details.setVisible(visible)
            self.connection_toggle.setText("Hide details" if visible else "Details")

        def set_connection_inputs_enabled(self, enabled: bool) -> None:
            for widget in self.connection_inputs:
                widget.setEnabled(enabled)
            if enabled:
                self.sync_auth_mode()

        def drop_inactive_remote_session(self, log: LogFn | None = None) -> bool:
            session = self.remote_session
            if session is None or session.is_connected():
                return False
            host = session.conn.host
            self.disconnect_remote_session(refresh=False)
            log_line(log, f"Connection lost: {host}")
            return True

        def refresh_connection_ui(self) -> None:
            self.drop_inactive_remote_session()
            if self.remote_session is None:
                self.connection_status.setText("Not connected")
                self.connection_status.setStyleSheet("color: #98989d;")
                self.connect_button.setText("Connect")
                self.connect_button.setProperty("role", "primary")
                self.connect_button.style().unpolish(self.connect_button)
                self.connect_button.style().polish(self.connect_button)
                self.set_connection_inputs_enabled(True)
                return
            self.connection_status.setText(f"Connected: {self.remote_session.conn.host}")
            self.connection_status.setStyleSheet("color: #30d158;")
            self.connect_button.setText("Disconnect")
            self.connect_button.setProperty("role", "danger")
            self.set_connection_inputs_enabled(False)
            self.connect_button.style().unpolish(self.connect_button)
            self.connect_button.style().polish(self.connect_button)

        def disconnect_remote_session(self, refresh: bool = True) -> None:
            session = self.remote_session
            self.remote_session = None
            if session is not None:
                session.close()
            if refresh and hasattr(self, "connection_status"):
                self.refresh_connection_ui()

        def connect_remote_session(self, conn: RemoteConnection, log: LogFn) -> None:
            self.disconnect_remote_session(refresh=False)
            session = RemoteSession(conn, log)
            try:
                session.__enter__()
                if session.transport != "paramiko":
                    raise DeployError("persistent GUI connection requires Paramiko transport")
                session.run("true")
            except Exception:
                session.close()
                raise
            self.remote_session = session
            log_line(log, f"Persistent connection ready: {conn.host}")

        def remote_for_action(self, log: LogFn) -> RemoteSession:
            self.drop_inactive_remote_session(log)
            if self.remote_session is None:
                conn = self.connection()
                log_line(log, f"Connecting to {conn.host}")
                self.connect_remote_session(conn, log)
            self.remote_session.log = log
            return self.remote_session

        def target_connection(self) -> RemoteConnection:
            if self.remote_session is not None:
                if not self.remote_session.is_connected():
                    conn = self.remote_session.conn
                    self.disconnect_remote_session()
                    return conn
                return self.remote_session.conn
            return self.connection()

        def show_connecting_if_needed(self, conn: RemoteConnection) -> None:
            self.drop_inactive_remote_session()
            if self.remote_session is not None:
                return
            self.connection_status.setText(f"Connecting: {conn.host}")
            self.connection_status.setStyleSheet("color: #0a84ff;")
            self.clear_input_selection()

        def gui_config_data(self) -> dict[str, object]:
            return {
                "host": self.host.text().strip(),
                "port": self.port.text().strip() or "22",
                "auth_mode": "password" if self.auth_password_button.isChecked() else "key",
                "key_path": self.key_path.text().strip(),
                "password": self.password.text(),
                "remote_tmp": self.remote_tmp.text().strip() or DEFAULT_REMOTE_TMP,
                "transport": self.transport.currentText().strip() or DEFAULT_SSH_TRANSPORT,
                "site": self.site.text().strip() or DEFAULT_GUI_SITE,
                "public_base_url": self.public_base_url.text().strip(),
                "install_dir": self.install_dir.text().strip() or DEFAULT_INSTALL_DIR,
                "config_file": self.config_file.text().strip() or DEFAULT_CONFIG_FILE,
                "master_key": self.master_key.text().strip(),
                "service_name": self.service.text().strip() or DEFAULT_SERVICE_NAME,
                "app_host": self.app_host.text().strip() or DEFAULT_APP_HOST,
                "app_port": self.app_port.text().strip() or DEFAULT_APP_PORT,
                "timeout": self.timeout.text().strip() or DEFAULT_TIMEOUT,
                "skip_tests": self.skip_tests.isChecked(),
                "install_caddy": self.install_caddy.isChecked(),
                "configure_caddy": self.configure_caddy.isChecked(),
                "reload_app_after_caddy": self.reload_app_after_caddy.isChecked(),
            }

        def save_gui_config(self, report_errors: bool = False) -> None:
            try:
                save_gui_config(self.gui_config_data())
            except Exception as exc:  # noqa: BLE001 - GUI should keep working if persistence fails.
                if report_errors:
                    self.append_log(f"Config save failed: {exc}")

        def closeEvent(self, event) -> None:  # noqa: N802 - Qt override name.
            self.save_gui_config()
            self.disconnect_remote_session()
            super().closeEvent(event)

        def message_box_button(self, name: str):
            standard_button = getattr(QtWidgets.QMessageBox, "StandardButton", None)
            if standard_button is not None and hasattr(standard_button, name):
                return getattr(standard_button, name)
            return getattr(QtWidgets.QMessageBox, name)

        def confirm_action(self, title: str, text: str, detail: str = "", parent=None) -> bool:
            box = QtWidgets.QMessageBox(parent or self)
            box.setObjectName("confirmBox")
            icon = getattr(QtWidgets.QMessageBox, "Icon", None)
            if icon is not None and hasattr(icon, "Warning"):
                box.setIcon(getattr(icon, "Warning"))
            else:
                box.setIcon(QtWidgets.QMessageBox.Warning)
            box.setWindowTitle(title)
            box.setText(text)
            if detail:
                box.setInformativeText(detail)
            yes = self.message_box_button("Yes")
            no = self.message_box_button("No")
            box.setStandardButtons(yes | no)
            box.setDefaultButton(no)
            box.setMinimumWidth(420)
            box.setStyleSheet(
                """
                QMessageBox#confirmBox {
                    background: #242426;
                    color: #f2f2f7;
                }
                QMessageBox#confirmBox QLabel {
                    color: #f2f2f7;
                    background: transparent;
                    font-size: 13px;
                }
                QMessageBox#confirmBox QLabel#qt_msgbox_informativelabel {
                    color: #c7c7cc;
                }
                QMessageBox#confirmBox QPushButton {
                    min-width: 76px;
                    min-height: 30px;
                    background: #2c2c2e;
                    border: 1px solid #48484a;
                    border-radius: 8px;
                    padding: 6px 14px;
                    color: #f2f2f7;
                    font-weight: 500;
                }
                QMessageBox#confirmBox QPushButton:hover {
                    background: #3a3a3c;
                    border-color: #5a5a5e;
                }
                QMessageBox#confirmBox QPushButton:default {
                    background: #0a84ff;
                    border-color: #0a84ff;
                    color: #ffffff;
                }
                """
            )
            result = box.exec() if hasattr(box, "exec") else box.exec_()
            return result == yes

        def browse_key(self) -> None:
            path, _ = QtWidgets.QFileDialog.getOpenFileName(self, "SSH private key")
            if path:
                self.key_path.setText(path)
                self.save_gui_config()

        def append_log(self, message: str) -> None:
            self.log.append(message)

        def finish_task(self, ok: bool, message: str) -> None:
            self.append_log(("Done: " if ok else "Failed: ") + message)
            for button in self.buttons:
                button.setEnabled(True)
            self.drop_inactive_remote_session(self.append_log)
            if not ok and self.active_task_title == "Connect" and self.remote_session is None:
                self.connection_status.setText("Connection failed")
                self.connection_status.setStyleSheet("color: #ff9f0a;")
                self.connect_button.setText("Connect")
                self.connect_button.setProperty("role", "primary")
                self.set_connection_inputs_enabled(True)
                self.connect_button.style().unpolish(self.connect_button)
                self.connect_button.style().polish(self.connect_button)
            else:
                self.refresh_connection_ui()
            self.clear_input_selection()
            self.active_task_title = ""

        def run_task(self, title: str, fn: Callable[[LogFn], None]) -> None:
            if self.worker and self.worker.is_alive():
                return
            self.active_task_title = title
            self.log.clear()
            self.save_gui_config(report_errors=True)
            self.append_log(title)
            for button in self.buttons:
                button.setEnabled(False)
            self.log.setFocus()
            self.clear_input_selection()

            def work() -> None:
                try:
                    fn(self.bridge.line.emit)
                except Exception as exc:  # noqa: BLE001 - GUI must surface any deployment failure.
                    self.bridge.done.emit(False, str(exc))
                    return
                self.bridge.done.emit(True, title)

            self.worker = threading.Thread(target=work, daemon=True)
            self.worker.start()

        def connection(self) -> RemoteConnection:
            use_password = self.auth_password_button.isChecked()
            key = "" if use_password else self.key_path.text().strip()
            password = self.password.text() if use_password else ""
            return RemoteConnection(
                host=self.host.text().strip(),
                port=int(self.port.text().strip() or "22"),
                key_path=key,
                password=password,
                remote_tmp=self.remote_tmp.text().strip() or DEFAULT_REMOTE_TMP,
                transport=self.transport.currentText().strip() or DEFAULT_SSH_TRANSPORT,
            )

        def install_options(self) -> InstallOptions:
            return InstallOptions(
                site=self.site.text().strip() or DEFAULT_SITE,
                install_dir=self.install_dir.text().strip() or DEFAULT_INSTALL_DIR,
                config_file=self.config_file.text().strip() or DEFAULT_CONFIG_FILE,
                master_key=self.master_key.text().strip(),
                service_name=self.service.text().strip() or DEFAULT_SERVICE_NAME,
                app_host=self.app_host.text().strip() or DEFAULT_APP_HOST,
                app_port=self.app_port.text().strip() or DEFAULT_APP_PORT,
                install_caddy=self.install_caddy.isChecked(),
                configure_caddy=self.configure_caddy.isChecked(),
            )

        def service_options(self) -> ServiceOptions:
            return ServiceOptions(
                install_dir=self.install_dir.text().strip() or DEFAULT_INSTALL_DIR,
                config_dir=DEFAULT_CONFIG_DIR,
                service_name=self.service.text().strip() or DEFAULT_SERVICE_NAME,
                timeout=self.timeout.text().strip() or DEFAULT_TIMEOUT,
            )

        def caddy_options(self) -> CaddyOptions:
            return CaddyOptions(
                site=self.site.text().strip(),
                install_dir=self.install_dir.text().strip() or DEFAULT_INSTALL_DIR,
                config_file=self.config_file.text().strip(),
                service_name=self.service.text().strip() or DEFAULT_SERVICE_NAME,
                app_host=self.app_host.text().strip(),
                app_port=self.app_port.text().strip(),
                timeout=self.timeout.text().strip() or DEFAULT_TIMEOUT,
                public_base_url=self.public_base_url.text().strip(),
                reload_caddy=True,
                reload_app=self.reload_app_after_caddy.isChecked(),
            )

        def master_key_options(self) -> MasterKeyOptions:
            return MasterKeyOptions(
                install_dir=self.install_dir.text().strip() or DEFAULT_INSTALL_DIR,
                config_file=self.config_file.text().strip() or DEFAULT_CONFIG_FILE,
                master_key=self.master_key.text().strip(),
                service_name=self.service.text().strip() or DEFAULT_SERVICE_NAME,
                restart_app=True,
            )

        def select_rollback_version(self, versions: list[dict[str, object]], delete_fn: Callable[[str], None]) -> dict[str, object] | None:
            if not versions:
                return None
            dialog = QtWidgets.QDialog(self)
            dialog.setObjectName("rollbackDialog")
            dialog.setWindowTitle("Rollback Version")
            dialog.setMinimumWidth(760)
            dialog.setMinimumHeight(320)

            layout = QtWidgets.QVBoxLayout(dialog)
            layout.setContentsMargins(18, 16, 18, 16)
            layout.setSpacing(12)

            title = QtWidgets.QLabel("Select rollback version")
            title.setObjectName("dialogTitle")
            layout.addWidget(title)

            list_widget = QtWidgets.QListWidget()
            list_widget.setObjectName("rollbackList")
            list_widget.setAlternatingRowColors(False)
            try:
                list_widget.setSelectionMode(QtWidgets.QAbstractItemView.SelectionMode.MultiSelection)
            except Exception:
                list_widget.setSelectionMode(QtWidgets.QAbstractItemView.MultiSelection)
            layout.addWidget(list_widget, 1)

            buttons = QtWidgets.QHBoxLayout()
            select_all = QtWidgets.QPushButton("Select All")
            select_all.setProperty("role", "neutral")
            batch_delete = QtWidgets.QPushButton("Batch Delete")
            batch_delete.setProperty("role", "danger")
            buttons.addWidget(select_all)
            buttons.addWidget(batch_delete)
            buttons.addStretch(1)
            cancel = QtWidgets.QPushButton("Cancel")
            cancel.setProperty("role", "neutral")
            ok_button = QtWidgets.QPushButton("OK")
            ok_button.setProperty("role", "primary")
            buttons.addWidget(cancel)
            buttons.addWidget(ok_button)
            layout.addLayout(buttons)

            cancel.clicked.connect(dialog.reject)
            ok_button.clicked.connect(dialog.accept)
            list_widget.itemDoubleClicked.connect(lambda _item: dialog.accept())

            def item_version(item) -> dict[str, object]:
                version = item.data(0x0100)
                return version if isinstance(version, dict) else {}

            def is_previous_item(item) -> bool:
                return bool(item_version(item).get("previous"))

            def selected_deletable_items() -> list:
                return [
                    list_widget.item(row_index)
                    for row_index in range(list_widget.count())
                    if list_widget.item(row_index).isSelected() and not is_previous_item(list_widget.item(row_index))
                ]

            def refresh_delete_buttons() -> None:
                has_items = list_widget.count() > 0
                has_deletable_items = any(not is_previous_item(list_widget.item(row_index)) for row_index in range(list_widget.count()))
                ok_button.setEnabled(has_items)
                select_all.setEnabled(has_deletable_items)
                batch_delete.setEnabled(bool(selected_deletable_items()))

            def set_delete_busy(busy: bool) -> None:
                list_widget.setEnabled(not busy)
                select_all.setEnabled(not busy)
                batch_delete.setEnabled(not busy and bool(selected_deletable_items()))
                cancel.setEnabled(not busy)
                ok_button.setEnabled(not busy)
                for row_index in range(list_widget.count()):
                    row_widget = list_widget.itemWidget(list_widget.item(row_index))
                    if row_widget is None:
                        continue
                    for button in row_widget.findChildren(QtWidgets.QPushButton):
                        button.setEnabled(not busy)

            def handle_delete(item, version: dict[str, object]) -> None:
                release = str(version.get("release") or "").strip()
                if not release:
                    return
                detail_lines = [f"Release: {release}"]
                backup_path = str(version.get("backup_path") or "").strip()
                if backup_path:
                    detail_lines.append(f"Backup: {backup_path}")
                if not self.confirm_action("Confirm Delete", "Delete the selected rollback version?", "\n".join(detail_lines), parent=dialog):
                    return
                set_delete_busy(True)
                try:
                    try:
                        QtWidgets.QApplication.setOverrideCursor(QtCore.Qt.CursorShape.WaitCursor)
                    except Exception:
                        try:
                            QtWidgets.QApplication.setOverrideCursor(QtCore.Qt.WaitCursor)
                        except Exception:
                            pass
                    delete_fn(release)
                    row = list_widget.row(item)
                    removed = list_widget.takeItem(row)
                    del removed
                    if list_widget.count() > 0:
                        list_widget.setCurrentRow(min(row, list_widget.count() - 1))
                    else:
                        ok_button.setEnabled(False)
                except Exception as exc:  # noqa: BLE001 - show remote delete failures without closing the dialog.
                    self.append_log(f"Failed: {exc}")
                finally:
                    try:
                        QtWidgets.QApplication.restoreOverrideCursor()
                    except Exception:
                        pass
                    set_delete_busy(False)
                    if list_widget.count() == 0:
                        ok_button.setEnabled(False)
                    refresh_delete_buttons()

            def handle_select_all() -> None:
                list_widget.clearSelection()
                first_selected = None
                for row_index in range(list_widget.count()):
                    item = list_widget.item(row_index)
                    if is_previous_item(item):
                        continue
                    item.setSelected(True)
                    if first_selected is None:
                        first_selected = item
                if first_selected is not None:
                    list_widget.setCurrentItem(first_selected)
                refresh_delete_buttons()

            def handle_batch_delete() -> None:
                items = selected_deletable_items()
                versions = [(item, item_version(item)) for item in items]
                versions = [(item, version) for item, version in versions if str(version.get("release") or "").strip()]
                if not versions:
                    return
                release_lines = [str(version.get("release") or "").strip() for _item, version in versions]
                detail = "\n".join(release_lines[:12])
                if len(release_lines) > 12:
                    detail += f"\n... and {len(release_lines) - 12} more"
                if not self.confirm_action(
                    "Confirm Batch Delete",
                    f"Delete {len(release_lines)} selected rollback version(s)?",
                    detail,
                    parent=dialog,
                ):
                    return
                set_delete_busy(True)
                try:
                    try:
                        QtWidgets.QApplication.setOverrideCursor(QtCore.Qt.CursorShape.WaitCursor)
                    except Exception:
                        try:
                            QtWidgets.QApplication.setOverrideCursor(QtCore.Qt.WaitCursor)
                        except Exception:
                            pass
                    for item, version in list(versions):
                        release = str(version.get("release") or "").strip()
                        delete_fn(release)
                        row = list_widget.row(item)
                        if row >= 0:
                            removed = list_widget.takeItem(row)
                            del removed
                    if list_widget.count() > 0:
                        list_widget.setCurrentRow(0)
                    else:
                        ok_button.setEnabled(False)
                except Exception as exc:  # noqa: BLE001 - keep the dialog open after a partial batch failure.
                    self.append_log(f"Failed: {exc}")
                finally:
                    try:
                        QtWidgets.QApplication.restoreOverrideCursor()
                    except Exception:
                        pass
                    set_delete_busy(False)
                    refresh_delete_buttons()

            def add_version_item(version: dict[str, object]) -> None:
                label_text = rollback_version_label(version)
                item = QtWidgets.QListWidgetItem()
                item.setData(0x0100, version)
                item.setToolTip(label_text)
                item.setSizeHint(QtCore.QSize(680, 48))
                row_widget = QtWidgets.QWidget()
                row_widget.setObjectName("rollbackRow")
                row_layout = QtWidgets.QHBoxLayout(row_widget)
                row_layout.setContentsMargins(10, 5, 8, 5)
                row_layout.setSpacing(10)
                label = QtWidgets.QLabel(label_text)
                label.setObjectName("rollbackItemLabel")
                label.setToolTip(label_text)
                label.setMinimumWidth(0)
                try:
                    label.setSizePolicy(QtWidgets.QSizePolicy.Policy.Ignored, QtWidgets.QSizePolicy.Policy.Preferred)
                except Exception:
                    label.setSizePolicy(QtWidgets.QSizePolicy.Ignored, QtWidgets.QSizePolicy.Preferred)
                row_layout.addWidget(label, 1)
                delete_button = QtWidgets.QPushButton("Delete")
                delete_button.setProperty("role", "danger")
                delete_button.setObjectName("rowDeleteButton")
                delete_button.clicked.connect(lambda _checked=False, target=item, payload=version: handle_delete(target, payload))
                row_layout.addWidget(delete_button)

                def select_item_event(event, target=item):
                    was_selected = target.isSelected()
                    list_widget.setCurrentItem(target)
                    target.setSelected(not was_selected)
                    refresh_delete_buttons()
                    event.accept()

                row_widget.mousePressEvent = select_item_event
                label.mousePressEvent = select_item_event
                list_widget.addItem(item)
                list_widget.setItemWidget(item, row_widget)

            for version in versions:
                add_version_item(version)
            list_widget.setCurrentRow(0)
            refresh_delete_buttons()

            select_all.clicked.connect(handle_select_all)
            batch_delete.clicked.connect(handle_batch_delete)
            list_widget.itemSelectionChanged.connect(refresh_delete_buttons)

            dialog.setStyleSheet(
                """
                QDialog#rollbackDialog {
                    background: #242426;
                    color: #f2f2f7;
                }
                QDialog#rollbackDialog QLabel#dialogTitle {
                    color: #f2f2f7;
                    font-size: 14px;
                    font-weight: 600;
                    background: transparent;
                }
                QListWidget#rollbackList {
                    background: #111113;
                    border: 1px solid #3a3a3c;
                    border-radius: 8px;
                    padding: 6px;
                    color: #f2f2f7;
                    outline: 0;
                }
                QListWidget#rollbackList::item {
                    min-height: 38px;
                    padding: 0;
                    border-radius: 6px;
                    color: #f2f2f7;
                }
                QListWidget#rollbackList::item:selected {
                    background: #0a84ff;
                    border-radius: 2px;
                    color: #ffffff;
                }
                QListWidget#rollbackList::item:hover {
                    background: #2c2c2e;
                }
                QWidget#rollbackRow {
                    background: transparent;
                }
                QLabel#rollbackItemLabel {
                    color: #f2f2f7;
                    background: transparent;
                }
                QDialog#rollbackDialog QPushButton {
                    min-width: 82px;
                    min-height: 30px;
                    background: #2c2c2e;
                    border: 1px solid #48484a;
                    border-radius: 8px;
                    padding: 6px 14px;
                    color: #f2f2f7;
                    font-weight: 500;
                }
                QDialog#rollbackDialog QPushButton:hover {
                    background: #3a3a3c;
                    border-color: #5a5a5e;
                }
                QDialog#rollbackDialog QPushButton[role="primary"] {
                    background: #0a84ff;
                    border-color: #0a84ff;
                    color: #ffffff;
                }
                QDialog#rollbackDialog QPushButton[role="primary"]:hover {
                    background: #2997ff;
                    border-color: #2997ff;
                }
                QDialog#rollbackDialog QPushButton[role="danger"] {
                    color: #ffb3b8;
                    border-color: #5c2c31;
                    background: #2f1f22;
                }
                QDialog#rollbackDialog QPushButton[role="danger"]:hover {
                    background: #43262b;
                    border-color: #7a343b;
                }
                QDialog#rollbackDialog QPushButton#rowDeleteButton {
                    min-width: 68px;
                    min-height: 26px;
                    padding: 4px 10px;
                }
                """
            )

            result = dialog.exec() if hasattr(dialog, "exec") else dialog.exec_()
            if result == 0:
                return None
            item = list_widget.currentItem()
            if item is None:
                return None
            selected = item.data(0x0100)
            if not isinstance(selected, dict):
                return None
            return selected

        def action_connect(self) -> None:
            if self.remote_session is not None:
                self.disconnect_remote_session()
                self.append_log("Disconnected")
                return
            conn = self.connection()
            self.connection_status.setText(f"Connecting: {conn.host}")
            self.connection_status.setStyleSheet("color: #0a84ff;")
            self.clear_input_selection()
            self.run_task("Connect", lambda log: self.connect_remote_session(conn, log))

        def action_build(self) -> None:
            if not self.confirm_action(
                "Confirm Build",
                "Build the local linux/amd64 binary?",
                f"Output: {default_binary_path()}",
            ):
                return
            self.run_task("Build linux/amd64", lambda log: build_linux_amd64(default_binary_path(), self.skip_tests.isChecked(), log))

        def action_install(self) -> None:
            conn = self.target_connection()
            detail_lines = [
                f"Host: {conn.host}",
                f"Site: {self.site.text().strip() or DEFAULT_SITE}",
            ]
            if self.master_key.text().strip():
                detail_lines.append("Master key: set after install")
            if not self.confirm_action(
                "Confirm Install",
                "Build and install the service on the remote host?",
                "\n".join(detail_lines),
            ):
                return
            self.show_connecting_if_needed(conn)
            self.run_task(
                "Build + install",
                lambda log: install_with_session(self.remote_for_action(log), self.install_options(), skip_tests=self.skip_tests.isChecked(), log=log),
            )

        def action_master_key_set(self) -> None:
            if not self.master_key.text().strip():
                self.append_log("Failed: master_key is required")
                return
            conn = self.target_connection()
            detail = f"Host: {conn.host}\nConfig file: {self.config_file.text().strip() or DEFAULT_CONFIG_FILE}\nRestart app: yes"
            if not self.confirm_action("Confirm Master Key", "Set config master_key on the remote host?", detail):
                return
            self.show_connecting_if_needed(conn)
            self.run_task("Set master_key", lambda log: set_master_key_with_session(self.remote_for_action(log), self.master_key_options(), log=log))

        def action_upgrade(self) -> None:
            conn = self.target_connection()
            if not self.confirm_action(
                "Confirm Upgrade",
                "Build and upgrade the installed service on the remote host?",
                f"Host: {conn.host}\nService: {self.service.text().strip() or DEFAULT_SERVICE_NAME}",
            ):
                return
            self.show_connecting_if_needed(conn)
            self.run_task(
                "Build + upgrade",
                lambda log: upgrade_with_session(self.remote_for_action(log), self.service_options(), skip_tests=self.skip_tests.isChecked(), log=log),
            )

        def action_stop(self) -> None:
            conn = self.target_connection()
            detail = f"Host: {conn.host}\nService: {self.service.text().strip() or DEFAULT_SERVICE_NAME}"
            if not self.confirm_action("Confirm Stop", "Stop the active remote service now?", detail):
                return
            self.show_connecting_if_needed(conn)
            self.run_task("Stop service", lambda log: stop_with_session(self.remote_for_action(log), self.service_options(), log=log))

        def action_start(self) -> None:
            conn = self.target_connection()
            detail = f"Host: {conn.host}\nService: {self.service.text().strip() or DEFAULT_SERVICE_NAME}"
            if not self.confirm_action("Confirm Start", "Start the active remote service now?", detail):
                return
            self.show_connecting_if_needed(conn)
            self.run_task("Start service", lambda log: start_with_session(self.remote_for_action(log), self.service_options(), log=log))

        def action_rollback(self) -> None:
            conn = self.target_connection()
            self.show_connecting_if_needed(conn)
            self.append_log("Loading remote rollback versions...")
            try:
                payload = list_rollback_versions_with_session(
                    self.remote_for_action(self.append_log),
                    self.service_options(),
                    log=self.append_log,
                )
            except Exception as exc:  # noqa: BLE001 - show remote inspection failures in the GUI.
                self.append_log(f"Failed: {exc}")
                self.refresh_connection_ui()
                return
            self.refresh_connection_ui()
            versions = payload.get("versions", [])
            if not isinstance(versions, list):
                self.append_log("Failed: remote rollback version list is invalid")
                return
            version_items = [item for item in versions if isinstance(item, dict)]
            if not version_items:
                current = str(payload.get("current_release") or "-")
                self.append_log(f"Failed: no rollback versions found. Current release: {current}")
                return
            self.append_log(f"Found {len(version_items)} rollback version(s)")
            selected = self.select_rollback_version(
                version_items,
                lambda release: delete_rollback_version_with_session(
                    self.remote_for_action(self.append_log),
                    self.service_options(),
                    release,
                    log=self.append_log,
                ),
            )
            if not selected:
                self.append_log("Rollback cancelled")
                return
            release = str(selected.get("release") or "").strip()
            conn = self.target_connection()
            detail_lines = [
                f"Host: {conn.host}",
                f"Current release: {payload.get('current_release') or '-'}",
                f"Rollback release: {release}",
            ]
            backup_path = str(selected.get("backup_path") or "").strip()
            if backup_path:
                detail_lines.append(f"Backup: {backup_path}")
            if not self.confirm_action("Confirm Rollback", "Rollback the remote service to the selected version?", "\n".join(detail_lines)):
                return
            self.show_connecting_if_needed(conn)
            self.run_task(
                "Rollback",
                lambda log: rollback_with_session(self.remote_for_action(log), self.service_options(), release, log=log),
            )

        def action_caddy_set(self) -> None:
            if not self.site.text().strip():
                self.append_log("Failed: Caddy site is required")
                return
            conn = self.target_connection()
            if not self.confirm_action(
                "Confirm Caddy Update",
                "Write the Caddy reverse proxy and reload the service?",
                f"Host: {conn.host}\nSite: {self.site.text().strip()}",
            ):
                return
            self.show_connecting_if_needed(conn)
            self.run_task("Apply Caddy", lambda log: caddy_set_with_session(self.remote_for_action(log), self.caddy_options(), log=log))

        def action_caddy_show(self) -> None:
            self.show_connecting_if_needed(self.target_connection())
            options = self.caddy_options()
            options.site = ""
            self.run_task("Caddy Show", lambda log: caddy_show_with_session(self.remote_for_action(log), options, log=log))

        def action_uninstall(self) -> None:
            conn = self.target_connection()
            install_dir = self.install_dir.text().strip() or DEFAULT_INSTALL_DIR
            detail = f"Host: {conn.host}\nInstall dir: {install_dir}\n\nThis will delete the install, config, and data directories."
            if not self.confirm_action(
                "Confirm Uninstall",
                "Delete all managed files while uninstalling?",
                detail,
            ):
                return
            self.show_connecting_if_needed(conn)
            options = self.service_options()
            options.keep_files = False
            self.run_task("Uninstall", lambda log: uninstall_with_session(self.remote_for_action(log), options, log=log))

        def action_status(self) -> None:
            self.show_connecting_if_needed(self.target_connection())
            self.run_task("Status", lambda log: status_with_session(self.remote_for_action(log), self.service_options(), log=log))

    configure_qt_rendering()
    app = QtWidgets.QApplication(sys.argv)
    apply_app_font(app)
    window = DeployWindow()
    window.show()
    return app.exec() if hasattr(app, "exec") else app.exec_()


def main() -> int:
    return run_cli(sys.argv[1:])


if __name__ == "__main__":
    raise SystemExit(main())
