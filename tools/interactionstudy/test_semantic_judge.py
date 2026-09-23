import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('semantic_judge', Path(__file__).with_name('semantic_judge.py'))
judge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(judge)

PAIR = {'id': 'x', 'instructions': 'Be helpful.', 'prefix': [{'text': 'start with butter'}],
        'variants': [{'id': 'corrected', 'label': 'use olive oil', 'events': [{'text': 'use olive oil'}],
                      'expect': {'require_any_of': [['olive oil']], 'forbid': ['butter']}}]}


def reply(content):
    return io.BytesIO(json.dumps({'choices': [{'message': {'content': content}}]}).encode())


class JudgeTest(unittest.TestCase):
    def test_goal_names_request_label_and_forbidden_content(self):
        text = judge.goal(PAIR, PAIR['variants'][0])
        for part in ('start with butter', 'use olive oil', 'olive oil', 'It must not say: butter'):
            self.assertIn(part, text)

    def test_pair_verdicts(self):
        self.assertEqual(judge.pair_verdict(True, False), 'feedback-only-pass')
        self.assertEqual(judge.pair_verdict(True, True), 'non-discriminating')
        self.assertEqual(judge.pair_verdict(False, True), 'non-discriminating')
        self.assertEqual(judge.pair_verdict(False, False), 'no-feedback-pass')

    def test_takes_last_verdict_and_rejects_malformed_replies(self):
        with patch('urllib.request.urlopen', return_value=reply('draft {"meets": true} final {"meets": false, "quote": ""}')):
            self.assertFalse(judge.ask('http://judge.invalid', 'm', 'g', 't')['meets'])
        for bad in ('no json here', '{"meets": "yes"}'):
            with patch('urllib.request.urlopen', return_value=reply(bad)), self.assertRaises(ValueError):
                judge.ask('http://judge.invalid', 'm', 'g', 't')


if __name__ == '__main__':
    unittest.main()


class GeminiBackendTest(unittest.TestCase):
    def test_parses_structured_verdict_and_sends_audio_inline(self):
        import tempfile, types
        seen = {}

        def fake(request, timeout):
            seen['body'] = json.loads(request.data)
            seen['key'] = request.get_header('X-goog-api-key')
            return io.BytesIO(json.dumps({'modelVersion': 'm-001', 'candidates': [{'content': {'parts': [
                {'text': '{"meets": true, "quote": "olive oil"}'}]}}]}).encode())
        with tempfile.NamedTemporaryFile(suffix='.wav') as wav, patch.dict('os.environ', {'GEMINI_API_KEY': 'k'}), \
                patch('urllib.request.urlopen', fake):
            wav.write(b'RIFF')
            wav.flush()
            verdict = judge.ask_gemini('m', 'goal', audio=wav.name)
        self.assertEqual((verdict['meets'], verdict['model_version']), (True, 'm-001'))
        parts = seen['body']['contents'][0]['parts']
        self.assertEqual(parts[1]['inline_data']['mime_type'], 'audio/wav')
        self.assertIn('audio recording', seen['body']['systemInstruction']['parts'][0]['text'])
        self.assertEqual(seen['key'], 'k')
        args = types.SimpleNamespace(backend='local', url='u', model='m')
        with self.assertRaisesRegex(ValueError, 'requires the gemini backend'):
            judge.judge(args, 'goal', audio=wav.name)
