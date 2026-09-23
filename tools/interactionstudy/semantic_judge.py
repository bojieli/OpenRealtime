#!/usr/bin/env python3
"""Local semantic judge over heard assistant speech in each scoring window.

For every non-silent variant, the feedback branch and its muted control are
judged against the same goal, using independent ASR of the window crop (what
was audible), never generated text. A pair counts only when the feedback
branch meets the goal and the control does not. The judge is a local LLM, and
its identity is recorded; if it is the policy backbone, that is a declared
limitation. It is a development scorer, not human rating or a frozen metric.
"""
import argparse
import base64
import hashlib
import os
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import wave

HERE = Path(__file__).resolve().parent
SYSTEM = ("You evaluate one assistant turn in a live spoken conversation. You receive what the user "
          "said, the goal the assistant's speech should meet in a fixed time window, and a speech "
          "recognition transcript of everything the assistant audibly said in that window. Recognition "
          "may misspell words. The window can begin with the end of speech that was already under way "
          "before the user spoke; that interrupted fragment is expected and neither meets nor fails the "
          "goal by itself. Judge what the assistant says after it. Judge only the transcript: an "
          "acknowledgement, a promise, a question or a greeting does not meet a goal that asks for content. "
          "Do not credit content the goal forbids. Reply with JSON only: {\"meets\": true|false, \"quote\": \"words from the transcript "
          "that decide it, or empty\"}.")


def words(events):
    return ' '.join(e['text'] for e in events if e.get('text'))


def goal(pair, variant):
    expect = variant['expect']
    lines = [f"User, before the window: {words(pair['prefix'])}",
             f"User, just before the window opens: {words(variant['events'])}",
             f"Assistant instructions: {pair['instructions']}",
             f"Goal for the assistant's speech in the window: {variant['label']}."]
    if expect.get('require_any_of'):
        groups = '; '.join(' / '.join(g) for g in expect['require_any_of'])
        lines.append(f"Content that would show it (ideas, not exact words): {groups}.")
    if expect.get('forbid'):
        lines.append(f"It must not say: {', '.join(expect['forbid'])}.")
    return '\n'.join(lines)


def ask(url, model, goal_text, transcript):
    body = {'model': model, 'temperature': 0, 'seed': 1729, 'max_tokens': 3000,
            'chat_template_kwargs': {'enable_thinking': True},
            'messages': [{'role': 'system', 'content': SYSTEM},
                         {'role': 'user', 'content': goal_text + '\n\nAssistant transcript in the window: '
                          + json.dumps(transcript or '')}]}
    request = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=300) as response:
        content = json.load(response)['choices'][0]['message']['content'] or ''
    found = re.findall(r'\{[^{}]*"meets"[^{}]*\}', content)
    if not found:
        raise ValueError('judge returned no verdict: ' + content[-200:])
    verdict = json.loads(found[-1])
    if not isinstance(verdict.get('meets'), bool):
        raise ValueError('judge verdict lacks boolean meets')
    return verdict


AUDIO_SYSTEM = SYSTEM.replace(
    "and a speech recognition transcript of everything the assistant audibly said in that window. "
    "Recognition may misspell words.",
    "and an audio recording of the assistant's channel during that window: everything it audibly "
    "said, and silence where it said nothing.").replace(
    "Judge only the transcript:", "Judge only what is audible:").replace(
    '"words from the transcript that decide it, or empty"', '"the words you heard that decide it, or empty"')

GEMINI_URL = 'https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent'


def ask_gemini(model, goal_text, heard=None, audio=None, retries=4):
    """One Gemini judgement from a transcript or from the window audio itself.
    The API key is read from GEMINI_API_KEY and never recorded."""
    key = os.environ.get('GEMINI_API_KEY')
    if not key:
        raise RuntimeError('GEMINI_API_KEY is not set')
    if audio is not None:
        system, parts = AUDIO_SYSTEM, [{'text': goal_text + '\n\nThe audio is the assistant channel in the window.'},
                                        {'inline_data': {'mime_type': 'audio/wav',
                                                         'data': base64.b64encode(Path(audio).read_bytes()).decode()}}]
    else:
        system, parts = SYSTEM, [{'text': goal_text + '\n\nAssistant transcript in the window: ' + json.dumps(heard or '')}]
    body = {'systemInstruction': {'parts': [{'text': system}]},
            'contents': [{'role': 'user', 'parts': parts}],
            'generationConfig': {'temperature': 0, 'responseMimeType': 'application/json',
                                 'responseSchema': {'type': 'OBJECT', 'properties': {
                                     'meets': {'type': 'BOOLEAN'}, 'quote': {'type': 'STRING'}},
                                     'required': ['meets', 'quote']}}}
    request = urllib.request.Request(GEMINI_URL.format(model=model), json.dumps(body).encode(),
                                     {'Content-Type': 'application/json', 'x-goog-api-key': key})
    for attempt in range(retries + 1):
        try:
            with urllib.request.urlopen(request, timeout=300) as response:
                data = json.load(response)
            break
        except urllib.error.HTTPError as exc:
            if exc.code not in (429, 500, 502, 503, 504) or attempt == retries:
                raise
            time.sleep(2 ** attempt * 5)
    text = ''.join(p.get('text', '') for p in data['candidates'][0]['content']['parts'])
    verdict = json.loads(text)
    if not isinstance(verdict.get('meets'), bool):
        raise ValueError('judge verdict lacks boolean meets')
    verdict['model_version'] = data.get('modelVersion')
    return verdict


