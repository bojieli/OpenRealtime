"""Whisper large-v3-turbo behind the transcription API the runtime already speaks.

SenseVoice was the recogniser, and it is fast and small and cannot say
"capybara". Round-tripped through this project's own synthesiser, "A capybara
wandered over and sat down next to me" came back as "A ki bara wandered over
and SAT down next to me", and in the live pipeline as "A cap borroworer" and
"A capy borroworough1". A scenario about counting animals cannot be measured
through a recogniser that deletes the animal, and neither can a phone call
about a person's name.

Turbo is both more accurate and faster here: 84 to 104 milliseconds against
SenseVoice's 271 on the same clips, because it decodes with a fraction of the
layers of full large-v3.

The wire contract is OpenAI's transcription route, which is what the runtime
already talks, so switching recognisers is a URL and not a code change.
"""

import argparse
import atexit
import ctypes
import errno
import fcntl
import hashlib
import importlib.machinery
import io
import json
import os
import pathlib
import shutil
import signal
import stat
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SERVICE_MODEL_ID = "whisper-turbo"
SERVICE_LOGICAL_ID = "memfd://openrealtime-whisper-service-v2"
MAXIMUM_MODEL_FILES = 200_000
MAXIMUM_MODEL_BYTES = 128 << 30
MAXIMUM_DEPENDENCY_DIRECTORIES = 100_000

# Every non-stdlib top-level module reached by faster-whisper import, model
# construction, and the warmed VAD path in the reviewed deployment. The host
# independently resolves and hashes the same ordered selection. The surrounding
# dependency roots are watched in full, so native sibling libraries and any
# future transitive import cannot change through load or runtime unnoticed.
WHISPER_DEPENDENCY_MODULES = (
    "81d243bd2c585b0f4821__mypyc", "PIL", "_brotli", "_cffi_backend",
    "av", "brotli", "certifi", "cffi", "chardet", "charset_normalizer",
    "coloredlogs", "ctranslate2", "defusedxml", "dill", "faster_whisper",
    "filelock", "flatbuffers", "flint", "fsspec", "hf_xet",
    "huggingface_hub", "humanfriendly", "idna", "jinja2", "markupsafe",
    "mpmath", "numpy", "onnxruntime", "packaging", "pynvml", "regex",
    "requests", "safetensors", "socks", "sympy", "tokenizers", "torch",
    "torchgen", "tqdm", "transformers", "typing_extensions", "urllib3",
    "yaml",
)

IN_MODIFY = 0x00000002
IN_ATTRIB = 0x00000004
IN_CLOSE_WRITE = 0x00000008
IN_MOVED_FROM = 0x00000040
IN_MOVED_TO = 0x00000080
IN_CREATE = 0x00000100
IN_DELETE = 0x00000200
IN_DELETE_SELF = 0x00000400
IN_MOVE_SELF = 0x00000800
IN_UNMOUNT = 0x00002000
IN_Q_OVERFLOW = 0x00004000
MODEL_MUTATION_EVENTS = (
    IN_MODIFY | IN_ATTRIB | IN_CLOSE_WRITE | IN_MOVED_FROM | IN_MOVED_TO |
    IN_CREATE | IN_DELETE | IN_DELETE_SELF | IN_MOVE_SELF | IN_UNMOUNT |
    IN_Q_OVERFLOW
)


def _digest_field(hasher, name, value):
    name_bytes = name.encode("utf-8")
    value_bytes = value.encode("utf-8")
    hasher.update(str(len(name_bytes)).encode("ascii"))
    hasher.update(b":")
    hasher.update(name_bytes)
    hasher.update(str(len(value_bytes)).encode("ascii"))
    hasher.update(b":")
    hasher.update(value_bytes)


def loaded_model_digest(path):
    """Return the exact cross-language digest independently checked by the host."""
    root, files = _model_files(path)
    manifest = hashlib.sha256()
    _digest_field(manifest, "format", "openrealtime.loaded-model.v1")
    for relative_path, candidate, resolved in files:
        relative = relative_path.as_posix()
        size = resolved.stat().st_size
        content = hashlib.sha256()
        with resolved.open("rb") as source:
            while True:
                chunk = source.read(1024 * 1024)
                if not chunk:
                    break
                content.update(chunk)
        if resolved.stat().st_size != size:
            raise RuntimeError(f"model material changed while hashing: {candidate}")
        _digest_field(manifest, "file", relative)
        _digest_field(manifest, "size", str(size))
        _digest_field(manifest, "sha256", "sha256:" + content.hexdigest())
    return "sha256:" + manifest.hexdigest()


