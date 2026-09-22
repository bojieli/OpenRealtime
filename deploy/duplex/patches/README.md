# VoiceChat runtime diagnostics

`voicechat-boundary-diagnostics.patch` applies to the pinned vLLM-Omni revision
`9005d789033b8c3ec5876a7a68c4e2d9238f5c69`. It logs text/audio frame counters
at model EOS tokens and every 100 text frames when
`VOICECHAT_TRACE_BOUNDARIES=1`. It does not change output or synthesize a
terminal event.

```bash
git -C .runtime/duplex-plan/src/vllm-omni-voicechat apply \
  "$PWD/deploy/duplex/patches/voicechat-boundary-diagnostics.patch"
VOICECHAT_TRACE_BOUNDARIES=1 deploy/duplex/services/voicechat.sh start
```

Stop an existing VoiceChat service before restarting with this environment.
The source setup script intentionally refuses an already patched tree; do not
rerun setup over a diagnostic installation. Record the applied patch with the
result provenance. Remove it with `git apply --reverse` using the same path
when returning to the unmodified serving pin.
