import importlib.util
import json
from pathlib import Path
import struct
import tempfile
import unittest
import wave

spec = importlib.util.spec_from_file_location('gap_fixture', Path(__file__).with_name('gap_fixture.py'))
gap_fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gap_fixture)

RATE = 1000


def write_wav(path, samples):
    with wave.open(str(path), 'wb') as w:
        w.setparams((1, 2, RATE, 0, 'NONE', 'not compressed'))
        w.writeframes(struct.pack('<%dh' % len(samples), *samples))


def read_wav(path):
    with wave.open(str(path)) as w:
        raw = w.readframes(w.getnframes())
    return list(struct.unpack('<%dh' % (len(raw) // 2), raw))


class GapFixtureTest(unittest.TestCase):
    def source(self, root, prefix_b=None):
        src = root / 'src'
        src.mkdir(parents=True)
        prefix = [1] * 200
        variants = []
        for vid, tail in (('a', [7] * 50), ('b', [9] * 80)):
            write_wav(src / f'{vid}.input.wav', (prefix_b if vid == 'b' and prefix_b else prefix) + tail)
            variants.append({'id': vid, 'expect': {'after': f'{vid}-w0', 'within': 5},
                             'events': [{'id': f'{vid}-w0', 'source_start': 200_000_000,
                                         'source_end': 250_000_000, 'available_at': 300_000_000}]})
        pair = {'id': 'x-01', 'prefix': [{'id': 'p', 'source_start': 0, 'source_end': 200_000_000,
                                          'available_at': 250_000_000}], 'variants': variants}
        (src / 'pair.json').write_text(json.dumps(pair))
        (src / 'recordings.json').write_text('{}')
        return src

    def test_inserts_silence_and_shifts_only_branch_events(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            gap_fixture.derive(self.source(root), root / 'out', 0.5)
            pair = json.loads((root / 'out' / 'pair.json').read_text())
            self.assertEqual(pair['prefix'][0]['available_at'], 250_000_000)
            event = pair['variants'][0]['events'][0]
            self.assertEqual((event['source_start'], event['available_at']), (700_000_000, 800_000_000))
            self.assertEqual(pair['variants'][0]['expect']['within'], 5)
            pcm = read_wav(root / 'out' / 'a.input.wav')
            self.assertEqual(pcm, [1] * 200 + [0] * 500 + [7] * 50)
            audit = json.loads((root / 'out' / 'preparation-audit.json').read_text())
            self.assertTrue(all(b['unchanged_feedback_pcm'] for b in audit['branches']))

    def test_rejects_branches_without_identical_prefix(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            src = self.source(root, prefix_b=[2] * 200)
            with self.assertRaisesRegex(ValueError, 'identical pre-feedback prefix'):
                gap_fixture.derive(src, root / 'out', 0.5)


    def test_verify_accepts_shared_prefix_and_rejects_differing_one(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            report = gap_fixture.verify(self.source(root))
            self.assertEqual(report['shared_prefix_samples'], 200)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with self.assertRaisesRegex(ValueError, 'identical pre-feedback prefix'):
                gap_fixture.verify(self.source(root, prefix_b=[1] * 199 + [3]))


    def test_branches_with_different_onsets_share_audio_to_the_earliest(self):
        # Revision pairs deliver the same feedback at different times.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            src = self.source(root)
            pair = json.loads((src / 'pair.json').read_text())
            pair['variants'][1]['events'][0].update(source_start=250_000_000, source_end=300_000_000,
                                                    available_at=350_000_000)
            (src / 'pair.json').write_text(json.dumps(pair))
            write_wav(src / 'b.input.wav', [1] * 200 + [0] * 50 + [9] * 80)
            self.assertEqual(gap_fixture.verify(src)['shared_prefix_samples'], 200)
            gap_fixture.derive(src, root / 'out', 0.5)
            self.assertEqual(read_wav(root / 'out' / 'b.input.wav'), [1] * 200 + [0] * 50 + [0] * 500 + [9] * 80)


    def test_selected_variants_only(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            gap_fixture.derive(self.source(root), root / 'out', 0.5, ['b'])
            pair = json.loads((root / 'out' / 'pair.json').read_text())
            self.assertEqual(pair['variants'][0]['events'][0]['source_start'], 200_000_000)
            self.assertEqual(pair['variants'][1]['events'][0]['source_start'], 700_000_000)
            self.assertEqual(read_wav(root / 'out' / 'a.input.wav'), [1] * 200 + [7] * 50)
            self.assertEqual(read_wav(root / 'out' / 'b.input.wav'), [1] * 200 + [0] * 500 + [9] * 80)
            with self.assertRaisesRegex(ValueError, 'unknown variants'):
                gap_fixture.derive(self.source(root / 'x'), root / 'out2', 0.5, ['nope'])


if __name__ == '__main__':
    unittest.main()