def file_digest(path):
    content = hashlib.sha256()
    with open(path, "rb") as source:
        while True:
            chunk = source.read(1024 * 1024)
            if not chunk:
                break
            content.update(chunk)
    return "sha256:" + content.hexdigest()


def dependency_material_digest(roots):
    """Digest the exact ordered non-stdlib runtime selection without importing it."""
    canonical_roots = canonical_dependency_roots(roots)
    manifest = hashlib.sha256()
    _digest_field(manifest, "format", "openrealtime.whisper-dependencies.v1")
    for name in WHISPER_DEPENDENCY_MODULES:
        spec = importlib.machinery.PathFinder.find_spec(name, list(canonical_roots))
        if spec is None:
            raise RuntimeError(f"Whisper dependency module is unavailable: {name}")
        if spec.submodule_search_locations:
            locations = list(spec.submodule_search_locations)
            if len(locations) != 1:
                raise RuntimeError(f"Whisper dependency module is ambiguous: {name}")
            module_root, files = _dependency_files(locations[0], canonical_roots)
        else:
            if not spec.origin:
                raise RuntimeError(f"Whisper dependency module has no origin: {name}")
            module_root, files = _dependency_files(spec.origin, canonical_roots)
        if not any(_path_within(module_root, pathlib.Path(root)) for root in canonical_roots):
            raise RuntimeError(f"Whisper dependency escaped its selected roots: {name}")
        _digest_field(manifest, "module", name)
        for relative_path, _candidate, resolved in files:
            size = resolved.stat().st_size
            _digest_field(manifest, "file", relative_path.as_posix())
            _digest_field(manifest, "size", str(size))
            _digest_field(manifest, "sha256", file_digest(resolved))
    return "sha256:" + manifest.hexdigest()


def _dependency_files(path, roots):
    """Inventory only canonical, single-name files beneath the selected roots."""
    lexical_root = pathlib.Path(path)
    if not lexical_root.is_absolute() or str(lexical_root) != os.path.normpath(str(lexical_root)):
        raise RuntimeError("Whisper dependency path is not canonical and absolute")
    try:
        root_info = lexical_root.lstat()
        resolved_root = lexical_root.resolve(strict=True)
    except OSError as error:
        raise RuntimeError("Whisper dependency path is unavailable") from error
    if resolved_root != lexical_root or stat.S_ISLNK(root_info.st_mode):
        raise RuntimeError("Whisper dependency path is an alias")
    canonical_roots = tuple(pathlib.Path(value) for value in roots)
    if not any(_path_within(resolved_root, root) for root in canonical_roots):
        raise RuntimeError("Whisper dependency path escaped its selected roots")

    if stat.S_ISREG(root_info.st_mode):
        if root_info.st_nlink != 1:
            raise RuntimeError("Whisper dependency file has an external hard link")
        return resolved_root.parent, [(pathlib.Path(resolved_root.name), resolved_root, resolved_root)]
    if not stat.S_ISDIR(root_info.st_mode):
        raise RuntimeError("Whisper dependency path is not regular")

    files = []
    total = 0
    for current, directories, names in os.walk(resolved_root, topdown=True, followlinks=False):
        current_path = pathlib.Path(current)
        if current_path.resolve(strict=True) != current_path:
            raise RuntimeError("Whisper dependency directory is an alias")
        retained_directories = []
        for name in directories:
            if name == "__pycache__":
                continue
            directory = current_path / name
            info = directory.lstat()
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or directory.resolve(strict=True) != directory:
                raise RuntimeError("Whisper dependency directory is an alias")
            retained_directories.append(name)
        directories[:] = retained_directories
        for name in names:
            if name.endswith(".pyc"):
                continue
            candidate = current_path / name
            info = candidate.lstat()
            if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_nlink != 1:
                raise RuntimeError("Whisper dependency file is aliased")
            resolved = candidate.resolve(strict=True)
            if resolved != candidate or not any(
                    _path_within(resolved, root) for root in canonical_roots):
                raise RuntimeError("Whisper dependency file escaped its selected roots")
            total += info.st_size
            if len(files) >= MAXIMUM_MODEL_FILES or total > MAXIMUM_MODEL_BYTES:
                raise RuntimeError("Whisper dependency material exceeds its bounded inventory")
            files.append((candidate.relative_to(resolved_root), candidate, resolved))
    if not files:
        raise RuntimeError("Whisper dependency path is empty")
    files.sort(key=lambda item: item[0].as_posix())
    return resolved_root, files


class DependencySourceLoader(importlib.machinery.SourceFileLoader):
    """Compile selected source directly; never consume a pre-existing .pyc."""

    def get_code(self, fullname):
        source_path = self.get_filename(fullname)
        source = self.get_data(source_path)
        return self.source_to_code(source, source_path)


