"""The PowerShell an installer test may use on this host.

Windows PowerShell 5.1 runs the fixtures' own semantics and runs only on a
Windows host. Under WSL, the powershell.exe on PATH is the Windows interpreter
reached through interop; it cannot open the Linux temporary paths these tests
write, so it never counts. A native pwsh (PowerShell 7) counts for tests that
are valid under it, such as parsing a script.
"""
import os
import platform
import shutil


def windows_powershell():
    """Windows PowerShell on a Windows host, otherwise None."""
    return shutil.which("powershell.exe") if os.name == "nt" else None


def native_pwsh():
    """A pwsh this host runs natively, otherwise None."""
    path = shutil.which("pwsh")
    if path and os.name != "nt" and (path.startswith("/mnt/") or path.lower().endswith(".exe")):
        return None
    return path


def parsing_powershell():
    """A PowerShell whose parser a test may use: Windows PowerShell on Windows, else a native pwsh."""
    return windows_powershell() or native_pwsh()


WINDOWS_ONLY = "Windows PowerShell 5.1 on a Windows host is required; this host is %s" % platform.system()
PARSER_ONLY = ("a PowerShell parser is required: Windows PowerShell on Windows or a native pwsh elsewhere; "
               "this %s host has neither" % platform.system())
PWSH_ONLY = "a native pwsh (PowerShell 7) is required; this %s host has none" % platform.system()
