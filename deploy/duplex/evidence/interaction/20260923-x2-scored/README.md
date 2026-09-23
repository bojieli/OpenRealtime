# X2-Turn scored in cells I0 and I1 under the accepted timing

The work directories were rebuilt from scratch after the 2026-09-23 cleanup:
`endpoint_eval.py prepare / transcribe / score --part smart,text,mono /
score-external --model x2turn / analyze`, and `overlap_eval.py prepare / score
/ score-external / analyze`. The rebuilt baselines reproduce the retained
2026-09-22 references. I0 test AUC at 200 ms: Smart Turn 0.681, LiveKit
0.879, fusion 0.883, each within 0.001 of before (1,196 test points vs 1,197).
I1 AUCs by time after onset are identical.

X2-Turn uses `x2turn_timing.json` (`../20260923-x2-prefix/x2turn_timing-accepted.json`):
frame f is available once (f + 7) × 80 ms of audio exist. That is the accepted
information-causal delay; probabilities can differ from a streaming decoder
by up to 0.12.

| Cell | X2-Turn result |
| --- | --- |
| I0 endpointing, 301 test files | AUC of p(turn_end) at 200 ms silence: **0.502** (Smart Turn on the same points: 0.681). Own argmax rule: 9.0% premature, 83% fall back to the 2 s timer, mean added wait 1,502 ms |
| I1 backchannel vs interruption, 297 events | Upstream barge-in rule (argmax speaking/turn_end): balanced accuracy **0.50**, every interruption missed. Sweep at θ = 0.3 catches 1.5% |

With honest availability, X2-Turn carries no usable endpoint or barge-in
signal at these decision times. A frame describing the silence onset exists
only ~560 ms later, after the 200 ms checkpoint. Its earlier offline numbers
looked better only because they read frames before they could exist.