def install_dependency_import_guard(roots):
    """Install the exact source/extension-only finder before dependency import."""
    canonical_roots = tuple(pathlib.Path(value) for value in canonical_dependency_roots(roots))
    for cached in tuple(sys.path_importer_cache):
        try:
            cached_path = pathlib.Path(cached).resolve(strict=True)
        except (OSError, RuntimeError):
            continue
        if any(_path_within(cached_path, root) for root in canonical_roots):
            sys.path_importer_cache.pop(cached, None)
    source_only = importlib.machinery.FileFinder.path_hook(
        (importlib.machinery.ExtensionFileLoader, importlib.machinery.EXTENSION_SUFFIXES),
        (DependencySourceLoader, importlib.machinery.SOURCE_SUFFIXES),
    )
    sys.path_hooks.insert(0, source_only)
    sys.dont_write_bytecode = True


def verify_loaded_dependency_modules(roots):
    """Refuse a loaded dependency whose top-level package was not reviewed."""
    canonical_roots = tuple(pathlib.Path(value) for value in canonical_dependency_roots(roots))
    allowed = set(WHISPER_DEPENDENCY_MODULES)
    for name, module in tuple(sys.modules.items()):
        origin = getattr(module, "__file__", None)
        if not origin or not os.path.isabs(origin):
            continue
        lexical = pathlib.Path(origin)
        if not any(_path_within(lexical, root) for root in canonical_roots):
            continue
        try:
            path = lexical.resolve(strict=True)
        except OSError as error:
            raise RuntimeError(f"loaded Whisper dependency disappeared: {name}") from error
        if not any(_path_within(path, root) for root in canonical_roots):
            raise RuntimeError(f"loaded Whisper dependency escaped its selected roots: {name}")
        if name.split(".", 1)[0] not in allowed:
            raise RuntimeError(f"loaded Whisper dependency was not reviewed: {name}")


def _path_within(path, root):
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def sealed_service_digest(path):
    target = os.readlink(path)
    if target != "/memfd:openrealtime-whisper-service-v2 (deleted)":
        raise RuntimeError("Whisper service handle is not the reviewed memfd")
    handle = os.open(path, os.O_RDONLY | os.O_CLOEXEC)
    try:
        seals = fcntl.fcntl(handle, fcntl.F_GET_SEALS)
        required = (fcntl.F_SEAL_SEAL | fcntl.F_SEAL_SHRINK |
                    fcntl.F_SEAL_GROW | fcntl.F_SEAL_WRITE)
        if seals & required != required:
            raise RuntimeError("Whisper service handle is not write-sealed")
        os.lseek(handle, 0, os.SEEK_SET)
        content = hashlib.sha256()
        with os.fdopen(os.dup(handle), "rb", closefd=True) as source:
            while True:
                chunk = source.read(1024 * 1024)
                if not chunk:
                    break
                content.update(chunk)
        return "sha256:" + content.hexdigest()
    finally:
        os.close(handle)


def service_module_identity():
    if __file__ == "/proc/self/fd/3":
        if not sys.flags.isolated or not sys.flags.no_site or not sys.flags.no_user_site:
            raise RuntimeError("Whisper sealed service did not start in isolated no-site mode")
        process_handle = f"/proc/{os.getpid()}/fd/3"
        return SERVICE_LOGICAL_ID, sealed_service_digest(process_handle)
    path = pathlib.Path(__file__).resolve(strict=True)
    if not path.is_file():
        raise RuntimeError("loaded Whisper service module is not a regular file")
    return str(path), file_digest(path)


def _model_files(path):
    root = pathlib.Path(path).resolve(strict=True)
    if not root.is_dir():
        raise RuntimeError("Whisper model path is not a directory")
    files = []
    total = 0
    for candidate in root.rglob("*"):
        if candidate.is_dir():
            continue
        if candidate.name.endswith(".pyc") or "__pycache__" in candidate.parts:
            continue
        resolved = candidate.resolve(strict=True)
        if not resolved.is_file():
            raise RuntimeError(f"model material is not a regular file: {candidate}")
        size = resolved.stat().st_size
        total += size
        if len(files) >= MAXIMUM_MODEL_FILES or total > MAXIMUM_MODEL_BYTES:
            raise RuntimeError("Whisper model material exceeds its bounded inventory")
        files.append((candidate.relative_to(root), candidate, resolved))
    if not files:
        raise RuntimeError("Whisper model path is empty")
    files.sort(key=lambda item: item[0].as_posix())
    return root, files


