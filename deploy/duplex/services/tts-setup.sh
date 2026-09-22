#!/usr/bin/env bash
# Build the per-model Python environments for the duplex-plan TTS services.
#
#   deploy/duplex/services/tts-setup.sh cosyvoice|vibevoice|qwen3|fish
#
# Each model gets its own venv under .runtime/duplex-plan/venvs/ because their
# pins conflict (CosyVoice wants transformers 4.51, Qwen3-TTS 4.57.3). Upstream
# pins of torch 2.3 cannot drive an RTX PRO 6000 (sm_120), so every venv uses
# torch 2.9.1+cu128 and relaxes the upstream torch pins.
set -euo pipefail
ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
TORCH_INDEX=https://download.pytorch.org/whl/cu128
COMMON="fastapi uvicorn[standard] websockets soundfile numpy scipy"

make_venv() {
  local name=$1
  if [ ! -x "$PLAN/venvs/$name/bin/python" ]; then
    uv venv --python 3.11 "$PLAN/venvs/$name"
  fi
  uv pip install --python "$PLAN/venvs/$name/bin/python" torch==2.9.1 torchaudio==2.9.1 --index-url "$TORCH_INDEX"
}

case "${1:?model}" in
  cosyvoice)
    # CosyVoice has no setup.py; the service puts src/CosyVoice and
    # src/Matcha-TTS (its third_party submodule, cloned beside it) on sys.path.
    [ -d "$PLAN/src/Matcha-TTS/matcha" ] || git clone https://github.com/shivammehta25/Matcha-TTS.git "$PLAN/src/Matcha-TTS"
    make_venv cosyvoice
    uv pip install --python "$PLAN/venvs/cosyvoice/bin/python" $COMMON \
      conformer==0.3.2 diffusers==0.29.0 hydra-core==1.3.2 HyperPyYAML==1.2.3 inflect==7.3.1 \
      librosa==0.10.2 omegaconf==2.3.0 onnx onnxruntime-gpu openai-whisper==20250625 \
      "transformers==4.51.3" x-transformers==2.11.24 wetext==0.0.4 modelscope==1.20.0 \
      einops tiktoken regex pyarrow lightning rich rootutils matplotlib gdown wget Cython pyworld "setuptools<81"
    # Upstream pins openai-whisper 20231117, whose sdist needs pkg_resources at
    # build time; only whisper.log_mel_spectrogram is used, so 20250625 is kept.
    # Training-only deps (deepspeed, tensorrt) are skipped; lightning and pyworld
    # stay because the model yaml imports Matcha and the dataset processor.
    "$PLAN/venvs/cosyvoice/bin/python" -c "import torch; print('cosyvoice venv', torch.__version__, torch.cuda.is_available())"
    ;;
  vibevoice)
    make_venv vibevoice
    # The realtime (streaming) model is pinned to transformers 4.51.3 upstream
    # (the [streamingtts] extra). flash-attn has no sm_120 build; SDPA is used.
    uv pip install --python "$PLAN/venvs/vibevoice/bin/python" $COMMON -e "$PLAN/src/VibeVoice[streamingtts]"
    "$PLAN/venvs/vibevoice/bin/python" -c "import torch, vibevoice; print('vibevoice venv', torch.__version__)"
    ;;
  qwen3)
    make_venv qwen3tts
    uv pip install --python "$PLAN/venvs/qwen3tts/bin/python" $COMMON -e "$PLAN/src/Qwen3-TTS"
    "$PLAN/venvs/qwen3tts/bin/python" -c "import torch, qwen_tts; print('qwen3tts venv', torch.__version__)"
    ;;
  fish)
    # Fish S2 Pro reuses the repository's existing fish-speech 2.0 environment
    # (.runtime/fish-env: torch 2.10+cu128, fish_speech editable from
    # .runtime/fish-speech). It only needs the service scaffold's web stack.
    "$ROOT/.runtime/fish-env/bin/python" -c "import fastapi, uvicorn, fish_speech; print('fish env ok')"
    ;;
  *) echo "unknown model $1" >&2; exit 2 ;;
esac
