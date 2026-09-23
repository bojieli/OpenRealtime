import copy
import json
from pathlib import Path
import tempfile
import unittest

from native_pair_probe import schedule
from summarize_native_live import summarize


class NativeIdentityTest(unittest.TestCase):
    def test_identity_corruption_and_schema_downgrade(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            word = {'id': 'open', 'group': 'open', 'kind': 'word', 'text': 'Hello',
                    'available_at': 100000000, 'source_start': 0}
            feedback = dict(word, id='feedback', group='feedback', text='Continue',
                            available_at=1000000000, source_start=900000000)
            variant = {'id': 'changed', 'feedback': 'feedback', 'events': [feedback],
                       'expect': {'after': 'feedback', 'within': 1000000000}}
            pair = {'id': 'fixture', 'prefix': [word], 'variants': [variant]}
            (root / 'prepared-pair.json').write_text(json.dumps(pair))
            trial = root / 'changed'
            trial.mkdir()
            identity = {'trace_schema_version': 3, 'session_id': root.name + '/changed',
                        'pair_id': 'fixture', 'variant_id': 'changed', 'cell_id': 'DC-native-diagnostic'}
            result = dict(identity, status='complete', text=[], contexts=[])
            rows = []
            for i, (at, fresh) in enumerate(schedule(pair, variant, words_per_tick=2)):
                rows.append(dict(identity, turn_id=f'decision-{i+1}', admission_s=at/1e9,
                                 fresh_events=fresh, input=' '.join(e['text'] for e in fresh),
                                 events=[], deadline_missed=False, duration_s=.01))
            for mutation in ['none', *identity.keys(), 'turn_id', 'terminal-downgrade']:
                with self.subTest(mutation=mutation):
                    changed = copy.deepcopy(rows)
                    terminal = dict(result)
                    if mutation == 'terminal-downgrade':
                        terminal.pop('trace_schema_version')
                    elif mutation != 'none':
                        changed[-1].pop(mutation)
                    (trial / 'result.json').write_text(json.dumps(terminal))
                    (trial / 'actions.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in changed))
                    branch = summarize(root)['branches'][0]
                    self.assertEqual(bool(branch['admission_violations']), mutation != 'none')
                    self.assertEqual(branch['identity_rows_checked'], len(rows))

    def test_declared_chunk_schedule_and_mismatch(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            words = [dict(id=f'word-{i}', group='open', kind='word', text=str(i),
                          available_at=100000000, source_start=0) for i in range(4)]
            feedback = dict(words[0], id='feedback', group='feedback', text='Continue',
                            available_at=3000000000)
            variant = dict(id='changed', feedback='feedback', events=[feedback],
                           expect=dict(after='feedback', within=1000000000))
            pair = dict(id='fixture', prefix=words, variants=[variant])
            (root / 'prepared-pair.json').write_text(json.dumps(pair))
            trial = root / 'changed'
            trial.mkdir()
            (trial / 'result.json').write_text(json.dumps(dict(
                status='complete', text=[], contexts=[], words_per_tick=1)))
            rows = [dict(admission_s=at/1e9, fresh_events=fresh,
                         input=' '.join(e['text'] for e in fresh), events=[],
                         deadline_missed=False, duration_s=.01)
                    for at, fresh in schedule(pair, variant, words_per_tick=1)]
            (trial / 'actions.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in rows))
            (root / 'manifest.json').write_text(json.dumps(dict(words_per_tick=1)))
            (trial / 'result.json').write_text(json.dumps(dict(
                status='failed', text=[], contexts=[], words_per_tick=1)))
            failed = summarize(root)
            self.assertFalse(failed['execution_complete'])
            self.assertIn('incomplete terminal branch: changed', failed['campaign_violations'])
            (trial / 'result.json').write_text(json.dumps(dict(
                status='complete', text=[], contexts=[], words_per_tick=1)))
            for declared in (1, 2):
                (root / 'manifest.json').write_text(json.dumps(dict(words_per_tick=declared)))
                violations = summarize(root)['branches'][0]['admission_violations']
                self.assertEqual(bool(violations), declared != 1)
                if declared == 2:
                    self.assertTrue(any('schedule' in v for v in violations))
                    self.assertTrue(any('terminal words_per_tick' in v for v in violations))


    def test_missing_and_unexpected_campaign_branches(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'prepared-pair.json').write_text(json.dumps(dict(
                variants=[dict(id='left'), dict(id='right')])) )
            summary = summarize(root)
            self.assertFalse(summary['execution_complete'])
            self.assertEqual(len(summary['campaign_violations']), 4)
            stray = root / 'unrelated'
            stray.mkdir()
            (stray / 'result.json').write_text('{}')
            summary = summarize(root)
            self.assertEqual(len(summary['campaign_violations']), 5)
            self.assertIn('unexpected terminal branch: unrelated', summary['campaign_violations'])