def _same_stat(left, right):
    return (
        stat.S_ISREG(left.st_mode) and stat.S_ISREG(right.st_mode) and
        left.st_dev == right.st_dev and left.st_ino == right.st_ino and
        left.st_size == right.st_size and left.st_mtime_ns == right.st_mtime_ns and
        left.st_ctime_ns == right.st_ctime_ns
    )


def _same_path_entry(left, right):
    return (
        left.st_dev == right.st_dev and left.st_ino == right.st_ino and
        left.st_mode == right.st_mode and left.st_size == right.st_size and
        left.st_mtime_ns == right.st_mtime_ns and left.st_ctime_ns == right.st_ctime_ns
    )


def materialize_model(path):
    """Copy the selected snapshot into a private, monitorable load root."""
    source_root, files = _model_files(path)
    campaign = pathlib.Path(f"/tmp/openrealtime-whisper-sealed-{os.getpid()}")
    model_root = campaign / "model"
    campaign.mkdir(mode=0o700)
    model_root.mkdir(mode=0o700)
    try:
        for relative, lexical, resolved in files:
            destination = model_root / relative
            destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            lexical_before = lexical.lstat()
            source = os.open(resolved, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
            try:
                source_before = os.fstat(source)
                target = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o400)
                try:
                    with os.fdopen(os.dup(source), "rb", closefd=True) as reader:
                        with os.fdopen(os.dup(target), "wb", closefd=True) as writer:
                            shutil.copyfileobj(reader, writer, 1024 * 1024)
                            writer.flush()
                            os.fsync(writer.fileno())
                    target_after = os.fstat(target)
                finally:
                    os.close(target)
                source_after = os.fstat(source)
            finally:
                os.close(source)
            lexical_after = lexical.lstat()
            if (not _same_stat(source_before, source_after) or
                    not _same_path_entry(lexical_before, lexical_after) or
                    target_after.st_size != source_before.st_size):
                raise RuntimeError(f"Whisper source changed while materializing: {relative}")
        return str(source_root), str(model_root), str(campaign)
    except Exception:
        shutil.rmtree(campaign, ignore_errors=True)
        raise


class ModelMutationMonitor:
    """Fail permanently if load material changes after its initial copy."""

    def __init__(self, root):
        self.root = pathlib.Path(os.path.abspath(root))
        self.campaign = self.root.parent
        if self.root.name != "model":
            raise RuntimeError("Whisper loaded model path is not the sealed campaign model")
        directory_flags = os.O_RDONLY | os.O_CLOEXEC | os.O_DIRECTORY | os.O_NOFOLLOW
        self._campaign_fd = os.open(self.campaign, directory_flags)
        try:
            self._root_fd = os.open(self.root.name, directory_flags, dir_fd=self._campaign_fd)
        except Exception:
            os.close(self._campaign_fd)
            raise
        self._library = ctypes.CDLL(None, use_errno=True)
        self._fd = self._library.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
        if self._fd < 0:
            os.close(self._root_fd)
            os.close(self._campaign_fd)
            raise OSError(ctypes.get_errno(), "initialize Whisper model mutation monitor")
        self._closed = False
        self._compromised = False
        try:
            directories = [self.campaign, self.root]
            directories.extend(path for path in self.root.rglob("*") if path.is_dir())
            for directory in directories:
                encoded = os.fsencode(directory)
                descriptor = self._library.inotify_add_watch(
                    self._fd, ctypes.c_char_p(encoded), ctypes.c_uint32(MODEL_MUTATION_EVENTS),
                )
                if descriptor < 0:
                    raise OSError(ctypes.get_errno(), f"watch Whisper model directory: {directory}")
            self._check_visible_identities()
        except Exception:
            os.close(self._fd)
            os.close(self._root_fd)
            os.close(self._campaign_fd)
            self._closed = True
            raise

    def _check_visible_identities(self):
        try:
            campaign_visible = os.stat(self.campaign, follow_symlinks=False)
            root_visible = os.stat(self.root, follow_symlinks=False)
            campaign_open = os.fstat(self._campaign_fd)
            root_open = os.fstat(self._root_fd)
        except OSError as error:
            self._compromised = True
            raise RuntimeError("Whisper sealed model campaign is no longer visible") from error
        if (not stat.S_ISDIR(campaign_visible.st_mode) or
                not stat.S_ISDIR(root_visible.st_mode) or
                not os.path.samestat(campaign_visible, campaign_open) or
                not os.path.samestat(root_visible, root_open)):
            self._compromised = True
            raise RuntimeError("Whisper sealed model campaign identity changed")

    def check(self):
        if self._closed or self._compromised:
            raise RuntimeError("Whisper loaded model material is no longer trusted")
        while True:
            try:
                events = os.read(self._fd, 1024 * 1024)
            except BlockingIOError:
                break
            except OSError as error:
                if error.errno == errno.EAGAIN:
                    break
                self._compromised = True
                raise RuntimeError("read Whisper model mutation monitor") from error
            if not events:
                self._compromised = True
                break
            self._compromised = True
        self._check_visible_identities()
        if self._compromised:
            raise RuntimeError("Whisper loaded model material changed after sealing")

    def close(self):
        if not self._closed:
            os.close(self._fd)
            os.close(self._root_fd)
            os.close(self._campaign_fd)
            self._closed = True


