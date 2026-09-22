# Public rerun with audio credits

The transport delivered 164.24 seconds of PCM without the previous keepalive
failure. The model emitted roughly 436 words across the initial response and
follow-up. The first FDB conversation exceeded its three-minute deadline;
this is a failed measurement, not a passing interruption score. Subsequent
connections were refused because the prior model session was still occupied.

The adapter now signals closure as soon as the protocol reader sees EOF or
BYE, before the base class joins the response worker. A regression test proves
that EOF unblocks a pending response promptly. This addresses an observed
shutdown ordering risk; the complete GPU disconnect/reconnect path still needs
validation, including ASR and synthesis cleanup. The timeout result remains
retained, and neither response truncation nor a larger benchmark timeout has
been used to turn it into a pass.
