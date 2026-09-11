"""Install pinned model sources and the official causal checkpoint locally.

Usage: python3 tools/targetvoice/setup.py [--runtime .runtime/targetvoice]
Dependencies: numpy, torch, torchaudio, requests. No system packages modified.
"""

import argparse
import hashlib
import re
import shutil
import subprocess
import zipfile
from pathlib import Path
import requests

SOURCES = {
    "wesep": (
        "https://github.com/REAL-TSE/wesep-real-tse.git",
        "2a540977a348fbaa92e623210505430e2cec608d",
    ),
    "wespeaker": (
        "https://github.com/wenet-e2e/wespeaker.git",
        "8f53b6485d9f88a207bd17e7f8dba899495ec794",
    ),
}


def prepare(runtime, archive=None):
    runtime = Path(runtime).resolve()
    runtime.mkdir(parents=True, exist_ok=True)
    for package, (url, revision) in SOURCES.items():
        source = runtime / (package + "-source")
        if not source.exists():
            subprocess.run(
                ["git", "clone", "--no-checkout", url, str(source)], check=True
            )
        subprocess.run(
            ["git", "-C", str(source), "checkout", "--detach", revision], check=True
        )
        target = runtime / "python" / package
        shutil.copytree(source / package, target, dirs_exist_ok=True)
        # Import only neural modules. Upstream package initializers import CLI
        # downloaders and unrelated optional models, unnecessary for inference.
        (target / "__init__.py").write_text("")
        for license_file in source.glob("LICENSE*"):
            shutil.copy2(license_file, target / license_file.name)
    if archive is None:
        archive = runtime / "checkpoints.zip"
        session = requests.Session()
        page = session.get(
            "https://drive.google.com/uc",
            params={"export": "download", "id": "1M4UqK2A2EeHmQ0pCevYqBgaYn3RvklgC"},
            timeout=30,
        )
        page.raise_for_status()
        params = dict(
            re.findall(
                r'<input type="hidden" name="([^"]+)" value="([^"]+)"', page.text
            )
        )
        if params.get("id") != "1M4UqK2A2EeHmQ0pCevYqBgaYn3RvklgC":
            raise ValueError("unexpected checkpoint download page")
        with session.get(
            "https://drive.usercontent.google.com/download",
            params=params,
            stream=True,
            timeout=60,
        ) as response:
            response.raise_for_status()
            total = 0
            with archive.open("wb") as output:
                for chunk in response.iter_content(1 << 20):
                    total += len(chunk)
                    if total > 1200 * 1024 * 1024:
                        raise ValueError("checkpoint archive exceeds size limit")
                    output.write(chunk)
    with zipfile.ZipFile(archive) as z:
        info = z.getinfo("spk_emb_causal_100/avg_model.pt")
        if info.file_size != 251856016:
            raise ValueError("checkpoint size mismatch")
        checkpoint = z.read(info)
    if hashlib.sha256(checkpoint).hexdigest() != CHECKPOINT_SHA256:
        raise ValueError("checkpoint checksum mismatch")
    (runtime / "avg_model.pt").write_bytes(checkpoint)
    print(runtime)


CHECKPOINT_SHA256 = "42b73219eefefcbba4b5da0dcb89dce292359c9f93b1a951c7b6d196da2155b3"
if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", default=".runtime/targetvoice")
    parser.add_argument("--archive", type=Path)
    args = parser.parse_args()
    prepare(args.runtime, args.archive)