class DependencyMutationMonitor:
    """Watch every selected dependency-root directory before any package import."""

    def __init__(self, roots):
        self.roots = tuple(pathlib.Path(value) for value in canonical_dependency_roots(roots))
        directory_flags = os.O_RDONLY | os.O_CLOEXEC | os.O_DIRECTORY | os.O_NOFOLLOW
        self._root_fds = []
        self._fd = -1
        self._closed = False
        self._compromised = False
        try:
            for root in self.roots:
                self._root_fds.append(os.open(root, directory_flags))
            before = self._directory_inventory()
            self._library = ctypes.CDLL(None, use_errno=True)
            self._fd = self._library.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
            if self._fd < 0:
                raise OSError(ctypes.get_errno(), "initialize Whisper dependency mutation monitor")
            for directory in before:
                descriptor = self._library.inotify_add_watch(
                    self._fd, ctypes.c_char_p(os.fsencode(directory)),
                    ctypes.c_uint32(MODEL_MUTATION_EVENTS),
                )
                if descriptor < 0:
                    raise OSError(ctypes.get_errno(), f"watch Whisper dependency directory: {directory}")
            after = self._directory_inventory()
            if before != after:
                raise RuntimeError("Whisper dependency directory set changed while monitoring")
            self._check_visible_roots()
        except Exception:
            self.close()
            raise

    def _directory_inventory(self):
        inventory = {}
        for root in self.roots:
            for current, directories, _files in os.walk(root, topdown=True, followlinks=False):
                directories[:] = [
                    name for name in directories
                    if not pathlib.Path(current, name).is_symlink()
                ]
                info = os.lstat(current)
                if not stat.S_ISDIR(info.st_mode):
                    raise RuntimeError("Whisper dependency directory is not regular")
                inventory[current] = (info.st_dev, info.st_ino, info.st_mode)
                if len(inventory) > MAXIMUM_DEPENDENCY_DIRECTORIES:
                    raise RuntimeError("Whisper dependency directory inventory exceeds its bound")
        return inventory

    def _check_visible_roots(self):
        try:
            visible = [os.stat(root, follow_symlinks=False) for root in self.roots]
            opened = [os.fstat(handle) for handle in self._root_fds]
        except OSError as error:
            self._compromised = True
            raise RuntimeError("Whisper dependency root is no longer visible") from error
        if any(
            not stat.S_ISDIR(left.st_mode) or not os.path.samestat(left, right)
            for left, right in zip(visible, opened)
        ):
            self._compromised = True
            raise RuntimeError("Whisper dependency root identity changed")

    def check(self):
        if self._closed or self._compromised:
            raise RuntimeError("Whisper dependency material is no longer trusted")
        while True:
            try:
                events = os.read(self._fd, 1024 * 1024)
            except BlockingIOError:
                break
            except OSError as error:
                if error.errno == errno.EAGAIN:
                    break
                self._compromised = True
                raise RuntimeError("read Whisper dependency mutation monitor") from error
            if not events:
                self._compromised = True
                break
            self._compromised = True
        self._check_visible_roots()
        if self._compromised:
            raise RuntimeError("Whisper dependency material changed after sealing")

    def close(self):
        if self._closed:
            return
        if self._fd >= 0:
            os.close(self._fd)
        for handle in self._root_fds:
            os.close(handle)
        self._root_fds = []
        self._closed = True


def canonical_dependency_roots(values):
    if not values:
        raise RuntimeError("Whisper requires explicit dependency roots")
    roots = []
    for value in values:
        if not value or "\x00" in value or "\r" in value or "\n" in value:
            raise RuntimeError("Whisper dependency root is invalid")
        lexical = pathlib.Path(value)
        if not lexical.is_absolute() or str(lexical) != os.path.normpath(value):
            raise RuntimeError("Whisper dependency root is not canonical and absolute")
        resolved = lexical.resolve(strict=True)
        if str(resolved) != str(lexical) or not resolved.is_dir():
            raise RuntimeError("Whisper dependency root is not an exact directory")
        if str(resolved) in roots:
            raise RuntimeError("Whisper dependency root is repeated")
        roots.append(str(resolved))
    return tuple(roots)