def judge(args, goal_text, heard=None, audio=None):
    """Dispatch to the declared backend; audio mode requires Gemini."""
    if args.backend == 'gemini':
        return ask_gemini(args.model, goal_text, heard=heard, audio=audio)
    if audio is not None:
        raise ValueError('audio judging requires the gemini backend')
    return ask(args.url, args.model, goal_text, heard)


def transcript(wav, endpoint):
    out = wav.with_suffix('.asr.json')
    if not out.exists():
        subprocess.run([sys.executable, str(HERE / 'transcribe_recording.py'), str(wav),
                        '--endpoint', endpoint, '--out', str(out)], check=True, capture_output=True)
    data = json.loads(out.read_text())
    if data.get('error'):
        raise RuntimeError(f'{wav}: recognition failed: {data["error"]}')
    return (data.get('final') or {}).get('text', '')


def pair_verdict(feedback_meets, control_meets):
    if control_meets:
        return 'non-discriminating'
    return 'feedback-only-pass' if feedback_meets else 'no-feedback-pass'


def judge_run(run, args):
    fixtures = {p['id']: p for p in json.loads((run / 'fixtures.json').read_text())}
    manifest = json.loads((run / 'windows' / 'manifest.json').read_text())
    by_branch = {(m['variant_id'], m['file'].rsplit('-', 1)[-1].removesuffix('.wav')): m for m in manifest}
    rows = []
    for (variant_id, repeat), m in sorted(by_branch.items()):
        if variant_id.endswith('-nofeedback'):
            continue
        pair = fixtures[m['pair_id']]
        variant = next(v for v in pair['variants'] if v['id'] == variant_id)
        if variant['expect'].get('silent'):
            continue
        control = by_branch.get((variant_id + '-nofeedback', repeat))
        row = {'run': str(run), 'pair_id': pair['id'], 'variant_id': variant_id, 'repeat': repeat,
               'window_ns': [m['start_ns'], m['end_ns']]}
        if control is None:
            rows.append({**row, 'verdict': 'incomplete-pair'})
            continue
        text = goal(pair, variant)
        for name, item in (('feedback', m), ('control', control)):
            wav = run / 'windows' / item['file']
            if args.mode == 'audio':
                row[name] = {'file': item['file'], **judge(args, text, audio=wav)}
            else:
                heard = transcript(wav, args.asr)
                row[name] = {'file': item['file'], 'transcript': heard, **judge(args, text, heard=heard)}
        row['goal'] = text
        row['verdict'] = pair_verdict(row['feedback']['meets'], row['control']['meets'])
        rows.append(row)
    return rows


# Known-answer checks: the goal and heard transcripts of confirmed cases, plus
# negatives the judge must reject. A judge that fails these is not used.
VALIDATION = [
    ('sc-02', 'corrected', 'The butter in a pan over medium first melt the olive oil in a pan over medium heat', True),
    ('sc-02', 'corrected', '', False),
    ('sc-02', 'corrected', 'Okay, got it.', False),
    ('sc-02', 'corrected', "Butter in a pan over medium heat. Next add finely chopped onions and saute until they're translucent", False),
    ('sc-02', 'corrected', 'Sure, I will change that.', False),
    ('st-02', 'deepen', 'Certainly the feeding schedule for your sourdough starter is crucial. Feed it every twelve hours: discard half, then add fifty grams each of flour and water.', True),
    ('st-02', 'deepen', 'Now let us move on to shaping the loaf and preheating the oven.', False),
    ('st-02', 'skip', 'To bake the loaf, preheat your oven with a dutch oven inside, then score the dough and bake it.', True),
    ('st-02', 'skip', 'Continue feeding your starter daily with equal parts flour and water.', False),
    ('pa-03', 'should-correct', "That's unsafe. Adding more acid to neutralize a spill is incorrect.", True),
    ('pa-03', 'should-correct', 'What lab safety instructions are you reviewing?', False),
    # Boundary cases added with the interrupted-fragment rule (2026-09-23):
    # the rule must not let a leading fragment carry an empty turn.
    ('sc-02', 'corrected', 'The butter in a pan over medium. Okay, sure.', False),
    ('st-02', 'skip', 'Feed it once a day with equal. To bake the loaf, preheat the oven and score the dough.', True),
    # Real window ASR from the 2026-09-23 fresh-clone campaign, labelled by a
    # single-rater review against each muted control.
    ('ov-03', 'before-playing', "Or a remote destination considering the need for a direct flight from Denver, "
     "let's explore options that meet this requirement", False),
    ('st-01', 'skip', 'To connect to once it knows the server, it establishes a connection using either HTTP or HTTPS protocols', True),
    ('st-01', 'skip', "Address is then used to locate the server where the website's files are stored. Once the "
     "server is located, it retrieves the website's files and sends them", False),
    ('st-03', 'deepen', 'Is using a refrigerant cycle. The refrigerant cycle starts with a compressor that pressurizes', True),
    ('st-03', 'deepen', 'This process is similar to how a refrigerator works but in reverse. This process is similar to', False),
]