class Recogniser:
    """Owns the model and serialises access to it.

    One CUDA stream shared by concurrent decodes interleaves badly, and an
    utterance is short enough that queueing costs less than the contention.
    """

    def __init__(self, model, device, compute_type, language, dependency_roots):
        model_path = pathlib.Path(model)
        if not model_path.is_absolute():
            raise RuntimeError("Whisper requires an absolute immutable model snapshot")
        source_path = str(model_path.resolve(strict=True))
        self.revision = pathlib.Path(source_path).name
        if len(self.revision) != 40 or any(character not in "0123456789abcdef"
                                           for character in self.revision):
            raise RuntimeError("Whisper model path is not an immutable snapshot revision")
        self.service_path, self.service_digest = service_module_identity()
        self.device = device
        self.compute_type = compute_type
        self.language = language
        self.dependency_roots = canonical_dependency_roots(dependency_roots)
        self.model = None
        self.monitor = None
        self.dependency_monitor = None
        self._closed = False
        self.source_model_path = source_path
        self.materialized_model_path = ""
        self.materialization_campaign = ""
        try:
            source_path, materialized_path, campaign = materialize_model(source_path)
            self.source_model_path = source_path
            self.materialized_model_path = materialized_path
            self.materialization_campaign = campaign
            self.monitor = ModelMutationMonitor(materialized_path)
            source_digest = loaded_model_digest(source_path)
            boot_model_digest = loaded_model_digest(materialized_path)
            self.monitor.check()
            if source_digest != boot_model_digest:
                raise RuntimeError("Whisper materialized model differs from the selected snapshot")
            # -I -S prevents every implicit site/user/PYTHONPATH hook. Add only
            # the exact roots selected by the launcher and independently hashed
            # by the host; importing a package never processes .pth or
            # sitecustomize files in these directories.
            self.dependency_monitor = DependencyMutationMonitor(self.dependency_roots)
            dependency_digest = dependency_material_digest(self.dependency_roots)
            self.dependency_monitor.check()
            install_dependency_import_guard(self.dependency_roots)
            sys.path.extend(self.dependency_roots)
            from faster_whisper import WhisperModel
            self.model = WhisperModel(materialized_path, device=device, compute_type=compute_type)
            self.lock = threading.Lock()
            # Warm-up is inside the monitored load boundary. This matters for
            # loaders that defer opening weight shards until first inference.
            self.transcribe(silence())
            if loaded_model_digest(materialized_path) != boot_model_digest:
                raise RuntimeError("Whisper model material changed while it was loaded")
            self.monitor.check()
            if dependency_material_digest(self.dependency_roots) != dependency_digest:
                raise RuntimeError("Whisper dependency material changed while it was loaded")
            self.dependency_monitor.check()
            verify_loaded_dependency_modules(self.dependency_roots)
            self.model_digest = boot_model_digest
            self.dependency_digest = dependency_digest
            atexit.register(self.close)
        except Exception:
            self.close()
            raise

    def transcribe(self, audio):
        self.monitor.check()
        self.dependency_monitor.check()
        verify_loaded_dependency_modules(self.dependency_roots)
        with self.lock:
            # Greedy, because this runs on the path between somebody speaking
            # and the agent deciding whether to answer. A beam buys accuracy
            # that a partial re-transcribed three hundred milliseconds later
            # will supersede anyway.
            # transcribe, never translate. Whisper can do both, and told the
            # audio is English when it is not, it produces English - which is
            # translation wearing a recogniser's clothes. Measured, a Mandarin
            # line came back as "Hello. I'm very happy to meet you.", so an
            # agent asked to interpret was handed the interpretation and asked
            # to interpret it again. It said "man. man. man. man."
            #
            # A recogniser that rewrites what somebody said is worse than one
            # that hears them badly, because nothing downstream can tell.
            # A speech gate in front of the decoder, because Whisper does not
            # decline to transcribe. Given pure silence it returns "Thank
            # you." - every time, at every length tried - and a phantom
            # utterance is worse than a missed one: the agent answers
            # something nobody said. Measured in the interpreting scenario,
            # that reached the conversation as the agent apparently saying
            # "Thank you for watching." and as user lines in languages nobody
            # in the room was speaking.
            segments, info = self.model.transcribe(
                io.BytesIO(audio), language=self.language, task="transcribe",
                beam_size=1, condition_on_previous_text=False, vad_filter=True,
            )
            result = "".join(segment.text for segment in segments).strip(), info.language
        self.monitor.check()
        self.dependency_monitor.check()
        verify_loaded_dependency_modules(self.dependency_roots)
        return result

    def warm(self, audio):
        self.transcribe(audio)

    def close(self):
        if self._closed:
            return
        self._closed = True
        if self.monitor is not None:
            self.monitor.close()
        if self.dependency_monitor is not None:
            self.dependency_monitor.close()
        if self.materialization_campaign:
            shutil.rmtree(self.materialization_campaign, ignore_errors=True)


def field(body, boundary, name):
    """Pull one multipart field out without a dependency."""
    marker = f'name="{name}"'.encode()
    for part in body.split(b"--" + boundary):
        if marker not in part:
            continue
        head, _, payload = part.partition(b"\r\n\r\n")
        return payload.rsplit(b"\r\n", 1)[0]
    return None


def handler_for(recogniser):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            if not self.path.rstrip("/").endswith("/audio/transcriptions"):
                self.send_error(404)
                return
            content_type = self.headers.get("Content-Type", "")
            if "boundary=" not in content_type:
                self.send_error(400, "expected multipart/form-data")
                return
            boundary = content_type.split("boundary=", 1)[1].strip().strip('"').encode()
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            audio = field(body, boundary, "file")
            if not audio:
                self.send_error(400, "no file")
                return
            if field(body, boundary, "model") != SERVICE_MODEL_ID.encode():
                self.send_error(400, "wrong model")
                return
            started = time.perf_counter()
            try:
                text, language = recogniser.transcribe(audio)
            except Exception as error:  # noqa: BLE001 - reported, not swallowed
                print(f"transcription failed: {error}", file=sys.stderr, flush=True)
                self.send_error(500, str(error))
                return
            payload = json.dumps({"text": text, "language": language}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            print(f"{(time.perf_counter() - started) * 1000:.0f} ms  {text[:60]!r}", flush=True)

        def do_GET(self):
            if self.path.rstrip("/").endswith("/models"):
                payload = json.dumps({"object": "list", "data": [{"id": SERVICE_MODEL_ID}]}).encode()
            elif self.path.rstrip("/") == "/health":
                started = time.perf_counter()
                try:
                    recogniser.transcribe(silence(0.5))
                except Exception as error:  # noqa: BLE001 - health must fail closed
                    payload = json.dumps({"status": "unhealthy", "error": str(error)}).encode()
                    self.send_response(503)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                    return
                payload = json.dumps({
                    "status": "ok", "model": SERVICE_MODEL_ID,
                    "model_path": recogniser.source_model_path,
                    "materialized_model_path": recogniser.materialized_model_path,
                    "device": recogniser.device, "compute_type": recogniser.compute_type,
                    "language": recogniser.language or "auto",
                    "dependency_roots": list(recogniser.dependency_roots),
                    "dependency_digest": recogniser.dependency_digest,
                    "revision": recogniser.revision, "digest": recogniser.model_digest,
                    "service_path": recogniser.service_path,
                    "service_digest": recogniser.service_digest,
                    "probe_seconds": round(time.perf_counter() - started, 3),
                }, sort_keys=True, separators=(",", ":")).encode()
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    return Handler


def silence(seconds=1.0, rate=16_000):
    import struct
    frames = b"\x00\x00" * int(rate * seconds)
    return (b"RIFF" + struct.pack("<I", 36 + len(frames)) + b"WAVEfmt "
            + struct.pack("<IHHIIHH", 16, 1, 1, rate, rate * 2, 2, 16)
            + b"data" + struct.pack("<I", len(frames)) + frames)


def listener_pid(port):
    want = f"0100007F:{port:04X}"
    inodes = []
    with open("/proc/net/tcp", "r", encoding="ascii") as source:
        for line in source:
            fields = line.split()
            if len(fields) >= 10 and fields[1] == want and fields[3] == "0A":
                inodes.append(fields[9])
    if len(inodes) != 1:
        raise RuntimeError(f"Whisper port {port} has {len(inodes)} exact loopback listeners")
    socket_target = f"socket:[{inodes[0]}]"
    matches = []
    for process in pathlib.Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            descriptors = (process / "fd").iterdir()
            if any(os.readlink(descriptor) == socket_target for descriptor in descriptors):
                matches.append(int(process.name))
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
    if len(matches) != 1:
        raise RuntimeError(f"Whisper listener socket belongs to {len(matches)} processes")
    return matches[0]


def read_health(url):
    with urllib.request.urlopen(url, timeout=15) as response:
        if response.status != 200:
            raise RuntimeError(f"Whisper health returned HTTP {response.status}")
        payload = response.read((1 << 20) + 1)
        if len(payload) > 1 << 20:
            raise RuntimeError("Whisper health response exceeds its bound")
    parsed = json.loads(payload)
    if not isinstance(parsed, dict):
        raise RuntimeError("Whisper health response is not an object")
    return parsed


def verify_existing_listener(arguments):
    """Adopt only the exact already-running service this script would launch."""
    pid = listener_pid(arguments.port)
    _source_service_path, source_service_digest = service_module_identity()
    source_model_path = str(pathlib.Path(arguments.model).resolve(strict=True))
    source_dependency_digest = dependency_material_digest(arguments.dependency_root)
    expected_command = [
        sys.executable, "-I", "-S", "-B", "/proc/self/fd/3",
        "--model", source_model_path, "--device", arguments.device,
        "--compute-type", arguments.compute_type, "--language", arguments.language,
    ]
    for dependency_root in arguments.dependency_root:
        expected_command.extend(("--dependency-root", dependency_root))
    expected_command.extend([
        "--port", str(arguments.port),
    ])
    command = (pathlib.Path(f"/proc/{pid}/cmdline").read_bytes()
               .rstrip(b"\x00").decode().split("\x00"))
    if command != expected_command:
        raise RuntimeError("existing Whisper listener command differs from the strict service")
    if not arguments.working_directory:
        raise RuntimeError("existing Whisper listener verification requires its working directory")
    working_directory = str(pathlib.Path(f"/proc/{pid}/cwd").resolve(strict=True))
    if working_directory != str(pathlib.Path(arguments.working_directory).resolve(strict=True)):
        raise RuntimeError("existing Whisper listener working directory differs")
    if not os.path.samefile(f"/proc/{pid}/exe", sys.executable):
        raise RuntimeError("existing Whisper listener executable differs")
    if sealed_service_digest(f"/proc/{pid}/fd/3") != source_service_digest:
        raise RuntimeError("existing Whisper listener service handle differs")
    materialized_model_path = f"/tmp/openrealtime-whisper-sealed-{pid}/model"
    source_digest = loaded_model_digest(source_model_path)
    materialized_digest = loaded_model_digest(materialized_model_path)
    if source_digest != materialized_digest:
        raise RuntimeError("existing Whisper loaded model differs from its selected snapshot")
    payload = read_health(f"http://127.0.0.1:{arguments.port}/health")
    expected_keys = {
        "status", "model", "model_path", "materialized_model_path", "device",
        "compute_type", "language", "revision", "digest", "service_path",
        "service_digest", "dependency_roots", "dependency_digest", "probe_seconds",
    }
    if (set(payload) != expected_keys or payload.get("status") != "ok" or
            payload.get("model") != SERVICE_MODEL_ID or
            payload.get("model_path") != source_model_path or
            payload.get("materialized_model_path") != materialized_model_path or
            payload.get("device") != arguments.device or
            payload.get("compute_type") != arguments.compute_type or
            payload.get("language") != arguments.language or
            payload.get("dependency_roots") != arguments.dependency_root or
            payload.get("dependency_digest") != source_dependency_digest or
            payload.get("revision") != pathlib.Path(source_model_path).name or
            payload.get("digest") != source_digest or
            payload.get("service_path") != SERVICE_LOGICAL_ID or
            payload.get("service_digest") != source_service_digest or
            not isinstance(payload.get("probe_seconds"), (int, float)) or
            isinstance(payload.get("probe_seconds"), bool) or
            payload.get("probe_seconds") < 0):
        raise RuntimeError("existing Whisper health differs from the strict service")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--device", default="cuda")
    parser.add_argument("--compute-type", default="float16")
    # The Realtime-CU task contract is English. Pinning that reviewed language
    # avoids unstable language detection on its very short spoken cues, and is
    # exposed by /health so profile attestation cannot hide the selection.
    parser.add_argument("--language", default="en")
    parser.add_argument("--dependency-root", action="append", default=[])
    parser.add_argument("--port", type=int, default=8003)
    parser.add_argument("--verify-listener", action="store_true")
    parser.add_argument("--working-directory", default="")
    arguments = parser.parse_args()
    arguments.dependency_root = list(canonical_dependency_roots(arguments.dependency_root))

    if arguments.verify_listener:
        verify_existing_listener(arguments)
        print(f"verified exact Whisper listener on :{arguments.port}", flush=True)
        return
    if arguments.working_directory:
        parser.error("--working-directory is only valid with --verify-listener")

    started = time.perf_counter()
    recogniser = Recogniser(arguments.model, arguments.device,
                            arguments.compute_type, arguments.language or None,
                            arguments.dependency_root)
    print(f"ready on :{arguments.port} after {time.perf_counter() - started:.1f}s", flush=True)
    server = ThreadingHTTPServer(("127.0.0.1", arguments.port), handler_for(recogniser))

    def terminate(_signal, _frame):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, terminate)
    signal.signal(signal.SIGINT, terminate)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        recogniser.close()


if __name__ == "__main__":
    main()