def validate(pairs, args):
    """Text cases always; audio mode adds labelled window recordings and
    generated silence, because an audio judge must hear, not read."""
    results = []
    for pair_id, variant_id, heard, expected in VALIDATION:
        pair = pairs[pair_id]
        variant = next(v for v in pair['variants'] if v['id'] == variant_id)
        verdict = judge(args, goal(pair, variant), heard=heard)
        results.append({'mode': 'transcript', 'pair_id': pair_id, 'variant_id': variant_id, 'transcript': heard,
                        'expected': expected, **verdict, 'correct': verdict['meets'] == expected})
    if args.mode == 'audio':
        cases = json.loads(args.audio_validation.read_text()) if args.audio_validation else []
        silence = args.out / 'validation-silence.wav'
        with wave.open(str(silence), 'wb') as w:
            w.setparams((1, 2, 24000, 0, 'NONE', 'not compressed'))
            w.writeframes(bytes(2 * 24000 * 9))
        cases.append({'wav': str(silence), 'pair_id': 'sc-02', 'variant_id': 'corrected', 'expected': False,
                      'label': 'generated silence'})
        for case in cases:
            pair = pairs[case['pair_id']]
            variant = next(v for v in pair['variants'] if v['id'] == case['variant_id'])
            verdict = judge(args, goal(pair, variant), audio=case['wav'])
            results.append({'mode': 'audio', **case, **verdict, 'correct': verdict['meets'] == case['expected']})
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path, help='campaign directory containing per-pair runs')
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--backend', choices=['local', 'gemini'], default='local')
    parser.add_argument('--mode', choices=['transcript', 'audio'], default='transcript')
    parser.add_argument('--url', default='http://127.0.0.1:9100/v1/chat/completions')
    parser.add_argument('--model', default='qwen3-8b')
    parser.add_argument('--asr', default='http://127.0.0.1:9110')
    parser.add_argument('--runs', default='*', help='glob of run directories under root')
    parser.add_argument('--audio-validation', type=Path, help='JSON list of {wav, pair_id, variant_id, expected, label}')
    parser.add_argument('--min-validation-accuracy', type=float, default=1.0,
                        help='declared threshold; failures below 1.0 are recorded and every pass needs review')
    args = parser.parse_args()
    args.out.mkdir()
    runs = sorted(p.parent for p in args.root.glob(args.runs + '/fixtures.json'))
    pairs = {}
    for run in runs:
        pairs.update({p['id']: p for p in json.loads((run / 'fixtures.json').read_text())})
    record = {'scope': 'development semantic judge; not human rating or frozen scoring',
              'backend': args.backend, 'mode': args.mode, 'judge_model': args.model,
              'judge_is_policy_backbone': args.backend == 'local' and args.model == 'qwen3-8b',
              'asr': args.asr if args.mode == 'transcript' else None,
              'source_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), 'runs': [str(r) for r in runs]}
    began = time.monotonic()
    record['validation'] = validate(pairs, args)
    record['validation_passed'] = all(v['correct'] for v in record['validation'])
    record['validation_accuracy'] = sum(v['correct'] for v in record['validation']) / len(record['validation'])
    record['min_validation_accuracy'] = args.min_validation_accuracy
    record['validation_failures'] = [v for v in record['validation'] if not v['correct']]
    (args.out / 'judge.json').write_text(json.dumps(record, indent=2) + '\n')
    if record['validation_accuracy'] < args.min_validation_accuracy:
        raise SystemExit('judge below the declared known-answer accuracy; see judge.json')
    rows = []
    with (args.out / 'judgments.jsonl').open('x') as log:
        for run in runs:
            for row in judge_run(run, args):
                rows.append(row)
                log.write(json.dumps(row) + '\n')
                log.flush()
                print(row['pair_id'], row['variant_id'], row['verdict'], flush=True)
    counts = {}
    for row in rows:
        counts[row['verdict']] = counts.get(row['verdict'], 0) + 1
    record.update(verdicts=counts, judged_variants=len(rows), duration_s=time.monotonic() - began)
    (args.out / 'judge.json').write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps(counts))


if __name__ == '__main__':
    main()
